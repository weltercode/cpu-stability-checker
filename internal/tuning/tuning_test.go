package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

func baseProf() co.Profile {
	return co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}, {Core: "core1", Value: -20}}}
}

func okCaps() co.Capabilities {
	return co.Capabilities{Tool: "fake", Version: "1",
		Supported: co.Yes("fake"), Read: co.Yes("fake"), Readback: co.Yes("fake"), Apply: co.Yes("fake"),
		RangeKnown: true, RangeMin: -99, RangeMax: 99}
}

func okMap() co.Mapping {
	i0, i1 := 0, 1
	return co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: &i0}, {ToolID: "core1", SMUIndex: &i1}}}
}

func okSnap() co.Snapshot {
	return co.Snapshot{Profile: baseProf(), Quality: "exact", HardwareID: "fp-machine"}
}

func fakeOK() *co.Fake {
	return &co.Fake{Caps: okCaps(), Map: okMap(), Reads: []co.Snapshot{okSnap()}}
}

// plainBackend satisfies co.Backend but deliberately NOT FakeBackend:
// it stands in for the real adapter, which P11 automation must refuse.
type plainBackend struct{}

func (plainBackend) Probe(context.Context) (co.Capabilities, error) {
	return co.Capabilities{}, &co.Error{Kind: co.KindNotValidated, Op: "probe", Err: errors.New("real backend: P11 must not touch it yet")}
}
func (plainBackend) Topology(context.Context) (co.Mapping, error) {
	return co.Mapping{}, errors.New("no")
}
func (plainBackend) Read(context.Context) (co.Snapshot, error) {
	return co.Snapshot{}, errors.New("no")
}
func (plainBackend) Validate(co.Profile) error { return errors.New("no") }
func (plainBackend) Apply(context.Context, co.Profile) (co.WriteResult, error) {
	return co.WriteResult{}, errors.New("no")
}
func (plainBackend) Verify(context.Context, co.Profile) (co.Verification, error) {
	return co.Verification{}, errors.New("no")
}
func (plainBackend) Restore(context.Context, co.Snapshot) (co.Verification, error) {
	return co.Verification{}, errors.New("no")
}

func optsOK() Options {
	return Options{TargetCore: "core1", RangeMin: -99, RangeMax: 99, TempLimitC: 85, AttemptLimit: 5, Seed: 7}
}

func TestCreatePlanRefusesNonFakeBackend(t *testing.T) {
	_, err := CreatePlan(context.Background(), plainBackend{}, optsOK())
	if !co.IsKind(err, co.KindNotValidated) {
		t.Fatalf("non-fake backend not refused: %v", err)
	}
}

func TestCreatePlanBindsIdentityAndBaseline(t *testing.T) {
	p, err := CreatePlan(context.Background(), fakeOK(), optsOK())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Fake || p.Status != StatusOpen || p.PlanID == "" || p.MappingFP == "" || p.HardwareID != "fp-machine" {
		t.Errorf("plan identity wrong: %+v", p)
	}
	if !profileEqual(p.Baseline, baseProf()) || p.Initial.Quality != "exact" {
		t.Errorf("baseline must be the user's real profile, not zeros: %+v", p.Baseline)
	}
	if p.TargetCore != "core1" || p.AttemptLimit != 5 || p.TempLimitC != 85 {
		t.Errorf("options not recorded: %+v", p)
	}
}

func TestCreatePlanRefusals(t *testing.T) {
	cases := []struct {
		name string
		b    co.Backend
		opts Options
		kind string
	}{
		{"unsupported", &co.Fake{Caps: co.Capabilities{Supported: co.No("no CO access"), Read: co.Yes("x"), Readback: co.Yes("x")}, Map: okMap(), Reads: []co.Snapshot{okSnap()}}, optsOK(), co.KindUnsupportedCPU},
		{"readback unknown", &co.Fake{Caps: co.Capabilities{Supported: co.Yes("y"), Read: co.Yes("y"), Readback: co.Unknown("cannot read back")}, Map: okMap(), Reads: []co.Snapshot{okSnap()}}, optsOK(), co.KindNotValidated},
		{"non-exact read", &co.Fake{Caps: okCaps(), Map: okMap(), Reads: []co.Snapshot{{Profile: baseProf(), Quality: "partial"}}}, optsOK(), co.KindNotValidated},
		{"core not in mapping", fakeOK(), func() Options { o := optsOK(); o.TargetCore = "core9"; return o }(), co.KindBadProfile},
		{"inverted range", fakeOK(), func() Options { o := optsOK(); o.RangeMin = 5; o.RangeMax = -5; return o }(), co.KindBadProfile},
		{"zero attempt limit", fakeOK(), func() Options { o := optsOK(); o.AttemptLimit = 0; return o }(), co.KindBadProfile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreatePlan(context.Background(), tc.b, tc.opts)
			if !co.IsKind(err, tc.kind) {
				t.Fatalf("want %s, got %v", tc.kind, err)
			}
		})
	}
}

func samplePlan(t *testing.T) Plan {
	t.Helper()
	p, err := CreatePlan(context.Background(), fakeOK(), optsOK())
	if err != nil {
		t.Fatal(err)
	}
	p.Trials = []Trial{{TrialID: "trial-1", Attempt: 1, Core: "core1", FromValue: -20, Candidate: -21,
		State: TrialDecided, Outcome: OutcomeAccepted}}
	return *p
}

func TestParsePlanStrictness(t *testing.T) {
	p := samplePlan(t)
	good, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePlan(good); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	cases := []struct{ name, mutate, want string }{
		{"unknown version", strings.Replace(string(good), `"plan_version":1`, `"plan_version":2`, 1), "unsupported plan_version"},
		{"missing version", strings.Replace(string(good), `"plan_version":1,`, ``, 1), "missing plan_version"},
		{"unknown field", strings.Replace(string(good), `"trials"`, `"bogus":1,"trials"`, 1), "unreadable"},
		{"unknown status", strings.Replace(string(good), `"status":"open"`, `"status":"weird"`, 1), "unknown plan status"},
		{"unknown trial state", strings.Replace(string(good), `"state":"decided"`, `"state":"half"`, 1), "unknown state"},
		{"empty plan id", strings.Replace(string(good), `"`+p.PlanID+`"`, `""`, 1), "empty plan_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlan([]byte(tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
	// A decided trial without an outcome is a shape error.
	bad := samplePlan(t)
	bad.Trials[0].Outcome = ""
	b, _ := json.Marshal(bad)
	if _, err := ParsePlan(b); err == nil || !strings.Contains(err.Error(), "no outcome") {
		t.Fatalf("decided trial without outcome accepted: %v", err)
	}
}

func TestStoreBackupAndLoad(t *testing.T) {
	st := &Store{Path: filepath.Join(t.TempDir(), "plan.json")}
	p1 := samplePlan(t)
	if err := st.Save(p1, "create"); err != nil {
		t.Fatal(err)
	}
	p2 := samplePlan(t)
	p2.AttemptsUsed = 3
	if err := st.Save(p2, "update"); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil || got == nil || got.AttemptsUsed != 3 {
		t.Fatalf("main not current: %+v err=%v", got, err)
	}
	bb, err := os.ReadFile(st.bakPath())
	if err != nil {
		t.Fatal(err)
	}
	var bak Plan
	if err := json.Unmarshal(bb, &bak); err != nil || bak.AttemptsUsed != 0 {
		t.Errorf("backup must hold the previous document: err=%v bak=%+v", err, bak)
	}
}

func TestStoreCorruptMainExplicitBackupPromotion(t *testing.T) {
	st := &Store{Path: filepath.Join(t.TempDir(), "plan.json")}
	p1 := samplePlan(t)
	p1.AttemptsUsed = 1
	if err := st.Save(p1, "a"); err != nil {
		t.Fatal(err)
	}
	p2 := samplePlan(t)
	p2.AttemptsUsed = 2
	if err := st.Save(p2, "b"); err != nil {
		t.Fatal(err)
	}
	// Truncate the main document (crash-shape).
	if err := os.WriteFile(st.path(), []byte(`{"plan_version":1,"plan_`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := st.Load()
	var ce *PlanCorruptError
	if !errors.As(err, &ce) || !ce.BakGood {
		t.Fatalf("corrupt main must report a good backup: %v", err)
	}
	// Reading never repairs: the corrupt bytes are still there.
	b, _ := os.ReadFile(st.path())
	if !strings.Contains(string(b), `"plan_`) {
		t.Errorf("Load rewrote the corrupt document: %q", b)
	}
	// Promotion is explicit.
	got, err := st.RecoverBackup()
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptsUsed != 1 {
		t.Errorf("promoted backup wrong: %+v", got)
	}
	// The corrupt bytes are preserved, never destroyed.
	keeps, _ := filepath.Glob(st.path() + ".corrupt-*")
	if len(keeps) != 1 {
		t.Errorf("corrupt document not preserved: %v", keeps)
	}
}

func TestStoreCorruptNoBackupFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	st := &Store{Path: path}
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	_, err := st.Load()
	if err == nil || !strings.Contains(err.Error(), "no backup exists") {
		t.Fatalf("want fail-closed error, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("reading the plan mutated it")
	}
	if _, err := st.RecoverBackup(); err == nil {
		t.Errorf("RecoverBackup must refuse when no backup exists")
	}
}

func TestLoadNeverMutates(t *testing.T) {
	st := &Store{Path: filepath.Join(t.TempDir(), "plan.json")}
	if err := st.Save(samplePlan(t), "create"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(st.path())
	if _, err := st.Load(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(st.path())
	if string(before) != string(after) {
		t.Errorf("Load rewrote the plan document")
	}
}
