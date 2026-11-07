package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
)

// --- stateful file-backed fake machine --------------------------------
//
// The current profile lives in a JSON file, so a crashed subprocess
// and the recovering parent genuinely share hardware state - the same
// shape cotx's crash tests use, required here because P11 must prove
// crash recovery at EVERY durable trial transition, not just the
// transaction's.

type hwState struct {
	Profile    co.Profile `json:"profile"`
	HardwareID string     `json:"hardware_id"`
}

type stateFake struct {
	hwPath  string
	crashAt string // "apply" | "restore" | ""
	calls   []string
}

func (f *stateFake) IsP11Fake() bool { return true }

func (f *stateFake) load() (hwState, error) {
	b, err := os.ReadFile(f.hwPath)
	if err != nil {
		return hwState{}, err
	}
	var hw hwState
	if err := json.Unmarshal(b, &hw); err != nil {
		return hwState{}, err
	}
	return hw, nil
}

func (f *stateFake) save(hw hwState) error {
	b, err := json.Marshal(hw)
	if err != nil {
		return err
	}
	return os.WriteFile(f.hwPath, b, 0o644)
}

func (f *stateFake) crashNow(point string) {
	if f.crashAt == point {
		os.Exit(7) // die right after the hardware change, mid-trial
	}
}

func (f *stateFake) Probe(context.Context) (co.Capabilities, error) {
	f.calls = append(f.calls, "probe")
	return okCaps(), nil
}

func (f *stateFake) Topology(context.Context) (co.Mapping, error) {
	f.calls = append(f.calls, "topology")
	return okMap(), nil
}

func (f *stateFake) Read(context.Context) (co.Snapshot, error) {
	f.calls = append(f.calls, "read")
	hw, err := f.load()
	if err != nil {
		return co.Snapshot{}, &co.Error{Kind: co.KindMalformedOutput, Op: "read", Err: err}
	}
	return co.Snapshot{Profile: hw.Profile, Quality: "exact", HardwareID: hw.HardwareID, ReadAt: time.Now()}, nil
}

func (f *stateFake) Validate(p co.Profile) error {
	f.calls = append(f.calls, "validate")
	for _, o := range p.Offsets {
		if o.Core != "core0" && o.Core != "core1" {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate", Err: fmt.Errorf("%q not addressable", o.Core)}
		}
		if o.Value < -99 || o.Value > 99 {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate", Err: errors.New("out of range")}
		}
	}
	return nil
}

func (f *stateFake) Apply(_ context.Context, p co.Profile) (co.WriteResult, error) {
	f.calls = append(f.calls, "apply")
	hw, err := f.load()
	if err != nil {
		return co.WriteResult{Mutated: co.MutatedUnknown}, err
	}
	hw.Profile = p
	if err := f.save(hw); err != nil {
		return co.WriteResult{Mutated: co.MutatedUnknown}, err
	}
	f.crashNow("apply")
	return co.WriteResult{Mutated: co.MutatedCompleted}, nil
}

func (f *stateFake) Verify(_ context.Context, p co.Profile) (co.Verification, error) {
	f.calls = append(f.calls, "verify")
	hw, err := f.load()
	if err != nil {
		return co.Verification{}, err
	}
	if profileEqual(hw.Profile, p) {
		return co.Verification{Match: co.Yes("file machine matches")}, nil
	}
	return co.Verification{Match: co.No("file machine differs")}, nil
}

func (f *stateFake) Restore(_ context.Context, s co.Snapshot) (co.Verification, error) {
	f.calls = append(f.calls, "restore")
	hw, err := f.load()
	if err != nil {
		return co.Verification{}, err
	}
	hw.Profile = s.Profile
	if err := f.save(hw); err != nil {
		return co.Verification{}, err
	}
	f.crashNow("restore")
	return co.Verification{Match: co.Yes("file machine restored")}, nil
}

func (f *stateFake) hwProfile(t *testing.T) co.Profile {
	t.Helper()
	hw, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	return hw.Profile
}

// --- scripted tester ---------------------------------------------------

type scriptTester struct {
	ev      Evidence
	crashAt string // "in_test" -> die inside the test
	ran     bool
}

func (s *scriptTester) RunTest(_ context.Context, _ string, _ int) Evidence {
	s.ran = true
	if s.crashAt == "in_test" {
		os.Exit(7) // die mid-test: test_running durable, decision not
	}
	return s.ev
}

// keyedTester decides evidence per candidate value: the non-monotonic
// fake tester the plan requires (instability is probabilistic and
// need not be monotonic in CO, so the search must never assume it).
type keyedTester struct {
	byCandidate map[int]Evidence // missing candidate = pass
	seen        []int
}

func (k *keyedTester) RunTest(_ context.Context, _ string, candidate int) Evidence {
	k.seen = append(k.seen, candidate)
	if ev, ok := k.byCandidate[candidate]; ok {
		return ev
	}
	return passEvidence()
}

func passEvidence() Evidence {
	return Evidence{
		Checks:      []Check{checkYes(CheckSteady, "30s steady compute clean"), checkYes(CheckTransitions, "20 idle/load edges clean"), checkYes(CheckExtended, "extended confirmation clean")},
		WheaGrowth:  checkNo("whea_corrected", "no corrected WHEA during the trial"),
		Temperature: checkYes("cpu_temp", "max 61C within the 85C limit"),
	}
}

// --- harness ------------------------------------------------------------

type harness struct {
	dir    string
	fake   *stateFake
	store  *Store
	engine *cotx.Engine
	runner *Runner
	tester *scriptTester
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	hw := filepath.Join(dir, "hardware.json")
	b, _ := json.Marshal(hwState{Profile: baseProf(), HardwareID: "fp-machine"})
	if err := os.WriteFile(hw, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &harness{dir: dir, fake: &stateFake{hwPath: hw}, tester: &scriptTester{ev: passEvidence()}}
	h.store = &Store{Path: filepath.Join(dir, "co-tune.json")}
	h.engine = &cotx.Engine{Backend: h.fake, LockPath: filepath.Join(dir, "co.lock"), StorePath: filepath.Join(dir, "co-tx.json")}
	h.runner = &Runner{Backend: h.fake, Engine: h.engine, Store: h.store, LockPath: filepath.Join(dir, "co-tune.lock"), Tester: h.tester}
	p, err := CreatePlan(context.Background(), h.fake, optsOK())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.Save(*p, "create"); err != nil {
		t.Fatal(err)
	}
	return h
}

// freshRunner returns a Runner over the same dir as a DIFFERENT
// instance (new locks, new in-memory state): the in-process stand-in
// for "a later process". Subprocess tests use the helper below.
func freshRunner(dir string, tester Tester) (*Runner, *stateFake) {
	f := &stateFake{hwPath: filepath.Join(dir, "hardware.json")}
	st := &Store{Path: filepath.Join(dir, "co-tune.json")}
	e := &cotx.Engine{Backend: f, LockPath: filepath.Join(dir, "co.lock"), StorePath: filepath.Join(dir, "co-tx.json")}
	return &Runner{Backend: f, Engine: e, Store: st, LockPath: filepath.Join(dir, "co-tune.lock"), Tester: tester}, f
}

func loadPlanT(t *testing.T, st *Store) *Plan {
	t.Helper()
	p, err := st.Load()
	if err != nil || p == nil {
		t.Fatalf("plan unreadable: %v", err)
	}
	return p
}

// --- happy path and classifications --------------------------------------

func TestTrialAcceptedEndToEnd(t *testing.T) {
	h := newHarness(t)
	tr, err := h.runner.RunTrial(context.Background(), -21)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Outcome != OutcomeAccepted || tr.State != TrialDecided || !tr.RestoreConfirmed {
		t.Fatalf("trial not accepted: %+v", tr)
	}
	if tr.CotxTxID == "" || tr.CotxStatus != cotx.StatusRestored {
		t.Errorf("cotx transaction not associated: %+v", tr)
	}
	if got := h.fake.hwProfile(t); !profileEqual(got, baseProf()) {
		t.Errorf("hardware not back at baseline: %+v", got)
	}
	p := loadPlanT(t, h.store)
	if p.AttemptsUsed != 1 || len(p.Trials) != 1 || p.Trials[0].Outcome != OutcomeAccepted {
		t.Errorf("plan history wrong: %+v", p)
	}
	if p.Trials[0].Evidence == nil || p.Trials[0].State != TrialDecided {
		t.Errorf("evidence/decision not durable: %+v", p.Trials[0])
	}
}

func classifyCase(t *testing.T, mutate func(*Evidence), want string, whyWant string) {
	t.Helper()
	h := newHarness(t)
	ev := passEvidence()
	mutate(&ev)
	h.tester.ev = ev
	tr, err := h.runner.RunTrial(context.Background(), -21)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Outcome != want {
		t.Fatalf("outcome = %s (%s), want %s", tr.Outcome, tr.Why, want)
	}
	if !strings.Contains(tr.Why, whyWant) {
		t.Errorf("why %q missing %q", tr.Why, whyWant)
	}
	if got := h.fake.hwProfile(t); !profileEqual(got, baseProf()) {
		t.Errorf("hardware must always return to baseline, got %+v", got)
	}
}

func TestTrialRejectedWHEAGrowth(t *testing.T) {
	classifyCase(t, func(e *Evidence) { e.WheaGrowth = Check{Name: "whea", Known: true, Yes: true, Why: "1 corrected WHEA"} },
		OutcomeRejected, "corrected WHEA grew")
}

func TestTrialRejectedCheckFailure(t *testing.T) {
	classifyCase(t, func(e *Evidence) {
		e.Checks[1] = Check{Name: CheckTransitions, Known: true, Yes: false, Why: "2 failed edges"}
	}, OutcomeRejected, "idle_load_transitions failed")
}

func TestTrialIncompleteUnknownCheck(t *testing.T) {
	classifyCase(t, func(e *Evidence) {
		e.Checks[2] = Check{Name: CheckExtended, Why: "sensor unavailable"}
	}, OutcomeIncomplete, "unavailable")
}

func TestTrialIncompleteMissingCheck(t *testing.T) {
	classifyCase(t, func(e *Evidence) { e.Checks = e.Checks[:2] },
		OutcomeIncomplete, "missing from the evidence")
}

func TestTrialIncompleteTemperatureUnknown(t *testing.T) {
	classifyCase(t, func(e *Evidence) { e.Temperature = Check{Name: "cpu_temp", Why: "no fresh CPU sensor"} },
		OutcomeIncomplete, "temperature observation unavailable")
}

func TestTrialRejectedTemperatureOverLimit(t *testing.T) {
	classifyCase(t, func(e *Evidence) {
		e.Temperature = Check{Name: "cpu_temp", Known: true, Yes: false, Why: "peak 91C > 85C"}
	},
		OutcomeRejected, "exceeded the configured limit")
}

func TestTrialIncompleteWithoutTempLimit(t *testing.T) {
	// A plan with no user temperature limit can never accept: the
	// CPU sensor check has no bound to evaluate against, and a
	// universal default is forbidden.
	h := newHarness(t)
	p := loadPlanT(t, h.store)
	p.TempLimitC = 0
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	tr, err := h.runner.RunTrial(context.Background(), -21)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Outcome != OutcomeIncomplete || !strings.Contains(tr.Why, "no user temperature limit") {
		t.Fatalf("want incomplete for missing limit, got %+v", tr)
	}
}

// --- refusals and gates ---------------------------------------------------

func TestTrialRefusals(t *testing.T) {
	t.Run("candidate equals current", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.runner.RunTrial(context.Background(), -20); !co.IsKind(err, co.KindBadProfile) {
			t.Fatalf("same-value trial not refused: %v", err)
		}
	})
	t.Run("candidate out of range", func(t *testing.T) {
		h := newHarness(t)
		p := loadPlanT(t, h.store)
		p.RangeMin, p.RangeMax = -30, -25
		if err := h.store.Save(*p, "edit"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.runner.RunTrial(context.Background(), -50); !co.IsKind(err, co.KindBadProfile) {
			t.Fatalf("out-of-range candidate not refused: %v", err)
		}
	})
	t.Run("attempt budget", func(t *testing.T) {
		h := newHarness(t)
		p := loadPlanT(t, h.store)
		p.AttemptLimit = 1
		if err := h.store.Save(*p, "edit"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.runner.RunTrial(context.Background(), -21); err != nil {
			t.Fatal(err)
		}
		if _, err := h.runner.RunTrial(context.Background(), -22); !co.IsKind(err, co.KindBadProfile) {
			t.Fatalf("budget exhaustion not refused: %v", err)
		}
	})
	t.Run("non-fake backend", func(t *testing.T) {
		h := newHarness(t)
		r := &Runner{Backend: plainBackend{}, Engine: &cotx.Engine{Backend: plainBackend{}}, Store: h.store, LockPath: filepath.Join(h.dir, "x.lock"), Tester: h.tester}
		if _, err := r.RunTrial(context.Background(), -21); !co.IsKind(err, co.KindNotValidated) {
			t.Fatalf("non-fake trial not refused: %v", err)
		}
		if _, err := r.Reconcile(context.Background()); !co.IsKind(err, co.KindNotValidated) {
			t.Fatalf("non-fake reconcile not refused: %v", err)
		}
	})
	t.Run("baseline drift refuses", func(t *testing.T) {
		h := newHarness(t)
		// A foreign tool moved the profile before the trial.
		hw := hwState{Profile: co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -11}, {Core: "core1", Value: -20}}}, HardwareID: "fp-machine"}
		b, _ := json.Marshal(hw)
		if err := os.WriteFile(filepath.Join(h.dir, "hardware.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := h.runner.RunTrial(context.Background(), -21); !co.IsKind(err, co.KindExternalChange) {
			t.Fatalf("baseline drift not refused: %v", err)
		}
		if contains(h.fake.calls, "apply") {
			t.Errorf("hardware was touched despite the refusal")
		}
	})
	t.Run("no plan", func(t *testing.T) {
		dir := t.TempDir()
		r, _ := freshRunner(dir, &scriptTester{ev: passEvidence()})
		if _, err := r.RunTrial(context.Background(), -21); !co.IsKind(err, co.KindBadProfile) {
			t.Fatalf("missing plan not refused: %v", err)
		}
	})
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// --- storage failures at each durable step (in-process) ---------------------

func TestStorageFaultAtPreparedMeansNoHardwareCall(t *testing.T) {
	h := newHarness(t)
	h.store.SetStoreFault(func(step string) error {
		if step == TrialPrepared {
			return errors.New("disk full")
		}
		return nil
	})
	if _, err := h.runner.RunTrial(context.Background(), -21); !co.IsKind(err, co.KindStorage) {
		t.Fatalf("want storage error, got %v", err)
	}
	if contains(h.fake.calls, "apply") {
		t.Errorf("hardware touched despite failed intent write")
	}
	p := loadPlanT(t, h.store)
	if len(p.Trials) != 0 || p.AttemptsUsed != 0 {
		t.Errorf("failed prepared write leaked into the plan: %+v", p)
	}
}

func TestStorageFaultInPhaseStillRestores(t *testing.T) {
	h := newHarness(t)
	h.store.SetStoreFault(func(step string) error {
		if step == TestRunning {
			return errors.New("disk full")
		}
		return nil
	})
	tr, err := h.runner.RunTrial(context.Background(), -21)
	if err != nil {
		t.Fatal(err)
	}
	// The apply happened, the test never ran, but cotx restored.
	if !contains(h.fake.calls, "apply") || !contains(h.fake.calls, "restore") {
		t.Errorf("expected apply+restore, calls=%v", h.fake.calls)
	}
	if got := h.fake.hwProfile(t); !profileEqual(got, baseProf()) {
		t.Errorf("hardware not restored after phase storage failure: %+v", got)
	}
	if tr.Outcome != OutcomeIncomplete || !strings.Contains(tr.Why, "untested, not rejected") {
		t.Fatalf("want incomplete/untested, got %+v", tr)
	}
}

// --- crash at cotx boundaries, reconciled by a later runner -----------------

func TestCrashAfterApplyReconciledInProcess(t *testing.T) {
	h := newHarness(t)
	h.engine.SetAbortAfter(func(step string) error {
		if step == "apply_result" {
			return errors.New("simulated process death after apply")
		}
		return nil
	})
	tr, err := h.runner.RunTrial(context.Background(), -21)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Outcome != OutcomeInterruptedUnknown || !strings.Contains(tr.Why, "not terminal") {
		t.Fatalf("want interrupted_unknown, got %+v", tr)
	}
	p := loadPlanT(t, h.store)
	if p.Status != StatusRecoveryRequired {
		t.Errorf("plan must demand reconciliation: %+v", p)
	}
	if got := h.fake.hwProfile(t); profileEqual(got, baseProf()) {
		t.Fatalf("test setup: hardware should still hold the candidate")
	}
	// A new trial must be refused until reconciliation.
	if _, err := h.runner.RunTrial(context.Background(), -22); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("undecided/recovery gate failed: %v", err)
	}
	// Reconcile in a fresh runner instance (new locks, fresh reads).
	// The trial was already decided interrupted_unknown by the runner
	// that saw the transaction stop; reconciliation here proves the
	// hardware back at baseline and clears the plan status.
	r2, f2 := freshRunner(h.dir, &scriptTester{})
	rec, err := r2.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Errorf("trial was already decided; reconcile should report nothing new: %+v", rec)
	}
	if got := f2.hwProfile(t); !profileEqual(got, baseProf()) {
		t.Errorf("reconcile did not restore the baseline: %+v", got)
	}
	p = loadPlanT(t, r2.Store)
	if p.Status != StatusOpen {
		t.Errorf("plan must return to open after reconciliation: %+v", p)
	}
	if p.AttemptsUsed != 1 {
		t.Errorf("attempt budget must NOT be refunded after an interrupted trial: %d", p.AttemptsUsed)
	}
}

func TestReconcileExternalProfileFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.engine.SetAbortAfter(func(step string) error {
		if step == "apply_result" {
			return errors.New("simulated death")
		}
		return nil
	})
	if _, err := h.runner.RunTrial(context.Background(), -21); err != nil {
		t.Fatal(err)
	}
	// A foreign tool moved the profile after the crash.
	hw := hwState{Profile: co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -11}, {Core: "core1", Value: -11}}}, HardwareID: "fp-machine"}
	b, _ := json.Marshal(hw)
	if err := os.WriteFile(filepath.Join(h.dir, "hardware.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	r2, _ := freshRunner(h.dir, &scriptTester{})
	_, err := r2.Reconcile(context.Background())
	if !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("reconcile over an external profile must fail closed, got %v", err)
	}
	// The external profile is never overwritten, the trial stays
	// undecided, and the plan keeps demanding reconciliation.
	after, _ := json.Marshal(hw)
	cur, _ := os.ReadFile(filepath.Join(h.dir, "hardware.json"))
	if string(after) != string(cur) {
		t.Errorf("reconcile overwrote the external profile")
	}
	p := loadPlanT(t, r2.Store)
	if p.Status != StatusRecoveryRequired {
		t.Errorf("plan must stay recovery_required: %+v", p)
	}
	// The trial was decided interrupted_unknown by the runner that
	// saw the transaction stop; reconciliation must NOT upgrade or
	// re-decide it over an external profile.
	for _, tr := range p.Trials {
		if tr.Outcome == OutcomeAccepted || tr.RestoreConfirmed {
			t.Errorf("reconcile over an external profile confirmed a trial: %+v", tr)
		}
	}
}

// --- true subprocess crashes at every durable trial transition --------------

// TestTuneHelperProcess is not a test: with TUNE_CRASH_AT set it is
// the crashed "previous process". Without it, it passes through
// instantly during normal package runs.
func TestTuneHelperProcess(t *testing.T) {
	crash := os.Getenv("TUNE_CRASH_AT")
	if crash == "" {
		return
	}
	dir := os.Getenv("TUNE_DIR")
	f := &stateFake{hwPath: filepath.Join(dir, "hardware.json")}
	switch crash {
	case "apply", "restore":
		f.crashAt = crash
	}
	st := &Store{Path: filepath.Join(dir, "co-tune.json")}
	switch crash {
	case "prepared":
		st.SetAbortAfter(func(step string) error {
			if step == TrialPrepared {
				os.Exit(7) // die after the prepared write, before cotx starts
			}
			return nil
		})
	case "test_finished":
		st.SetAbortAfter(func(step string) error {
			if step == TestFinished {
				os.Exit(7) // die with evidence durable, decision not
			}
			return nil
		})
	}
	e := &cotx.Engine{Backend: f, LockPath: filepath.Join(dir, "co.lock"), StorePath: filepath.Join(dir, "co-tx.json")}
	tt := &scriptTester{ev: passEvidence()}
	if crash == "in_test" {
		tt.crashAt = "in_test" // die inside the test: test_running durable
	}
	r := &Runner{Backend: f, Engine: e, Store: st, LockPath: filepath.Join(dir, "co-tune.lock"), Tester: tt}
	if _, err := r.RunTrial(context.Background(), -21); err != nil {
		fmt.Fprintf(os.Stderr, "helper: RunTrial returned %v\n", err)
		os.Exit(8)
	}
	os.Exit(0)
}

func spawnCrashedChild(t *testing.T, dir, crashAt string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestTuneHelperProcess$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "TUNE_CRASH_AT="+crashAt, "TUNE_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("helper %q: err=%v out=%s", crashAt, err, out)
	}
}

func TestSubprocessCrashRecoveryAtEachTransition(t *testing.T) {
	for _, crashAt := range []string{"prepared", "apply", "in_test", "test_finished"} {
		t.Run(crashAt, func(t *testing.T) {
			h := newHarness(t) // parent creates the plan in-process
			spawnCrashedChild(t, h.dir, crashAt)

			// The parent is a genuinely different process from the
			// crashed child: exactly the crash + restart shape.
			r2, f2 := freshRunner(h.dir, &scriptTester{})
			rec, err := r2.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("reconcile after crash at %s: %v", crashAt, err)
			}
			if rec == nil || rec.Outcome != OutcomeInterruptedUnknown || rec.State != TrialDecided {
				t.Fatalf("trial after crash at %s: %+v", crashAt, rec)
			}
			if got := f2.hwProfile(t); !profileEqual(got, baseProf()) {
				t.Errorf("hardware not back at baseline after crash at %s: %+v", crashAt, got)
			}
			p := loadPlanT(t, r2.Store)
			if p.Status != StatusOpen {
				t.Errorf("plan must return to open after reconciliation: %+v", p)
			}
			if p.AttemptsUsed != 1 {
				t.Errorf("attempt budget must not be refunded: %d", p.AttemptsUsed)
			}
			// The interrupted trial is never auto-accepted, even when
			// the evidence survived the crash (test_finished case):
			// acceptance requires a durable DECISION, not just data.
			for _, tr := range p.Trials {
				if tr.Outcome == OutcomeAccepted {
					t.Errorf("crash at %s produced an accepted trial", crashAt)
				}
			}
		})
	}
}
