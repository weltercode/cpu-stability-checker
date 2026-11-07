package subprocess

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// Q05c entry-level pair-switch: the real ExecuteSwitch* paths with
// real worker processes, the shared safety lifecycle, and the
// journal as the authoritative story. Exit vocabulary, verdict
// folding and the commanded-vs-observed separation are asserted here
// end to end, not just at the driver level.

func switchExecOpt(dir string) plan.Options {
	return plan.Options{
		TestName:     "general",
		A:            "float",
		Workers:      1,
		PerProcess:   true,
		ControlCore:  -1,
		BeatEvery:    50 * time.Millisecond,
		LogFile:      filepath.Join(dir, "run.log"),
		SwitchPairs:  [][]int{{0, 1}, {2, 3}},
		SwitchRounds: 2,
		SwitchGap:    40 * time.Millisecond,
		SwitchRun:    100 * time.Millisecond,
	}
}

// Clean pair-switch through the entry: exit 0, validation verified,
// and the recovered journal carries the prepared timeline plus the
// four closed turns plus two complete member views per participant.
func TestExecuteSwitchE2E(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := journal.Path(filepath.Join(dir, "run.log"))

	opt := switchExecOpt(dir)
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (%+v)", res.Exit, res.Findings)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("validation = %s, want verified (every turn closed and verified, parking off)", res.Validation)
	}

	rep := readRecoveredRun(t, jp)
	if rep.SwitchSchedule == nil || len(rep.SwitchSchedule.Turns) != 4 {
		t.Fatalf("prepared switch missing: %+v", rep.SwitchSchedule)
	}
	if rep.Validation != "verified" || rep.Exit != 0 {
		t.Errorf("run_end: validation=%q exit=%d, want verified/0", rep.Validation, rep.Exit)
	}
	if len(rep.Switches) != 4 {
		t.Fatalf("switch turn records = %d, want 4", len(rep.Switches))
	}
	for _, o := range rep.Switches {
		if !o.Complete {
			t.Errorf("turn %d not closed: %+v", o.Turn, o)
		}
	}
	if len(rep.Cores) != 4 {
		t.Fatalf("participants = %d, want 4", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if c.Status != journal.OK {
			t.Errorf("core %d status = %s, want ok", c.Core, c.Status)
		}
		if len(c.SessionObs) != 2 {
			t.Errorf("core %d member views = %d, want 2", c.Core, len(c.SessionObs))
		}
	}
}

// The scope-matrix increment's engine proof: the SAME entry and the
// SAME ledger drive three prepared groups - six prepared turns over
// two rounds, every turn closed, no overlap, all six participants
// ok. This is the N-genericity the roadmap close-out claims, measured
// end to end rather than inferred from the schedule builder.
func TestExecuteSwitchE2EThreeGroups(t *testing.T) {
	if runtime.NumCPU() < 6 {
		t.Skipf("three prepared groups need six logical CPUs, this box has %d", runtime.NumCPU())
	}
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := journal.Path(filepath.Join(dir, "run.log"))

	opt := switchExecOpt(dir)
	opt.SwitchPairs = [][]int{{0, 1}, {2, 3}, {4, 5}} // 3 groups x 2 rounds = 6 turns
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (%+v)", res.Exit, res.Findings)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("validation = %s, want verified", res.Validation)
	}

	rep := readRecoveredRun(t, jp)
	if rep.SwitchSchedule == nil || len(rep.SwitchSchedule.Turns) != 6 {
		t.Fatalf("prepared turns = %+v, want the 6 the three groups x 2 rounds promised", rep.SwitchSchedule)
	}
	groups := map[int]bool{}
	for _, tn := range rep.SwitchSchedule.Turns {
		groups[tn.Group] = true
	}
	if len(groups) != 3 {
		t.Errorf("prepared turns cover %d distinct groups, want 3: %+v", len(groups), rep.SwitchSchedule.Turns)
	}
	if len(rep.Switches) != 6 {
		t.Fatalf("switch turn records = %d, want 6", len(rep.Switches))
	}
	for _, o := range rep.Switches {
		if !o.Complete {
			t.Errorf("turn %d not closed: %+v", o.Turn, o)
		}
	}
	if len(rep.Cores) != 6 {
		t.Fatalf("participants = %d, want 6", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if c.Status != journal.OK {
			t.Errorf("core %d status = %s, want ok", c.Core, c.Status)
		}
		if len(c.SessionObs) != 2 {
			t.Errorf("core %d member views = %d, want 2 (one per round of its group)", c.Core, len(c.SessionObs))
		}
	}
}

// A faulting observer (workload B = the synthetic probe) breaks the
// first turn: the entry must exit 1 (a real fault outranks
// everything), the run_end says the verification is partial, the
// faulted member's story says fault, and the fault finding names the
// synthetic probe (never dressed as a real mismatch).
func TestExecuteSwitchFaultExit(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := journal.Path(filepath.Join(dir, "run.log"))

	opt := switchExecOpt(dir)
	opt.B = "fault" // observers (cores 1 and 3): the deliberate prober
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1 (a fault probe must surface as a fault)", res.Exit)
	}
	var synth bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindSyntheticProbe {
			synth = true
		}
	}
	if !synth {
		t.Errorf("no synthetic-probe finding: %+v", res.Findings)
	}

	rep := readRecoveredRun(t, jp)
	if rep.Exit != 1 || !rep.Ended {
		t.Errorf("run_end: ended=%v exit=%d, want ended/1", rep.Ended, rep.Exit)
	}
	if rep.Validation == "verified" {
		t.Errorf("a faulted switch must not claim full verification: %q", rep.Validation)
	}
	if len(rep.Switches) != 1 || rep.Switches[0].Complete {
		t.Fatalf("turn records = %+v, want exactly one unclosed turn", rep.Switches)
	}
	if !strings.Contains(rep.Switches[0].Error, "injected fault") {
		t.Errorf("faulted turn does not name the fault: %+v", rep.Switches[0])
	}
	var faulted, healthySeen bool
	for _, c := range rep.Cores {
		if c.Core == 1 {
			faulted = true
			if c.Status != journal.Fault {
				t.Errorf("faulted member status = %s, want fault", c.Status)
			}
		}
		if c.Core == 0 && len(c.SessionObs) == 1 && !c.SessionObs[0].Complete {
			healthySeen = true // the sibling's story: TRIGGERed but the turn never closed
		}
	}
	if !faulted || !healthySeen {
		t.Errorf("member stories wrong (faulted=%v sibling=%v): %+v", faulted, healthySeen, rep.Cores)
	}
}

// Sensor loss mid-switch cancels every participant at its honest
// boundary: interrupted (never a fault, never a clean end), the turn
// record says it never closed and why, and the run stops far short
// of the prepared span.
func TestSwitchSensorLossCancelsWorkers(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	opt := switchExecOpt(dir)
	opt.MaxTempC = 85 // the guard only watches a sensor when a cutoff is declared (P08 parity)
	opt.SwitchRounds = 1
	opt.SwitchGap = 50 * time.Millisecond
	opt.SwitchRun = 4 * time.Second // one long turn: the cancel must cut it, not ride it out

	r := &childTemperature{value: 60, loseAfterStart: journal.Path(opt.LogFile)}
	m := temperature.NewWithReaders(30*time.Millisecond, time.Millisecond, r)
	panel := dashboard.NewWithTemperature(os.Stdout, m)
	defer panel.Stop()

	start := time.Now()
	res := ExecuteSwitchResult(opt, panel)
	elapsed := time.Since(start)

	if !res.Aborted || res.Exit != 1 || len(res.Faults) != 0 {
		t.Fatalf("sensor loss did not cancel the switch honestly: %+v", res)
	}
	var loss bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindSensorLoss && f.Source == "temperature" {
			loss = true
		}
	}
	if !loss {
		t.Errorf("no temperature sensor-loss finding: %+v", res.Findings)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("the cancel waited for the whole prepared window (%v)", elapsed)
	}

	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Switches) != 1 || rep.Switches[0].Complete {
		t.Fatalf("turn records = %+v, want one unclosed turn (cancelled mid-turn)", rep.Switches)
	}
	if !strings.Contains(rep.Switches[0].Error, "cancelled mid-turn") {
		t.Errorf("cancelled turn does not name its cause: %+v", rep.Switches[0])
	}
}

// Entry refusals: unknown workloads, a geometry the ONE shape rule
// refuses (a lone group has nothing to alternate against), an unknown
// order word and an unknown parking word are all setup errors named
// with their reason - and not one journal byte written for a run that
// was never allowed to exist.
func TestSwitchEntryRefusals(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	for _, tc := range []struct {
		name string
		mut  func(*plan.Options)
		want string
	}{
		{"unknown workload", func(o *plan.Options) { o.A = "no-such-workload-zz" }, "unknown workload"},
		{"unknown observer", func(o *plan.Options) { o.B = "no-such-workload-zz" }, "unknown workload"},
		{"lone group", func(o *plan.Options) { o.SwitchPairs = [][]int{{0, 1}} }, "at least two prepared groups"},
		{"core in two groups", func(o *plan.Options) { o.SwitchPairs = [][]int{{0, 1}, {1, 2}} }, "two switch groups at once"},
		{"unknown order word", func(o *plan.Options) { o.SwitchOrder = "chaos" }, `unknown switch_order "chaos"`},
		{"unknown parking word", func(o *plan.Options) { o.Parking = "maybe" }, "parking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := switchExecOpt(dir)
			opt.LogFile = filepath.Join(dir, "refuse-"+strings.ReplaceAll(tc.name, " ", "_")+".log")
			tc.mut(&opt)
			res := ExecuteSwitchResult(opt, nil)
			if res.Exit != 2 {
				t.Fatalf("exit = %d, want 2 (setup refusal): %+v", res.Exit, res)
			}
			// The refusal names its reason: structured Result carries it
			// as the abort message (Finalize's mapping), and the journal
			// was never even opened for a run this refused.
			if !strings.Contains(res.AbortMsg, tc.want) {
				t.Errorf("refusal must name %q, got AbortMsg %q (findings %+v)", tc.want, res.AbortMsg, res.Findings)
			}
			if _, err := os.Stat(journal.Path(opt.LogFile)); !os.IsNotExist(err) {
				t.Errorf("a refused run must not create journal bytes: %v", err)
			}
		})
	}
}

// ---- Q04 parking over switch gaps, at the entry level ----
//
// The turn gaps ARE the measured idle windows: the observation must
// ride them end to end (hermetic scripted source, silent WHEA), and
// the required promise must be REAL - refused up front when the
// source cannot answer, partial when it answers without evidence,
// never silently dropped because the mode is new.

func switchParkingOpt(dir, policy string) plan.Options {
	opt := switchExecOpt(dir)
	// Generous gaps: every turn's idle window comfortably outlives the
	// 200ms sampling cadence, so coverage never decides these verdict
	// expectations (the timing-safe shape proven in parking_test.go).
	opt.SwitchGap = 600 * time.Millisecond
	opt.SwitchRun = 100 * time.Millisecond
	opt.Parking = policy
	return opt
}

func cpusetSourceAll(parked bool) func() ([]sysinfo.CpuSetState, error) {
	return func() ([]sysinfo.CpuSetState, error) {
		st := make([]sysinfo.CpuSetState, 4)
		for i := range st {
			st[i] = sysinfo.CpuSetState{LogicalCPU: i, Parked: parked}
		}
		return st, nil
	}
}

// try on switch gaps: the run stays exactly as verified as without the
// policy (zero verdict power), every participant carries its own
// parking_obs record, and the finding states the honest counts.
func TestExecuteSwitchParkingTryObserved(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, cpusetSourceAll(true))

	opt := switchParkingOpt(dir, "try")
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %v", res.Exit, res.Findings)
	}
	f, ok := parkingFinding(res)
	if !ok || !strings.Contains(f.Message, "observed 4 of 4") {
		t.Errorf("parking finding must report every participant observed: %+v", res.Findings)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("try must not touch the verdict: %s", res.Validation)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Cores) != 4 {
		t.Fatalf("participants = %d, want 4", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if len(c.Parking) != 1 || c.Parking[0].Status != string(transition.ParkingObserved) {
			t.Errorf("core %d parking record: %+v, want exactly one observed", c.Core, c.Parking)
		}
	}
}

// required + dead source: refusal BEFORE any journal or worker exists
// (exit 2 naming the fix), never a run whose promise was dead on
// arrival.
func TestExecuteSwitchRequiredRefusesDeadSource(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	swapParkingProbe(t, func() ([]sysinfo.CpuSetState, error) {
		return nil, errors.New("fake cpuset died before the run")
	})

	opt := switchParkingOpt(dir, "required")
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 2 || !strings.Contains(res.AbortMsg, "cannot be queried") {
		t.Fatalf("required + dead source: %+v, want exit 2 naming the fix", res)
	}
	if _, err := os.Stat(journal.Path(opt.LogFile)); !os.IsNotExist(err) {
		t.Errorf("refused run must not create journal bytes: %v", err)
	}
}

// required + alive source that never sees parked: the run ends clean
// (exit 0) but withholds the full verification claim - the promise
// said evidence was needed, the evidence says "the source lives and
// saw nothing", and that is exactly the unevidenced case.
func TestExecuteSwitchRequiredUnconfirmedDropsToPartial(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, cpusetSourceAll(false))

	opt := switchParkingOpt(dir, "required")
	res := ExecuteSwitchResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("clean-but-unevidenced must end 0, got %d", res.Exit)
	}
	if res.Validation != plan.ValPartial {
		t.Errorf("validation = %s, want partial (required without observed evidence)", res.Validation)
	}
	f, ok := parkingFinding(res)
	if !ok || !strings.Contains(f.Message, "full verification withheld") {
		t.Errorf("parking finding must name the withheld promise: %+v", f)
	}
}
