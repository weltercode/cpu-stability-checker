package plan

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

func switchBase() Options {
	return Options{
		TestName:     "general",
		PerProcess:   true,
		SwitchPairs:  [][]int{{0, 1}, {2, 3}},
		SwitchRounds: 3,
		SwitchGap:    DefaultSwitchGap,
		SwitchRun:    DefaultSwitchRun,
	}
}

// requireFixtureCPUs guards fixtures that NAME fixed logical CPU
// numbers: ValidateSwitch bounds them by runtime.NumCPU(), so on a
// smaller machine the numbers cannot exist and a failure would judge
// machine size, not the code (the pattern is set by the matrix guard
// below, and by the WMI-level skips R02 / memory-admission skips R12:
// restricted machine reports by name, capable machine runs the exact
// original assertions).
func requireFixtureCPUs(t *testing.T, need int) {
	t.Helper()
	if n := runtime.NumCPU(); n < need {
		t.Skipf("fixture shape names CPUs 0..%d: needs %d logical CPUs to stay in range, this box has %d", need-1, need, n)
	}
}

func TestValidateSwitchAbsent(t *testing.T) {
	if err := ValidateSwitch(Options{TestName: "general", Duration: time.Second}); err != nil {
		t.Errorf("absent pair-switch must be legal and shape nothing: %v", err)
	}
}

func TestValidateSwitchValidShape(t *testing.T) {
	requireFixtureCPUs(t, 4) // switchBase names cores 0..3
	opt := switchBase()
	if err := ValidateSwitch(opt); err != nil {
		t.Fatalf("valid minimal pair-switch refused: %v", err)
	}
	// Seeded order and jitter are legal once the shape is whole.
	opt.SwitchOrder = "seeded"
	opt.SwitchJitter = 100 * time.Millisecond
	opt.SwitchSeed = 7
	if err := ValidateSwitch(opt); err != nil {
		t.Fatalf("seeded pair-switch refused: %v", err)
	}
}

// The scope-matrix increment (2026-11-02) sanctioning N>=2 groups is
// only honest if N>2 shapes actually validate: three groups is the
// smallest matrix, and a many-group sweep must still pass every rule
// (distinct participants, geometry builds). Guarded on CPU count so
// the range check never refuses for the wrong reason on a small box.
func TestValidateSwitchMatrixGroupsLegal(t *testing.T) {
	if runtime.NumCPU() < 12 {
		t.Skipf("matrix shape needs 12 logical CPUs to stay in range, this box has %d", runtime.NumCPU())
	}
	opt := switchBase()
	opt.SwitchPairs = [][]int{{0, 1}, {2, 3}, {4, 5}}
	if err := ValidateSwitch(opt); err != nil {
		t.Errorf("three prepared groups refused after the matrix increment: %v", err)
	}
	opt.SwitchPairs = [][]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}, {8, 9}, {10, 11}}
	opt.SwitchRounds = 2
	if err := ValidateSwitch(opt); err != nil {
		t.Errorf("six prepared groups refused after the matrix increment: %v", err)
	}
}

func TestValidateSwitchRefusals(t *testing.T) {
	requireFixtureCPUs(t, 5) // the three-core group names core 4; a size refusal would mask the named refusals under test
	huge := runtime.NumCPU() + 8
	for _, c := range []struct {
		name      string
		mutate    func(*Options)
		wantInErr string
	}{
		{"present but no rounds", func(o *Options) { o.SwitchRounds = 0 }, "needs switch_rounds"},
		{"one group", func(o *Options) { o.SwitchPairs = [][]int{{0, 1}} }, "at least two prepared groups, got 1"},
		{"group of three cores", func(o *Options) { o.SwitchPairs[1] = []int{2, 3, 4} }, "exactly two cores [aggressor, observer]"},
		{"core in two groups", func(o *Options) { o.SwitchPairs[1] = []int{1, 3} }, "sits in two switch groups at once"},
		{"negative core", func(o *Options) { o.SwitchPairs[0] = []int{-1, 1} }, "is negative"},
		{"core out of range", func(o *Options) { o.SwitchPairs[1] = []int{huge, 3} }, "outside the available range"},
		{"zero run", func(o *Options) { o.SwitchRun = 0 }, "run window must be positive"},
		{"negative gap", func(o *Options) { o.SwitchGap = -time.Millisecond }, "must not be negative"},
		{"negative jitter", func(o *Options) { o.SwitchJitter = -time.Millisecond }, "must not be negative"},
		{"unknown order word", func(o *Options) { o.SwitchOrder = "random" }, `unknown switch_order "random"`},
		{"stray seed demands rounds", func(o *Options) {
			o.SwitchPairs, o.SwitchRounds, o.SwitchGap, o.SwitchRun = nil, 0, 0, 0
			o.SwitchSeed = 9
		}, "needs switch_rounds"},
		{"bursts mix", func(o *Options) { o.Bursts = 2 }, "defines two phases at once"},
		{"handoff mix", func(o *Options) { o.HandoffBlocks = 4 }, "one parent-arbiter schedule"},
		{"in-process", func(o *Options) { o.PerProcess = false }, "needs per-process mode"},
		{"rotate mix", func(o *Options) { o.Rotate = true }, "do not mix"},
		{"workers>1", func(o *Options) { o.Workers = 2 }, "parsed but never run"},
	} {
		opt := switchBase()
		c.mutate(&opt)
		err := ValidateSwitch(opt)
		if err == nil {
			t.Errorf("%s: want refusal, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantInErr) {
			t.Errorf("%s: err = %v, want substring %q", c.name, err, c.wantInErr)
		}
	}
}

// The hash-identity contract at the marshaling level (the same test
// role parking_test pins for Q04): Options is what planHash marshals
// wholesale, so the whole pair-switch mode must be INVISIBLE at its
// zero values - every pre-Q05 saved recipe keeps its exact bytes and
// therefore its exact hash - and fully VISIBLE once active, so a
// switch run can never share an experiment identity with a run that
// never commanded a switch.
func TestSwitchFieldsIdentityContract(t *testing.T) {
	b, err := json.Marshal(Options{TestName: "general", Bursts: 2, Parking: "try"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"Switch`) {
		t.Errorf(`absent pair-switch must be invisible to the marshaled plan (omitempty is a machine contract, not style): %s`, b)
	}
	b, err = json.Marshal(switchBase())
	if err != nil {
		t.Fatal(err)
	}
	// Duration marshals as integer nanoseconds (encoding/json, no
	// custom marshaler): pin that too - the byte shape of the recipe
	// is the contract.
	for _, key := range []string{`"SwitchPairs":[[0,1],[2,3]]`, `"SwitchRounds":3`, `"SwitchGap":600000000`, `"SwitchRun":200000000`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("active pair-switch must be visible to the plan hash, missing %s in %s", key, b)
		}
	}
}

func TestReproductionParametersStatesSwitch(t *testing.T) {
	line := ReproductionParameters(switchBase())
	if !strings.Contains(line, "switch=2groups x 3rounds/gap600ms/run200ms") {
		t.Errorf("reproduction line must state the turn-taking shape: %q", line)
	}
	plain := ReproductionParameters(Options{TestName: "general", Bursts: 2, BurstGap: DefaultBurstGap, BurstRun: DefaultBurstRun, Duration: 0})
	if strings.Contains(plain, "switch=") {
		t.Errorf("a run that never commanded a switch must not claim one: %q", plain)
	}
}

// switch_scope/switch_cores are scenario-COMPILER words: they pair
// groups off the detected topology before the plan ever sees them.
// Generation words arriving at OptionsFromPhase with no generated
// groups come from a path that skipped the compiler - and would then
// silently define nothing - so they are refused by name.
func TestOptionsFromPhaseRefusesUnresolvedScopeWords(t *testing.T) {
	ph := config.Phase{Name: "gen", SwitchScope: "ccx", SwitchRounds: 1}
	if _, err := OptionsFromPhase(config.Default(), ph); err == nil || !strings.Contains(err.Error(), "without generated groups") {
		t.Errorf("unresolved switch_scope must be named, got %v", err)
	}
	ph2 := config.Phase{Name: "gen2", SwitchCores: []int{0, 1, 2, 3}, SwitchRounds: 1}
	if _, err := OptionsFromPhase(config.Default(), ph2); err == nil || !strings.Contains(err.Error(), "without generated groups") {
		t.Errorf("unresolved switch_cores must be named, got %v", err)
	}
}
