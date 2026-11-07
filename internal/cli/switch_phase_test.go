package cli

// Q05d scenario integration for prepared pair-switch phases: the
// compile resolves the whole shape up front (defaults included, a
// half-shape named-refused before any load), the plan line states the
// groups, the order word and the measured quantity instead of a
// clock, the experiment identity covers every switch knob (and slot B
// DOES belong here - the observer loads it), and the journal estimate
// counts the turn records the parent will write.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func switchPhaseOpt() plan.Options {
	return plan.Options{
		TestName: "general", Phase: "sw", A: "float", B: "float",
		Cores: []int{0, 1, 2, 3}, PerProcess: true, AllOrNone: true, StopOnFault: true,
		SwitchPairs:  [][]int{{0, 1}, {2, 3}},
		SwitchRounds: 2, SwitchGap: plan.DefaultSwitchGap, SwitchRun: plan.DefaultSwitchRun,
		SwitchOrder: "seeded", SwitchSeed: 7,
		Duration: 30 * time.Second, // inherited, never used by this phase
	}
}

func TestSwitchScenarioCompileFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA, cfg.WorkloadB = "float", "float"
	cfg.Phases = []config.Phase{{
		Name: "sw", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchRounds: 2,
		SwitchGap: "40ms", SwitchRun: "100ms", SwitchOrder: "seeded", SwitchSeed: 7,
	}}
	opts, _, err := compileScenario(cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("phases = %d, want 1", len(opts))
	}
	o := opts[0]
	if o.SwitchRounds != 2 || o.SwitchGap != 40*time.Millisecond || o.SwitchRun != 100*time.Millisecond {
		t.Errorf("compiled geometry = %+v, want 2 rounds / 40ms / 100ms", o)
	}
	if o.SwitchOrder != "seeded" || o.SwitchSeed != 7 {
		t.Errorf("compiled order = %q seed %d, want seeded/7", o.SwitchOrder, o.SwitchSeed)
	}
	if !o.PerProcess {
		t.Error("a prepared participant IS a pinned process: compile must force per-process")
	}
	if !o.AllOrNone {
		t.Error("compiled switch phase lacks the forced all_or_none (participants are load-bearing)")
	}
	if len(o.Cores) != 4 || o.Cores[0] != 0 || o.Cores[3] != 3 {
		t.Errorf("compiled cores = %v, want the participant list [0 1 2 3] (not the config default)", o.Cores)
	}
	if !plan.SwitchActive(o) {
		t.Error("compiled phase must read as switch-active to the executor dispatch")
	}

	// Defaults come from the ONE source of truth, and the parking word
	// is allowed on a switch phase (its gaps ARE measured idle windows).
	cfg2 := config.Default()
	cfg2.WorkloadA, cfg2.WorkloadB = "float", ""
	cfg2.Phases = []config.Phase{{
		Name: "sw2", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchRounds: 1, ParkingPolicy: "try",
	}}
	opts2, _, err := compileScenario(cfg2)
	if err != nil {
		t.Fatalf("compile defaults: %v", err)
	}
	if opts2[0].SwitchGap != plan.DefaultSwitchGap || opts2[0].SwitchRun != plan.DefaultSwitchRun {
		t.Errorf("defaults = %s/%s, want %s/%s", opts2[0].SwitchGap, opts2[0].SwitchRun, plan.DefaultSwitchGap, plan.DefaultSwitchRun)
	}
	if opts2[0].Parking != "try" {
		t.Errorf("parking word on a switch phase must survive compile, got %q", opts2[0].Parking)
	}
}

func TestSwitchScenarioCompileRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		ph   config.Phase
		want string
	}{
		{"no rounds", config.Phase{Name: "a", SwitchPairs: [][]int{{0, 1}, {2, 3}}}, "needs switch_rounds"},
		{"one group", config.Phase{Name: "c", SwitchPairs: [][]int{{0, 1}}, SwitchRounds: 1}, "at least two prepared groups"},
		{"core in two groups", config.Phase{Name: "d", SwitchPairs: [][]int{{0, 1}, {1, 2}}, SwitchRounds: 1}, "two switch groups at once"},
		{"three-slot group", config.Phase{Name: "e", SwitchPairs: [][]int{{0, 1}, {2, 3, 4}}, SwitchRounds: 1}, "exactly two cores"},
		{"stray gap word", config.Phase{Name: "f", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchGap: "300ms"}, "needs switch_rounds"},
		{"bursts and switch", config.Phase{Name: "g", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchRounds: 1, Bursts: 2}, "pair-switch schedule"},
		{"bad duration word", config.Phase{Name: "h", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchRounds: 1, SwitchGap: "banana"}, "Go duration"},
		{"unknown order word", config.Phase{Name: "i", SwitchPairs: [][]int{{0, 1}, {2, 3}}, SwitchRounds: 1, SwitchOrder: "chaos"}, `unknown switch_order`},
		{"parking without shape", config.Phase{Name: "j", Cores: []int{0}, ParkingPolicy: "required", Duration: "1s"}, "no bursts or pair-switch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.WorkloadA, cfg.WorkloadB = "float", ""
			cfg.Phases = []config.Phase{tc.ph}
			_, _, err := compileScenario(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("compile error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestSwitchScenarioPlanLine(t *testing.T) {
	var buf bytes.Buffer
	printScenarioPlan(&buf, config.Default(), []plan.Options{switchPhaseOpt()}, 1)
	out := buf.String()
	for _, want := range []string{
		"mode=switch(g0[aggressor 0, observer 1] g1[aggressor 2, observer 3]",
		"order=seeded seed=7",
		"all_or_none",
		"upper bound incl IPC",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan line lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "duration=30s") {
		t.Errorf("switch phase advertised a clock criterion it never uses:\n%s", out)
	}
}

func TestSwitchPhaseHashIdentity(t *testing.T) {
	base := planHash(lastRun{}, switchPhaseOpt())
	muts := []struct {
		name string
		mut  func(*plan.Options)
	}{
		{"seed", func(o *plan.Options) { o.SwitchSeed = 8 }},
		{"order word", func(o *plan.Options) { o.SwitchOrder = "" }},
		{"rounds", func(o *plan.Options) { o.SwitchRounds = 3 }},
		{"pair cores", func(o *plan.Options) { o.SwitchPairs = [][]int{{0, 1}, {2, 4}} }},
		{"gap", func(o *plan.Options) { o.SwitchGap = plan.DefaultSwitchGap + time.Second }},
		// Unlike handoff, the switch observer DOES load slot B: it
		// belongs to the identity, and the hash must say so.
		{"observer B", func(o *plan.Options) { o.B = "int" }},
	}
	for _, m := range muts {
		o := switchPhaseOpt()
		m.mut(&o)
		if planHash(lastRun{}, o) == base {
			t.Errorf("%s must be part of the switch identity", m.name)
		}
	}
	if planHash(lastRun{}, switchPhaseOpt()) != base {
		t.Error("the same compiled shape must hash identically (replay contract)")
	}
}

// The journal estimate counts what the parent will actually write:
// one switch_schedule, one session_switch per turn, both views of
// every turn per participant, plus per-participant lifecycle records.
func TestSwitchJournalEstimate(t *testing.T) {
	opt := switchPhaseOpt()
	records, beats, bytes := journalEstimate([]plan.Options{opt}, 1, 32)
	// 2 groups x 2 rounds = 4 turns, 4 participants:
	// 1 + 4 + 4*4*2 + 4 = 41 records at the exact parent-side floor.
	if records < 41 {
		t.Errorf("records = %d, want >= 41 (the parent's own floor)", records)
	}
	if beats <= 0 || bytes < records*200 {
		t.Errorf("beats/bytes look empty: beats=%d bytes=%d", beats, bytes)
	}
}
