package subprocess

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// A01: the transition mode must run the SAME safety lifecycle as the
// other executors. These E2E tests drive real session children under
// a fake sensor / fake WHEA and prove the shared Guard is wired in.

// A hot sensor refuses the run BEFORE any session spawns (runtime
// condition); a missing sensor blocks it as a setup error. Neither is
// a CPU fault, and the journal must show no session activity.
func TestSessionTemperaturePreflightBlocksWorkers(t *testing.T) {
	useTestBinaryAsWorker(t)
	for _, tc := range []struct {
		name    string
		value   float64
		missing bool
		exit    int
		kind    plan.FindingKind
	}{
		{"hot", 95, false, 1, plan.FindThermalStop},
		{"missing", 60, true, 2, plan.FindSensorLoss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := chdirTemp(t)
			r := &childTemperature{value: tc.value, missing: tc.missing}
			m := temperature.NewWithReaders(20*time.Millisecond, time.Millisecond, r)
			panel := dashboard.NewWithTemperature(os.Stdout, m)
			defer panel.Stop()
			opt := plan.Options{
				TestName: "general", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
				ControlCore: -1, MaxTempC: 85, BeatEvery: 50 * time.Millisecond,
				LogFile: filepath.Join(dir, "run.log"),
				Bursts:  1, BurstGap: 0, BurstRun: 300 * time.Millisecond,
			}
			res := ExecuteSessionResult(opt, panel)
			if res.Exit != tc.exit || len(res.Faults) != 0 {
				t.Fatalf("unsafe session preflight: %+v", res)
			}
			var kind bool
			for _, f := range res.Findings {
				if f.Kind == tc.kind {
					kind = true
				}
			}
			if !kind {
				t.Errorf("no %q finding: %+v", tc.kind, res.Findings)
			}
			rep := readRecoveredRun(t, journal.Path(opt.LogFile))
			for _, c := range rep.Cores {
				if len(c.SessionObs) > 0 || len(c.Bursts) > 0 {
					t.Fatalf("a session started despite the unsafe preflight: %+v", c)
				}
			}
		})
	}
}

// Sensor loss mid-session cancels every session at its honest
// boundary: interrupted, never a fault, never a clean end.
func TestSessionSensorLossCancelsWorkers(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	opt := plan.Options{
		TestName: "general", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, MaxTempC: 85, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		Bursts:  4, BurstGap: 200 * time.Millisecond, BurstRun: 300 * time.Millisecond,
	}
	r := &childTemperature{value: 60, loseAfterStart: journal.Path(opt.LogFile)}
	m := temperature.NewWithReaders(30*time.Millisecond, time.Millisecond, r)
	panel := dashboard.NewWithTemperature(os.Stdout, m)
	defer panel.Stop()
	start := time.Now()
	res := ExecuteSessionResult(opt, panel)
	if res.Exit != 1 || !res.Aborted || len(res.Faults) != 0 {
		t.Fatalf("sensor loss did not cancel the session: %+v", res)
	}
	if time.Since(start) >= 4*(500*time.Millisecond) {
		t.Fatal("sensor loss waited for the whole schedule")
	}
	var loss bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindSensorLoss && f.Source == "temperature" {
			loss = true
		}
	}
	if !loss {
		t.Errorf("no sensor_loss finding: %+v", res.Findings)
	}
}

// fakeSessionWhea scripts the WHEA observation through the package
// seam: the journal durability gate is honored, and Close ends the
// stream like the real final drain.
type fakeSessionWhea struct {
	ch      chan whea.Event
	state   whea.State
	persist func(whea.Event) error
	closed  sync.Once
}

func (f *fakeSessionWhea) Events() <-chan whea.Event { return f.ch }
func (f *fakeSessionWhea) State() whea.State         { return f.state }
func (f *fakeSessionWhea) Close()                    { f.closed.Do(func() { close(f.ch) }) }
func (f *fakeSessionWhea) push(ev whea.Event) {
	if f.persist != nil {
		_ = f.persist(ev)
	}
	f.ch <- ev
}

// A01: the transition path now carries WHEA findings, state records
// and the cancel policy - an uncorrected record stops every session
// even though no worker faulted.
func TestSessionWheaUncorrectedCancels(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	fw := &fakeSessionWhea{ch: make(chan whea.Event, 4), state: whea.State{Available: true}}
	prev := wheaFactoryForRun
	wheaFactoryForRun = func(controlCore int) runctl.WheaFactory {
		return func(persist func(whea.Event) error) runctl.WheaMonitor {
			fw.persist = persist
			return fw
		}
	}
	t.Cleanup(func() { wheaFactoryForRun = prev })

	opt := plan.Options{
		TestName: "general", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		Bursts:  4, BurstGap: 200 * time.Millisecond, BurstRun: 400 * time.Millisecond,
	}
	go func() {
		time.Sleep(400 * time.Millisecond) // let the first burst run
		fw.push(whea.Event{
			Provider: "Microsoft-Windows-WHEA-Logger", EventID: 1, RecordID: 77,
			Severity: whea.SevUncorrected, Nature: whea.NatureCPU,
			Time: time.Now().UTC(), ReceivedAt: time.Now().UTC(),
		})
	}()
	start := time.Now()
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 1 || len(res.Faults) != 0 {
		t.Fatalf("uncorrected WHEA did not stop the session cleanly: %+v", res)
	}
	if time.Since(start) >= 4*(600*time.Millisecond) {
		t.Fatal("WHEA cancel waited for the whole schedule")
	}
	var wheaFinding bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindWHEA {
			wheaFinding = true
		}
	}
	if !wheaFinding {
		t.Errorf("no whea finding: %+v", res.Findings)
	}
	recs, _ := journal.ReadAll(journal.Path(opt.LogFile))
	var state, event bool
	for _, r := range recs {
		if r.Type == journal.WheaState {
			state = true
		}
		if r.Type == journal.Whea {
			event = true
		}
	}
	if !state || !event {
		t.Errorf("journal lacks the whea event/state records: state=%v event=%v", state, event)
	}
}

// A01: stop-on-fault is the shared policy - a faulting session
// cancels its peers instead of riding the schedule out. The peer is
// interrupted (not faulted, not silently skipped), and the faulting
// core keeps its (synthetic) per-CPU attribution.
func TestSessionStopOnFaultCancelsPeers(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	if runtime.NumCPU() < 2 {
		t.Skip("needs two cores")
	}
	opt := plan.Options{
		TestName: "general", A: "fault", Cores: []int{0, 1}, Workers: 1, PerProcess: true,
		ControlCore: -1, StopOnFault: true, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		Bursts:  3, BurstGap: 0, BurstRun: 300 * time.Millisecond, BurstStagger: 600 * time.Millisecond,
	}
	start := time.Now()
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1: %+v", res.Exit, res)
	}
	if time.Since(start) >= 3*(600*time.Millisecond)+time.Second {
		t.Fatal("stop-on-fault waited for the staggered schedule")
	}
	if res.Faults[0] == "" {
		t.Errorf("faults = %v, want core 0 attributed (synthetic pipeline contract)", res.Faults)
	}
	if res.Faults[1] != "" {
		t.Errorf("faults = %v, want core 1 NOT faulted (it was cancelled, not guilty)", res.Faults)
	}
	// The peer's own worker ended clean (idle STOP is honest for it),
	// but the parent cancelled it BEFORE any load: the proof is that
	// no burst was ever commanded or observed on core 1, and the
	// run's verification claim is capped - a cancelled peer plus a
	// faulting one never reads as a pass.
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	for _, c := range rep.Cores {
		if c.Core != 1 {
			continue
		}
		if len(c.Bursts) != 0 || len(c.SessionObs) != 0 {
			t.Errorf("peer core 1 received load before the cancel: bursts=%d obs=%d",
				len(c.Bursts), len(c.SessionObs))
		}
	}
	if rep.Validation == "verified" || rep.Validation == "external_confirmed" {
		t.Errorf("run_end validation = %q, want a capped claim (cancelled peer + fault)", rep.Validation)
	}
}

// The journal must carry the run's verification claim for a session
// too (A02 rode along): a clean transition run says validation=verified.
func TestSessionRunEndCarriesValidation(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	opt := plan.Options{
		TestName: "general", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		Bursts:  2, BurstGap: 0, BurstRun: 150 * time.Millisecond,
	}
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("clean session: %+v", res)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if rep.Validation != "verified" {
		t.Errorf("journal validation = %q, want verified", rep.Validation)
	}
	var buf bytes.Buffer
	journal.PrintReport(&buf, []journal.RunReport{rep})
	if !strings.Contains(buf.String(), "validation=verified") {
		t.Errorf("report lacks the validation claim:\n%s", buf.String())
	}
}
