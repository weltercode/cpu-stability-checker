package cli

// The Q05 scope matrix (2026-11-02): switch_scope/switch_cores pair
// groups off the DETECTED topology instead of naming them by hand,
// feeding ONE pair-switch session with N prepared groups. The golden
// vectors below run on SYNTHETIC maps (a two-CCX box, SMT siblings)
// so the derivation is pinned independently of any real machine -
// the generality rule is that no test, and no code, hardcodes
// per-machine numbers.

import (
	"runtime"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// synthTopology: 8 logical CPUs, 4 physical (SMT siblings 0|1, 2|3,
// 4|5, 6|7), two L3 domains: 0-3 in ccx 0, 4-7 in ccx 1.
func synthTopology() (inv []int, ccx, smt map[int]int) {
	inv = []int{0, 1, 2, 3, 4, 5, 6, 7}
	ccx = map[int]int{0: 0, 1: 0, 2: 0, 3: 0, 4: 1, 5: 1, 6: 1, 7: 1}
	smt = map[int]int{0: 0, 1: 0, 2: 1, 3: 1, 4: 2, 5: 2, 6: 3, 7: 3}
	return inv, ccx, smt
}

func genPhase(fields ...func(*config.Phase)) config.Phase {
	ph := config.Phase{Name: "mtx", SwitchRounds: 2}
	for _, f := range fields {
		f(&ph)
	}
	return ph
}

func TestExpandSwitchGroupsGolden(t *testing.T) {
	inv, ccx, smt := synthTopology()
	for _, tc := range []struct {
		name string
		ph   config.Phase
		want [][]int
	}{
		{"ccx default pool pairs within domains",
			genPhase(func(p *config.Phase) { p.SwitchScope = "ccx" }),
			[][]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}}},
		{"smt scope pairs siblings only",
			genPhase(func(p *config.Phase) { p.SwitchScope = "smt" }),
			[][]int{{0, 1}, {2, 3}, {4, 5}, {6, 7}}},
		{"all scope crosses domains by pool ORDER alone",
			genPhase(func(p *config.Phase) {
				p.SwitchScope = "all"
				p.SwitchCores = []int{0, 4, 1, 5, 2, 6, 3, 7}
			}),
			[][]int{{0, 4}, {1, 5}, {2, 6}, {3, 7}}},
		{"pool without scope defaults to ccx like pairs",
			genPhase(func(p *config.Phase) { p.SwitchCores = []int{0, 2, 4, 6} }),
			[][]int{{0, 2}, {4, 6}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := expandSwitchGroups([]config.Phase{tc.ph}, inv, ccx, smt)
			if err != nil {
				t.Fatalf("generation refused: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("phases out = %d, want the switch phase kept as ONE session", len(out))
			}
			got := out[0].SwitchPairs
			if len(got) != len(tc.want) {
				t.Fatalf("groups = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i][0] != tc.want[i][0] || got[i][1] != tc.want[i][1] {
					t.Errorf("group %d = %v, want %v (greedy listed order is the pinned spec)", i, got[i], tc.want[i])
				}
			}
		})
	}

	// The cross-domain claim is data: under scope "all" with the
	// interleaved pool every generated group crosses ccx - and the
	// scope word itself never made that promise (it is the pool order
	// that did).
	t.Run("interleaved pool yields cross-domain groups", func(t *testing.T) {
		out, err := expandSwitchGroups([]config.Phase{genPhase(func(p *config.Phase) {
			p.SwitchScope = "all"
			p.SwitchCores = []int{0, 4, 1, 5, 2, 6, 3, 7}
		})}, inv, ccx, smt)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range out[0].SwitchPairs {
			if ccx[g[0]] == ccx[g[1]] {
				t.Errorf("group %v stayed inside one domain despite the interleaved pool", g)
			}
		}
	})

	// A manual phase (no generation words) passes through untouched.
	t.Run("manual groups untouched", func(t *testing.T) {
		ph := genPhase(func(p *config.Phase) { p.SwitchPairs = [][]int{{7, 3}, {1, 0}} })
		out, err := expandSwitchGroups([]config.Phase{ph}, inv, ccx, smt)
		if err != nil {
			t.Fatal(err)
		}
		if out[0].SwitchPairs[0][0] != 7 || out[0].SwitchPairs[1][1] != 0 {
			t.Errorf("generation rewrote a manual list: %v", out[0].SwitchPairs)
		}
	})
}

func TestExpandSwitchGroupsRefusals(t *testing.T) {
	inv, ccx, smt := synthTopology()
	for _, tc := range []struct {
		name string
		ph   config.Phase
		want string
	}{
		{"explicit pairs with scope", genPhase(func(p *config.Phase) {
			p.SwitchScope = "all"
			p.SwitchPairs = [][]int{{0, 1}, {2, 3}}
		}), "one source of switch groups"},
		{"unknown scope word", genPhase(func(p *config.Phase) { p.SwitchScope = "ccd" }), `must be "ccx", "smt" or "all"`},
		{"pool too small", genPhase(func(p *config.Phase) { p.SwitchScope = "all"; p.SwitchCores = []int{0} }), "at least two logical CPUs"},
		{"duplicate pool member", genPhase(func(p *config.Phase) { p.SwitchScope = "all"; p.SwitchCores = []int{0, 1, 1, 4} }), "listed more than once"},
		{"negative pool member", genPhase(func(p *config.Phase) { p.SwitchScope = "all"; p.SwitchCores = []int{0, -2} }), "is negative"},
		{"pool names a CPU this system lacks", genPhase(func(p *config.Phase) { p.SwitchScope = "all"; p.SwitchCores = []int{0, 99} }), "not a logical CPU this system reported"},
		{"ccx scope without cache topology", genPhase(func(p *config.Phase) { p.SwitchScope = "ccx" }), "needs CPU cache topology"},
		{"smt scope without CPU topology", genPhase(func(p *config.Phase) { p.SwitchScope = "smt" }), "needs CPU topology"},
		{"odd ccx domain leaves a core behind", genPhase(func(p *config.Phase) {
			p.SwitchScope = "ccx"
			p.SwitchCores = []int{0, 1, 2}
		}), "left core 2 unpaired"},
		{"smt sibling outside the pool leaves a core behind", genPhase(func(p *config.Phase) {
			p.SwitchScope = "smt"
			p.SwitchCores = []int{0, 1, 4}
		}), "left core 4 unpaired"},
		{"pool of two produces one group", genPhase(func(p *config.Phase) {
			p.SwitchScope = "all"
			p.SwitchCores = []int{0, 1}
		}), "produced one group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ccxIn, smtIn map[int]int
			if !strings.Contains(tc.name, "without cache topology") {
				ccxIn = ccx
			}
			if !strings.Contains(tc.name, "without CPU topology") {
				smtIn = smt
			}
			_, err := expandSwitchGroups([]config.Phase{tc.ph}, inv, ccxIn, smtIn)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// compileScenario wiring on the REAL detected topology: a small named
// pool (0..3 exists on every >=4-CPU box) under scope "all" compiles
// to generated disjoint groups, per-process, all_or_none, hashed.
func TestCompileScenarioSwitchScopeGeneratedGroups(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skipf("pool 0..3 needs 4 logical CPUs, this box has %d", runtime.NumCPU())
	}
	cfg := config.Default()
	cfg.WorkloadA, cfg.WorkloadB = "float", "float"
	cfg.Phases = []config.Phase{{
		Name: "mtx", SwitchScope: "all", SwitchCores: []int{0, 1, 2, 3},
		SwitchRounds: 1, SwitchGap: "40ms", SwitchRun: "100ms",
	}}
	opts, _, err := compileScenario(cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("phases = %d, want one generated switch session", len(opts))
	}
	o := opts[0]
	if len(o.SwitchPairs) != 2 || o.SwitchPairs[0][0] != 0 || o.SwitchPairs[0][1] != 1 || o.SwitchPairs[1][0] != 2 || o.SwitchPairs[1][1] != 3 {
		t.Errorf("generated groups = %v, want [[0 1] [2 3]] from the listed pool", o.SwitchPairs)
	}
	if !plan.SwitchActive(o) || !o.PerProcess || !o.AllOrNone {
		t.Errorf("generated switch phase lost the shape contract: per_process=%v all_or_none=%v", o.PerProcess, o.AllOrNone)
	}
	if len(o.Cores) != 4 {
		t.Errorf("participant cores = %v, want the generated members", o.Cores)
	}

	// Replay honesty: the same authoring words recompile to the same
	// identity, and a different pool is a different experiment.
	h1 := planHash(lastRun{}, o)
	opts2, _, err := compileScenario(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if planHash(lastRun{}, opts2[0]) != h1 {
		t.Error("the same scope+pool must recompile to the same experiment identity")
	}
	cfg2 := cfg
	cfg2.Phases = []config.Phase{{
		Name: "mtx", SwitchScope: "all", SwitchCores: []int{0, 2, 1, 3},
		SwitchRounds: 1, SwitchGap: "40ms", SwitchRun: "100ms",
	}}
	opts3, _, err := compileScenario(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if planHash(lastRun{}, opts3[0]) == h1 {
		t.Error("a different pool order generates different groups and must hash differently")
	}
}

// The pre-expansion conflict rule: generator shapes that would DROP
// the switch words during expansion are refused at entry, before
// anything can be lost.
func TestSwitchScopeEntryConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		ph   config.Phase
		want string
	}{
		{"pairs generator", config.Phase{Name: "c1", SwitchScope: "all", Pairs: &config.Pairs{Cores: []int{0, 1}}}, "one shape per entry"},
		{"curve generator", config.Phase{Name: "c2", SwitchScope: "all", Curve: &config.Curve{Cores: []int{0, 1}}}, "one shape per entry"},
		{"burst generator would drop the switch words", config.Phase{Name: "c3", SwitchScope: "all", Burst: &config.Burst{Cores: []int{0}}}, "one shape per entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Phases = []config.Phase{tc.ph}
			_, _, err := compileScenario(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
