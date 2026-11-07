package subprocess

// P07e parent-side handoff coordinator: one shared section, one
// prepared handoff worker per selected core, and a parent-arbitrated
// schedule that hands every block from its owner (writer) to a
// reader on another core - preferring a different last-level-cache
// domain when the OS's cpu-set view can see more than one.
//
// The ordering guarantee lives in transition.Arbiter: a reader is
// only ever granted a block after the writer's WRITTEN ack reached
// THIS parent, and only one lease is outstanding per block. The
// workers never talk to each other, so there is no distributed race
// to lose - a mismatch a reader reports is memory-path evidence,
// never an artifact of tester ordering.
//
// Every lease step is journaled as a session_lease record the moment
// its ack (or its failure) is observed: the journaled steps ARE the
// reproduction artifact for the order (like the P08 schedule
// windows), and the seeded PRNG order is reproducible on top.
//
// Exit vocabulary is the shared one: fault 1 > setup 2 > interrupted
// 130 > clean 0, and every path reaps every worker: no handoff run
// ever leaves a process holding a mapping.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// HandoffSpec is one handoff phase: the cores to pair, the uniform
// block geometry of the shared section, how many rounds every block
// goes through, and the seeds. Seed drives both the schedule order
// and the ram-handoff image (one knob; the journal records every
// step that actually ran regardless).
type HandoffSpec struct {
	Cores       []int
	BlockBytes  uint64
	Blocks      int
	Rounds      int
	Seed        uint64
	Lock        string // memory lock policy for the engine regions
	Beat        time.Duration
	JournalPath string // the journal FILE itself (journal.Path output; not idempotent - apply once)
	RunID       string
	Timeouts    transition.Timeouts

	// Opt is the resolved safety request for this run (audit B09):
	// the SAME Guard the threaded and transition engines run -
	// temperature limit (MaxTempC), stop-on-fault policy, control
	// core for the observers (-1 = unpinned). The CLI fills it from
	// config; MaxTempC 0 means no thermal control is possible, which
	// the Guard reports as such rather than pretending safety.
	Opt plan.Options

	// OnLease (P07e S4) receives the parent's view of every lease
	// step at the SAME moment its session_lease journal record is
	// written - same core, same LeaseObservation value, no re-stated
	// fields. The callback runs on the lease loop's own goroutine:
	// it must never block the coordinator (the CLI's event sink
	// treats these records as droppable and counts drops; the
	// journal record stays the complete order artifact that the
	// S3 replay verifies). nil = no stream watching: the helper form
	// and every non-managed run pay nothing.
	OnLease func(core int, obs journal.LeaseObservation)

	// tamper is a TEST-ONLY seam called with a block's view between
	// the WRITTEN ack and the read lease: it proves the whole chain
	// reports injected damage as a kernel fault. Production specs
	// leave it nil; nothing in this file ever damages memory itself.
	tamper func(block uint64, view []byte)
}

// HandoffOutcome summarizes one handoff phase.
type HandoffOutcome struct {
	HandoffsTried int    // blocks whose write lease was sent
	HandoffsDone  int    // full write+read handoffs that completed verified
	Fault         string // a worker reported a kernel fault ("" = none)
	FaultCore     int    // logical CPU of the worker that reported the fault (-1 = none/not attributed)
	SetupErr      string // setup/resource failure or an arbiter refusal ("" = none)
	Interrupted   bool
	Checks        uint64 // words verified across READ_OK acks
	ChecksKnown   bool   // false once any completed read omitted its count
	Exits         []int  // worker exit codes, reap order
	LLCInfo       string // what the OS cpu-set view could see about cache domains
}

// RunHandoff drives one handoff phase and writes the session_lease
// records to jw. It always reaps every worker it started.
func RunHandoff(ctx context.Context, spec HandoffSpec, jw *journal.Writer) HandoffOutcome {
	out := HandoffOutcome{ChecksKnown: true, FaultCore: -1}
	timeouts := spec.Timeouts // zero fields fall back to the documented defaults via ForKind

	refuse := func(format string, a ...any) HandoffOutcome {
		out.SetupErr = fmt.Sprintf(format, a...)
		return out
	}

	n := len(spec.Cores)
	if n < 2 {
		return refuse("handoff needs at least two cores (a block handed to itself proves nothing): got %d", n)
	}
	// B10 (audit): every selected CPU must exist on THIS machine and
	// must be selected exactly once - before any allocation or spawn.
	// A duplicate is not a smaller handoff: a block handed from a
	// core to "another" worker on the same core exchanges nothing
	// across cores, the coverage claim would be a lie, and the two
	// workers would collide on journal identities for that core.
	seen := map[int]bool{}
	for _, c := range spec.Cores {
		if c < 0 || c >= runtime.NumCPU() {
			return refuse("cpu %d does not exist on this machine (logical cpus 0..%d): a worker could never pin to it", c, runtime.NumCPU()-1)
		}
		if seen[c] {
			return refuse("cpu %d selected twice: a handoff between identical CPUs exchanges nothing between different cores, and the workers would collide on journal identities", c)
		}
		seen[c] = true
	}
	if spec.Blocks < n || spec.Blocks%n != 0 {
		return refuse("blocks (%d) must divide evenly across the %d selected cores: a ragged partition would silently change what each worker holds", spec.Blocks, n)
	}
	if spec.BlockBytes == 0 || spec.BlockBytes%4 != 0 {
		return refuse("block size %d must be a positive multiple of the 4-byte word the kernel verifies", spec.BlockBytes)
	}
	if spec.Rounds <= 0 {
		return refuse("handoff needs at least one round, got %d", spec.Rounds)
	}
	lock, lerr := memtest.ParseLockMode(spec.Lock)
	if lerr != nil {
		return refuse("memory lock: %v", lerr)
	}
	if uint64(spec.Blocks) > ^uint64(0)/spec.BlockBytes {
		return refuse("section size %d x %d overflows 64-bit bytes", spec.BlockBytes, spec.Blocks)
	}
	section := uint64(spec.Blocks) * spec.BlockBytes

	// A cancelled run touches nothing (audit B09: cancellation is
	// honored on prepare too, not only mid-lease): no mapping, no
	// worker, no journal lease records.
	if ctx.Err() != nil {
		out.Interrupted = true
		return out
	}
	// B09 (audit): the shared section passes the SAME budget rule the
	// memory engine uses - physical headroom, commit headroom,
	// checked arithmetic - BEFORE CreateSharedMapping. blocks x
	// block-size fitting in a uint64 is not an admission guarantee:
	// an oversized pagefile-backed section must be refused with the
	// constraint that blocks it, named.
	if _, berr := memtest.ComputeBudget(memtest.Request{Bytes: section}, sysinfo.MemorySnapshotNow(), memtest.DefaultTrialParams()); berr != nil {
		return refuse("shared section admission (%d x %d bytes): %v", spec.Blocks, spec.BlockBytes, berr)
	}

	m, err := memtest.CreateSharedMapping(section)
	if err != nil {
		return refuse("shared section: %v", err)
	}
	defer m.Close() // runs LAST: children hold inherited handles, and the section outlives the parent's own handle by design

	per := spec.Blocks / n
	var all []memtest.Grant
	perSlot := make([][]memtest.Grant, n)
	for i := 0; i < n; i++ {
		for j := 0; j < per; j++ {
			b := uint64(i*per + j)
			g := memtest.Grant{Worker: i, Region: j, Role: memtest.RoleDest, Offset: b * spec.BlockBytes, Requested: spec.BlockBytes}
			all = append(all, g)
			perSlot[i] = append(perSlot[i], g)
		}
	}
	if err := memtest.ValidateGrants(memtest.Budget{Bytes: section}, memtest.PartitionResult{Grants: all}); err != nil {
		return refuse("handoff grant plan rejected: %v", err)
	}

	blockIDs := make([]uint64, spec.Blocks)
	for b := range blockIDs {
		blockIDs[b] = uint64(b)
	}
	arb, err := transition.NewArbiter(blockIDs)
	if err != nil {
		return refuse("arbiter: %v", err)
	}

	// LLC domains and physical cores come from the OS's own cpu-set
	// view (sysinfo), labeled honestly (B10): unknown stays unknown,
	// an SMT-sibling pair is never promoted to a cross-core pair, a
	// same-domain pair is never promoted to a cross-llc pair.
	states, serr := sysinfo.CpuSetStates()
	var llc map[int]int
	var llcKnown bool
	out.LLCInfo, llc, llcKnown = handoffPairing(spec.Cores, states, serr)

	memPlan := &memPlanSpec{
		budgetBytes: section,
		allowance:   0,
		lock:        string(lock),
		ramSeed:     spec.Seed,
		grants:      perSlot,
	}

	type sess struct {
		h    *SessionHandle
		core int
	}
	sessions := make([]*sess, 0, n)
	// shutdown asks every started worker to stop cleanly, then reaps
	// every one of them. Idempotent: the deferred safety net runs
	// after the explicit calls and finds nothing left.
	shutdown := func() {
		for _, s := range sessions {
			_ = s.h.Stop(timeouts.ForKind(transition.CmdStop))
		}
		for _, s := range sessions {
			out.Exits = append(out.Exits, s.h.Reap(15*time.Second))
		}
		sessions = nil
	}
	defer shutdown()

	for i, core := range spec.Cores {
		// Cancellation honored on prepare too (B09): a cancel that
		// arrives before this worker is even started must not spawn
		// it "to be tidy".
		if ctx.Err() != nil {
			out.Interrupted = true
			return out // the deferred shutdown reaps what did start
		}
		argv := sessionWorkerArgs(core, "ram-handoff", spec.JournalPath, spec.RunID, spec.Beat, memPlan.forSlot(i))
		argv = append(argv,
			"--handoff",
			"--mem-shared-handle", strconv.FormatUint(uint64(m.Handle()), 10),
			"--mem-shared-size", strconv.FormatUint(section, 10))
		h, herr := StartSessionShared(argv, fmt.Sprintf("%s-hc%d", spec.RunID, core), timeouts, m.Handle())
		if herr != nil {
			out.SetupErr = fmt.Sprintf("handoff worker on core %d: %v", core, herr)
			shutdown()
			return out
		}
		h.Machine.AllowLeases()
		sessions = append(sessions, &sess{h: h, core: core})

		cmd, cerr := h.Command(transition.CmdPrepare)
		if cerr != nil {
			out.SetupErr = fmt.Sprintf("handoff prepare core %d: %v", core, cerr)
			shutdown()
			return out
		}
		if _, aerr := h.Await(cmd.Seq, transition.AckReady, timeouts.ForKind(transition.CmdPrepare)); aerr != nil {
			out.SetupErr = fmt.Sprintf("handoff prepare core %d: %v", core, aerr)
			shutdown()
			out.SetupErr += workerStory(h)
			return out
		}
		if ierr := h.AwaitIdle(timeouts.ForKind(transition.CmdIdle)); ierr != nil {
			out.SetupErr = fmt.Sprintf("handoff idle core %d: %v", core, ierr)
			shutdown()
			out.SetupErr += workerStory(h)
			return out
		}
	}

	// pickReader chooses the reader slot for one handoff: never the
	// writer itself, preferring a core in a DIFFERENT llc domain when
	// the OS view can see the domains, rotating deterministically by
	// (block, round) so the pairing is reproducible and every worker
	// gets its turn as a reader.
	pickReader := func(writer int, block uint64, round uint64) int {
		var pref, rest []int
		for i := 0; i < n; i++ {
			if i == writer {
				continue
			}
			if llcKnown && llc[spec.Cores[i]] != llc[spec.Cores[writer]] {
				pref = append(pref, i)
			} else {
				rest = append(rest, i)
			}
		}
		pool := pref
		if len(pool) == 0 {
			pool = rest
		}
		if len(pool) == 0 {
			return -1 // single-core phase: refused up front, unreachable
		}
		return pool[(block+round)%uint64(len(pool))]
	}

	rng := rand.New(rand.NewSource(int64(spec.Seed)))
	stop := false
	for r := 1; r <= spec.Rounds && !stop; r++ {
		for _, bi := range rng.Perm(spec.Blocks) {
			if ctx.Err() != nil {
				out.Interrupted = true
				stop = true
				break
			}
			block := uint64(bi)
			writer := int(block) / per
			reader := pickReader(writer, block, uint64(r))
			if reader < 0 {
				out.SetupErr = fmt.Sprintf("no reader available for block %d (arbiter wiring bug)", block)
				stop = true
				break
			}
			wCore, rCore := spec.Cores[writer], spec.Cores[reader]

			// --- write lease: the owner publishes its block.
			out.HandoffsTried++
			lw, rerr := arb.Reserve(transition.CmdLeaseWrite, block)
			if rerr != nil {
				// The arbiter refusing our own schedule is a wiring
				// bug in THIS file: said out loud, never retried.
				out.SetupErr = fmt.Sprintf("arbiter reserve (write block %d): %v", block, rerr)
				stop = true
				break
			}
			wcmd, werr := sessions[writer].h.CommandLease(transition.CmdLeaseWrite, block, lw.Gen)
			if werr != nil {
				arb.Cancel(block)
				out.SetupErr = fmt.Sprintf("send lease-write block %d: %v", block, werr)
				stop = true
				break
			}
			// The reservation commits under the stamped seq: the
			// ledger now tracks an OUTSTANDING lease, retired only
			// by Complete or Taint.
			if ierr := arb.Issue(wcmd.Seq, lw); ierr != nil {
				_ = arb.TaintReservation(block, "arbiter issue failed")
				out.SetupErr = fmt.Sprintf("arbiter issue (write block %d): %v", block, ierr)
				stop = true
				break
			}
			wack, werr := sessions[writer].h.Await(wcmd.Seq, transition.AckWritten, timeouts.ForKind(transition.CmdLeaseWrite))
			wobs := journalLease(jw, sessions[writer].core, sessions[writer].h, wcmd, "lease-write", block, lw.Gen, rCore, wack, werr)
			if spec.OnLease != nil {
				spec.OnLease(sessions[writer].core, wobs)
			}
			if werr != nil {
				_ = arb.TaintSeq(wcmd.Seq, "write lease failed")
				if errors.As(werr, new(*transition.WorkerError)) {
					out.Fault = werr.Error()
					out.FaultCore = wCore
				} else {
					out.SetupErr = werr.Error()
				}
				stop = true
				break
			}
			if cerr := arb.Complete(wcmd.Seq, transition.AckWritten); cerr != nil {
				out.SetupErr = fmt.Sprintf("arbiter complete (write block %d): %v", block, cerr)
				stop = true
				break
			}

			if spec.tamper != nil {
				if v, verr := m.View(block*spec.BlockBytes, spec.BlockBytes); verr == nil {
					spec.tamper(block, v)
				}
			}

			// --- read lease: a different core (different LLC domain
			// when one is visible) verifies the published generation.
			lr, rerr := arb.Reserve(transition.CmdLeaseRead, block)
			if rerr != nil {
				out.SetupErr = fmt.Sprintf("arbiter reserve (read block %d): %v", block, rerr)
				stop = true
				break
			}
			rcmd, rerr := sessions[reader].h.CommandLease(transition.CmdLeaseRead, block, lr.Gen)
			if rerr != nil {
				arb.Cancel(block)
				out.SetupErr = fmt.Sprintf("send lease-read block %d: %v", block, rerr)
				stop = true
				break
			}
			if ierr := arb.Issue(rcmd.Seq, lr); ierr != nil {
				_ = arb.TaintReservation(block, "arbiter issue failed")
				out.SetupErr = fmt.Sprintf("arbiter issue (read block %d): %v", block, ierr)
				stop = true
				break
			}
			rack, rerr := sessions[reader].h.Await(rcmd.Seq, transition.AckReadOK, timeouts.ForKind(transition.CmdLeaseRead))
			robs := journalLease(jw, rCore, sessions[reader].h, rcmd, "lease-read", block, lr.Gen, wCore, rack, rerr)
			if spec.OnLease != nil {
				spec.OnLease(rCore, robs)
			}
			if rerr != nil {
				_ = arb.TaintSeq(rcmd.Seq, "read lease failed")
				if errors.As(rerr, new(*transition.WorkerError)) {
					out.Fault = rerr.Error()
					out.FaultCore = rCore
				} else {
					out.SetupErr = rerr.Error()
				}
				stop = true
				break
			}
			if cerr := arb.Complete(rcmd.Seq, transition.AckReadOK); cerr != nil {
				out.SetupErr = fmt.Sprintf("arbiter complete (read block %d): %v", block, cerr)
				stop = true
				break
			}
			if rack.Checks != nil {
				out.Checks += *rack.Checks
			} else {
				out.ChecksKnown = false
			}
			out.HandoffsDone++
		}
	}

	shutdown()
	return out
}

// handoffPairing labels what the OS cpu-set view can and cannot
// promise about the selected CPUs (audit B10). LLC domains and
// physical cores are SEPARATE facts stated separately: the wording
// never promotes an SMT-sibling pair to a cross-core pair, never a
// same-domain pair to a cross-llc pair, and unknown stays unknown.
// The returned llc map + llcKnown drive the reader preference; the
// info string is what the report says out loud.
func handoffPairing(cores []int, states []sysinfo.CpuSetState, viewErr error) (string, map[int]int, bool) {
	llc := make(map[int]int, len(states))
	coreOf := make(map[int]int, len(states))
	for _, st := range states {
		llc[st.LogicalCPU] = st.LastLevelCache
		coreOf[st.LogicalCPU] = st.Core
	}
	known := viewErr == nil
	for _, c := range cores {
		if _, ok := llc[c]; !ok {
			known = false
			break
		}
	}
	if !known {
		return "llc domains unknown (cpu-set view unavailable for the selected cores): pairing by core only", llc, false
	}
	n := len(cores)
	domains := map[int]bool{}
	phys := map[int]bool{}
	for _, c := range cores {
		domains[llc[c]] = true
		phys[coreOf[c]] = true
	}
	var info string
	switch {
	case len(phys) == 1:
		info = fmt.Sprintf("all %d selected logical cpus are SMT siblings of ONE physical core: pairing is cross-thread only, never cross-core", n)
	case len(domains) < 2:
		info = fmt.Sprintf("single llc domain observed across the %d selected cores (%d physical cores): cross-core only, no cross-llc pairing possible", n, len(phys))
	default:
		info = fmt.Sprintf("%d llc domains across %d cores (%d physical cores): readers preferred in a different domain", len(domains), n, len(phys))
	}
	if len(phys) > 1 && len(phys) < n {
		info += fmt.Sprintf(" · %d of the %d cpus share a physical core with another selected cpu (SMT siblings): those pairs are cross-thread, not cross-core", n-len(phys), n)
	}
	return info, llc, true
}

// journalLease writes the parent's view of one lease step the moment
// its outcome is known: acked or failed, counts as reported (nil =
// unknown, never zero), the interval labeled as an upper bound that
// includes the IPC round trip. It returns the very observation it
// journaled, so the S4 stream sees the identical value: one source
// per step, and the stream can never drift from the journal record.
func journalLease(jw *journal.Writer, core int, h *SessionHandle, cmd transition.Command, mode string, block, gen uint64, peerCore int, ack transition.Ack, err error) journal.LeaseObservation {
	obs := journal.LeaseObservation{
		Session:  h.Machine.Session(),
		Seq:      cmd.Seq,
		Block:    block,
		Gen:      gen,
		Mode:     mode,
		PeerCore: &peerCore,
		ToAckMS:  ms(time.Since(cmd.CommandedAt)),
		Acked:    err == nil,
		Checks:   ack.Checks,
		Bytes:    ack.Bytes,
	}
	if err != nil {
		obs.Error = err.Error()
	}
	jw.SessionLease(core, obs)
	return obs
}

// ExecuteHandoff runs one handoff phase as a top-level run: it owns
// the journal's run_start/run_end, classifies the outcome through
// the shared Finalize vocabulary (the same exit contract every
// executor keeps: fault 1, setup 2, interrupted 130, clean 0) and
// prints the summary.
//
// B09 (audit): the phase now runs the SAME shared Guard as the
// threaded and transition engines (the A01 lesson): the signal
// handler watches from the moment the Guard exists, the temperature
// preflight decides BEFORE any mapping or worker exists, the thermal
// cutoff and WHEA observation run through the phase, and the bounded
// WHEA drain lands before the verdict. The CLI now also saves the
// durable manifest before the first load (S2) and carries the two
// replay jobs in its internal helper form (S3: settings-repeat with
// a machine-drift gate, and offline lease-order verification through
// VerifyHandoffJournal below), and every executed lease step rides
// the -events jsonl stream when a run drives one (S4: the OnLease
// seam emits the journaled observation itself, field for field).
// S6 opened the public mode in the CLI: the tracked form (menu
// repeat, 'handoff -repeat', a kind=handoff request file and the bare
// command's public form) carries the session manifest, the attempt
// receipt, the lastresult binding and run_meta provenance, and runs
// through the single scenario dispatch. ExecuteHandoff itself stays
// the legacy helper-form seam: no tracking wrappers, journals
// byte-identical to the accepted P12-era records.
func ExecuteHandoff(spec HandoffSpec, panel *dashboard.Console) int {
	exit, _ := executeHandoffPhase(spec, panel, os.Stdout, nil)
	return exit
}

// ExecuteHandoffResult runs one handoff as a tracked run (the S5
// scenario phase and the S6 public standalone/request/repeat forms)
// and reports it as a plan.Result, so every pipeline classifies it
// through the same Finalize as every other executor - the run is not
// a second-class citizen of the handoff mode. The journal file is the
// phase's own log (the scenario session lists it, the per-phase files
// cycle through it) unless opt.HandoffJournal names the journal FILE
// itself - a manifest replay stores that file directly and must not
// have journal.Path's suffix applied twice. The run carries run_meta
// provenance (session id + plan hash the pipeline stamped - the
// helper form's journal stays byte-compatible, this path adds it),
// Esc on a live panel cancels through the shared Guard, and the
// caller hands in the OnLease binding for the S4 stream. Geometry was
// compiled and admission-gated up front (or re-gated by the public
// entry); ValidateHandoff here is defense in depth, same as the
// transition path revalidates.
func ExecuteHandoffResult(opt plan.Options, panel *dashboard.Console, onLease func(int, journal.LeaseObservation)) plan.Result {
	if err := plan.ValidateHandoff(opt); err != nil {
		fmt.Fprintf(os.Stderr, "handoff phase: %v\n", err)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: err.Error()})
	}
	journalFile := opt.HandoffJournal
	if journalFile == "" {
		journalFile = journal.Path(opt.LogFile)
	}
	spec := HandoffSpec{
		Cores:       plan.SelectedLogicalCPUs(opt.Cores, runtime.NumCPU()),
		BlockBytes:  opt.HandoffBlockBytes,
		Blocks:      opt.HandoffBlocks,
		Rounds:      opt.HandoffRounds,
		Seed:        opt.HandoffSeed,
		Lock:        opt.MemLock,
		Beat:        opt.BeatEvery,
		JournalPath: journalFile,
		Opt:         opt,
		OnLease:     onLease,
	}
	info := io.Writer(os.Stdout)
	if panel != nil && panel.Active() {
		info = panel.Writer() // the panel owns the screen: prints must not smear the frame
	}
	meta := plan.RunProvenance(opt)
	_, res := executeHandoffPhase(spec, panel, info, &meta)
	return res
}

// executeHandoffPhase is the shared lifecycle for both forms: journal
// identity, Guard, RunHandoff, Finalize and the honest summary. A nil
// provenance keeps the helper form's journal exactly as the accepted
// P12-era records wrote it; a scenario phase passes run_meta so its
// generation carries the session id and plan hash like every other
// engine's run.
func executeHandoffPhase(spec HandoffSpec, panel *dashboard.Console, info io.Writer, provenance *journal.RunMetaFields) (int, plan.Result) {
	// spec.JournalPath is the journal FILE itself (the CLI derives it
	// from the run log with journal.Path; Path is not idempotent and
	// must not be applied twice).
	jpath := spec.JournalPath
	jf, err := journal.OpenFile(jpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff: cannot open journal %q: %v\n", jpath, err)
		res := plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: fmt.Sprintf("cannot open journal %q: %v", jpath, err)})
		return res.Exit, res
	}
	spec.RunID = journal.NewRunID()
	jw := journal.NewWriter(jf, spec.RunID)
	jw.RunStart("handoff", "", spec.Cores, len(spec.Cores), "", true)
	if provenance != nil {
		jw.RunMeta(*provenance)
	}

	dash := panel // caller-owned in tests (fake sensor); nil = our own, never painting
	if dash == nil {
		dash = dashboard.New(os.Stdout)
		defer dash.Stop()
	}
	thermal := dash.Temperature()
	defer thermal.Disarm()
	guard := runctl.New(context.Background(), spec.Opt, runctl.Deps{
		Journal: jw,
		Temp:    thermal,
		Whea:    wheaFactoryForRun(spec.Opt.ControlCore),
	})
	defer guard.Stop()
	guard.Start()
	if panel != nil && panel.Active() {
		panel.SetOnEscape(func() { guard.Cancel(plan.ExecCancelled, "user esc") })
		defer panel.SetOnEscape(nil)
	}

	// Preflight BEFORE any allocation or spawn: a missing sensor
	// blocks the run (setup error), an already tripped limit is a
	// runtime condition. Neither is a CPU fault, and nothing ran.
	if ok, exec, f := guard.Preflight(); !ok {
		guard.Finish()
		res := plan.Finalize(plan.FinalInput{Execution: exec, Reason: f.Message,
			Findings: []plan.Finding{*f}})
		jw.RunEndValidation(res.Exit, string(res.Validation))
		if jerr, _ := jw.Errors(); jerr != nil {
			fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v\n", jerr)
		}
		jw.Close()
		fmt.Fprintf(os.Stderr, "handoff: %s\n", f.Message)
		return res.Exit, res
	}

	out := RunHandoff(guard.Ctx(), spec, jw)
	if out.Fault != "" && out.FaultCore >= 0 {
		guard.ReportFault(out.FaultCore, out.Fault) // stop-on-fault policy lives in the shared Guard
	}
	guard.Finish() // bounded WHEA drain: late records belong to this verdict
	aborted, abortMsg, abortKind := guard.AbortInfo()

	res := plan.Finalize(handoffFinalInput(out, guard.Findings(), aborted, abortMsg, abortKind))
	jw.RunEndValidation(res.Exit, string(res.Validation))
	if jerr, _ := jw.Errors(); jerr != nil {
		fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v\n", jerr)
	}
	jw.Close()

	fmt.Fprintf(info, "handoff: %d of %d block handoffs completed · %s", out.HandoffsDone, out.HandoffsTried, verificationPhrase(out))
	fmt.Fprintf(info, " · %s\n", out.LLCInfo)
	if out.Fault != "" {
		fmt.Fprintf(os.Stderr, "handoff fault on core %d: %s\n", out.FaultCore, out.Fault)
	}
	if out.SetupErr != "" {
		fmt.Fprintf(os.Stderr, "handoff: %s\n", out.SetupErr)
	}
	if out.Interrupted {
		fmt.Fprintln(os.Stderr, "handoff: interrupted - the remaining handoffs never ran")
	}
	return res.Exit, res
}

// verificationPhrase states what the completed reads actually proved.
// A READ_OK ack means every word of that block was recomputed and
// compared independently - but only counts the parent actually saw
// may back a "verified" claim; a missing count stays unknown.
func verificationPhrase(out HandoffOutcome) string {
	switch {
	case out.HandoffsDone == 0:
		return "no completed handoff to verify"
	case !out.ChecksKnown:
		return fmt.Sprintf("verification unknown (a completed read did not report its count; %d words seen)", out.Checks)
	default:
		return fmt.Sprintf("%d words independently verified", out.Checks)
	}
}

// handoffFinalInput turns the phase into the facts Finalize
// classifies - one vocabulary for the exit code and the validation
// claim, so the handoff command is not a second-class citizen. A
// reader's mismatch is memory evidence with the observing core
// recorded, never indicted (the A06 rule the session mode already
// keeps); the lease records in the journal carry the block, the
// generation and both cores for every step.
//
// B09: the Guard's run-level observations join the same verdict -
// a WHEA record during an otherwise clean handoff is a fault, and a
// thermal cutoff reclassifies "interrupted" as the runtime stop it
// actually was (the Guard's first cause wins over the loop's
// bookkeeping, exactly like every other engine).
func handoffFinalInput(out HandoffOutcome, guardFindings []plan.Finding, aborted bool, abortMsg string, abortKind plan.Execution) plan.FinalInput {
	in := plan.FinalInput{Execution: plan.ExecCompleted}
	u := plan.UnitInput{ChecksKnown: out.ChecksKnown, Checks: out.Checks, VerificationRequired: true}
	switch {
	case out.Fault != "":
		in.Execution = plan.ExecRuntimeError
		u.Execution = plan.ExecRuntimeError
		attr := plan.Attribution{Kind: plan.AttrMemoryRegion, Confidence: plan.ConfConfirmed}
		if out.FaultCore >= 0 {
			attr.ObservedOn = plan.IntPtr(out.FaultCore)
		}
		in.Findings = append(in.Findings, plan.Finding{
			Kind: plan.FindDataMismatch, Source: "workload", Message: out.Fault, Attribution: attr,
		})
	case out.SetupErr != "":
		in.Execution = plan.ExecSetupError
		u.Execution = plan.ExecSetupError
		in.Findings = append(in.Findings, plan.Finding{
			Kind: plan.FindSetupError, Source: "runner", Message: out.SetupErr,
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfUnknown},
		})
	case out.Interrupted:
		in.Execution = plan.ExecCancelled
		u.Execution = plan.ExecCancelled
	default:
		u.Execution = plan.ExecCompleted
	}
	in.Findings = append(in.Findings, guardFindings...)
	if aborted && abortKind != "" {
		in.Reason = abortMsg
		// The Guard's recorded cause outranks the loop's own
		// bookkeeping (thermal cutoff looks "interrupted" from the
		// lease loop; it was a runtime stop). A confirmed fault
		// verdict and a setup failure keep their own class.
		if in.Execution == plan.ExecCompleted || in.Execution == plan.ExecCancelled {
			in.Execution = abortKind
			u.Execution = abortKind
		}
	}
	in.Units = []plan.UnitInput{u}
	return in
}
