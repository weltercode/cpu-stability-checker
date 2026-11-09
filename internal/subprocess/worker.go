package subprocess

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/procjob"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// coreCounter is the minimal workload.Reporter for a worker process: it only
// needs iteration counts for the journal beats. In rotate mode it also
// closes the wake measurement: wakeStart is stamped the moment our frozen
// process thaws, and the first confirmed work after that reports how long
// the core took to really come back (the wake transient proxy).
type coreCounter struct {
	iters       atomic.Uint64
	wakeStart   atomic.Int64 // UnixNano of thaw, 0 = no wake pending
	pausedNS    atomic.Int64 // wall time frozen by rotation, excluded from load duration
	evidence    atomic.Value // string: latest protocol evidence state (external adapters)
	memVerified atomic.Uint64
	jw          *journal.Writer
	core        int
	// ramSeed is the configured seed for the ram-* family, handed to
	// seed-consuming workloads through the reporter and journaled
	// when such a workload actually ran (0 is a valid seed).
	ramSeed uint64
	// regions are this slot's memory-engine regions (nil = no
	// engine): handed to region-capable workloads through the
	// optional workload.RegionsReporter interface.
	regions []memtest.AllocatedRegion
	// ram evidence (P06): every mismatch is counted, only the first
	// journal.MaxRamFindings keep their details, and truncation is
	// flagged rather than hidden; pass coverage accumulates the
	// unique-vs-total byte counts the plan demands.
	ramMu       sync.Mutex
	ramFindings []journal.RamFinding
	ramTotal    uint64
	ramTrunc    bool
	ramUnique   uint64 // bytes covered by the most recent completed pass
	ramBytes    uint64 // cumulative verified bytes across completed passes
	ramPasses   uint64
}

// MemRegions, MemVerified and RamSeed implement workload.RegionsReporter:
// the engine's regions reach the workload, the byte count its last
// completed pass actually verified comes back for the journal -
// reported by the kernel that did the work, never inferred later -
// and the configured seed reaches the kernel that consumes it.
func (c *coreCounter) MemRegions() []memtest.AllocatedRegion { return c.regions }
func (c *coreCounter) MemVerified(bytes uint64)              { c.memVerified.Store(bytes) }
func (c *coreCounter) RamSeed() uint64                       { return c.ramSeed }

// Compile-time pin: workloads find their regions through a runtime
// interface assertion, so if coreCounter ever stopped implementing
// RegionsReporter the child would silently run them on the heap
// fallback. This line turns that drift into a compile error.
var _ workload.RegionsReporter = (*coreCounter)(nil)

// ReportRamMismatch and RamPass implement workload.RamReporter. The
// reporter - not the kernel - stamps the observation time and the
// slot's logical CPU, computes the XOR (so a misreported pair cannot
// hide which bits differ), caps the details at journal.MaxRamFindings
// while counting EVERY observation, and flags truncation instead of
// hiding it. PhysicalCPU stays nil: this OS API does not map logical
// CPUs to physical cores, and guessing one from the logical number
// would be fabrication, not evidence.
func (c *coreCounter) ReportRamMismatch(m workload.RamMismatch) {
	c.ramMu.Lock()
	defer c.ramMu.Unlock()
	c.ramTotal++
	if len(c.ramFindings) >= journal.MaxRamFindings {
		c.ramTrunc = true
		return
	}
	c.ramFindings = append(c.ramFindings, journal.RamFinding{
		Algorithm: m.Algorithm, Seed: m.Seed, Pass: m.Pass,
		Region: m.Region, Offset: m.Offset,
		Expected: m.Expected, Actual: m.Actual, Xor: m.Expected ^ m.Actual,
		LogicalCPU: c.core, PhysicalCPU: nil,
		Phase: m.Phase, When: time.Now().Format(time.RFC3339Nano),
	})
}

func (c *coreCounter) RamPass(bytes uint64) {
	c.ramMu.Lock()
	defer c.ramMu.Unlock()
	c.ramPasses++
	c.ramUnique = bytes
	c.ramBytes += bytes
}

// ramCoverage exposes the accumulated pass coverage under the lock.
func (c *coreCounter) ramCoverage() (unique, total uint64, passes uint64) {
	c.ramMu.Lock()
	defer c.ramMu.Unlock()
	return c.ramUnique, c.ramBytes, c.ramPasses
}

// ramEvidence assembles the slot's verdict: a ram kernel that ran
// clean still gets evidence (Total 0 is a result, not a shrug),
// while a slot that ran no ram kernel gets nil - unknown, never
// "clean".
func (c *coreCounter) ramEvidence(ranRam bool) *journal.RamEvidence {
	c.ramMu.Lock()
	defer c.ramMu.Unlock()
	if !ranRam && c.ramTotal == 0 && c.ramPasses == 0 {
		return nil
	}
	return &journal.RamEvidence{
		Findings:  append([]journal.RamFinding(nil), c.ramFindings...),
		Total:     c.ramTotal,
		Truncated: c.ramTrunc,
	}
}

// Compile-time pin for the evidence half of the contract too.
var _ workload.RamReporter = (*coreCounter)(nil)

// threadScope is one RAM-worker thread's reporter view (audit C01).
// The slot's shared counters, evidence and journal stay behind the
// embedded counter - the journal story is per slot and findings are
// attributed to the slot's logical CPU - but the regions this thread's
// kernel receives are ONLY the grants the parent assigned to it, and
// the per-pass byte accounting is kept per thread. The slot's coverage
// then sums every thread's own work: bytes are never double-counted
// because two threads were handed the same region, and one thread's
// last completed pass never loses to another's last-writer-wins store.
type threadScope struct {
	*coreCounter
	regions      []memtest.AllocatedRegion
	lastPass     atomic.Uint64 // this thread's most recent completed ram pass
	lastVerified atomic.Uint64 // this thread's most recent MemVerified report
}

func (t *threadScope) MemRegions() []memtest.AllocatedRegion { return t.regions }

// RamPass records this thread's own pass bytes, then accumulates the
// slot's shared totals through the embedded counter.
func (t *threadScope) RamPass(bytes uint64) {
	t.lastPass.Store(bytes)
	t.coreCounter.RamPass(bytes)
}

func (t *threadScope) MemVerified(bytes uint64) { t.lastVerified.Store(bytes) }

var _ workload.RegionsReporter = (*threadScope)(nil)
var _ workload.RamReporter = (*threadScope)(nil)

// ReportEvidence implements the optional workload.EvidenceReporter for
// the child's reporter: an external adapter says what the tool's
// protocol proved, and the child records it in its own core_done so
// the parent's verdict rests on the journal, not on guesses.
func (c *coreCounter) ReportEvidence(state string) { c.evidence.Store(state) }

func (c *coreCounter) evidenceNow() string {
	if v, ok := c.evidence.Load().(string); ok {
		return v
	}
	return ""
}

func (c *coreCounter) Add(n uint64) {
	c.iters.Add(n)
	if ws := c.wakeStart.Load(); ws != 0 && n > 0 {
		if c.wakeStart.CompareAndSwap(ws, 0) {
			c.jw.Wake(c.core, float64(time.Now().UnixNano()-ws)/1e6)
		}
	}
}
func (c *coreCounter) Beat() {}

// workerArgs builds one child's command line. Pure, so the contract with
// WorkerCmd is testable without spawning anything.
func workerArgs(core int, workloadName string, dur, settle, beat time.Duration, journalPath, runID string, workers int, mem *memPlanSpec) []string {
	args := append([]string{}, workerArgsPrefix...)
	args = append(args,
		"worker",
		"--core", strconv.Itoa(core),
		"--workload", workloadName,
		"--duration", dur.String(),
		"--journal", journalPath,
		"--run-id", runID,
		"--workers", strconv.Itoa(workers),
	)
	if settle > 0 {
		args = append(args, "--settle", settle.String())
	}
	if beat > 0 {
		args = append(args, "--beat", beat.String())
	}
	return memArgs(args, mem)
}

// WorkerCmd is the child side: pin to one core, run one workload, report to
// the shared journal. Exit codes: 0 ok, 1 fault, 2 setup error.
func WorkerCmd(args []string) int {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	core := fs.Int("core", -1, "logical CPU to pin this process to")
	workloadName := fs.String("workload", "", "workload to run")
	duration := fs.String("duration", "", "how long to run, e.g. 30s")
	journalPath := fs.String("journal", "", "shared journal file (appended)")
	runID := fs.String("run-id", "", "run id shared with the parent process")
	workers := fs.Int("workers", 1, "worker goroutines in this process")
	settle := fs.String("settle", "", "idle this long before load, then measure the wake")
	beat := fs.String("beat", "", "core_beat journal cadence (empty = 1s)")
	memBudget := fs.Uint64("mem-budget", 0, "phase memory budget in bytes (0 = engine off)")
	memAllowance := fs.Uint64("mem-allowance", 0, "service allowance carved out of the budget, bytes")
	memLock := fs.String("mem-lock", "", "page lock policy for engine regions: off/try/required")
	ramSeed := fs.Uint64("ram-seed", 0, "seed for seed-consuming workloads (ram-* family); 0 is a valid seed")
	session := fs.Bool("session", false, "session mode (P08): prepared worker serving verified bursts over stdin/stdout; no -duration, the session ends on STOP")
	handoff := fs.Bool("handoff", false, "handoff session (P07e): accept block-ownership leases over the shared mapping; requires -session")
	memSharedHandle := fs.Uint64("mem-shared-handle", 0, "inherited shared-mapping handle for handoff sessions (P07e; the handle value, passed via AdditionalInheritedHandles)")
	memSharedSize := fs.Uint64("mem-shared-size", 0, "shared-mapping size in bytes (P07e)")
	var memGrants grantFlag
	fs.Var(&memGrants, "mem-grant", "memory region grant worker:region:role:offset:len (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *core < 0 || *core >= runtime.NumCPU() {
		fmt.Fprintf(os.Stderr, "worker: -core must be in [0, %d]\n", runtime.NumCPU()-1)
		return 2
	}
	if *workloadName == "" {
		fmt.Fprintln(os.Stderr, "worker: -workload is required")
		return 2
	}
	if *workers < 1 {
		fmt.Fprintln(os.Stderr, "worker: -workers must be at least 1")
		return 2
	}
	// Session mode (P08) has no fixed window: the session lives until
	// STOP or the channel closes. A -duration here would be a
	// corrupted contract, so it is refused, not ignored.
	var dur time.Duration
	var err error
	if *session {
		if *duration != "" {
			fmt.Fprintln(os.Stderr, "worker: session mode takes no -duration: the session ends on STOP")
			return 2
		}
	} else {
		dur, err = time.ParseDuration(*duration)
		if err != nil || dur <= 0 {
			fmt.Fprintln(os.Stderr, "worker: -duration must be a positive duration")
			return 2
		}
	}
	// Handoff (P07e) is a session-mode kind: leases need the prepared
	// session channel, and a one-shot worker with these flags is a
	// corrupted contract - refused loudly, never half-honored.
	if *handoff && !*session {
		fmt.Fprintln(os.Stderr, "worker: -handoff requires -session")
		return 2
	}
	if *handoff && (*memSharedSize == 0 || *memSharedHandle == 0) {
		fmt.Fprintln(os.Stderr, "worker: -handoff requires -mem-shared-handle and -mem-shared-size (the inherited section)")
		return 2
	}
	var settleDur time.Duration
	if *settle != "" {
		settleDur, err = time.ParseDuration(*settle)
		if err != nil || settleDur < 0 {
			fmt.Fprintln(os.Stderr, "worker: -settle must be a non-negative duration")
			return 2
		}
	}
	beatEvery := time.Second
	if *beat != "" {
		beatEvery, err = time.ParseDuration(*beat)
		if err != nil || beatEvery <= 0 {
			fmt.Fprintln(os.Stderr, "worker: -beat must be a positive duration")
			return 2
		}
	}

	// Pinning is the whole point of this mode: the OS must not move us.
	if err := sysinfo.SetAffinity([]int{*core}); err != nil {
		fmt.Fprintf(os.Stderr, "worker: pin to core %d: %v\n", *core, err)
		return 2
	}
	// Mandatory affinity must be APPLIED, not merely attempted: read
	// the mask back from the OS. A job object or processor-group
	// restriction that leaves us unconfined makes this per-core mode
	// meaningless - that is a setup error, never a silent lie.
	mask, ok := sysinfo.ProcessAffinity()
	if !ok {
		fmt.Fprintf(os.Stderr, "worker: cannot read back our affinity: pin to core %d unconfirmed\n", *core)
		return 2
	}
	if *core < 64 && mask != uint64(1)<<uint(*core) {
		fmt.Fprintf(os.Stderr, "worker: pin to core %d not applied: process mask is %#x\n", *core, mask)
		return 2
	}
	runtime.GOMAXPROCS(1)

	// External workloads come from the config, same as in the parent.
	// R15 defense in depth: the parent refuses a broken config before
	// ever spawning, but a worker that sees an unparsable config.json
	// fails as a whole process instead of silently running with
	// defaults: an external kernel measured with no registration
	// journals a wrong story.
	extCfg, cerr := config.LoadOrDefault(config.DefaultPath)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "worker: configuration error: %v\n", cerr)
		return 2
	}
	workload.RegisterExternal(extCfg.External)

	// Each worker owns exactly one journal file: its own fragment
	// under the run's fragments dir. The parent never writes here and
	// the worker never writes to the main journal — no shared file, so
	// no OS must guarantee append atomicity. The parent creates and
	// registers the dir before spawning; MkdirAll here keeps a direct
	// worker invocation (no parent) working too, and is a no-op
	// otherwise.
	fragPath := journal.FragmentPath(*journalPath, *runID, *core)
	if err := os.MkdirAll(filepath.Dir(fragPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "worker: cannot create journal fragment dir: %v\n", err)
		return 2
	}
	jf, err := journal.OpenFile(fragPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "worker: cannot open journal fragment %q: %v\n", fragPath, err)
		return 2
	}
	jw := journal.NewWriter(jf, *runID)
	defer func() {
		// A child that could not journal its own story says so loudly:
		// the parent will see a missing core_done and must not guess.
		if jerr, jn := jw.Errors(); jerr != nil {
			fmt.Fprintf(os.Stderr, "worker: journal incomplete: %v (%d record(s) lost)\n", jerr, jn)
		}
		jw.Close()
	}()
	jw.CoreStart(*core, *workloadName)

	// Audit C01 closeout, child side: the one-shot path is made safe
	// by per-thread grant ownership (validated from argv below).
	// Session mode is a different, deliberate shape: exactly ONE
	// prepared engine per pinned core serves the whole schedule
	// (P08), and handoff ownership is the per-block grant plus the
	// parent-arbitrated lease (P07e) - proven end to end with the
	// corruption probe. --workers > 1 here would be PARSED AND NEVER
	// RUN: a request shape this project never silently drops, so the
	// child refuses it before any allocation - a visible setup error
	// (exit 2), never the 1 that would pose a software defect as a
	// memory fault. Defense in depth: the parent refuses the same
	// shape before spawning (plan preflight and the session driver).
	if *workers > 1 && *session {
		msg := fmt.Sprintf("worker: --workers %d with --session: session mode runs exactly one prepared engine per pinned core; the extra threads would be parsed and never run (audit C01 shape: silently ignored concurrency). workers>1 belongs to one-shot windows, where per-thread grant ownership is proven", *workers)
		jw.CoreDoneChecks(*core, journal.SetupError, msg, 0, nil, "", "")
		fmt.Fprintln(os.Stderr, msg)
		return 2
	}

	// The duration names the load window; settle time is budgeted on top,
	// so a settle phase never eats into the load it is meant to precede.
	ctx, cancel := context.WithTimeout(context.Background(), dur+settleDur)
	defer cancel()

	cs := &coreCounter{jw: jw, core: *core}

	// Session mode (P08): the engine opens on PREPARE (exactly once),
	// bursts run on command, and the terminal records are written by
	// the session loop. Everything below - fixed window, rotate S/R
	// channel, settle - belongs to the one-shot mode.
	if *session {
		return runWorkerSession(sessionParams{
			core: *core, workloadName: *workloadName, beat: beatEvery,
			memBudget: *memBudget, memAllowance: *memAllowance, memLock: *memLock,
			ramSeed: *ramSeed, grants: []memtest.Grant(memGrants),
			jw: jw, cs: cs,
			Handoff: *handoff, SharedHandle: uintptr(*memSharedHandle), SharedSize: *memSharedSize,
		})
	}

	// Memory engine: the parent computed the phase budget once and
	// handed this child its exact grants; the child re-validates them
	// BEFORE allocating (a tampered or corrupted command line is a
	// setup error, never an overlapping allocation), commits its
	// regions all-or-nothing, first-touches them (cancellable) and
	// applies the lock policy - all BEFORE the measured window. Every
	// failure here is a resource or setup condition: exit 2, never the
	// 1 that means CPU fault. The deferred Close releases every
	// committed byte on every path: cancel, fault, setup error, or
	// normal end.
	var memAcct *journal.MemAccounting
	lockMode, lerr := memtest.ParseLockMode(*memLock)
	if lerr != nil {
		// A garbled lock mode is a typo in the contract, checked even
		// when the engine turns out to be off for this slot: a wrong
		// word must surface, never default quietly.
		msg := fmt.Sprintf("worker: memory engine: %v", lerr)
		jw.CoreDoneChecks(*core, journal.SetupError, msg, 0, nil, "", "")
		fmt.Fprintln(os.Stderr, msg)
		return 2
	}
	if *memBudget == 0 && len(memGrants) > 0 {
		msg := "worker: memory engine: grants arrived without a budget (corrupted command line)"
		jw.CoreDoneChecks(*core, journal.SetupError, msg, 0, nil, "", "")
		fmt.Fprintln(os.Stderr, msg)
		return 2
	}
	// Audit C01 child-side ownership contract: with engine regions and
	// worker threads, the wire's grant Worker fields are LOCAL thread
	// indexes, every thread 0..workers-1 must own at least one grant,
	// and no grant may name a thread outside that range. For a
	// memory-verifying workload a broken ownership map (tampered,
	// corrupted or written by an older parent) is refused as a setup
	// error BEFORE allocating anything - never a thread silently
	// running over the whole set or over nothing. Loads that verify
	// nothing tolerate the odd map: their coverage is unchanged.
	verifyingEngineHere := len(memGrants) > 0
	if wl, ok := workload.Get(*workloadName); ok {
		verifyingEngineHere = verifyingEngineHere && (wl.RequiresMemory || *workloadName == "mem")
	}
	if verifyingEngineHere {
		owned := make([]bool, *workers)
		bad := ""
		for _, g := range memGrants {
			if g.Worker < 0 || g.Worker >= *workers {
				bad = fmt.Sprintf("grant names thread %d outside 0..%d", g.Worker, *workers-1)
				break
			}
			owned[g.Worker] = true
		}
		if bad == "" {
			for i, ok := range owned {
				if !ok {
					bad = fmt.Sprintf("thread %d owns no grant", i)
					break
				}
			}
		}
		if bad != "" {
			msg := fmt.Sprintf("worker: memory grant ownership invalid: %s (audit C01)", bad)
			jw.CoreDoneChecks(*core, journal.SetupError, msg, 0, nil, "", "")
			fmt.Fprintln(os.Stderr, msg)
			return 2
		}
	}

	if *memBudget > 0 && len(memGrants) > 0 {
		var merr error
		var engine *memtest.Engine
		engine, memAcct, merr = openMemEngine(ctx, *memBudget, *memAllowance, []memtest.Grant(memGrants), lockMode)
		if merr != nil {
			memAcct = nil // nothing stands: the accounting is unknown, not partial
			if ctx.Err() != nil {
				// The run window closed during pre-touch: that is a
				// cancellation, not a fault and not a setup error. The
				// engine already released what it had committed; fall
				// through to the normal cancellation path (the
				// workloads below return immediately on this context).
				fmt.Fprintf(os.Stderr, "worker: memory pre-touch cancelled: %v\n", merr)
			} else {
				msg := fmt.Sprintf("worker: memory engine: %v", merr)
				jw.CoreDoneChecks(*core, journal.SetupError, msg, 0, nil, "", "")
				fmt.Fprintln(os.Stderr, msg)
				return 2
			}
		} else {
			cs.regions = engine.Regions()
			cs.ramSeed = *ramSeed
			// The seed is journaled only when a workload that
			// consumes it ran: recording it for a workload that
			// never looked at it would claim a reproducibility
			// the run does not actually have.
			if wl, ok := workload.Get(*workloadName); ok && wl.RequiresMemory {
				seed := *ramSeed
				memAcct.Seed = &seed
			}
			defer func() {
				if cerr := engine.Close(); cerr != nil {
					fmt.Fprintf(os.Stderr, "worker: memory release: %v\n", cerr)
				}
			}()
		}
	} else if *memBudget > 0 {
		// The parent's budget could not give this slot even one page
		// set: say so loudly, never run memory-less in silence.
		fmt.Fprintln(os.Stderr, "worker: memory engine: no regions for this core (the budget skipped it)")
	}

	// One scope per worker thread: its kernel sees only its own grants
	// (empty view = no engine: the honest unknown every consumer
	// already handles), while beats, checks, wake and evidence stay on
	// the shared slot counter.
	scopes := make([]*threadScope, *workers)
	for i := range scopes {
		var regs []memtest.AllocatedRegion
		for _, r := range cs.regions {
			if r.Worker == i {
				regs = append(regs, r)
			}
		}
		scopes[i] = &threadScope{coreCounter: cs, regions: regs}
	}

	// Rotation control channel: in rotate mode the parent sends S to
	// freeze this core and R to thaw it. We suspend every external tool
	// we own first — the tools live in our job objects, which the parent
	// cannot reach through job nesting — acknowledge on stdout, then
	// freeze ourselves last. The ack means the parent never queues a
	// command we have not consumed, so S and R cannot arrive out of order.
	// SuspendProcess returns exactly when the parent thaws us: that is the
	// wake moment, and coreCounter reports the first confirmed work after it.
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			switch strings.TrimSpace(sc.Text()) {
			case "S":
				for _, j := range procjob.LiveJobs() {
					_ = j.Suspend()
				}
				fmt.Println("s")
				pauseStart := time.Now()
				if err := procjob.SuspendProcess(os.Getpid()); err == nil {
					cs.pausedNS.Add(int64(time.Since(pauseStart)))
					cs.wakeStart.Store(time.Now().UnixNano())
				}
			case "R":
				for _, j := range procjob.LiveJobs() {
					_ = j.Resume()
				}
			}
		}
	}()

	// Settle: hold the pinned core idle so the platform can let it go
	// deep, then start load and measure the wake from that instant. If
	// the run is cancelled during settle, the workloads below return
	// immediately and the normal clean-exit path applies.
	if settleDur > 0 {
		select {
		case <-time.After(settleDur):
		case <-ctx.Done():
		}
		cs.wakeStart.Store(time.Now().UnixNano())
	}

	// Telemetry for this core's load window: the child process lives
	// entirely on its pinned core, so its process cycle time over wall
	// time is that core's observed rate, and CoreTimes deltas give the
	// idle share. Settle is deliberately outside this window.
	cyc0, cycErr := sysinfo.ProcessCycleTimeSelf()
	ct0, ctErr := sysinfo.CoreTimes()
	loadStart := time.Now()
	pausedBase := cs.pausedNS.Load()

	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		tick := time.NewTicker(beatEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				jw.CoreBeat(*core, cs.iters.Load())
			}
		}
	}()

	var wg sync.WaitGroup
	var mu sync.Mutex
	faultMsg := ""
	setupMsg := ""
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, werr := workload.RunOne(ctx, *workloadName, scopes[i]); werr != nil {
				mu.Lock()
				if workload.IsSetupError(werr) {
					if setupMsg == "" {
						setupMsg = werr.Error()
					}
					cancel()
				} else if faultMsg == "" {
					faultMsg = werr.Error()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := min(dur, max(time.Duration(0), time.Since(loadStart)-time.Duration(cs.pausedNS.Load()-pausedBase)))
	cancel()
	<-beatDone

	windowMS := time.Since(loadStart).Milliseconds()
	var clockMHz float64
	if cycErr == nil && windowMS > 0 {
		if cyc1, err := sysinfo.ProcessCycleTimeSelf(); err == nil {
			clockMHz = float64(cyc1-cyc0) / float64(windowMS) / 1000 // cycles per microsecond = MHz
		}
	}
	var idlePct *float64
	if ctErr == nil && *core >= 0 && *core < len(ct0) {
		if ct1, err := sysinfo.CoreTimes(); err == nil && *core < len(ct1) {
			if v, ok := ct0[*core].IdlePct(ct1[*core]); ok {
				idlePct = &v
			}
		}
	}
	if windowMS > 0 {
		// Ship the self-measured window to the parent (who owns the
		// combined journal record); the worker itself writes no
		// telemetry record. Unknown pieces simply stay absent.
		publishWorkerTelemetry(workerTelemetry{ClockMHz: clockMHz, IdlePct: idlePct, WindowMS: windowMS})
	}

	// Confirmed verified checks: only self-checking kernels count their
	// iterations; external tools (beats/lines) and pure loaders stay
	// unknown, so the parent can never inflate coverage.
	var checks *uint64
	if workload.CountsIterationsAsChecks(*workloadName) {
		n := cs.iters.Load()
		checks = &n
	}
	// What the adapter's protocol proved for this slot: an external
	// tool with no adapter evidence is explicitly "unknown" (never a
	// silent gap the parent could misread as success); built-in
	// kernels carry no evidence string at all.
	ev := cs.evidenceNow()
	if ev == "" && workload.VerificationOf(*workloadName) == workload.VerifyExternal {
		ev = workload.EvUnknown
	}
	kv := workload.KernelVersionOf(*workloadName)
	// ram evidence: attached when a ram kernel ran on this slot
	// (RequiresMemory) or left any trace at all; nil otherwise -
	// unknown, never "clean". A clean ram run carries Total 0
	// evidence: "saw nothing wrong" is a result worth recording.
	ranRam := false
	if wl, ok := workload.Get(*workloadName); ok && wl.RequiresMemory {
		ranRam = true
	}
	ramEv := cs.ramEvidence(ranRam)
	// The engine's accounting rides to the journal before the core's
	// final verdict: requested/allocated/touched/lock come from the
	// engine and the OS, verified is the byte count the workload's
	// last completed pass reported through the reporter, and the
	// unique-vs-total coverage comes from the passes the ram kernel
	// declared. A slot whose engine never opened has no mem_done at
	// all - unknown numbers, never leftovers of a released allocation.
	if memAcct != nil {
		// With per-thread grant ownership (audit C01) the verified
		// numbers are the SUM of what each thread's last completed pass
		// reported over its own regions - never one thread's store
		// overwriting another's, and never the same bytes counted for
		// two threads.
		var verified, unique uint64
		for _, s := range scopes {
			verified += s.lastVerified.Load()
			unique += s.lastPass.Load()
		}
		memAcct.Verified = verified
		if _, total, passes := cs.ramCoverage(); passes > 0 {
			memAcct.VerifiedUnique = &unique
			memAcct.VerifiedTotal = &total
		}
		jw.MemDone(*core, *memAcct)
	}
	if setupMsg != "" {
		jw.CoreDoneRam(*core, journal.SetupError, setupMsg, 0, nil, ev, kv, ramEv)
		fmt.Fprintln(os.Stderr, setupMsg)
		return 2
	}
	if faultMsg != "" {
		jw.CoreDoneRam(*core, journal.Fault, faultMsg, elapsed, checks, ev, kv, ramEv)
		fmt.Fprintln(os.Stderr, faultMsg) // the parent quotes stderr in its message
		return 1
	}
	status := journal.OK
	if *workloadName == workload.Idle {
		status, elapsed = journal.Skipped, 0
	}
	jw.CoreDoneRam(*core, status, "", elapsed, checks, ev, kv, ramEv)
	return 0
}
