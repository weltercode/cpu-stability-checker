package subprocess

// Q05 pair-switch entry points: the same shared safety lifecycle as
// every other executor (temperature preflight and cutoff, WHEA with
// its final drain, stop-on-fault, cancel) around the prepared
// pair-switch driver, and the same exit contract: fault 1 > setup 2
// > interrupted 130 > clean 0. The CLI command and the scenario
// pipeline classify ONE set of reported facts through the same
// Finalize - a switch phase is not a second-class citizen of the
// transition mode.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// ExecuteSwitch runs one prepared pair-switch and returns the process
// exit code (the CLI contract).
func ExecuteSwitch(opt plan.Options) int {
	exit, _, _ := executeSwitch(opt, nil)
	return exit
}

// ExecuteSwitchResult runs one pair-switch phase from the Options'
// switch_* shape and reports it as a plan.Result, so the scenario
// pipeline classifies it through the same Finalize as every other
// executor. The panel, when the menu hands one, owns the screen and
// its Esc key cancels the run.
func ExecuteSwitchResult(opt plan.Options, panel *dashboard.Console) plan.Result {
	_, res, _ := executeSwitch(opt, panel)
	return res
}

// switchWorkloads names each participant's workload by slot: slot 0
// of a group is the aggressor (workload A), slot 1 the observer
// (workload B, falling back to A when only one workload was named -
// same pair, loaded and watching).
func switchWorkloads(opt plan.Options) []string {
	names := make([]string, 0, 2*len(opt.SwitchPairs))
	for _, group := range opt.SwitchPairs {
		for slot := range group {
			if slot == 0 {
				names = append(names, opt.A) // aggressor: the load that switches in
			} else if opt.B != "" {
				names = append(names, opt.B)
			} else {
				names = append(names, opt.A)
			}
		}
	}
	return names
}

// switchParticipants flattens the plan's groups into driver slots
// (group index x slot order, mirroring ValidateSwitch's geometry).
func switchParticipants(opt plan.Options) []SwitchParticipant {
	var parts []SwitchParticipant
	for gi, group := range opt.SwitchPairs {
		for _, core := range group {
			parts = append(parts, SwitchParticipant{Core: core, Group: gi})
		}
	}
	return parts
}

// commandedTurn reports how many prepared turns have ENDED by t -
// the command side only, never a claim about what any worker
// observed (the observations live in the journal).
func commandedTurn(sched transition.SwitchSchedule, t time.Duration) int {
	var acc time.Duration
	done := 0
	for _, tt := range sched.Turns {
		acc += tt.Gap + tt.RunFor
		if t < acc {
			break
		}
		done++
	}
	return done
}

// switchRunExit maps the driven switch onto the exit contract: fault
// 1 > setup error 2 > interrupted 130 > clean 0 - the exact session
// vocabulary (a fault observed anywhere is the louder claim; an
// interrupted run never claims success).
func switchRunExit(out SwitchOutcome) int {
	switch {
	case out.Fault != "":
		return 1
	case out.SetupErr != "":
		return 2
	case out.Interrupted:
		return 130
	}
	return 0
}

// foldSwitchParking buckets the participants' parking evidence with
// the same ParkingFold rule as sessions (required gates on OBSERVED
// evidence and only on observed; a nil fold is "never asked", its
// own honest bucket). The switch outcome carries one fold per
// participant core.
func foldSwitchParking(cores []int, out SwitchOutcome) ParkingFold {
	f := ParkingFold{Total: len(cores)}
	for _, core := range cores {
		ev := out.ParkingPerCore[core]
		if ev == nil {
			f.NeverAsked = append(f.NeverAsked, core)
			continue
		}
		f.SamplesInIdle += ev.SamplesInIdle
		f.ParkedInIdle += ev.ParkedInIdle
		switch ev.Status {
		case transition.ParkingObserved:
			f.Observed = append(f.Observed, core)
		case transition.ParkingUnconfirmed:
			f.Unconfirmed = append(f.Unconfirmed, core)
		case transition.ParkingUnavailable:
			f.Unavailable = append(f.Unavailable, core)
		default:
			f.Unavailable = append(f.Unavailable, core) // unknown: worst case, never a pass
		}
	}
	return f
}

// switchFinalInput turns the driven switch into the reported facts
// Finalize classifies. One unit for the whole switch run (the P07e
// handoff vocabulary): the switch is ONE coordinated session - the
// verified checks aggregate across turns and participants, and the
// per-participant story rides the journal records, not per-core
// units the driver cannot honestly attribute.
func switchFinalInput(out SwitchOutcome, parts []SwitchParticipant, names []string, guardFindings []plan.Finding, aborted bool, abortMsg string, abortKind plan.Execution) plan.FinalInput {
	in := plan.FinalInput{Execution: plan.ExecCompleted}
	u := plan.UnitInput{ChecksKnown: out.ChecksKnown, Checks: out.Checks, VerificationRequired: true}
	switch {
	case out.Fault != "":
		in.Execution = plan.ExecRuntimeError
		u.Execution = plan.ExecRuntimeError
		// A06 vocabulary through the FAULTING member's workload: a
		// ram-* member's mismatch is memory evidence with the CPU
		// recorded as observed_on, never a CPU blacklist entry.
		faultName := ""
		for i, p := range parts {
			if p.Core == out.FaultCore {
				faultName = names[i]
				break
			}
		}
		kind := plan.FindDataMismatch
		attr := plan.Attribution{Kind: plan.AttrLogicalCPU, Confidence: plan.ConfConfirmed}
		if out.FaultCore < 0 {
			attr = plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfUnknown}
		} else {
			attr.Index = plan.IntPtr(out.FaultCore)
		}
		switch workload.FaultClassOf(faultName) {
		case workload.FaultMemory:
			attr = plan.Attribution{Kind: plan.AttrMemoryRegion, Confidence: plan.ConfConfirmed}
			if out.FaultCore >= 0 {
				attr.ObservedOn = plan.IntPtr(out.FaultCore)
			}
		case workload.FaultSynthetic:
			// The deliberate prober proves the reporting path;
			// reports must not render it as a real mismatch.
			kind = plan.FindSyntheticProbe
		}
		in.Findings = append(in.Findings, plan.Finding{
			Kind: kind, Source: "workload", Message: out.Fault, Attribution: attr,
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
		// The Guard's recorded cause outranks the driver's own
		// bookkeeping (a thermal cutoff looks "interrupted" from the
		// turn loop; it was a runtime stop), exactly as in every
		// other engine. A confirmed fault and a setup failure keep
		// their own class.
		if in.Execution == plan.ExecCompleted || in.Execution == plan.ExecCancelled {
			in.Execution = abortKind
			u.Execution = abortKind
		}
		if in.Reason == "" {
			in.Reason = abortMsg
		}
	}
	in.Units = []plan.UnitInput{u}
	return in
}

// executeSwitch is the shared body: it returns the CLI exit code, the
// structured Result (the same facts classified by Finalize for every
// caller), and the participant cores. A refusal BEFORE any
// participant spawned comes back as a finalized setup Result - no
// worker ever spawned for it.
func executeSwitch(opt plan.Options, panel *dashboard.Console) (int, plan.Result, []int) {
	refuse := func(reason string) (int, plan.Result, []int) {
		res := plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: reason})
		return res.Exit, res, nil
	}
	if _, ok := workload.Get(opt.A); !ok {
		fmt.Fprintf(os.Stderr, "switch: unknown workload %q (see 'list')\n", opt.A)
		return refuse(fmt.Sprintf("unknown workload %q", opt.A))
	}
	if opt.B != "" {
		if _, ok := workload.Get(opt.B); !ok {
			fmt.Fprintf(os.Stderr, "switch: unknown workload %q (see 'list')\n", opt.B)
			return refuse(fmt.Sprintf("unknown workload %q", opt.B))
		}
	}
	// Defense in depth beside the plan preflight: the entry re-runs
	// the ONE shape rule (never a second hand-copied reading of it),
	// so no call path can reach the driver with a geometry the plan
	// refused - silently widened groups included.
	if verr := plan.ValidateSwitch(opt); verr != nil {
		fmt.Fprintf(os.Stderr, "switch: %v\n", verr)
		return refuse(verr.Error())
	}
	// Q04: the parking policy is resolved BEFORE any journal or
	// process opens (the execution-vocabulary rule); switch gaps are
	// the measured idle windows the observation rides.
	parking, perr := transition.ParseParkingPolicy(opt.Parking)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "switch: %v\n", perr)
		return refuse(perr.Error())
	}
	if parking == transition.ParkingRequired {
		if _, pprobeErr := parkingProbe(); pprobeErr != nil {
			msg := fmt.Sprintf("parking evidence required but source %s cannot be queried on this machine: %v (set parking_policy to \"try\" to keep the honest answer, or \"off\" to stop asking)", ParkingSourceName, pprobeErr)
			fmt.Fprintf(os.Stderr, "switch: %s\n", msg)
			return refuse(msg)
		}
	}
	if opt.Workers > 1 {
		fmt.Fprintf(os.Stderr, "switch: workers=%d with a pair-switch phase: one engine per pinned core serves every turn; workers>1 is a one-shot shape (audit C01 contract)\n", opt.Workers)
		return refuse(fmt.Sprintf("workers=%d in a pair-switch phase: single-engine-per-core contract", opt.Workers))
	}
	sched, serr := transition.BuildSwitchSchedule(len(opt.SwitchPairs), opt.SwitchRounds, opt.SwitchGap, opt.SwitchRun, opt.SwitchJitter, opt.SwitchOrder, opt.SwitchSeed)
	if serr != nil {
		fmt.Fprintf(os.Stderr, "switch: %v\n", serr)
		return refuse(serr.Error())
	}
	parts := switchParticipants(opt)
	names := switchWorkloads(opt)
	cores := make([]int, len(parts))
	for i, p := range parts {
		cores[i] = p.Core
	}
	if len(cores) == 0 {
		fmt.Fprintln(os.Stderr, "switch: no participants")
		return refuse("no participants")
	}
	if _, err := workerExecutable(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return refuse(err.Error())
	}

	var memPlan *memPlanSpec
	if opt.Memory != "" {
		mp, merr := buildMemPlan(opt, len(parts))
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
	span := switchSpan(sched)
	jw.RunStart(opt.TestName, "", cores, 1, span.String(), true)
	jw.RunMeta(plan.RunProvenance(opt))

	// A01: the shared safety lifecycle - the same temperature
	// preflight and cutoff, the same WHEA observation with its final
	// drain, the same stop-on-fault policy and cancel path every
	// other executor runs. The switch mode must not drift.
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
	writeSwitchHeader(info, memPlan, parts, names, sched, span, parking)

	// Cancel (Esc, Ctrl+C, a thermal stop, an uncorrected WHEA record
	// or a peer's stop-on-fault) reaches the switch through this
	// context: every participant aborts its running turn at its
	// honest boundary and releases memory on the STOP path, and the
	// run never exits 0 and never claims a fault it did not observe.
	ctx := guard.Ctx()

	if panel != nil {
		pushDone := make(chan struct{})
		go func() {
			defer close(pushDone)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			start := time.Now()
			runDesc := fmt.Sprintf("pair switch: %d group(s), %d turn(s)", sched.GroupCount, len(sched.Turns))
			for {
				select {
				case <-ctx.Done():
					panel.ClearRun()
					return
				case <-tick.C:
					elapsed := time.Since(start)
					pos := commandedTurn(sched, elapsed) + 1
					if pos > len(sched.Turns) {
						pos = len(sched.Turns) // the closing barrier still rides out
					}
					panel.Push(dashboard.Snapshot{
						Label: opt.TestName, Run: runDesc,
						Status: fmt.Sprintf("commanded turn %d/%d", pos, len(sched.Turns)),
						HasRun: true, Elapsed: elapsed, Duration: span,
						WheaN: int(guard.WheaCount()), WheaState: guard.WheaStateString(),
						TestMem: memPlan.testedBytes(),
					})
				}
			}
		}()
		defer func() { <-pushDone }()
	}

	argvs := make([][]string, len(parts))
	sids := make([]string, len(parts))
	parks := make([]*ParkingObserver, len(parts))
	for i, p := range parts {
		argvs[i] = sessionWorkerArgs(p.Core, names[i], jpath, runID, opt.BeatEvery, memPlan.forSlot(i))
		sids[i] = fmt.Sprintf("%s-p%d", runID, i)
		// Q04: each participant gets its own observer tied to its
		// logical CPU, sampling inside ITS measured idle windows (the
		// turn gaps). Off policy = nil = not one extra syscall.
		if parking.Active() {
			parks[i] = NewParkingObserver(p.Core, ParkingSourceName, ParkingPollInterval, CpuSetPoller)
		}
	}
	out := RunSwitchSession(ctx, argvs, sids, parts, sched, transition.DefaultTimeouts(), jw, parks)
	if out.Fault != "" && out.FaultCore >= 0 {
		// Stop-on-fault is the shared policy: the switch itself
		// already stopped at the faulted turn (the ledger refuses to
		// grant turns after any unclosed one), but the Guard must
		// know a fault happened - thermal stops, WHEA claims and
		// scenario peers all key off it.
		guard.ReportFault(out.FaultCore, out.Fault)
	}

	guard.Finish()

	exit := switchRunExit(out)
	aborted, abortMsg, abortKind := guard.AbortInfo()
	in := switchFinalInput(out, parts, names, guard.Findings(), aborted, abortMsg, abortKind)
	res := plan.Finalize(in)
	// Q04 verdict gate - the required promise, enforced: no full
	// verification claim without observed parking evidence on every
	// participant. The exit code is untouched; the finding rides the
	// structured result for try too (honest marking with zero verdict
	// power, per the policy's own meaning).
	parkLine := ""
	if parking.Active() {
		pfold := foldSwitchParking(cores, out)
		msg := pfold.Message()
		parkLine = fmt.Sprintf("parking (policy %q): %s", string(parking), msg)
		if parking == transition.ParkingRequired && pfold.Unevidenced() &&
			(res.Validation == plan.ValVerified || res.Validation == plan.ValExternalConfirmed) {
			res.Validation = plan.ValPartial
			msg += "; required: full verification withheld - the parking question was not answered with observed evidence on every participant core"
			parkLine += " -> verification partial"
		}
		res.Findings = append(res.Findings, plan.Finding{
			Kind: plan.FindParking, Source: "parking", Message: msg,
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed},
		})
	}
	// run_end carries the FINAL exit (see session_exec: the journal
	// and the returned Result must state the same ending).
	jw.RunEndValidation(res.Exit, string(res.Validation))
	if jerr, _ := jw.Errors(); jerr != nil {
		fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v\n", jerr)
	}
	jw.Close()

	writeSwitchReport(info, jpath, parts, names, out, parkLine)
	return exit, res, cores
}

// writeSwitchHeader states the run shape in a single write (the
// panel repaints once per block, not per line).
func writeSwitchHeader(w io.Writer, memPlan *memPlanSpec, parts []SwitchParticipant, names []string, sched transition.SwitchSchedule, span time.Duration, parking transition.ParkingPolicy) {
	var head strings.Builder
	if memPlan != nil {
		fmt.Fprintf(&head, "memory engine: %s\n", memPlan.rule)
		if memPlan.skipped > 0 {
			fmt.Fprintf(&head, "memory engine: %d of %d participant(s) received no regions (the budget cannot give them a page set)\n",
				memPlan.skipped, len(parts))
		}
	}
	order := sched.Order
	if order == "" {
		order = "fixed alternation"
	}
	byGroup := make(map[int][]string, sched.GroupCount)
	for i, p := range parts {
		role := "observer"
		if i%2 == 0 {
			role = "aggressor"
		}
		byGroup[p.Group] = append(byGroup[p.Group], fmt.Sprintf("%s core %d (%s)", role, p.Core, names[i]))
	}
	fmt.Fprintf(&head, "pair switch mode: %d group(s) x %d round(s), gap %s run %s, prepared span %s, turn order %s\n",
		sched.GroupCount, len(sched.Turns)/sched.GroupCount, sched.Turns[0].Gap, sched.Turns[0].RunFor, span, order)
	if sched.Jitter > 0 {
		if sched.Seed != nil {
			fmt.Fprintf(&head, "turn jitter: +/- %s (seeded, seed %d; the prepared turns ride the journal, not the seed)\n", sched.Jitter, *sched.Seed)
		} else {
			fmt.Fprintf(&head, "turn jitter: +/- %s (no seed consumed)\n", sched.Jitter)
		}
	}
	for g := 0; g < sched.GroupCount; g++ {
		fmt.Fprintf(&head, "  group %d takes the load together: %s\n", g, strings.Join(byGroup[g], ", "))
	}
	if parking.Active() {
		fmt.Fprintf(&head, "parking observation: policy %q, source %s, sampled every %s inside each participant's measured idle windows (the turn gaps)\n",
			string(parking), ParkingSourceName, ParkingPollInterval)
	}
	fmt.Fprint(w, head.String())
}

func printSwitchSummary(w io.Writer, parts []SwitchParticipant, names []string, out SwitchOutcome) {
	fmt.Fprintln(w, "\nPair-switch summary:")
	for i, p := range parts {
		role := "observer"
		if i%2 == 0 {
			role = "aggressor"
		}
		fmt.Fprintf(w, "  group %d %s core %d (%s)\n", p.Group, role, p.Core, names[i])
	}
	status := "OK"
	switch {
	case out.Fault != "":
		status = "FAULT"
	case out.SetupErr != "":
		status = "SETUP-ERROR"
	case out.Interrupted:
		status = "INTERRUPTED"
	}
	fmt.Fprintf(w, "  turns %d/%d closed · exit %d · %s\n", out.TurnsDone, out.TurnsTried, out.Exit, status)
	if out.ChecksKnown {
		fmt.Fprintf(w, "  verified checks across closed turns: %d\n", out.Checks)
	} else {
		fmt.Fprintf(w, "  verified checks: %d counted, but at least one verified block reported no count (total unknown, never guessed)\n", out.Checks)
	}
	if out.Fault != "" {
		fmt.Fprintf(w, "  fault: %s", out.Fault)
		if out.FaultCore >= 0 {
			fmt.Fprintf(w, " (core %d)", out.FaultCore)
		}
		fmt.Fprintln(w)
	}
	if out.SetupErr != "" {
		fmt.Fprintf(w, "  setup: %s\n", out.SetupErr)
	}
}

// writeSwitchReport assembles the summary plus the recovered journal
// report into exactly one write (panel-friendly). The reader sees the
// prepared switch timeline, the parent's turn records and every
// member's own session_burst records as separate evidence, exactly
// as written.
func writeSwitchReport(w io.Writer, jpath string, parts []SwitchParticipant, names []string, out SwitchOutcome, parkLine string) {
	var report strings.Builder
	printSwitchSummary(&report, parts, names, out)
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
