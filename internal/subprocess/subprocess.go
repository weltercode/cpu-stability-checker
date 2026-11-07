// Package subprocess implements per-core subprocess mode (the CoreCycler
// approach): instead of pinning one process to every core, we start one child
// process per logical CPU. Each child pins itself to its core, runs one
// workload, and writes its own core_start/core_beat/core_done records to the
// shared journal with its own PID. A child that dies — hung and killed, or
// taken by a power loss — leaves a core_start without a core_done, which is
// exactly the crash story the journal recovery is built to tell.
package subprocess

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/clocksrc"
	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/procjob"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// selfExecutable and workerArgsPrefix are test seams: tests point them at
// this test binary so the "children" run TestWorkerHelperProcess.
var (
	selfExecutable   = os.Executable
	workerArgsPrefix []string
)

// ExecuteSubprocess is the parent side of per-core subprocess mode. It owns
// run_start/run_end; every child writes its own core_* records. Exit codes
// match runner.Execute: 0 ok, 1 fault, 2 setup error.
func ExecuteSubprocess(opt plan.Options) int {
	return ExecuteSubprocessWithResult(opt).Exit
}

// ExecuteSubprocessWithResult runs per-core subprocess mode and reports the
// full outcome. Child faults are keyed by logical CPU — each child is pinned
// to exactly one — so the scenario's faulted-core blacklist works in this
// mode too. A Critical WHEA record is a run-level signal: it aborts the run
// and lands in AbortMsg, honestly not attributed to any single core.
func ExecuteSubprocessWithResult(opt plan.Options) plan.Result {
	return ExecuteSubprocessWithPanel(opt, nil)
}

// ExecuteSubprocessWithPanel runs per-core subprocess mode with an optional
// caller-owned panel: when panel is non-nil the engine paints into it but
// does not Start or Stop it — the owner (the menu's session) keeps the
// hardware watch alive between runs and releases the screen when the menu
// takes over.
func ExecuteSubprocessWithPanel(opt plan.Options, panel *dashboard.Console) (res plan.Result) {
	test, ok := plan.GetTest(opt.TestName)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown test %q (see 'list')\n", opt.TestName)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: fmt.Sprintf("unknown test %q", opt.TestName)})
	}
	exe, err := workerExecutable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: err.Error()})
	}

	cores := plan.SelectedLogicalCPUs(opt.Cores, runtime.NumCPU())
	for _, c := range cores {
		if c < 0 || c >= runtime.NumCPU() {
			fmt.Fprintf(os.Stderr, "core %d out of range [0, %d]\n", c, runtime.NumCPU()-1)
			return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
				Reason: fmt.Sprintf("core %d out of range", c)})
		}
	}
	runPlan := test.Plan(len(cores), opt.A, opt.B)

	// The memory engine, when configured: the parent computes the
	// phase budget ONCE here and hands every child its exact grants
	// on the command line. Children never re-derive an auto rule -
	// their view of the machine could differ from the parent's and
	// silently multiply the budget. A budget that cannot give even
	// one page per role is a setup error for the whole phase: the
	// request was explicit, and quietly running without memory would
	// be a silent reduction of it.
	var memPlan *memPlanSpec
	if opt.Memory != "" {
		mp, merr := buildMemPlan(opt, len(cores))
		if merr != nil {
			fmt.Fprintf(os.Stderr, "memory engine: %v\n", merr)
			return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
				Reason: fmt.Sprintf("memory engine: %v", merr)})
		}
		memPlan = mp
		fmt.Printf("memory engine: %s\n", mp.rule)
		if mp.skipped > 0 {
			fmt.Printf("memory engine: %d of %d core(s) received no regions (the budget cannot give them a page set)\n",
				mp.skipped, len(cores))
		}
	}

	// In this mode the journal is the only live record of what each core is
	// doing, so failing to open it is a setup error, not a warning.
	jpath := journal.Path(opt.LogFile)
	jf, err := journal.OpenFile(jpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot open journal %q: %v\n", jpath, err)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: fmt.Sprintf("cannot open journal %q", jpath)})
	}
	runID := journal.NewRunID()
	jw := journal.NewWriter(jf, runID)
	defer func() {
		jw.RunEndValidation(res.Exit, string(res.Validation))
		if jerr, jn := jw.Errors(); jerr != nil {
			// Result and stderr stay honest even when the journal
			// cannot record its own failure.
			fmt.Fprintf(os.Stderr, "warning: journal incomplete: %v (%d record(s) lost)\n", jerr, jn)
		}
		jw.Close()
	}()
	jw.RunStart(test.Name, opt.Phase, cores, opt.Workers, opt.Duration.String(), true)
	jw.RunMeta(plan.RunProvenance(opt))

	// Each child owns its own fragment file: no two writers ever share
	// one, so no append-atomicity assumption is made anywhere. The
	// fragments are created and registered durably BEFORE any child
	// starts, so recovery can tell a lost story from a quiet core.
	if err := os.MkdirAll(journal.FragmentsDir(jpath, runID), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot create worker journal dir %q: %v\n", journal.FragmentsDir(jpath, runID), err)
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: fmt.Sprintf("cannot create worker journal dir for %q", jpath)})
	}
	jw.Sources(cores)

	fmt.Printf("per-core subprocess mode: %d processes, one per CPU (journal: %s)\n", len(cores), jpath)

	// The duration belongs to each child (started per process); the parent
	// only cancels early on Ctrl+C or stop-on-fault. A parent deadline of
	// its own would kill children before they can write core_done.
	var mu sync.Mutex
	faults := map[int]string{}
	faultAt := map[int]time.Time{} // when each core's fault landed (live panel context line)
	faultWL := map[int]string{}    // the workload that child was running (plan slot)
	findings := []plan.Finding{}   // per-core observed facts (run-level ones come from the Guard)
	setupErrs := 0
	startFailed := map[int]string{} // children we never even started: core -> workload
	childExit := map[int]int{}      // core -> exit code, or -1 killed by our own cancel

	var wg sync.WaitGroup

	// The panel (caller-owned in a session) owns the screen while it
	// lives; notices created before it starts still route through it so
	// they never land between two frames of a running panel.
	dash := panel
	ownDash := dash == nil
	if ownDash {
		dash = dashboard.New(os.Stdout)
		defer dash.Stop()
	}
	noticeErr := func(msg string) {
		if dash.Active() {
			dash.Print(msg + "\n")
			return
		}
		fmt.Fprintln(os.Stderr, msg)
	}

	// A01: the shared safety lifecycle (runctl) - signal cancel from
	// the moment the Guard exists, temperature preflight and cutoff,
	// WHEA with journal-gated delivery and a bounded final drain,
	// stop-on-fault. The threaded and transition engines run the very
	// same Guard: drift between these lifecycles was the audit's A01.
	thermal := dash.Temperature()
	defer thermal.Disarm()
	guard := runctl.New(context.Background(), opt, runctl.Deps{
		Journal: jw,
		Notice:  noticeErr,
		Temp:    thermal,
		Whea:    wheaFactoryForRun(opt.ControlCore),
	})
	defer guard.Stop()
	ctx := guard.Ctx()

	// Start the observers BEFORE the preflight wait: a WHEA record
	// arriving while the sensor wait is still running belongs to this
	// run, and the signal handler has been watching since New. The
	// thermal ticker is inert until Prepare arms the limit below.
	guard.Start()

	// Preflight BEFORE any child spawns: a missing sensor blocks a
	// safe run (setup error); an already tripped limit is a runtime
	// condition. Neither is a CPU fault, and no child ever spawned.
	if ok, exec, f := guard.Preflight(); !ok {
		guard.Finish() // release the poller started above; no children ran
		return plan.Finalize(plan.FinalInput{Execution: exec, Reason: f.Message,
			Findings: []plan.Finding{*f}})
	}

	// Children are started first and waited on in goroutines: in rotate
	// mode the parent needs live handles to freeze and thaw them while
	// they run.
	childDur := opt.Duration
	rotating := opt.Rotate && len(cores) > 1
	if rotating {
		// Each core gets an exclusive window of opt.Duration, so every
		// child must stay alive for the whole rotation (plus a little
		// slack for tool startup inside its own window).
		childDur = time.Duration(len(cores))*opt.Duration + 2*time.Second
	}

	type worker struct {
		core      int
		wl        string // the workload this child runs (per-core plan slot)
		cmd       *exec.Cmd
		job       *procjob.Job
		stdin     io.WriteCloser
		ack       *bufio.Scanner
		stderr    *bytes.Buffer
		exited    bool
		frozen    bool
		tel       *sysinfo.ProcessCycleCounter
		telEpoch  int              // bumped on every freeze/thaw transition
		telRep    *workerTelemetry // the child's self-measured window (nil = never published)
		startedAt time.Time        // child launch...
		endedAt   time.Time        // ...and reap: fallback window when the child died mid-flight
	}
	kids := make([]*worker, 0, len(cores))
	for i, core := range cores {
		w := &worker{core: core, wl: runPlan[i], stderr: &bytes.Buffer{}}
		w.cmd = exec.CommandContext(ctx, exe, workerArgs(core, runPlan[i], childDur, opt.Settle, opt.BeatEvery, jpath, runID, opt.Workers, memPlan.forSlot(i))...)
		w.cmd.Env = append(os.Environ(), workerChildEnv+"=1")
		w.cmd.Stderr = w.stderr
		if rotating {
			pipe, err := w.cmd.StdinPipe()
			if err != nil {
				noticeErr(fmt.Sprintf("error: worker stdin: %v", err))
				setupErrs++
				startFailed[core] = runPlan[i]
				continue
			}
			w.stdin = pipe
			out, err := w.cmd.StdoutPipe()
			if err != nil {
				noticeErr(fmt.Sprintf("error: worker stdout: %v", err))
				setupErrs++
				startFailed[core] = runPlan[i]
				continue
			}
			w.ack = bufio.NewScanner(out)
		}
		w.startedAt = time.Now()
		job, err := startWorker(w.cmd)
		if err != nil {
			noticeErr(fmt.Sprintf("error: start worker for core %d: %v", core, err))
			setupErrs++
			startFailed[core] = runPlan[i]
			continue
		}
		w.job = job
		// Live telemetry binds to the child's identity: open the cycle
		// counter now, while the process object is certainly ours, and
		// keep the handle until the child is reaped. Failure only shrinks
		// the telemetry line (idle still works), so it is not an error.
		w.tel, _ = sysinfo.NewProcessCycleCounter(w.cmd.Process.Pid)
		kids = append(kids, w)
	}

	// The parent is pure orchestration: once every child owns its core,
	// confine the parent's threads (journal writes, rotation control, GC)
	// to the control core so they never preempt a measured one. Children
	// keep their own pinning: SetProcessAffinityMask is bounded by the
	// system mask, not by what a child inherited from us.
	if opt.ControlCore >= 0 {
		if prev, ok := sysinfo.ProcessAffinity(); ok {
			if err := sysinfo.SetAffinity([]int{opt.ControlCore}); err != nil {
				noticeErr(fmt.Sprintf("warning: control core %d: %v", opt.ControlCore, err))
			} else {
				defer func() {
					var restore []int
					for c := range 64 {
						if prev&(uint64(1)<<uint(c)) != 0 {
							restore = append(restore, c)
						}
					}
					if err := sysinfo.SetAffinity(restore); err != nil {
						noticeErr("warning: restore process affinity: " + err.Error())
					}
				}()
			}
		}
	}

	// Real producer core clocks (HWiNFO preferred, WMI fallback) are
	// sampled once per run by the parent: the pinned children stay clean
	// of observation syscalls (even a shared-memory copy runs on the
	// core whose clock the record is about), and the combined journal
	// Telemetry record needs coverage over the whole window no matter
	// whether the live line or the panel is on. A missing logical to
	// physical mapping keeps per-core attribution unknown: the coverage
	// groups are simply absent, never guessed onto siblings.
	aggStart := time.Now()
	coreOf := lookupCoreOf()
	aggs := make(map[int]*clocksrc.Agg, len(kids))
	for _, w := range kids {
		aggs[w.core] = clocksrc.NewAgg(aggStart)
	}
	var liveSmp clocksrc.Sample
	var liveOK bool
	aggCtx, aggStop := context.WithCancel(context.Background())
	aggDone := make(chan struct{})
	if len(kids) > 0 {
		go func() {
			defer close(aggDone)
			defer aggStop() // no cancel leak on any exit path
			rd := clocksrc.Open()
			defer rd.Close()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-aggCtx.Done():
					return
				case <-tick.C:
					now := time.Now()
					smp, ok := rd.Sample(now)
					mu.Lock()
					liveSmp, liveOK = smp, ok
					if ok && coreOf != nil {
						for _, w := range kids {
							// A frozen or epoch-crossed child says nothing
							// about clock right now: skip its tick.
							if w.exited || w.frozen {
								continue
							}
							if w.core < len(coreOf) {
								aggs[w.core].Add(smp, coreOf[w.core], now)
							}
						}
					}
					mu.Unlock()
				}
			}
		}()
	} else {
		close(aggDone)
	}

	// Live hardware panel (VT-capable terminals only): the parent watches
	// OS counters plus the children's cycle counters — the same pure
	// observation stance as the telemetry sampler below. A caller-owned
	// panel (menu session) is painted into but never started or stopped
	// here: the session keeps monitoring alive between runs.
	var dashDone chan struct{}
	if dash.Active() {
		// Escape stops the run like the first Ctrl+C: workers are
		// cancelled and the run is recorded as aborted. The watcher
		// lives only while this run is armed.
		dash.SetOnEscape(func() {
			// Escape stops the run like the first Ctrl+C: workers are
			// cancelled and the run is recorded as aborted. The watcher
			// lives only while this run is armed.
			guard.Cancel(plan.ExecCancelled, "user escape (ESC)")
		})
		if ownDash {
			dash.Start()
		}
		dashDone = make(chan struct{})
		go func() {
			defer close(dashDone)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			tel := &procTel{}
			span := opt.Duration
			if opt.Rotate && len(cores) > 1 {
				span = opt.Duration * time.Duration(len(cores))
			}
			run := plan.RunSummary(opt, len(cores))
			start := time.Now()
			for {
				select {
				case <-ctx.Done():
					// The run is over: hand the (possibly session-owned)
					// panel back to plain monitoring.
					dash.ClearRun()
					return
				case <-tick.C:
					mu.Lock()
					snaps := make(map[int]kidSnap, len(kids))
					for _, w := range kids {
						if w.exited || w.tel == nil {
							continue
						}
						snaps[w.core] = kidSnap{cycles: w.tel.Read, frozen: w.frozen, epoch: w.telEpoch}
					}
					faultN := len(faults)
					// The newest fault on its own red line: the counter
					// says how many, this says where - core, the plan-slot
					// workload, the moment, and the child's own story (the
					// ram kernels name themselves plus pass/region/offset
					// inside the stderr tail). The stored message leads
					// with "core %d" only because it is a finding text;
					// the line prefix carries the core, so the redundant
					// lead is trimmed here, not in the stored finding.
					lastFault := ""
					if len(faultAt) > 0 {
						fc, fAt := 0, time.Time{}
						for c, at := range faultAt {
							if fAt.IsZero() || at.After(fAt) {
								fc, fAt = c, at
							}
						}
						tail := strings.TrimPrefix(faults[fc], fmt.Sprintf("core %d ", fc))
						lastFault = fmt.Sprintf("FAULT core %d · %s · %s · %s", fc, faultWL[fc], fAt.Format("15:04:05"), tail)
					}
					ls, lok := liveSmp, liveOK
					mu.Unlock()
					tel.sample(snaps, cores, ls, lok, coreOf, time.Now())
					dash.Push(dashboard.Snapshot{
						Label: opt.TestName, Phase: opt.Phase, Run: run, HasRun: true,
						Elapsed: time.Since(start), Duration: span,
						MHz: tel.last, WheaN: int(guard.WheaCount()), Faults: faultN,
						WheaState: guard.WheaStateString(), LastFault: lastFault,
						TestMem: memPlan.testedBytes(),
					})
				}
			}
		}()
	}

	// Live telemetry (opt-in): once a second, report the children's cycle
	// rate and the selected cores' idle share — the same compact line the
	// in-process watchdog prints. The parent is pure observation here: no
	// child, journal or flag changes are involved.
	telDone := make(chan struct{})
	if opt.Telemetry {
		go func() {
			defer close(telDone)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			tel := &procTel{}
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					mu.Lock()
					snaps := make(map[int]kidSnap, len(kids))
					for _, w := range kids {
						if w.exited || w.tel == nil {
							continue
						}
						snaps[w.core] = kidSnap{cycles: w.tel.Read, frozen: w.frozen, epoch: w.telEpoch}
					}
					ls, lok := liveSmp, liveOK
					mu.Unlock()
					// With the panel on screen the line would tear its
					// repaint; the journal and log keep every sample.
					if line := tel.sample(snaps, cores, ls, lok, coreOf, time.Now()); line != "" && !dash.Active() {
						fmt.Println(line)
					}
				}
			}
		}()
	} else {
		close(telDone)
	}

	for _, w := range kids {
		wg.Add(1)
		go func(w *worker) {
			defer wg.Done()
			err := w.cmd.Wait()
			_ = w.job.Close()
			// Wait has drained the captured stderr: extract the child's
			// self-measured telemetry line before the raw buffer could
			// surface it inside an error message.
			tr := takeWorkerTelemetry(w.stderr)
			mu.Lock()
			w.exited = true
			w.telRep = tr
			w.endedAt = time.Now()
			mu.Unlock()
			// We stopped the child ourselves (deadline, Ctrl+C, stop-on-fault):
			// its exit code says nothing about the hardware.
			if ctx.Err() != nil {
				mu.Lock()
				childExit[w.core] = -1
				mu.Unlock()
				return
			}
			exit := 0
			var ee *exec.ExitError
			switch {
			case errors.As(err, &ee):
				exit = ee.ExitCode()
			case err != nil:
				exit = 2
			}
			mu.Lock()
			childExit[w.core] = exit
			mu.Unlock()
			if exit == 0 {
				return
			}

			msg := fmt.Sprintf("core %d child exited with code %d", w.core, exit)
			if s := strings.TrimSpace(w.stderr.String()); s != "" {
				msg += ": " + s
			}
			noticeErr("error: " + msg)
			mu.Lock()
			if exit == 2 {
				setupErrs++
			} else {
				faults[w.core] = msg
				faultAt[w.core] = time.Now()
				faultWL[w.core] = w.wl
			}
			mu.Unlock()
			if exit == 2 {
				// A child that could not even set up blocks a valid run:
				// stop the rest and record the setup cause.
				guard.Cancel(plan.ExecSetupError, msg)
			} else {
				// A faulting child ends the run's story as a runtime
				// fault even when StopOnFault lets the clock run out;
				// only the policy decides whether the peers stop now.
				guard.Abort(plan.ExecRuntimeError, msg)
				if opt.StopOnFault {
					guard.Stop()
				}
			}
		}(w)
	}

	if rotating {
		fmt.Printf("rotation: one core at a time, %s per core\n", opt.Duration)
		running := func(w *worker) bool {
			mu.Lock()
			defer mu.Unlock()
			return !w.exited
		}
		freeze := func(w *worker) {
			mu.Lock()
			skip := w.frozen || w.exited
			mu.Unlock()
			if skip {
				return
			}
			if _, err := io.WriteString(w.stdin, "S\n"); err != nil {
				return // the child is gone; nothing to freeze
			}
			// Wait for the ack so S and R stay strictly ordered; a child
			// that cannot answer must not stall the rotation.
			acked := make(chan struct{})
			go func() {
				if w.ack.Scan() && strings.TrimSpace(w.ack.Text()) == "s" {
					close(acked)
				}
			}()
			select {
			case <-acked:
				mu.Lock()
				w.frozen = true
				w.telEpoch++ // the telemetry sampler must rebaseline across this
				mu.Unlock()
			case <-time.After(5 * time.Second):
			}
		}
		thaw := func(w *worker) {
			mu.Lock()
			neverFrozen := !w.frozen
			w.frozen = false
			if !neverFrozen {
				w.telEpoch++ // crossing a freeze boundary: rebaseline, no clock
			}
			mu.Unlock()
			if neverFrozen {
				return
			}
			_ = procjob.ResumeProcess(w.cmd.Process.Pid) // wake the worker itself
			_, _ = io.WriteString(w.stdin, "R\n")        // wake its tools
		}

		for {
			anyRunning := false
			for _, active := range kids {
				if ctx.Err() != nil {
					break
				}
				if !running(active) {
					continue
				}
				anyRunning = true
				thaw(active)
				for _, other := range kids {
					if other != active {
						freeze(other)
					}
				}
				select {
				case <-time.After(opt.Duration):
				case <-ctx.Done():
				}
			}
			if ctx.Err() != nil || !anyRunning {
				break
			}
		}
		for _, w := range kids {
			thaw(w) // never leave a child frozen: it must finish or be killable
		}
	}
	wg.Wait()
	// The journal is the children's own record: scan this run's
	// core_start/core_done lines once to recover each child's reported
	// status and its confirmed verified check count. Children killed by
	// our own cancellation never wrote core_done: they are marked
	// interrupted (in the journal and for the result), preserving real
	// faults and already completed windows. Their unmeasured time stays
	// unknown.
	outcomes, scanIss := scanCoreOutcomes(jpath, runID)
	if ctx.Err() != nil {
		for core, o := range outcomes {
			if o.status == journal.Crashed {
				jw.CoreDone(core, journal.Interrupted, "worker stopped by parent")
				o.status = journal.Interrupted
				outcomes[core] = o
			}
		}
	}

	// One combined Telemetry record per started child, written by the
	// parent alone: the child's self-measured window (transported over
	// stderr; absent when the child died before publishing one) plus
	// this parent's producer-sampled core-clock coverage. The child
	// lifetime is the fallback window: it spans more than the child's
	// load window (setup and teardown included), which is why it is
	// only the fallback. Unknowns stay absent from the record; the
	// record is written whenever at least one fact is known, so the
	// journal states what we can attest instead of staying silent.
	aggStop()
	<-aggDone
	for _, w := range kids {
		var clockMHz float64
		var idlePct *float64
		var windowMS int64
		if w.telRep != nil {
			clockMHz, idlePct, windowMS = w.telRep.ClockMHz, w.telRep.IdlePct, w.telRep.WindowMS
		}
		if windowMS == 0 && !w.startedAt.IsZero() && !w.endedAt.IsZero() {
			windowMS = w.endedAt.Sub(w.startedAt).Milliseconds()
		}
		var groups []journal.CoreClockGroup
		if a, okAgg := aggs[w.core]; okAgg {
			groups = a.Groups()
		}
		if windowMS > 0 || clockMHz > 0 || idlePct != nil || groups != nil {
			jw.Telemetry(w.core, clockMHz, idlePct, windowMS, groups)
		}
	}

	// Stop the sampler before the summary (no telemetry lines after the
	// verdict) and release the child handles we own. The thermal ticker
	// exits on the same cancel; the Guard's Finish below waits for it.
	guard.Stop()
	<-telDone
	if dash.Active() {
		<-dashDone
		dash.SetOnEscape(nil) // this run is over: the menu may read input again
		if ownDash {
			dash.Stop()
		}
	}
	for _, w := range kids {
		w.tel.Close()
	}

	// Mark the run end, stop the poller, and let its bounded final
	// cycle plus the consumer drain land late records in the journal
	// before the verdict (the Guard owns this sequence for every
	// engine). This must happen BEFORE mu is taken for the final
	// assembly below: the consumer still appends findings while the
	// drain runs, and the poller's final queries run under a
	// deadline, so this wait cannot drift.
	guard.Finish()

	mu.Lock()
	defer mu.Unlock()

	units := make([]plan.UnitInput, 0, len(kids)+len(startFailed))
	for _, w := range kids {
		wl := w.wl
		verification := workload.VerificationOf(wl)
		u := plan.UnitInput{VerificationRequired: wl != workload.Idle && verification != workload.VerifyNone}
		switch verification {
		case workload.VerifyIndependent, workload.VerifyRepeated, workload.VerifyKAT:
			// The child counted its own self-checking iterations and
			// published them in core_done; a killed child leaves the
			// count unknown, never zero.
			if o, ok := outcomes[w.core]; ok && o.checks != nil {
				u.Checks, u.ChecksKnown = *o.checks, true
			} else {
				u.ChecksKnown = false
			}
		case workload.VerifyNone:
			u.ChecksKnown = true // known: this kernel verifies nothing
		default: // external tool: beats and lines are not checks
			u.ChecksKnown = false
			// The child recorded the adapter's protocol evidence in
			// its core_done; a child that never wrote one (killed,
			// crashed) leaves the evidence unknown, never implied.
			u.Evidence = workload.EvUnknown
			if o, ok := outcomes[w.core]; ok && o.evidence != "" {
				u.Evidence = o.evidence
			}
		}

		// The child's own core_done is the primary truth; the exit code
		// we watched is the fallback for children that never wrote one.
		status, msg := journal.Crashed, ""
		if o, ok := outcomes[w.core]; ok {
			status, msg = o.status, o.msg
		} else {
			switch childExit[w.core] {
			case 0:
				status = journal.OK
			case 2:
				status = journal.SetupError
			case 1:
				status, msg = journal.Fault, faults[w.core]
			default: // -1 or unseen: killed by our cancel
				status = journal.Interrupted
			}
		}
		switch status {
		case journal.OK:
			if wl == workload.Idle {
				u.Execution = plan.ExecSkipped
			} else {
				u.Execution = plan.ExecCompleted
			}
		case journal.Skipped:
			u.Execution = plan.ExecSkipped
		case journal.Fault:
			u.Execution = plan.ExecRuntimeError
			if msg == "" {
				msg = faults[w.core]
			}
			var ram *journal.RamEvidence
			if o, ok := outcomes[w.core]; ok {
				ram = o.ram
			}
			findings = append(findings, faultFinding(wl, w.core, verification, msg, ram))
		case journal.SetupError:
			u.Execution = plan.ExecSetupError
			findings = append(findings, plan.Finding{Kind: plan.FindSetupError, Source: "workload", Message: msg,
				Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
		default: // Crashed, Interrupted: killed or died
			u.Execution = plan.ExecInterrupted
		}
		units = append(units, u)
	}
	for core, wl := range startFailed {
		units = append(units, plan.UnitInput{Execution: plan.ExecSetupError, ChecksKnown: true,
			VerificationRequired: wl != workload.Idle && workload.VerificationOf(wl) != workload.VerifyNone})
		findings = append(findings, plan.Finding{Kind: plan.FindSetupError, Source: "runner",
			Message:     fmt.Sprintf("worker for core %d never started", core),
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
	}

	// A journal that lost records is a visible finding even when the
	// children's own verdicts stand (see the threaded engine's note).
	if jerr, jn := jw.Errors(); jerr != nil {
		noticeErr(fmt.Sprintf("warning: journal incomplete: %v (%d record(s) lost)", jerr, jn))
		findings = append(findings, plan.Finding{Kind: plan.FindJournalError, Source: "journal",
			Message:     fmt.Sprintf("%v (%d record(s) lost)", jerr, jn),
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
	}
	// Torn or corrupt lines in the shared file are damage too — the
	// children's own writers cannot see them. File-wide on purpose:
	// the file spans runs, so we never pin the damage on this one.
	if !scanIss.Empty() {
		msg := fmt.Sprintf("journal file damage: %d malformed line(s), %d record(s) missing fields", scanIss.Malformed, scanIss.MissingField)
		if scanIss.Unterminated {
			msg += ", final line truncated"
		}
		if scanIss.ReadErr != "" {
			msg += ", read error: " + scanIss.ReadErr
		}
		noticeErr("warning: " + msg)
		findings = append(findings, plan.Finding{Kind: plan.FindJournalError, Source: "journal",
			Message:     msg,
			Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
	}

	// The window's final WHEA verdict was journaled by guard.Finish
	// above (written last there so recovery's last-state-wins sees the
	// whole window, not one lucky cycle).

	abortedNow, abortMsg, exec := guard.AbortInfo()
	_ = abortedNow // exec already says "completed" when nothing aborted
	// The Guard's run-level findings (WHEA records, thermal stops,
	// sensor loss) join the per-core facts collected above.
	findings = append(findings, guard.Findings()...)
	res = plan.Finalize(plan.FinalInput{Execution: exec, Reason: abortMsg, Units: units, Findings: findings})

	checksNote := fmt.Sprintf("%s confirmed checks", human.Count(res.Coverage.Verified))
	if !res.Coverage.ChecksKnown {
		checksNote = "check count not knowable from our side (external tool or killed child)"
	}
	noticeErr(fmt.Sprintf("verdict: %s · validation %s · %s · slots %d/%d completed",
		res.Execution, res.Validation, checksNote, res.Coverage.Completed, res.Coverage.Planned))
	return res
}

// wheaFactoryForRun is the package seam for the WHEA observation:
// both engines build their Guard's Whea dependency through it, so a
// test can script the whole observation (events, state, drain)
// without the Windows event log. A nil monitor means the platform
// has no WHEA at all: recorded as unavailable, never as "no errors".
var wheaFactoryForRun = func(controlCore int) runctl.WheaFactory {
	return func(persist func(whea.Event) error) runctl.WheaMonitor {
		m := whea.Start(controlCore, persist)
		if m == nil {
			return nil // typed-nil guard: a nil *Monitor must not become a non-nil interface
		}
		return m
	}
}

// faultFinding builds the finding for one faulted pinned worker (A06):
// the workload's registered fault class decides what the fault is
// evidence about. A compute mismatch indicts the pinned CPU (the
// classic stability finding, confirmed per-CPU, blacklist-eligible).
// A memory mismatch indicts the MEMORY: the region/offset from the
// child's own ram records (when it kept them) is what a repair
// decision acts on, and the CPU merely observed it - recorded as
// observed_on, never a confirmed CPU-blacklist entry. The synthetic
// prober is pipeline proof, never a real mismatch claim.
func faultFinding(wl string, core int, verification workload.Verification, msg string, ram *journal.RamEvidence) plan.Finding {
	kind := plan.FindDataMismatch
	if verification == workload.VerifyExternal {
		kind = plan.FindToolFailure
	}
	attr := plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(core), Confidence: plan.ConfConfirmed}
	switch workload.FaultClassOf(wl) {
	case workload.FaultMemory:
		attr = plan.Attribution{Kind: plan.AttrMemoryRegion, Confidence: plan.ConfConfirmed, ObservedOn: plan.IntPtr(core)}
		if ram != nil && len(ram.Findings) > 0 {
			attr.Region = ram.Findings[0].Region
			msg = fmt.Sprintf("%s (memory region %s, offset %d)", msg, ram.Findings[0].Region, ram.Findings[0].Offset)
		}
	case workload.FaultSynthetic:
		kind = plan.FindSyntheticProbe
	}
	return plan.Finding{Kind: kind, Source: "workload", Message: msg, Attribution: attr}
}

// coreOutcomeRec is one core's reported ending inside a run.
type coreOutcomeRec struct {
	status   string
	msg      string
	checks   *uint64
	evidence string // adapter's protocol evidence from the child's core_done ("" = none recorded)
	// ram is the child's ram-* mismatch evidence from its core_done
	// (nil = no ram kernel ran here). A06: it tells the parent that a
	// fault is MEMORY evidence (with region/offset) rather than an
	// indictment of the CPU that observed it.
	ram *journal.RamEvidence
}

// scanCoreOutcomes reads the endings for one run from the main journal
// AND the run's worker fragments, keeping the last status per core.
// Main is read first (the parent's own records, and core records in
// legacy pre-fragment journals); the children's fragments come after
// because a child's own core_done is the authoritative ending for its
// core. An overnight journal can contain millions of beats, so only
// the endings are retained. Torn or corrupt lines are NOT silently
// dropped: the returned Issues count them file-wide (the files are
// shared across runs, so damage is reported as file-level, never
// falsely pinned to this run).
func scanCoreOutcomes(mainPath, runID string) (map[int]coreOutcomeRec, *journal.Issues) {
	states := map[int]coreOutcomeRec{}
	apply := func(recs []journal.Record) {
		for _, rec := range recs {
			if rec.RunID != runID || rec.Core == nil {
				continue
			}
			switch rec.Type {
			case journal.CoreStart:
				// Never downgrade a known ending: a fragment core_start
				// can be read after a core_done the parent wrote.
				if o, ok := states[*rec.Core]; !ok || o.status == "" {
					states[*rec.Core] = coreOutcomeRec{status: journal.Crashed}
				}
			case journal.CoreDone:
				states[*rec.Core] = coreOutcomeRec{status: rec.Result, msg: rec.Msg, checks: rec.Checks, evidence: rec.Evidence, ram: rec.Ram}
			}
		}
	}
	f, err := os.Open(mainPath)
	if err != nil {
		return states, &journal.Issues{ReadErr: err.Error(), Unreadable: 1}
	}
	recs, iss := journal.ParseAll(f, mainPath)
	f.Close()
	apply(recs)
	for _, file := range journal.FragmentFiles(mainPath, runID) {
		af, err := os.Open(file)
		if err != nil {
			iss.Unreadable++
			continue
		}
		frag, fiss := journal.ParseAll(af, file)
		af.Close()
		apply(frag)
		iss.Malformed += fiss.Malformed
		iss.MissingField += fiss.MissingField
		iss.Unterminated = iss.Unterminated || fiss.Unterminated
		if fiss.ReadErr != "" && iss.ReadErr == "" {
			iss.ReadErr = fiss.ReadErr
		}
	}
	return states, iss
}
