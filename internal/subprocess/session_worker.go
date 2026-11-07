package subprocess

// P08 session mode: one prepared worker process serves many verified
// bursts instead of being spawned (and allocating) per phase. The
// parent sends versioned JSONL commands on our stdin; we answer with
// JSONL acks on stdout. stdout is protocol-only in this mode: any
// human message goes to stderr.
//
// Lifecycle: PREPARE opens the memory engine (validate grants, commit,
// first-touch) exactly once; the worker then blocks idle on the
// command channel (no busy spin on the pinned core). TRIGGER starts a
// burst of the configured workload; VERIFY stops it at the kernel's
// documented graceful boundary (the current chunk/batch is completed
// AND verified before the stop returns - written bytes never go
// unchecked) and reports the burst's counts; IDLE stops a burst
// marked incomplete; STOP releases everything and exits.
//
// Honesty rules held here: an allocation/setup refusal is an ERROR
// ack and exit 2 (never a CPU fault, never a silent retry); a kernel
// mismatch is an ERROR ack and exit 1 with the ram evidence written;
// a parent that closes the channel without STOP ends the session as
// an incomplete setup condition (exit 2), never as "ok"; the journal
// carries exactly one mem_done and one core_done per session, so
// bursts can never double-count the allocation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

type sessionParams struct {
	core         int
	workloadName string
	beat         time.Duration
	memBudget    uint64
	memAllowance uint64
	memLock      string
	ramSeed      uint64
	grants       []memtest.Grant
	jw           *journal.Writer
	cs           *coreCounter
	// Handoff (P07e): this session accepts block-ownership leases over
	// one shared section. SharedHandle/SharedSize describe the mapping
	// inherited from the parent (handle value + declared size); the
	// engine then allocates its regions as subviews of that section,
	// so the same Grant set names the same physical bytes in every
	// worker of the phase.
	Handoff      bool
	SharedHandle uintptr
	SharedSize   uint64
}

// burstResult is what a burst goroutine hands back.
type burstResult struct {
	iters uint64
	err   error
}

type workerSession struct {
	p      sessionParams
	enc    *json.Encoder
	engine *memtest.Engine
	// shared is the P07e section this session's engine was opened
	// over (nil for ordinary sessions). Lease operations address
	// blocks through this view: the section is shared by design, and
	// who may touch which block is the parent arbiter's decision,
	// serialized on the channel - the worker's own guard is the
	// section bound, never an ownership claim it cannot see.
	shared  *memtest.SharedMapping
	memAcct *journal.MemAccounting
	burst   context.CancelFunc
	burstCh chan burstResult
	burstID string
	// worker-local stamps for the burst_done record: when TRIGGER was
	// read, and when the burst goroutine actually began.
	burstTrigAt  time.Time
	burstStartAt time.Time
	// snapshots for per-burst counts: the counters are session-wide,
	// the acks must carry what THIS burst did.
	itersAtBurst    uint64
	ramBytesAtBurst uint64
	beatStop        chan struct{}
}

// runWorkerSession is the session-mode main loop. It returns the
// process exit code and has written its terminal journal records by
// then.
func runWorkerSession(p sessionParams) int {
	s := &workerSession{p: p, enc: json.NewEncoder(os.Stdout)}

	// The command channel is a reader goroutine so the main loop can
	// select between "new command", "burst finished on its own" (a
	// fault with no VERIFY pending must still surface) and nothing
	// else. Blocking idle = blocked on this channel, no spin.
	cmdCh := make(chan transition.Command)
	dec := json.NewDecoder(os.Stdin)
	go func() {
		for {
			var cmd transition.Command
			if err := dec.Decode(&cmd); err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "worker: session command channel: %v\n", err)
				}
				close(cmdCh) // parent gone or protocol broken: the loop cleans up
				return
			}
			cmdCh <- cmd
		}
	}()

	// The loop selects on BOTH channels: a burst can end on its own
	// (a fault with no VERIFY pending, or a workload that completes
	// by itself), and that must surface immediately - not wait for
	// the next command to notice.
	for {
		var cmd transition.Command
		var ok bool
		select {
		case cmd, ok = <-cmdCh:
			if !ok {
				// stdin closed without STOP: the session end was
				// never confirmed. That is an incomplete tool-level
				// condition (exit 2), never "ok" and never a CPU
				// fault.
				return s.finishSetup("session channel closed without STOP (parent gone?)")
			}
			code, done := s.handle(cmd)
			if done {
				return code
			}
			continue
		case res := <-s.burstCh:
			// burstCh is nil until the first TRIGGER (a nil channel
			// blocks forever), and cleared after every drain, so a
			// stale value can never wake this branch twice.
			if s.burst == nil {
				continue
			}
			s.burst = nil
			s.burstCh = nil
			close(s.beatStop)
			checks, bytes := s.burstDeltas()
			if res.err != nil {
				// The burst's partial verified work stays on the
				// record even as the fault verdict ends the session.
				s.burstRecord(0, false, checks, bytes)
				msg := res.err.Error()
				s.ackErr(0, msg) // unsolicited: no command owns the fault
				if workload.IsSetupError(res.err) {
					return s.finishSetup(msg)
				}
				return s.finishFault(msg)
			}
			// The workload finished on its own (every built-in loops
			// until cancelled, so this is the rare honest case): the
			// worker is idle again whether or not the parent knows.
			// No VERIFY closed the block, so the burst is not
			// "complete" - its counts are still recorded.
			s.burstRecord(0, false, checks, bytes)
			s.ack(transition.Ack{Seq: 0, Kind: transition.AckIdle})
		}
	}
}

func (s *workerSession) ack(a transition.Ack) {
	a.V = transition.ProtocolVersion
	if err := s.enc.Encode(a); err != nil {
		// stdout is the parent's pipe: a broken pipe means the parent
		// is gone. The main loop's EOF path handles cleanup; the ack
		// itself is simply lost, and we never spin on it.
		fmt.Fprintf(os.Stderr, "worker: session ack: %v\n", err)
	}
}

func (s *workerSession) ackErr(seq uint64, msg string) {
	s.ack(transition.Ack{Seq: seq, Kind: transition.AckError, Msg: msg})
}

// handle applies one command and reports (exitCode, sessionOver).
func (s *workerSession) handle(cmd transition.Command) (int, bool) {
	if cmd.V != transition.ProtocolVersion {
		s.ackErr(cmd.Seq, fmt.Sprintf("command protocol version %d, want %d", cmd.V, transition.ProtocolVersion))
		return 0, false
	}
	switch cmd.Kind {
	case transition.CmdPrepare:
		return s.prepare(cmd), false
	case transition.CmdTrigger:
		s.trigger(cmd)
		return 0, false
	case transition.CmdVerify:
		return s.closeBurst(cmd, true), false
	case transition.CmdIdle:
		return s.closeBurst(cmd, false), false
	case transition.CmdStop:
		return s.stop(cmd), true
	case transition.CmdLeaseWrite, transition.CmdLeaseRead:
		return s.lease(cmd)
	default:
		s.ackErr(cmd.Seq, fmt.Sprintf("unknown command %q", cmd.Kind))
		return 0, false
	}
}

// openSharedEngine (P07e) opens the engine over the section inherited
// from the parent instead of private commits. Refusals here are all
// setup/resource conditions: a bad or un-inherited handle, a mapping
// smaller than the declared size, non-uniform grant sizes (block ids
// are offset/size, so uniformity is part of the contract), or the
// platform having no shared layer at all (ErrUnsupported).
func (s *workerSession) openSharedEngine(lockMode memtest.LockMode) (*memtest.Engine, *journal.MemAccounting, error) {
	if s.p.SharedSize == 0 {
		return nil, nil, fmt.Errorf("handoff session without a shared mapping size (corrupted command line)")
	}
	size := s.p.grants[0].Requested
	for _, g := range s.p.grants {
		if g.Requested != size {
			return nil, nil, fmt.Errorf("handoff needs uniform grant sizes: worker=%d region=%d has %d, worker=%d region=%d has %d",
				s.p.grants[0].Worker, s.p.grants[0].Region, size, g.Worker, g.Region, g.Requested)
		}
	}
	sm, err := memtest.OpenSharedMapping(s.p.SharedHandle, s.p.SharedSize)
	if err != nil {
		return nil, nil, fmt.Errorf("shared mapping: %w", err)
	}
	alloc, err := sm.Allocator(s.p.grants)
	if err != nil {
		_ = sm.Close()
		return nil, nil, fmt.Errorf("shared allocator: %w", err)
	}
	// openMemEngineWith owns the allocator from here: every failure
	// path closes it, and its Close releases the view and this
	// process's handle.
	engine, acct, err := openMemEngineWith(context.Background(), s.p.memBudget, s.p.memAllowance, s.p.grants, lockMode, alloc)
	if err != nil {
		return nil, nil, err
	}
	s.shared = sm
	return engine, acct, nil
}

// lease answers one block-ownership command. The parent arbiter has
// already serialized ownership (one writer, readers only after
// WRITTEN); the worker's job is the DATA half of the contract:
//
//   - LEASE_WRITE: fill the block with the ram-handoff-v1 image for
//     (seed, generation), then PublicationBarrier BEFORE the WRITTEN
//     ack - the documented Windows store-visibility fence, not an
//     assumption that pipe writes fence memory;
//   - LEASE_READ: PublicationBarrier first, then verify every word
//     against the independently reconstructed image. A mismatch is a
//     kernel fault: evidence written, ERROR ack, exit 1. The tester
//     never raced (the arbiter says so), so a mismatch here is
//     memory-path evidence, exactly like every other ram-* finding.
//
// A lease on a session that did not opt into handoff is a wiring bug
// on the parent side: refused with an ERROR ack, session continues.
// A lease fault or plumbing failure ends the session immediately
// (done=true): the arbiter above us has no safe continuation for a
// block it can no longer trust.
func (s *workerSession) lease(cmd transition.Command) (int, bool) {
	if !s.p.Handoff {
		s.ackErr(cmd.Seq, fmt.Sprintf("session is not a handoff session: %s refused", cmd.Kind))
		return 0, false
	}
	if cmd.Block == nil || cmd.Gen == nil {
		s.ackErr(cmd.Seq, fmt.Sprintf("%s without block/gen payload", cmd.Kind))
		return 0, false
	}
	if s.engine == nil {
		s.ackErr(cmd.Seq, "leases need a prepared engine over the shared mapping")
		return 0, false
	}
	block, gen := *cmd.Block, *cmd.Gen
	// Lease blocks are addressed by id over the WHOLE section:
	// block id = offset / uniform region size (uniformity enforced at
	// PREPARE). The arbiter above us serialized who holds the block
	// and when; the worker's own guard is that the block lies inside
	// the section it was given - an out-of-section id is a wiring bug
	// on the parent side and is refused, never served from memory
	// outside the sharing.
	size := s.p.grants[0].Requested
	if block > ^uint64(0)/size || block*size >= s.p.SharedSize {
		s.ackErr(cmd.Seq, fmt.Sprintf("block %d is outside the shared section (%d bytes, block size %d)", block, s.p.SharedSize, size))
		return 0, false
	}
	view, verr := s.shared.View(block*size, size)
	if verr != nil {
		msg := fmt.Sprintf("handoff block %d: %v", block, verr)
		s.ackErr(cmd.Seq, msg)
		return s.finishSetup(msg), true
	}
	wordBase := block * size / 4
	rid := fmt.Sprintf("section=shared block=%d gen=%d size=%d", block, gen, size)

	if cmd.Kind == transition.CmdLeaseWrite {
		if err := workload.HandoffWrite(view, s.p.ramSeed, gen, wordBase); err != nil {
			msg := fmt.Sprintf("handoff lease-write: %v", err)
			s.ackErr(cmd.Seq, msg)
			return s.finishSetup(msg), true
		}
		if err := memtest.PublicationBarrier(); err != nil {
			msg := fmt.Sprintf("handoff publication barrier: %v", err)
			s.ackErr(cmd.Seq, msg)
			return s.finishSetup(msg), true
		}
		b := uint64(len(view))
		s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckWritten, Bytes: &b})
		return 0, false
	}

	if err := memtest.PublicationBarrier(); err != nil {
		msg := fmt.Sprintf("handoff publication barrier: %v", err)
		s.ackErr(cmd.Seq, msg)
		return s.finishSetup(msg), true
	}
	checks, mm, serr := workload.HandoffVerify(view, s.p.ramSeed, gen, wordBase, rid)
	if serr != nil {
		msg := fmt.Sprintf("handoff lease-read: %v", serr)
		s.ackErr(cmd.Seq, msg)
		return s.finishSetup(msg), true
	}
	if mm != nil {
		s.p.cs.ReportRamMismatch(*mm)
		msg := fmt.Sprintf("handoff lease-read fault: block=%d gen=%d region=%s offset=%d want=%08x got=%08x",
			block, gen, rid, mm.Offset, mm.Expected, mm.Actual)
		s.ackErr(cmd.Seq, msg)
		return s.finishFault(msg), true // a kernel mismatch: exit 1, evidence written, never a retry
	}
	b := uint64(len(view))
	s.p.cs.RamPass(b)
	s.p.cs.MemVerified(b)
	c := checks
	s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckReadOK, Checks: &c, Bytes: &b})
	return 0, false
}

// prepare opens the engine exactly once. Every failure is a setup or
// resource condition: ERROR ack + exit 2, never exit 1.
func (s *workerSession) prepare(cmd transition.Command) int {
	if s.engine != nil {
		s.ackErr(cmd.Seq, "PREPARE sent twice: the session allocated once")
		return 2
	}
	if s.p.memBudget > 0 && len(s.p.grants) == 0 {
		return s.finishSetup("memory engine: grants arrived without a budget (corrupted command line)")
	}
	if wl, ok := workload.Get(s.p.workloadName); ok && wl.RequiresMemory && s.p.memBudget == 0 {
		// Defense in depth: the parent's preflight should never send a
		// memory workload to a budget-less slot, but the child does
		// not trust that - it refuses outright rather than let the
		// kernel discover empty regions mid-burst.
		return s.finishSetup(fmt.Sprintf("workload %s needs the memory engine but this slot has no budget", s.p.workloadName))
	}
	lockMode, lerr := memtest.ParseLockMode(s.p.memLock)
	if lerr != nil {
		return s.finishSetup(fmt.Sprintf("memory engine: %v", lerr))
	}
	if s.p.memBudget > 0 && len(s.p.grants) > 0 {
		// No deadline here on purpose: first-touch of gigabytes is
		// legitimately slow, and the parent enforces its Prepare
		// timeout by killing us (killed = our story stays incomplete,
		// never a fake refusal).
		var engine *memtest.Engine
		var acct *journal.MemAccounting
		var merr error
		if s.p.Handoff {
			engine, acct, merr = s.openSharedEngine(lockMode)
		} else {
			engine, acct, merr = openMemEngine(context.Background(), s.p.memBudget, s.p.memAllowance, s.p.grants, lockMode)
		}
		if merr != nil {
			// The refusal reason goes to the parent verbatim in the
			// ERROR ack AND the journal: exit 2, never a CPU fault.
			msg := fmt.Sprintf("memory engine: %v", merr)
			s.ackErr(cmd.Seq, msg)
			return s.finishSetup(msg)
		}
		s.engine = engine
		s.memAcct = acct
		s.p.cs.regions = engine.Regions()
		s.p.cs.ramSeed = s.p.ramSeed
		if wl, ok := workload.Get(s.p.workloadName); ok && wl.RequiresMemory {
			seed := s.p.ramSeed
			s.memAcct.Seed = &seed
		}
	} else if s.p.memBudget > 0 {
		// Budget without grants for this slot: the parent's partition
		// skipped us - say so loudly, never run memory-less in
		// silence (the preflight should have kept a memory workload
		// out of this slot, so treat it as setup).
		return s.finishSetup("memory engine: no regions for this core (the budget skipped it)")
	}
	s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckReady})
	// READY then IDLE_ACK: the worker is about to block on the command
	// channel - the idle claim is the very next fact, not a promise.
	s.ack(transition.Ack{Seq: 0, Kind: transition.AckIdle})
	return 0
}

func (s *workerSession) trigger(cmd transition.Command) {
	if s.engine == nil && s.p.memBudget > 0 {
		s.ackErr(cmd.Seq, "TRIGGER before a successful PREPARE")
		return
	}
	if wl, ok := workload.Get(s.p.workloadName); ok && wl.RequiresMemory && len(s.p.cs.regions) == 0 {
		s.ackErr(cmd.Seq, fmt.Sprintf("workload %s needs engine regions and this slot has none", s.p.workloadName))
		return
	}
	bctx, cancel := context.WithCancel(context.Background())
	s.burst = cancel
	s.burstCh = make(chan burstResult, 1)
	s.burstID = cmd.Burst
	s.burstTrigAt = time.Now() // worker-local: when WE read TRIGGER
	s.itersAtBurst = s.p.cs.iters.Load()
	_, s.ramBytesAtBurst, _ = s.p.cs.ramCoverage()
	go func() {
		s.burstStartAt = time.Now() // the burst actually began inside the worker
		iters, werr := workload.RunOne(bctx, s.p.workloadName, s.p.cs)
		s.burstCh <- burstResult{iters, werr}
	}()
	// beat cadence lives only while a burst runs: idle must stay quiet.
	s.beatStop = make(chan struct{})
	go func() {
		tick := time.NewTicker(s.p.beat)
		defer tick.Stop()
		for {
			select {
			case <-s.beatStop:
				return
			case <-tick.C:
				s.p.jw.CoreBeat(s.p.core, s.p.cs.iters.Load())
			}
		}
	}()
	// STARTED is stamped inside the worker at burst acceptance: the
	// parent's trigger->STARTED interval is IPC+wake, an upper bound
	// it labels as such.
	s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckStarted, Burst: cmd.Burst})
}

// burstDeltas computes what THIS burst contributed: the counters are
// session-wide, the records and acks must carry the per-burst share
// (snapshot at TRIGGER, difference at close).
func (s *workerSession) burstDeltas() (checks, bytes uint64) {
	checks = s.p.cs.iters.Load() - s.itersAtBurst
	_, ramTotal, _ := s.p.cs.ramCoverage()
	return checks, ramTotal - s.ramBytesAtBurst
}

// burstRecord writes the worker-side burst_done journal record.
// complete=true only when VERIFY closed the block honestly; aborted
// or self-completed bursts still get a record - their partial counts
// are real observations, and silence would erase them. seq names the
// command that closed the burst (0 = none did).
func (s *workerSession) burstRecord(seq uint64, complete bool, checks, bytes uint64) {
	now := time.Now()
	s.p.jw.BurstDone(s.p.core, journal.BurstAccounting{
		Burst:       s.burstID,
		Seq:         seq,
		TriggeredAt: s.burstTrigAt.UTC().Format(time.RFC3339Nano),
		StartedAt:   s.burstStartAt.UTC().Format(time.RFC3339Nano),
		ClosedAt:    now.UTC().Format(time.RFC3339Nano),
		ExecMS:      float64(now.Sub(s.burstStartAt)) / float64(time.Millisecond),
		Checks:      checks,
		Bytes:       bytes,
		Complete:    complete,
	})
}

// closeBurst stops the running burst at the kernel's graceful
// boundary and reports. verified=true answers VERIFY (the block
// completed its honest close and counts ride the ack); verified=false
// answers IDLE (the burst is marked incomplete by the parent).
// A kernel fault outranks both: the ERROR ack and exit 1 with the
// evidence written.
func (s *workerSession) closeBurst(cmd transition.Command, verified bool) int {
	if s.burst == nil {
		s.ackErr(cmd.Seq, "no burst is running")
		return 0
	}
	s.burst() // graceful stop: the kernel finishes + verifies the current chunk/batch
	res := <-s.burstCh
	close(s.beatStop)
	s.burst = nil
	s.burstCh = nil
	itersDelta, bytesDelta := s.burstDeltas()
	if res.err != nil {
		// The burst's partial verified work is still a real
		// observation: record it before the fault verdict erases
		// nothing.
		s.burstRecord(cmd.Seq, false, itersDelta, bytesDelta)
		msg := res.err.Error()
		s.ackErr(cmd.Seq, msg)
		if workload.IsSetupError(res.err) {
			// A plumbing failure (unknown workload, no regions) is a
			// setup condition: exit 2, never the 1 that claims a CPU
			// fault.
			return s.finishSetup(msg)
		}
		return s.finishFault(msg)
	}
	// The worker-side record first: whether the parent ever learns
	// the counts, the journal story of this burst is complete.
	s.burstRecord(cmd.Seq, verified, itersDelta, bytesDelta)
	if verified {
		a := transition.Ack{Seq: cmd.Seq, Kind: transition.AckVerified, Burst: s.burstID, Checks: &itersDelta}
		if bytesDelta > 0 {
			b := bytesDelta
			a.Bytes = &b
		}
		s.ack(a)
		s.ack(transition.Ack{Seq: 0, Kind: transition.AckIdle})
		return 0
	}
	s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckIdle, Burst: s.burstID, Checks: &itersDelta})
	return 0
}

// stop ends the session cleanly: a running burst is closed gracefully
// first (its partial work stays honest), the engine releases, the
// terminal records are written, STOPPED is acked, exit 0.
func (s *workerSession) stop(cmd transition.Command) int {
	if s.burst != nil {
		s.burst()
		res := <-s.burstCh
		close(s.beatStop)
		s.burst = nil
		s.burstCh = nil
		checks, bytes := s.burstDeltas()
		// A burst still running at STOP is incomplete by
		// definition - its partial counts are recorded, never
		// credited as a closed verified block.
		s.burstRecord(cmd.Seq, false, checks, bytes)
		if res.err != nil {
			return s.finishFault(res.err.Error())
		}
	}
	s.ack(transition.Ack{Seq: cmd.Seq, Kind: transition.AckStopped})
	return s.finishOK()
}

// --- terminal paths (one core_done + one mem_done per session) ---

func (s *workerSession) releaseEngine() {
	if s.engine != nil {
		if cerr := s.engine.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "worker: memory release: %v\n", cerr)
		}
		s.engine = nil // Close is safe twice, but we never try
	}
}

func (s *workerSession) writeTerminal(result, msg string, elapsed time.Duration) {
	s.releaseEngine()
	// Same accounting rules as the one-shot path: only kernels whose
	// iterations ARE checks report a count (nil = unknown, never
	// zero); external-adapter slots carry explicit evidence state;
	// ram evidence rides when a ram kernel ran here (Total 0 clean is
	// a result, nil is "no ram kernel ran").
	var checks *uint64
	if workload.CountsIterationsAsChecks(s.p.workloadName) {
		n := s.p.cs.iters.Load()
		checks = &n
	}
	ev := s.p.cs.evidenceNow()
	if ev == "" && workload.VerificationOf(s.p.workloadName) == workload.VerifyExternal {
		ev = workload.EvUnknown
	}
	kv := workload.KernelVersionOf(s.p.workloadName)
	ranRam := false
	if wl, ok := workload.Get(s.p.workloadName); ok && wl.RequiresMemory {
		ranRam = true
	}
	ramEv := s.p.cs.ramEvidence(ranRam)
	if s.memAcct != nil {
		s.memAcct.Verified = s.p.cs.memVerified.Load()
		if uniq, total, passes := s.p.cs.ramCoverage(); passes > 0 {
			s.memAcct.VerifiedUnique = &uniq
			s.memAcct.VerifiedTotal = &total
		}
		s.p.jw.MemDone(s.p.core, *s.memAcct)
	}
	s.p.jw.CoreDoneRam(s.p.core, result, msg, elapsed, checks, ev, kv, ramEv)
}

func (s *workerSession) finishOK() int {
	s.writeTerminal(journal.OK, "", 0)
	return 0
}

func (s *workerSession) finishFault(msg string) int {
	s.writeTerminal(journal.Fault, msg, 0)
	fmt.Fprintln(os.Stderr, msg)
	return 1
}

func (s *workerSession) finishSetup(msg string) int {
	s.writeTerminal(journal.SetupError, "worker: "+msg, 0)
	fmt.Fprintln(os.Stderr, "worker: "+msg)
	return 2
}
