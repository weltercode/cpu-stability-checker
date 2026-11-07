package subprocess

// P08 transition mode entry: one PREPARED worker per selected core,
// each driven through the same prepared burst schedule (optionally
// staggered into waves). The parent journals the prepared timeline
// before anything spawns and both views of every burst (its own
// session_burst + the worker's burst_done), then recovers the
// journal and prints it: the report distinguishes the commanded
// transitions from the observations, and the recovery pass proves
// the story reads back.
//
// Exit contract held: fault 1, setup error 2, interrupted 130,
// clean 0. A fault on one core is never erased by another core's
// setup error, and an interrupted session never exits 0.

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// sessionWorkerArgs builds one worker's session argv: same identity
// flags as the one-shot worker, --session instead of a fixed window
// (the session ends on STOP), and the exact memory grants this slot
// was given (children never re-derive the budget). The test-seam
// prefix is NOT included here: StartSession applies it exactly once
// (double prefix would put the seam flags in front of the worker's
// own argv and break its flag parsing).
func sessionWorkerArgs(core int, workloadName, journalPath, runID string, beat time.Duration, mem *memPlanSpec) []string {
	args := []string{
		"worker",
		"--core", strconv.Itoa(core),
		"--workload", workloadName,
		"--journal", journalPath,
		"--run-id", runID,
		"--workers", "1",
		"--session",
	}
	if beat > 0 {
		args = append(args, "--beat", beat.String())
	}
	return memArgs(args, mem)
}

// scheduleSpan is the full prepared timeline across all cores: the
// sum of windows plus the stagger waves. It is the session's
// expected length - journaled at run_start so a reader knows what
// the run was supposed to last.
func scheduleSpan(sched transition.Schedule, stagger time.Duration, cores int) time.Duration {
	var span time.Duration
	for _, w := range sched.Windows {
		span += w.Gap + w.RunFor
	}
	if cores > 1 {
		span += time.Duration(cores-1) * stagger
	}
	return span
}

// ExecuteSession runs P08 transition mode and returns the process
// exit code (the CLI contract: fault 1, setup 2, interrupted 130).
func ExecuteSession(opt plan.Options, sched transition.Schedule, stagger time.Duration) int {
	exit, _, _ := executeSession(opt, sched, stagger, nil)
	return exit
}

// ExecuteSessionResult runs one transition-mode phase (prepared
// sessions driven by the Options' burst schedule) and reports it as a
// plan.Result, so the scenario pipeline classifies it through the same
// Finalize as every other executor. The shared safety lifecycle
// (temperature, WHEA, stop-on-fault, cancel) and the exit contract are
// identical to ExecuteSession: a phase is not a second-class citizen
// of the transition mode. The panel, when the menu hands one, owns the
// screen and its Esc key cancels the run.
func ExecuteSessionResult(opt plan.Options, panel *dashboard.Console) plan.Result {
	sched := transition.BuildSchedule(opt.Bursts, opt.BurstGap, opt.BurstRun, opt.BurstJitter, opt.BurstSeed)
	_, res, _ := executeSession(opt, sched, opt.BurstStagger, panel)
	return res
}

// sessionFinalInput turns the driven sessions into the reported facts
// Finalize classifies: one vocabulary for the CLI 'session' command and
// for the scenario pipeline, so a phase is not a second-class citizen
// of the transition mode. The workload name decides the fault class
// (A06): a ram-* session's mismatch is memory evidence with the CPU
// recorded as observed_on, never a confirmed CPU-blacklist entry.
func sessionFinalInput(workloadName string, cores []int, outcomes []SessionOutcome) plan.FinalInput {
	in := plan.FinalInput{Execution: plan.ExecCompleted}
	for i, o := range outcomes {
		u := plan.UnitInput{
			// The worker reported verified checks on every VERIFIED
			// ack; aborted bursts contributed none (see SessionOutcome).
			ChecksKnown:          o.ChecksKnown,
			Checks:               o.Checks,
			VerificationRequired: true,
		}
		switch {
		case o.Fault != "":
			u.Execution = plan.ExecRuntimeError
			if in.Execution != plan.ExecRuntimeError {
				in.Execution = plan.ExecRuntimeError
			}
			kind := plan.FindDataMismatch
			attr := plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(cores[i]), Confidence: plan.ConfConfirmed}
			switch workload.FaultClassOf(workloadName) {
			case workload.FaultMemory:
				// A06: the worker's ERROR ack does not carry region
				// detail, but the workload identity alone is enough:
				// this mismatch lives in engine memory. The CPU that
				// observed it is recorded, never indicted, and never
				// blacklisted.
				attr = plan.Attribution{Kind: plan.AttrMemoryRegion, Confidence: plan.ConfConfirmed, ObservedOn: plan.IntPtr(cores[i])}
			case workload.FaultSynthetic:
				// The deliberate prober proves the reporting path;
				// reports must not render it as a real mismatch.
				kind = plan.FindSyntheticProbe
			}
			in.Findings = append(in.Findings, plan.Finding{
				Kind: kind, Source: "workload", Message: o.Fault, Attribution: attr,
			})
		case o.SetupErr != "":
			u.Execution = plan.ExecSetupError
			if in.Execution == plan.ExecCompleted {
				in.Execution = plan.ExecSetupError
			}
			in.Findings = append(in.Findings, plan.Finding{
				Kind: plan.FindSetupError, Source: "runner", Message: o.SetupErr,
				Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfUnknown},
			})
		case o.Interrupted:
			u.Execution = plan.ExecCancelled
			if in.Execution == plan.ExecCompleted {
				in.Execution = plan.ExecCancelled
			}
		default:
			u.Execution = plan.ExecCompleted
		}
		in.Units = append(in.Units, u)
	}
	return in
}

// executeSession is the shared body: it returns the CLI exit code,
// the structured Result (the same facts classified by Finalize for
// every caller), and the selected cores. A refusal BEFORE any session
// started comes back as a finalized setup Result - no worker ever
// spawned for it.
func executeSession(opt plan.Options, sched transition.Schedule, stagger time.Duration, panel *dashboard.Console) (int, plan.Result, []int) {
	refuse := func(reason string) (int, plan.Result, []int) {
		res := plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: reason})
		return res.Exit, res, nil
	}
	if _, ok := workload.Get(opt.A); !ok {
		fmt.Fprintf(os.Stderr, "session: unknown workload %q (see 'list')\n", opt.A)
		return refuse(fmt.Sprintf("unknown workload %q", opt.A))
	}
	// Q04: the parking policy is resolved BEFORE any journal or process
	// opens: an unknown word is a visible setup error naming itself and
	// its vocabulary, never a silent fall back to off (the execution-
	// vocabulary rule). Off (the empty word) stays exactly today's run.
	parking, perr := transition.ParseParkingPolicy(opt.Parking)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "session: %v\n", perr)
		return refuse(perr.Error())
	}
	// Q04: a required policy on a machine where the observation
	// source cannot answer is a setup problem, said BEFORE any journal
	// or worker exists: refusing up front and naming the fix beats
	// starting a run whose promise was dead on arrival (exit 2
	// vocabulary, never a post-hoc surprise). try keeps asking
	// honestly instead - its answer may well be "unavailable", and
	// that answer gets journaled, not hidden.
	if parking == transition.ParkingRequired {
		if _, pprobeErr := parkingProbe(); pprobeErr != nil {
			msg := fmt.Sprintf("parking evidence required but source %s cannot be queried on this machine: %v (set parking_policy to \"try\" to keep the honest answer, or \"off\" to stop asking)", ParkingSourceName, pprobeErr)
			fmt.Fprintf(os.Stderr, "session: %s\n", msg)
			return refuse(msg)
		}
	}
	// Audit C01 closeout (parent choke point): a session worker is
	// one prepared engine per pinned core; workers>1 is the one-shot
	// shape. Refuse the corrupted combination here too - defense in
	// depth beside the plan preflight and the child gate - so no
	// entry point can reach the child with threads the session would
	// silently never run.
	if opt.Workers > 1 {
		fmt.Fprintf(os.Stderr, "session: workers=%d with prepared bursts: one engine per pinned core serves every burst; workers>1 is a one-shot shape with per-thread grant ownership\n", opt.Workers)
		return refuse(fmt.Sprintf("workers=%d in a prepared session: single-engine-per-core contract (workers>1 belongs to one-shot windows)", opt.Workers))
	}
	if len(sched.Windows) == 0 {
		fmt.Fprintln(os.Stderr, "session: the schedule has no windows")
		return refuse("the schedule has no windows")
	}
	cores := plan.SelectedLogicalCPUs(opt.Cores, runtime.NumCPU())
	if len(cores) == 0 {
		fmt.Fprintln(os.Stderr, "session: no cores selected")
		return refuse("no cores selected")
	}
	if _, err := workerExecutable(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return refuse(err.Error())
	}

	var memPlan *memPlanSpec
	if opt.Memory != "" {
		mp, merr := buildMemPlan(opt, len(cores))
		if merr != nil {
			fmt.Fprintf(os.Stderr, "memory engine: %v\n", merr)
			return refuse(fmt.Sprintf("memory engine: %v", merr))
		}
		memPlan = mp
	}

	jpath := journal.Path(opt.LogFile)
	jf, err := journal.OpenFile(jpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open journal %q: %v\n", jpath, err)
		return refuse(fmt.Sprintf("cannot open journal %q: %v", jpath, err))
	}
	runID := journal.NewRunID()
	jw := journal.NewWriter(jf, runID)
	span := scheduleSpan(sched, stagger, len(cores))
	jw.RunStart(opt.TestName, "", cores, 1, span.String(), true)
	jw.RunMeta(plan.RunProvenance(opt))

	// A01: the shared safety lifecycle - the same temperature
	// preflight and cutoff, the same WHEA observation with its final
	// drain, the same stop-on-fault policy and cancel path every
	// other executor runs. The transition mode must not drift.
	deps, releaseTemp := sessionDeps(opt, jw, panel)
	defer releaseTemp()
	guard := runctl.New(context.Background(), opt, deps)
	if ok, exec, f := guard.Preflight(); !ok {
		// A missing sensor blocks a safe run (setup); an already
		// tripped limit is a runtime condition. Neither is a CPU
		// fault, and no worker ever spawned.
		res := plan.Finalize(plan.FinalInput{Execution: exec, Reason: f.Message,
			Findings: []plan.Finding{*f}})
		jw.RunEndValidation(res.Exit, string(res.Validation))
		if jerr, _ := jw.Errors(); jerr != nil {
			fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v\n", jerr)
		}
		jw.Close()
		return res.Exit, res, cores
	}
	guard.Start()
	if panel != nil && panel.Active() {
		panel.SetOnEscape(func() { guard.Cancel(plan.ExecCancelled, "user esc") })
		defer panel.SetOnEscape(nil)
	}

	var info io.Writer = os.Stdout
	if panel != nil && panel.Active() {
		info = panel.Writer() // the panel owns the screen: direct prints would smear it
	}
	// One Print per logical block, not per line: on a live panel every
	// Print repaints the whole frame, and a line-by-line header would
	// already start the flicker storm before the run does.
	writeTransitionHeader(info, memPlan, len(cores), sched, span, parking)

	// Cancel (Esc, Ctrl+C, a thermal stop, an uncorrected WHEA record
	// or a peer's stop-on-fault) reaches every session through this
	// context: each aborts its running burst at the worker's honest
	// boundary, releases memory on the STOP path, and the run never
	// exits 0 and never claims a fault it did not observe.
	ctx := guard.Ctx()
	outcomes := make([]SessionOutcome, len(cores))
	var faultN atomic.Int32

	// The live panel's red context line: the most recent burst fault,
	// recorded at the moment it lands (the journal keeps the
	// authoritative copy; this is the "where did it break" display
	// next to the counter). Guarded by its own mutex: the push loop
	// and the per-core workers touch it concurrently.
	var fmu sync.Mutex
	lastFaultStr := ""

	// The panel gets the same run context every other executor pushes
	// (the subprocess engine does this too). Without it a transition
	// run paints as bare hardware bars whose idle/load dancing - the
	// commanded bursts really are toggling cores - reads as unrelated
	// noise: the bars are the fact side, and only a footer naming the
	// run, the elapsed time against the prepared span, and the
	// commanded burst position makes them readable. A pipe console
	// never paints, but Push/ClearRun still record the state - that
	// costs one store per second and lets tests observe the contract.
	if panel != nil {
		pushDone := make(chan struct{})
		go func() {
			defer close(pushDone)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			start := time.Now()
			runDesc := fmt.Sprintf("transition: %d core(s), %d burst(s) per core", len(cores), len(sched.Windows))
			for {
				select {
				case <-ctx.Done():
					// Run over: hand the (session-owned) panel back to
					// plain monitoring, never a stale run footer.
					panel.ClearRun()
					return
				case <-tick.C:
					elapsed := time.Since(start)
					pos := commandedBurst(sched, elapsed) + 1
					if pos > len(sched.Windows) {
						pos = len(sched.Windows) // trailing waves still ride out
					}
					status := fmt.Sprintf("commanded burst %d/%d", pos, len(sched.Windows))
					if stagger > 0 {
						status += fmt.Sprintf(" (core waves %s apart)", stagger)
					}
					fmu.Lock()
					lf := lastFaultStr
					fmu.Unlock()
					panel.Push(dashboard.Snapshot{
						Label: opt.TestName, Run: runDesc, Status: status, HasRun: true,
						Elapsed: elapsed, Duration: span,
						WheaN: int(guard.WheaCount()), WheaState: guard.WheaStateString(),
						Faults: int(faultN.Load()), LastFault: lf,
						TestMem: memPlan.testedBytes(),
					})
				}
			}
		}()
		defer func() { <-pushDone }()
	}

	var wg sync.WaitGroup
	for i, core := range cores {
		wg.Add(1)
		go func(i, core int) {
			defer wg.Done()
			if stagger > 0 && i > 0 {
				// Wave start: core i begins its session i*stagger
				// after the previous one. Cancel during the wait
				// starts the session anyway - RunSessionBursts sees
				// the cancelled ctx and ends it as interrupted
				// (with the journal records saying so), never as a
				// silent skip.
				timer := time.NewTimer(time.Duration(i) * stagger)
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
			}
			argv := sessionWorkerArgs(core, opt.A, jpath, runID, opt.BeatEvery, memPlan.forSlot(i))
			// Q04: with an active parking policy each core gets its own
			// observer, tied to this logical CPU, sampling the kernel
			// through the real poller (unavailable source on this
			// platform folds to unavailable, never to parked/not-parked).
			// Off policy = nil observer = not one extra syscall, and the
			// run's journal is byte-identical to before Q04.
			var park *ParkingObserver
			if parking.Active() {
				park = NewParkingObserver(core, ParkingSourceName, ParkingPollInterval, CpuSetPoller)
			}
			outcomes[i] = RunSessionBursts(ctx, argv, fmt.Sprintf("%s-c%d", runID, core), core,
				sched, transition.DefaultTimeouts(), jw, park)
			if outcomes[i].Fault != "" {
				// Stop-on-fault is the shared policy: with it on, a
				// faulting session cancels its peers instead of
				// letting the machine ride out to a thermal stop.
				faultN.Add(1)
				fmu.Lock()
				lastFaultStr = fmt.Sprintf("FAULT core %d · %s · %s · %s",
					core, opt.A, time.Now().Format("15:04:05"), outcomes[i].Fault)
				fmu.Unlock()
				guard.ReportFault(core, outcomes[i].Fault)
			}
		}(i, core)
	}
	wg.Wait()

	// Mark the workload end, stop the WHEA poller and drain late
	// records before the verdict is assembled (the consumer still
	// appends while the drain runs).
	guard.Finish()

	exit := sessionRunExit(outcomes)
	in := sessionFinalInput(opt.A, cores, outcomes)
	in.Findings = append(in.Findings, guard.Findings()...)
	if aborted, msg, _ := guard.AbortInfo(); aborted && in.Reason == "" {
		in.Reason = msg
	}
	res := plan.Finalize(in)
	// Q04 verdict gate - the required promise, enforced: no full
	// verification claim without observed parking evidence on every
	// core. The exit code is untouched (a clean but unevidenced run
	// ends 0 with a PARTIAL verification claim - exactly the shape
	// Finalize already uses for missing evidence elsewhere); and the
	// finding rides the structured result for try too: honest marking
	// with zero verdict power, per the policy's own meaning.
	parkLine := ""
	if parking.Active() {
		pfold := FoldParkingOutcomes(cores, outcomes)
		msg := pfold.Message()
		parkLine = fmt.Sprintf("parking (policy %q): %s", string(parking), msg)
		if parking == transition.ParkingRequired && pfold.Unevidenced() &&
			(res.Validation == plan.ValVerified || res.Validation == plan.ValExternalConfirmed) {
			res.Validation = plan.ValPartial
			msg += "; required: full verification withheld - the parking question was not answered with observed evidence on every core"
			parkLine += " -> verification partial"
		}
		res.Findings = append(res.Findings, plan.Finding{
			Kind: plan.FindParking, Source: "parking", Message: msg,
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed},
		})
	}
	// The structured verification claim rides into run_end beside the
	// FINAL exit code: run_end must agree with the Result the CLI
	// returns, or a report reads a different ending than the run had.
	// The preliminary sessionRunExit can disagree when Finalize judges
	// the contract (completed bursts that verified nothing are exit 1
	// to the tool while every child exited fine) - the journal must
	// carry the same exit 1 the receipt and menu then show.
	jw.RunEndValidation(res.Exit, string(res.Validation))
	if jerr, _ := jw.Errors(); jerr != nil {
		fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v\n", jerr)
	}
	jw.Close()

	// The summary and the recovered journal report go out as ONE
	// block: on a live panel every Print repaints the whole frame, so
	// writing the report line by line was a repaint storm (the panel
	// visibly flickered while the report assembled itself). The
	// reader still sees the prepared schedule, the parent's
	// transition observations and the worker's burst records as
	// separate evidence, exactly as written.
	writeSessionReport(info, jpath, cores, outcomes, parkLine)
	return exit, res, cores
}

// writeTransitionHeader states the run shape in a single write.
func writeTransitionHeader(w io.Writer, memPlan *memPlanSpec, nCores int, sched transition.Schedule, span time.Duration, parking transition.ParkingPolicy) {
	var head strings.Builder
	if memPlan != nil {
		fmt.Fprintf(&head, "memory engine: %s\n", memPlan.rule)
		if memPlan.skipped > 0 {
			fmt.Fprintf(&head, "memory engine: %d of %d core(s) received no regions (the budget cannot give them a page set)\n",
				memPlan.skipped, nCores)
		}
	}
	fmt.Fprintf(&head, "transition mode: %d core(s), %d burst(s) per core, prepared span %s\n",
		nCores, len(sched.Windows), span)
	if parking.Active() {
		// Q04: the observation half of the contract is stated where
		// the run shape is stated - what will be sampled, from which
		// source, at which cadence, before the first sample exists.
		fmt.Fprintf(&head, "parking observation: policy %q, source %s, sampled every %s inside each session's measured idle windows\n",
			string(parking), ParkingSourceName, ParkingPollInterval)
	}
	fmt.Fprint(w, head.String())
}

// writeSessionReport assembles the summary plus the recovered journal
// report into exactly one write - the panel repaints once for the
// whole block instead of once per line. The parking line (empty = no
// observation ran) rides the summary half: the observation story
// belongs to the summary of what happened, and the per-core detail
// belongs to the journal records.
func writeSessionReport(w io.Writer, jpath string, cores []int, outcomes []SessionOutcome, parkLine string) {
	var report strings.Builder
	printSessionSummary(&report, cores, outcomes)
	if parkLine != "" {
		fmt.Fprintf(&report, "%s\n", parkLine)
	}
	recs, iss := journal.ReadAll(jpath)
	if iss.ReadErr != "" {
		fmt.Fprintf(os.Stderr, "warning: journal recovery read error: %s\n", iss.ReadErr)
	} else {
		journal.PrintReport(&report, journal.RecoverRuns(recs))
	}
	fmt.Fprint(w, report.String())
}

// commandedBurst reports how many of the leading core's prepared
// windows have ENDED by t. It is the command side only - what the
// schedule ordered - never a claim about what any worker observed;
// the observations live in the journal.
func commandedBurst(sched transition.Schedule, t time.Duration) int {
	var acc time.Duration
	done := 0
	for _, w := range sched.Windows {
		acc += w.Gap + w.RunFor
		if t < acc {
			break
		}
		done++
	}
	return done
}

// sessionDeps wires the shared lifecycle observers. The panel (when
// the menu handed one) owns the temperature monitor and the screen;
// a standalone session creates its own monitor and releases it. The
// WHEA factory goes through a package seam so tests can script the
// observation without the Windows event log.
func sessionDeps(opt plan.Options, jw *journal.Writer, panel *dashboard.Console) (runctl.Deps, func()) {
	notice := func(msg string) {
		if panel != nil && panel.Active() {
			panel.Print(msg + "\n")
			return
		}
		fmt.Fprintln(os.Stderr, msg)
	}
	deps := runctl.Deps{
		Journal: jw,
		Notice:  notice,
		Whea:    wheaFactoryForRun(opt.ControlCore),
	}
	release := func() {}
	if panel != nil {
		deps.Temp = panel.Temperature()
	} else {
		m := temperature.New()
		deps.Temp = m
		release = func() { m.Disarm(); m.Close() }
	}
	return deps, release
}

// sessionRunExit maps the per-core outcomes onto the exit contract:
// fault 1 > setup error 2 > interrupted 130 > clean 0. A fault
// observed anywhere outranks another core's setup trouble (the
// hardware suspicion is the louder claim), and an interrupted run
// never claims success.
func sessionRunExit(outcomes []SessionOutcome) int {
	var fault, setup, intr bool
	for _, o := range outcomes {
		if o.Fault != "" {
			fault = true
		}
		if o.SetupErr != "" {
			setup = true
		}
		if o.Interrupted {
			intr = true
		}
	}
	switch {
	case fault:
		return 1
	case setup:
		return 2
	case intr:
		return 130
	}
	return 0
}

func printSessionSummary(out io.Writer, cores []int, outcomes []SessionOutcome) {
	fmt.Fprintln(out, "\nSession summary:")
	for i, o := range outcomes {
		status := "OK"
		switch {
		case o.Fault != "":
			status = "FAULT"
		case o.SetupErr != "":
			status = "SETUP-ERROR"
		case o.Interrupted:
			status = "INTERRUPTED"
		}
		fmt.Fprintf(out, "  core %d: %s · bursts %d/%d verified · exit %d\n",
			cores[i], status, o.BurstsDone, o.BurstsTried, o.Exit)
		if o.Fault != "" {
			fmt.Fprintf(out, "    fault: %s\n", o.Fault)
		}
		if o.SetupErr != "" {
			fmt.Fprintf(out, "    setup: %s\n", o.SetupErr)
		}
	}
}
