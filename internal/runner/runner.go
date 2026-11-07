// Package runner executes a resolved plan: it pins the process, starts the
// workload workers, the watchdog, the live UI and the crash journal, and
// reports the outcome.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/clocksrc"
	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/logging"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Execute runs the test and returns a process exit code
// (0 ok, 1 fault/abort, 130 user force-quit, 2 setup error).
func Execute(opt plan.Options) int {
	return ExecuteWithResult(opt).Exit
}

// ExecuteWithResult runs the test and reports the full outcome.
func ExecuteWithResult(opt plan.Options) plan.Result {
	return ExecuteWithPanel(opt, nil)
}

// ExecuteWithPanel runs the test with an optional caller-owned panel:
// when panel is non-nil the engine paints into it but does not Start or
// Stop it — the owner (the menu's session) keeps the hardware watch
// alive between runs and releases the screen when the menu takes over.
func ExecuteWithPanel(opt plan.Options, panel *dashboard.Console) (res plan.Result) {
	test, ok := plan.GetTest(opt.TestName)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown test %q (see 'list')\n", opt.TestName)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: fmt.Sprintf("unknown test %q", opt.TestName)})
	}

	n := len(opt.Cores)
	if n == 0 {
		n = runtime.NumCPU()
	}
	runPlan := test.Plan(n, opt.A, opt.B)

	// Process-wide state is borrowed for the run, never owned by it:
	// the menu host, the next run and every child they start must see
	// exactly what they handed us. Snapshot BEFORE the first mutation
	// and hand it back through a defer registered right there, so every
	// exit path - setup refusals included - gives it back (audit A01: a
	// leaked narrowed mask poisoned the next test's and the next tool's
	// whole view of the machine).
	savedState := sysinfo.SaveProcessState()
	defer func() {
		if rerr := savedState.Restore(); rerr != nil {
			// The failed handback is infrastructure evidence, not a CPU
			// fault: the run's own verdict was already decided and
			// stands; this finding records what stayed wrong.
			fmt.Fprintf(os.Stderr, "warning: process state not fully handed back: %v\n", rerr)
			res.Findings = append(res.Findings, plan.Finding{Kind: plan.FindProcessState, Source: "process",
				Message:     rerr.Error(),
				Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
		}
	}()

	// Pin to the selected cores (if any) and cap the scheduler to them.
	if err := sysinfo.ApplyAffinity(opt.Cores); err != nil {
		fmt.Fprintf(os.Stderr, "warning: affinity: %v\n", err)
	}
	// slot i of the plan runs on selected[i]; when no cores were
	// configured that is just CPU i.
	selected := opt.Cores
	if len(selected) == 0 {
		selected = make([]int, n)
		for i := range selected {
			selected[i] = i
		}
	}
	// control is the CPU the helper threads live on. Defensive: a control
	// core that collides with a target (possible for hand-built Options)
	// degrades to "none" rather than stealing a measured core.
	control := opt.ControlCore
	for _, c := range selected {
		if c == control {
			control = -1
			fmt.Fprintln(os.Stderr, "warning: control core collides with a target core; helpers stay unpinned")
			break
		}
	}
	// A process can inherit a narrower affinity mask from its parent
	// shell, and thread pinning rejects cores outside that mask: widen
	// the mask (bounded by the system mask) and be honest about whatever
	// stays unreachable.
	widen := selected
	if control >= 0 {
		widen = append(append([]int{}, selected...), control)
	}
	if unreachable := sysinfo.WidenAffinity(widen); len(unreachable) > 0 {
		fmt.Fprintf(os.Stderr, "warning: cores %v are outside this process's affinity mask; workers there run unpinned\n", human.Ints(unreachable))
	}
	// Every worker runs locked to its own OS thread, so the scheduler
	// needs one P per worker to keep them all running at once.
	runtime.GOMAXPROCS(n * max(1, opt.Workers))

	log, err := logging.New(opt.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open log file %q: %v\n", opt.LogFile, err)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: fmt.Sprintf("cannot open log file %q", opt.LogFile)})
	}
	defer log.Close()

	// Crash journal: machine-readable per-core start/beat/finish records.
	// If the process or the machine dies, recovery finds cores that started
	// but never finished. A nil journal is a safe no-op.
	jw := journal.OpenRun(opt.LogFile)
	defer func() {
		jw.RunEndValidation(res.Exit, string(res.Validation))
		if jerr, jn := jw.Errors(); jerr != nil {
			// Result and stderr are the channels that stay honest even
			// when the journal itself cannot record its own failure.
			fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v (%d record(s) lost)\n", jerr, jn)
		}
		jw.Close()
	}()
	jw.RunStart(test.Name, opt.Phase, opt.Cores, opt.Workers, opt.Duration.String(), false)
	jw.RunMeta(plan.RunProvenance(opt))

	log.Logf("INFO", "=== cpu-stability run start ===")
	if opt.Phase != "" {
		log.Logf("INFO", "phase=%s", opt.Phase)
	}
	log.Logf("INFO", "test=%s cores=%v workers/core=%d duration=%s stop_on_fault=%v",
		test.Name, coreLabel(opt.Cores), opt.Workers, opt.Duration, opt.StopOnFault)
	log.Logf("INFO", "plan: %s", strings.Join(runPlan, " "))
	if total, used, ok := sysinfo.MemoryStats(); ok {
		log.Logf("INFO", "memory: %s total, %s used", human.Bytes(total), human.Bytes(used))
	}

	dash := panel
	ownDash := dash == nil
	if ownDash {
		dash = dashboard.New(os.Stdout)
		defer dash.Stop()
	}
	thermal := dash.Temperature()
	defer thermal.Disarm()

	rs := newRunStatus(n, opt.Duration)

	ctx, cancel := context.WithTimeout(context.Background(), opt.Duration)
	defer cancel()

	// Helper threads (the Guard's observers, the watchdog, the UI) get
	// confined to the control core when one exists; the counters feed
	// the run's summary.
	var controlOK, controlTotal int64

	// The panel owns the screen while it lives, so every text write from
	// here on goes through it: a raw fmt.Print between two frames would
	// desynchronize the painter's cursor bookkeeping and smear the
	// screen (the user's "screen breaks at run end").
	notice := func(msg string) {
		log.Log("WARN", msg)
		if dash.Active() {
			dash.Print("\n" + msg + "\n")
			return
		}
		fmt.Println("\n" + msg)
	}

	// A01: the shared safety lifecycle (runctl) - signal watch from
	// New on (first Ctrl+C stops cleanly, second force-quits with 130),
	// temperature preflight and cutoff, WHEA with journal-gated
	// delivery and a bounded final drain. The per-process and
	// transition engines run the very same Guard; the observer causes
	// flow into this engine's run status through the OnAbort hook so
	// the watchdog, the progress UI and the summary read one ledger.
	guard := runctl.New(ctx, opt, runctl.Deps{
		Journal: jw,
		Notice:  notice,
		Temp:    thermal,
		Whea:    wheaFactoryForRun(control),
		OnAbort: func(kind plan.Execution, msg string) { rs.requestAbort(kind, msg) },
		Helper: func(name string, fn func()) {
			go func() {
				pinHelper(control, name, &controlOK, &controlTotal)
				fn()
			}()
		},
	})
	defer guard.Stop()
	runCtx := guard.Ctx()

	// Observers start BEFORE the preflight wait: a WHEA record that
	// lands while the sensor wait is still running belongs to this run.
	guard.Start()
	if ok, exec, f := guard.Preflight(); !ok {
		// A missing sensor blocks a safe run (setup); an already
		// tripped limit is a runtime condition. Neither is a CPU fault,
		// and no worker ever started.
		guard.Finish()
		return plan.Finalize(plan.FinalInput{Execution: exec, Reason: f.Message,
			Findings: []plan.Finding{*f}})
	}

	for i, wl := range runPlan {
		rs.cores[i].workload = wl
		jw.CoreStart(i, wl)
	}

	// Workers: one (or more) per core. Each worker locks its OS thread and
	// pins that thread to the slot's logical CPU, so a fault reported by
	// slot i really happened on selected[i] — the same guarantee the
	// per-process mode gives. No UnlockOSThread on purpose: a locked thread
	// is destroyed when its goroutine returns, so single-core masks never
	// leak back into the runtime's thread pool for the next phase.
	var wg sync.WaitGroup
	var pinWarn sync.Once
	var pinnedOK, pinnedBad int64
	// Telemetry: per-logical-CPU time accounting around the load window
	// gives the idle share; clocks[core] holds the first worker's observed
	// cycle rate (one writer per slot, so no locking is needed).
	coreBase, coreBaseErr := sysinfo.CoreTimes()
	baseAt := time.Now()
	clocks := make([]float64, n)
	// Producer real-clock coverage over the load window, sampled by the
	// watchdog (an observer, never a pinned worker): one accumulator per
	// plan slot, keyed to the logical CPU the slot runs on for physical
	// core attribution. nil-safe everywhere: a run without a producer
	// simply records no core-clock groups.
	telAgg := make(map[int]*clocksrc.Agg, n)
	for core := range runPlan {
		telAgg[core] = clocksrc.NewAgg(baseAt)
	}
	for core, wl := range runPlan {
		rep := rs.cores[core]
		cpu := selected[core]
		for wi := range opt.Workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				if err := sysinfo.SetThreadAffinity(cpu); err != nil {
					pinWarn.Do(func() {
						fmt.Fprintf(os.Stderr, "warning: worker pinning: %v\n", err)
					})
				} else if mask, ok := sysinfo.CurrentThreadAffinity(); ok {
					// Self-verification: read the mask back from the OS.
					if cpu < 64 && mask == uint64(1)<<uint(cpu) {
						atomic.AddInt64(&pinnedOK, 1)
					} else {
						atomic.AddInt64(&pinnedBad, 1)
						pinWarn.Do(func() {
							fmt.Fprintf(os.Stderr, "warning: worker pinning: thread for CPU %d reports mask %x\n", cpu, mask)
						})
					}
				}
				// The slot's first worker times its own thread's cycles
				// across the load window: Δcycles/Δwall is that core's
				// observed rate (an effective-clock proxy). It also
				// publishes the thread id so the watchdog can sample
				// that rate live, once a second.
				var cyc0 uint64
				cycOK := false
				loadStart := time.Now()
				if wi == 0 {
					rep.tid.Store(int64(sysinfo.CurrentThreadID()))
					if c, err := sysinfo.ThreadCycleTimeSelf(); err == nil {
						cyc0, cycOK = c, true
					}
				}
				started := runCtx.Err() == nil
				_, werr := workload.RunOne(runCtx, wl, rep)
				if started && wl != workload.Idle && !workload.IsSetupError(werr) {
					end := time.Now()
					if deadline, ok := runCtx.Deadline(); ok && end.After(deadline) {
						end = deadline
					}
					rep.recordLoad(loadStart, end)
				}
				if werr != nil {
					if sh, ok := workload.SharedRunOf(werr); ok {
						// One shared process, one run-level fact: the
						// slots that followed it end interrupted, not
						// faulted — a blacklist of every CPU the tool
						// served would be invented attribution.
						rs.recordSharedOutcome(core, sh)
						if workload.IsSetupError(sh.Err) {
							cancel()
						}
					} else if workload.IsSetupError(werr) {
						rs.recordSetupError(core, werr.Error())
						cancel() // execution errors always stop; they are not CPU faults
					} else {
						rs.recordFault(core, werr)
					}
				}
				if cycOK {
					if c1, err := sysinfo.ThreadCycleTimeSelf(); err == nil {
						if ns := time.Since(loadStart).Nanoseconds(); ns > 0 {
							clocks[core] = float64(c1-cyc0) * 1000 / float64(ns)
						}
					}
				}
			}()
		}
	}

	// Watchdog: periodic log, stall detection, and clean stop on fault.
	stopUI := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		pinHelper(control, "watchdog", &controlOK, &controlTotal)
		watchdog(runCtx, rs, opt, log, guard.Stop, jw, selected, telAgg)
	}()
	var uiDone chan struct{}
	if dash.Active() {
		// Escape in the panel stops the run exactly like the first
		// Ctrl+C: clean stop, journal records the reason. The watcher
		// lives only while this run is armed.
		dash.SetOnEscape(func() {
			// Escape in the panel stops the run exactly like the first
			// Ctrl+C (the Guard records the cause once for every
			// observer path): clean stop, journal records the reason.
			guard.Cancel(plan.ExecCancelled, "user escape (ESC)")
		})
		// A VT-capable terminal gets the full hardware panel; the plain
		// progress line below is the fallback for classic consoles.
		uiDone = make(chan struct{})
		go func() {
			defer close(uiDone)
			pinHelper(control, "ui", &controlOK, &controlTotal)
			startDashboard(dash, rs, test.Name, opt,
				func() (int64, string) { return guard.WheaCount(), guard.WheaStateString() },
				selected, stopUI)
		}()
		if ownDash {
			dash.Start()
		}
	} else if isTerminal(os.Stdout) {
		uiDone = make(chan struct{})
		go func() {
			defer close(uiDone)
			pinHelper(control, "ui", &controlOK, &controlTotal)
			startProgress(rs, test.Name, opt.ShowMetrics, stopUI)
		}()
	}

	<-runCtx.Done()
	close(stopUI)
	if uiDone != nil {
		<-uiDone
	}
	if dash.Active() {
		dash.SetOnEscape(nil) // this run is over: the menu may read input again
		if ownDash {
			dash.Stop()
		}
	}
	wg.Wait()
	<-watchdogDone
	// Mark the workload end, stop the WHEA poller (one bounded final
	// cycle) and let the consumer drain finish: late-published
	// hardware evidence must be inside the journal and counted before
	// the verdict. The Guard owns this sequence for every engine.
	guard.Finish()

	if reported := pinnedOK + pinnedBad; reported == 0 {
		log.Logf("INFO", "worker pinning: not verifiable on this platform")
	} else {
		log.Logf("INFO", "worker pinning: %d/%d workers confirmed on their CPUs", pinnedOK, reported)
	}
	if control >= 0 {
		log.Logf("INFO", "control core: %d (helpers pinned %d/%d)", control, atomic.LoadInt64(&controlOK), atomic.LoadInt64(&controlTotal))
	} else {
		log.Logf("INFO", "control core: none (helpers float over the target cores)")
	}

	// Telemetry: one record per core for this run's load window. Unknown
	// values stay out of the record rather than posing as zeros.
	coreNow, coreNowErr := sysinfo.CoreTimes()
	windowMS := time.Since(baseAt).Milliseconds()
	var tel human.TelemetryStats
	for core, cpu := range selected {
		clock := clocks[core]
		var idlePct *float64
		if coreBaseErr == nil && coreNowErr == nil && cpu < len(coreBase) && cpu < len(coreNow) {
			if v, ok := coreBase[cpu].IdlePct(coreNow[cpu]); ok {
				idlePct = &v
			}
		}
		var groups []journal.CoreClockGroup
		if a, okAgg := telAgg[core]; okAgg {
			groups = a.Groups()
		}
		if clock > 0 {
			tel.AddClock(clock)
		}
		for _, g := range groups {
			tel.AddCoreClock(g.AvgMHz, g.Source)
		}
		tel.AddIdle(idlePct)
		if clock > 0 || idlePct != nil || groups != nil {
			jw.Telemetry(core, clock, idlePct, windowMS, groups)
		}
	}
	if msg := tel.Line(); msg != "" {
		log.Logf("INFO", "%s", msg)
	}

	aborted, abortMsg := rs.abortInfo()
	// One pass builds both the journal's core_done records and the
	// structured units/findings for Finalize. Faults are reported by
	// logical CPU so callers (the scenario blacklist) can act on them
	// across phases — but only for confirmed per-CPU observations; a
	// shared external process fault is run-level and names nobody.
	units := make([]plan.UnitInput, 0, len(rs.cores))
	// The run status carries this engine's per-core facts; the Guard
	// carries the observer facts (WHEA records, thermal stops, sensor
	// loss) collected on the shared lifecycle.
	findings := append(rs.collectedFindings(), guard.Findings()...)
	for _, c := range rs.cores {
		wl := c.workload
		verification := workload.VerificationOf(wl)
		u := plan.UnitInput{VerificationRequired: wl != workload.Idle && verification != workload.VerifyNone}
		switch verification {
		case workload.VerifyIndependent, workload.VerifyRepeated, workload.VerifyKAT:
			u.Checks, u.ChecksKnown = c.itersNow(), true
		case workload.VerifyNone:
			u.ChecksKnown = true // known: this kernel verifies nothing
		default: // external tool or unknown name: the count is not knowable here
			u.ChecksKnown = false
			// What the adapter's protocol proved for this slot. An
			// adapter that never reported evidence leaves it unknown
			// explicitly - a silent gap would invite reading "OK" as
			// "the tool verified itself", which we never saw.
			u.Evidence = c.evidenceNow()
			if u.Evidence == "" {
				u.Evidence = workload.EvUnknown
			}
		}

		status, msg := journal.OK, ""
		if setupMsg, ok := rs.setupInfo(c.core); ok {
			status, msg = journal.SetupError, setupMsg
			u.Execution = plan.ExecSetupError
			findings = append(findings, plan.Finding{Kind: plan.FindSetupError, Source: "workload", Message: setupMsg,
				Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
		} else if faultMsg, ok := rs.firstFault(c.core); ok {
			status, msg = journal.Fault, faultMsg
			u.Execution = plan.ExecRuntimeError
			kind := rs.faultKind(c.core)
			attr := plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(selected[c.core]), Confidence: plan.ConfConfirmed}
			switch workload.FaultClassOf(c.workload) {
			case workload.FaultMemory:
				// A06: memory-class evidence indicts the memory, not
				// the observing CPU (ram-* kernels are refused in
				// this engine today, but the rule lives once).
				attr = plan.Attribution{Kind: plan.AttrMemoryRegion, Confidence: plan.ConfConfirmed, ObservedOn: plan.IntPtr(selected[c.core])}
			case workload.FaultSynthetic:
				// The deliberate prober: pipeline proof, not a real
				// mismatch claim.
				kind = plan.FindSyntheticProbe
			}
			findings = append(findings, plan.Finding{Kind: kind, Source: "workload", Message: faultMsg, Attribution: attr})
		} else if c.workload == workload.Idle {
			status, msg = journal.Skipped, "idle slot"
			u.Execution = plan.ExecSkipped
		} else if aborted || c.loadDuration() == 0 {
			status, msg = journal.Interrupted, abortMsg
			u.Execution = plan.ExecInterrupted
		} else {
			u.Execution = plan.ExecCompleted
		}
		units = append(units, u)

		elapsed := c.loadDuration()
		if status == journal.SetupError || status == journal.Skipped {
			elapsed = 0
		}
		var checks *uint64
		if u.ChecksKnown && u.Checks > 0 {
			n := u.Checks
			checks = &n
		}
		jw.CoreDoneChecks(c.core, status, msg, elapsed, checks, u.Evidence, workload.KernelVersionOf(c.workload))
	}

	// A journal that lost records is a visible finding: the run's own
	// verdict may stand, but the machine-readable story is incomplete
	// and must say so (CO transactions will refuse hardware writes on
	// this, P10).
	if jerr, jn := jw.Errors(); jerr != nil {
		log.Logf("WARN", "journal incomplete: %v (%d record(s) lost)", jerr, jn)
		findings = append(findings, plan.Finding{Kind: plan.FindJournalError, Source: "journal",
			Message:     fmt.Sprintf("%v (%d record(s) lost)", jerr, jn),
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
	}

	// The window's final WHEA verdict was journaled by guard.Finish
	// above (written last there so recovery's last-state-wins sees the
	// whole window, not one lucky cycle).

	res = plan.Finalize(plan.FinalInput{
		Execution: rs.abortExecution(),
		Reason:    abortMsg,
		Units:     units,
		Findings:  findings,
	})

	log.Flush()
	printSummary(dash, rs, test.Name, aborted, abortMsg, res.Validation)
	checksNote := fmt.Sprintf("%s confirmed checks", human.Count(res.Coverage.Verified))
	if !res.Coverage.ChecksKnown {
		checksNote = "check count not knowable from our side (external tool)"
	}
	log.Logf("INFO", "verdict: %s · validation %s · %s · slots %d/%d completed",
		res.Execution, res.Validation, checksNote, res.Coverage.Completed, res.Coverage.Planned)
	if res.Execution != plan.ExecCompleted {
		log.Logf("ERROR", "run aborted: %s", abortMsg)
	}
	log.Logf("INFO", "=== run end (exit=%d) ===", res.Exit)
	return res
}

// wheaFactoryForRun is the package seam for the WHEA observation: the
// Guard builds its Whea dependency through it, so a test can script
// the whole observation (events, state, drain) without the Windows
// event log. A nil monitor means the platform has no WHEA at all:
// recorded as unavailable, never as "no errors".
var wheaFactoryForRun = func(controlCore int) runctl.WheaFactory {
	return func(persist func(whea.Event) error) runctl.WheaMonitor {
		m := whea.Start(controlCore, persist)
		if m == nil {
			return nil // typed-nil guard: a nil *Monitor must not become a non-nil interface
		}
		return m
	}
}

// pinHelper confines one helper goroutine (signal, WHEA, watchdog, UI) to
// the control core so it never preempts a measured core. Like the workers
// these threads lock without unlocking: the thread dies with the goroutine
// and never leaks the single-core mask back into the runtime's pool.
func pinHelper(core int, name string, ok, total *int64) {
	if core < 0 {
		return
	}
	atomic.AddInt64(total, 1)
	runtime.LockOSThread()
	if err := sysinfo.SetThreadAffinity(core); err != nil {
		fmt.Fprintf(os.Stderr, "warning: helper pinning (%s): %v\n", name, err)
		return
	}
	if mask, avail := sysinfo.CurrentThreadAffinity(); avail && mask == uint64(1)<<uint(core) {
		atomic.AddInt64(ok, 1)
	} else if avail {
		fmt.Fprintf(os.Stderr, "warning: helper pinning (%s): thread reports mask %x\n", name, mask)
	}
}

// watchdog ticks once a second: logs progress, warns on stalls, enforces
// the temperature cutoff, and (when stop-on-fault is on) cancels the run
// as soon as a fault is recorded so we stop applying load instead of
// letting the machine ride out to a thermal shutdown.
func watchdog(ctx context.Context, rs *runStatus, opt plan.Options, log *logging.Logger, cancel context.CancelFunc, jw *journal.Writer, selected []int, telAgg map[int]*clocksrc.Agg) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	beatEvery := opt.BeatEvery
	if beatEvery <= 0 {
		beatEvery = time.Second
	}
	nextBeat := time.Time{} // zero: the first tick always writes a beat
	// Live telemetry state: last cycle reading per slot, last CoreTimes
	// snapshot, and its timestamp. Same math as the final telemetry
	// record, just per second.
	tel := &liveTel{}
	if opt.Telemetry {
		tel.cyc = make([]uint64, len(rs.cores))
		if ts, err := sysinfo.CoreTimes(); err == nil {
			tel.times = ts
		}
		tel.at = time.Now()
	}
	// Producer real-clock sampling lives here too (journal coverage, not
	// just live lines): the watchdog is an observer on helper threads,
	// never a pinned worker, so its syscalls do not contaminate a
	// measured core's own cycle counters.
	var rd *clocksrc.Reader
	var coreOf []int
	var lastSmp clocksrc.Sample
	var lastOK bool
	if telAgg != nil && selected != nil {
		rd = clocksrc.Open()
		defer rd.Close()
		coreOf = clocksrc.LookupCoreOf()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			log.Log("INFO", progressLine(rs, opt.TestName))
			// core_beat is the journal's liveness record, but at 1/s x 32
			// cores it dominates an overnight journal's size. The cadence
			// is configurable (beat_interval); the live UI, stall detection
			// and telemetry keep sampling every tick regardless.
			if now := time.Now(); !now.Before(nextBeat) {
				for _, c := range rs.cores {
					jw.CoreBeat(c.core, c.itersNow())
				}
				nextBeat = now.Add(beatEvery)
			}

			if rd != nil {
				nowC := time.Now()
				lastSmp, lastOK = rd.Sample(nowC)
				if lastOK && coreOf != nil {
					for slot, cpu := range selected {
						if a, okA := telAgg[slot]; okA && cpu >= 0 && cpu < len(coreOf) {
							a.Add(lastSmp, coreOf[cpu], nowC)
						}
					}
				}
			}
			if opt.Telemetry {
				tel.sample(rs, log, selected, lastSmp, lastOK, coreOf)
			}

			// Temperature is the Guard's job now (shared cutoff ticker
			// on every engine): a hot zone or lost sensor aborts and
			// cancels through the OnAbort hook, and this loop sees the
			// abort in the run status like any other cause.

			if aborted, _ := rs.abortInfo(); aborted && opt.StopOnFault {
				cancel()
				return
			}

			// Stall detection: no non-idle core has beaten in the last 3s.
			now := time.Now().UnixNano()
			active := false
			stale := true
			for _, c := range rs.cores {
				if c.workload == workload.Idle {
					continue
				}
				active = true
				if now-c.lastBeatNano() < int64(3*time.Second) {
					stale = false
				}
			}
			if active && stale {
				log.Log("WARN", "watchdog: no core reported progress in >3s (possible hang)")
			}
		}
	}
}

// liveTel carries the watchdog's per-second telemetry window between
// ticks: cycle-counter baselines per slot and the last CoreTimes snapshot.
type liveTel struct {
	cyc   []uint64 // per slot: last cycle reading (0 = no baseline yet)
	times []sysinfo.CoreTime
	at    time.Time
}

// sample logs one compact telemetry line for the window since the last
// call. Unknown pieces stay out of the line: a slot whose worker vanished
// or a platform without CoreTimes simply shrinks the line, never fakes a
// zero. live is the watchdog sampler's most recent producer tick
// (liveOK false when that tick produced nothing): the core-clock segment
// names its source and never merges with the observed cycle rate.
func (t *liveTel) sample(rs *runStatus, log *logging.Logger, selected []int, live clocksrc.Sample, liveOK bool, coreOf []int) {
	now := time.Now()
	el := now.Sub(t.at).Nanoseconds()
	var tel human.TelemetryStats
	for i, c := range rs.cores {
		tid := c.tid.Load()
		if tid == 0 {
			continue
		}
		cyc, err := sysinfo.ThreadCycleTime(int(tid))
		if err != nil {
			continue
		}
		if t.cyc != nil && t.cyc[i] != 0 && el > 0 {
			tel.AddClock(float64(cyc-t.cyc[i]) * 1000 / float64(el))
		}
		if t.cyc != nil {
			t.cyc[i] = cyc
		}
	}

	if liveOK && coreOf != nil {
		for _, cpu := range selected {
			if cpu >= 0 && cpu < len(coreOf) {
				if mhz, ok := live.Clocks[coreOf[cpu]]; ok {
					tel.AddCoreClock(mhz, live.Src)
				}
			}
		}
	}

	if t.times != nil {
		if ts, err := sysinfo.CoreTimes(); err == nil && len(ts) == len(t.times) {
			for _, cpu := range selected {
				if cpu >= 0 && cpu < len(ts) {
					if p, ok := t.times[cpu].IdlePct(ts[cpu]); ok {
						tel.AddIdle(&p)
					}
				}
			}
			t.times = ts
		}
	}
	t.at = now

	if msg := tel.Line(); msg != "" {
		log.Log("INFO", msg)
	}
}

// coreLabel renders the selected cores for log lines ("all" when unset).
func coreLabel(cores []int) string {
	if len(cores) == 0 {
		return "all"
	}
	return human.Ints(cores)
}

// printSummary writes a compact final report. Per-core progress remains in the
// log; the console expands only actual faults. The panel's Writer arbitrates:
// while the panel lives the report erases the current frame and the next
// frame repaints below it; after Stop (or without a panel) it simply appends.
func printSummary(dash *dashboard.Console, rs *runStatus, test string, aborted bool, abortMsg string, validation plan.Validation) {
	writeSummary(dash.Writer(), rs, test, aborted, abortMsg, validation)
}

func writeSummary(w io.Writer, rs *runStatus, test string, aborted bool, abortMsg string, validation plan.Validation) {
	type aggregate struct {
		slots int
		total uint64
		min   uint64
		max   uint64
	}

	aggregates := make(map[string]*aggregate)
	var order []string
	var total uint64
	for _, c := range rs.cores {
		it := c.itersNow()
		total += it
		agg, exists := aggregates[c.workload]
		if !exists {
			agg = &aggregate{min: it}
			aggregates[c.workload] = agg
			order = append(order, c.workload)
		}
		agg.slots++
		agg.total += it
		if it < agg.min {
			agg.min = it
		}
		if it > agg.max {
			agg.max = it
		}
	}

	fmt.Fprintln(w, "\nResults:")
	if aborted {
		fmt.Fprintf(w, "  Status: ABORTED — %s\n", abortMsg)
	} else if validation == plan.ValVerified || validation == plan.ValExternalConfirmed {
		fmt.Fprintln(w, "  Status: OK")
	} else {
		// The window closed, but the run never confirmed its own
		// checking (nothing verified, only partially verified, or an
		// external tool we cannot confirm): "OK" would invent the
		// verification claim, so the claim itself is printed.
		v := string(validation)
		if v == "" {
			v = "unknown"
		}
		fmt.Fprintf(w, "  Status: ran clean, verification: %s (not a pass claim)\n", v)
	}
	fmt.Fprintf(w, "  Test: %s\n", test)
	fmt.Fprintf(w, "  Total: %s iterations across %d CPU slots\n", human.Count(total), len(rs.cores))
	for _, workload := range order {
		agg := aggregates[workload]
		average := uint64(0)
		if agg.slots > 0 {
			average = agg.total / uint64(agg.slots)
		}
		fmt.Fprintf(w, "  %-7s slots=%d total=%s avg=%s range=%s..%s [%s]\n",
			workload, agg.slots, human.Count(agg.total), human.Count(average), human.Count(agg.min), human.Count(agg.max),
			verificationLabel(workload))
	}

	faults := 0
	for _, c := range rs.cores {
		if msg, ok := rs.firstFault(c.core); ok {
			if faults == 0 {
				fmt.Fprintln(w, "  Faults:")
			}
			faults++
			fmt.Fprintf(w, "    CPU slot %d (%s): %s\n", c.core, c.workload, msg)
		}
	}
}

// verificationLabel tags each workload in the run summary so a
// load-only loader can never read like a self-checking one: the tag
// states what the kernel's verification kind actually proves.
func verificationLabel(name string) string {
	switch workload.VerificationOf(name) {
	case workload.VerifyIndependent:
		return "self-checking vs independent reference"
	case workload.VerifyRepeated:
		return "self-checking by repeating the same algorithm"
	case workload.VerifyKAT:
		return "known-answer probes"
	case workload.VerifyNone:
		return "load only, no data verification"
	case workload.VerifyExternal:
		return "external tool"
	default:
		return "verification unknown"
	}
}
