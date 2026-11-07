package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// cycleFixtureGroups: an ASYMMETRIC topology on purpose — physical core 2
// has a single logical CPU (no SMT sibling), so "both" must cope with
// groups of different sizes and "one" must not assume pairs.
func cycleFixtureGroups() []CycleGroup {
	return []CycleGroup{
		{Physical: 0, Logicals: []int{0, 8}},
		{Physical: 1, Logicals: []int{1, 9}},
		{Physical: 2, Logicals: []int{2}},
		{Physical: 3, Logicals: []int{3, 11}},
	}
}

func cyclePhase(cycle *config.Cycle) []config.Phase {
	return []config.Phase{{Name: "cc", Cycle: cycle}}
}

func cycleOrder(groups []CycleGroup) []int {
	var out []int
	for _, g := range groups {
		out = append(out, g.Physical)
	}
	return out
}

func TestExpandCycle(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA = "int"
	cfg.Duration = "30s"

	t.Run("smt one sequential: one phase per physical core", func(t *testing.T) {
		got, err := expandCycle(cyclePhase(&config.Cycle{}), cfg, cycleFixtureGroups(), &[]string{})
		if err != nil {
			t.Fatalf("expandCycle: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("phases = %d, want 4 (one per physical core)", len(got))
		}
		for i, ph := range got {
			if ph.Name != []string{"cc-p0", "cc-p1", "cc-p2", "cc-p3"}[i] {
				t.Errorf("phase %d name = %q, want sequential physical order", i, ph.Name)
			}
			if len(ph.Cores) != 1 {
				t.Errorf("phase %s cores = %v, want one logical CPU (smt one)", ph.Name, ph.Cores)
			}
			if !ph.PerProcess {
				t.Errorf("phase %s is not per-process: participants would not be pinned", ph.Name)
			}
			if ph.AllOrNone {
				t.Errorf("phase %s all_or_none set: a single-core phase has no group to keep whole", ph.Name)
			}
			if ph.Settle != "2s" || ph.Duration != "30s" || ph.A != "int" {
				t.Errorf("phase %s defaults wrong: settle=%s duration=%s a=%s", ph.Name, ph.Settle, ph.Duration, ph.A)
			}
		}
	})

	t.Run("smt both: whole sibling group, indivisible", func(t *testing.T) {
		got, err := expandCycle(cyclePhase(&config.Cycle{SMT: "both"}), cfg, cycleFixtureGroups(), &[]string{})
		if err != nil {
			t.Fatalf("expandCycle: %v", err)
		}
		want := map[string][]int{"cc-p0": {0, 8}, "cc-p1": {1, 9}, "cc-p2": {2}, "cc-p3": {3, 11}}
		for _, ph := range got {
			w, ok := want[ph.Name]
			if !ok {
				t.Fatalf("unexpected phase %q", ph.Name)
			}
			if len(ph.Cores) != len(w) {
				t.Errorf("phase %s cores = %v, want the whole sibling group %v", ph.Name, ph.Cores, w)
			}
			if !ph.AllOrNone {
				t.Errorf("phase %s all_or_none unset: half a core loading is not that core", ph.Name)
			}
		}
	})

	t.Run("seeded order is deterministic and pinned", func(t *testing.T) {
		seed := int64(42)
		var first []string
		for run := 0; run < 3; run++ {
			got, err := expandCycle(cyclePhase(&config.Cycle{Order: "seeded", Seed: &seed}), cfg, cycleFixtureGroups(), &[]string{})
			if err != nil {
				t.Fatalf("expandCycle: %v", err)
			}
			var names []string
			for _, ph := range got {
				names = append(names, ph.Name)
			}
			if run == 0 {
				first = names
				// Golden vector: the realized walk for seed 42 over the
				// fixture. Pinned as a literal on purpose — a shuffle that
				// silently changes would change replayed incidents.
				want := "cc-p3 cc-p2 cc-p1 cc-p0"
				if strings.Join(names, " ") != want {
					t.Fatalf("seeded walk = %v, want pinned %q", names, want)
				}
			} else if strings.Join(names, " ") != strings.Join(first, " ") {
				t.Fatalf("seeded walk changed between runs: %v vs %v", names, first)
			}
		}
	})

	t.Run("randomized draws once, writes back, replays as seeded", func(t *testing.T) {
		c := &config.Cycle{Order: "randomized"}
		var notices []string
		got, err := expandCycle(cyclePhase(c), cfg, cycleFixtureGroups(), &notices)
		if err != nil {
			t.Fatalf("expandCycle: %v", err)
		}
		if c.Order != "seeded" || c.Seed == nil {
			t.Fatalf("generator after randomized = order %q seed %v, want the realized walk written back", c.Order, c.Seed)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], "realized seed") {
			t.Errorf("notices = %v, want the realized-seed notice", notices)
		}
		// Replay: the SAME (now seeded) generator reproduces the order.
		again, err := expandCycle(cyclePhase(c), cfg, cycleFixtureGroups(), &[]string{})
		if err != nil {
			t.Fatalf("expandCycle replay: %v", err)
		}
		for i := range got {
			if got[i].Name != again[i].Name {
				t.Fatalf("replay changed the walk: %s vs %s", got[i].Name, again[i].Name)
			}
		}
	})

	t.Run("core selection", func(t *testing.T) {
		t.Run("one: the selected sibling is the tested one", func(t *testing.T) {
			got, err := expandCycle(cyclePhase(&config.Cycle{Cores: []int{9}}), cfg, cycleFixtureGroups(), &[]string{})
			if err != nil {
				t.Fatalf("expandCycle: %v", err)
			}
			if len(got) != 1 || got[0].Name != "cc-p1" || len(got[0].Cores) != 1 || got[0].Cores[0] != 9 {
				t.Fatalf("selection = %+v, want only core 1 tested on its selected sibling 9", got)
			}
		})
		t.Run("both: selecting one sibling loads the whole group", func(t *testing.T) {
			got, err := expandCycle(cyclePhase(&config.Cycle{SMT: "both", Cores: []int{9}}), cfg, cycleFixtureGroups(), &[]string{})
			if err != nil {
				t.Fatalf("expandCycle: %v", err)
			}
			if len(got) != 1 || len(got[0].Cores) != 2 {
				t.Fatalf("selection = %+v, want the whole sibling group of core 1", got)
			}
		})
		t.Run("one: selecting both siblings tests each separately", func(t *testing.T) {
			got, err := expandCycle(cyclePhase(&config.Cycle{Cores: []int{0, 8}}), cfg, cycleFixtureGroups(), &[]string{})
			if err != nil {
				t.Fatalf("expandCycle: %v", err)
			}
			if len(got) != 2 || got[0].Name != "cc-p0-c0" || got[1].Name != "cc-p0-c8" {
				t.Fatalf("selection = %+v, want two separately named single-CPU phases", got)
			}
		})
		t.Run("unknown core is refused, never silently dropped", func(t *testing.T) {
			_, err := expandCycle(cyclePhase(&config.Cycle{Cores: []int{999}}), cfg, cycleFixtureGroups(), &[]string{})
			if err == nil || !strings.Contains(err.Error(), "999") {
				t.Fatalf("expandCycle error = %v, want it to name the unmatched core 999", err)
			}
		})
	})

	t.Run("validation table", func(t *testing.T) {
		tests := []struct {
			name   string
			cycle  *config.Cycle
			groups []CycleGroup
			wantEr string
		}{
			{name: "bad smt", cycle: &config.Cycle{SMT: "maybe"}, groups: cycleFixtureGroups(), wantEr: `smt must be "one" or "both"`},
			{name: "bad order", cycle: &config.Cycle{Order: "backwards"}, groups: cycleFixtureGroups(), wantEr: `order must be "sequential"`},
			{name: "seeded without seed", cycle: &config.Cycle{Order: "seeded"}, groups: cycleFixtureGroups(), wantEr: "needs a seed"},
			{name: "no topology", cycle: &config.Cycle{}, groups: nil, wantEr: "needs CPU topology"},
			{name: "negative core", cycle: &config.Cycle{Cores: []int{-1}}, groups: cycleFixtureGroups(), wantEr: "negative"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := expandCycle(cyclePhase(tt.cycle), cfg, tt.groups, &[]string{})
				if err == nil || !strings.Contains(err.Error(), tt.wantEr) {
					t.Fatalf("expandCycle error = %v, want it to contain %q", err, tt.wantEr)
				}
			})
		}
	})

	t.Run("generator conflict with burst", func(t *testing.T) {
		ph := []config.Phase{{Name: "x", Cycle: &config.Cycle{}, Burst: &config.Burst{Cores: []int{0}}}}
		if _, err := expandBursts(ph, cfg); err == nil || !strings.Contains(err.Error(), "only one generator") {
			t.Fatalf("expandBursts error = %v, want the generator-conflict error", err)
		}
		if _, err := expandCycle(ph, cfg, cycleFixtureGroups(), &[]string{}); err == nil || !strings.Contains(err.Error(), "only one generator") {
			t.Fatalf("expandCycle error = %v, want the generator-conflict error", err)
		}
	})
}

func TestShuffleGroups(t *testing.T) {
	groups := cycleFixtureGroups()

	// Golden vector pinned at the shuffle-spec level (seed 7 over the
	// fixture): the permutation is part of the reproducibility contract.
	got := cycleOrder(shuffleGroups(groups, 7))
	if s := fmtItoa(got); s != "0,1,3,2" {
		t.Fatalf("shuffle(seed 7) = %s, want the pinned permutation 0,1,3,2", s)
	}

	// Determinism and permutation integrity across many seeds.
	for seed := uint64(0); seed < 200; seed++ {
		perm := shuffleGroups(groups, seed)
		if len(perm) != len(groups) {
			t.Fatalf("seed %d: shuffle changed the group count", seed)
		}
		seen := map[int]bool{}
		for _, g := range perm {
			if seen[g.Physical] {
				t.Fatalf("seed %d: physical core %d appears twice", seed, g.Physical)
			}
			seen[g.Physical] = true
		}
		if again := cycleOrder(shuffleGroups(groups, seed)); fmtItoa(again) != fmtItoa(cycleOrder(perm)) {
			t.Fatalf("seed %d: shuffle is not deterministic", seed)
		}
	}

	// Different seeds actually differ (a shuffle that ignored the seed
	// would replay the same "random" walk forever).
	differs := false
	base := fmtItoa(cycleOrder(shuffleGroups(groups, 1)))
	for seed := uint64(2); seed < 20 && !differs; seed++ {
		if fmtItoa(cycleOrder(shuffleGroups(groups, seed))) != base {
			differs = true
		}
	}
	if !differs {
		t.Fatal("shuffle produced the same permutation for seeds 1..19: the seed is being ignored")
	}
}

// A transition-mode phase must read as a schedule in the dry-run plan
// (the prepared windows, not a duration), and the journal estimate must
// count the two views of every burst.
func TestTransitionPhaseDisplay(t *testing.T) {
	opt := plan.Options{
		TestName: "general", A: "ram-constant", Phase: "tr",
		Cores: []int{0, 1}, PerProcess: true, Memory: "1GiB",
		Bursts: 3, BurstGap: 100 * time.Millisecond, BurstRun: 300 * time.Millisecond,
		BurstJitter: 20 * time.Millisecond, BurstSeed: 7,
	}
	var buf bytes.Buffer
	printScenarioPlan(&buf, config.Default(), []plan.Options{opt}, 1)
	out := buf.String()
	for _, want := range []string{"transitions(", "3 bursts gap=100ms run=300ms", "jitter=±20ms seed=7", "span≈1.2s"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "duration=0s") {
		t.Errorf("dry-run plan shows a duration for a schedule-defined phase:\n%s", out)
	}

	records, beats, approxBytes := journalEstimate([]plan.Options{opt}, 1, 32)
	// 2 run records + 2 schedules + 2x2 core records + 2x3x2 burst views
	// + 2 mem_done = 20 fixed; beats must be positive and dominate.
	if records < 20 || beats <= 0 {
		t.Fatalf("estimate = (%d, %d), want at least the 20 fixed records and beats", records, beats)
	}
	// The byte figure must be in the right order of magnitude: 20+
	// records at measured per-type averages is at least a few KB.
	if approxBytes < records*100 {
		t.Errorf("approxBytes = %d for %d records, want the measured per-record scale", approxBytes, records)
	}
}

// The dry-run must say NOW what execution would refuse: a ram workload
// without a budget, and a budget on an in-process phase.
func TestScenarioRequirementWarnings(t *testing.T) {
	bare := config.Default()
	bare.Memory = ""
	warns := scenarioRequirements(bare, []plan.Options{{Phase: "ram", A: "ram-constant", PerProcess: true}})
	if len(warns) != 1 || !strings.Contains(warns[0], "ram") || !strings.Contains(warns[0], "memory engine") {
		t.Fatalf("warnings = %v, want the missing-budget refusal named for the ram phase", warns)
	}
	if !strings.Contains(warns[0], "ram:") {
		t.Errorf("warning %q must name the phase it refuses", warns[0])
	}

	budgeted := config.Default()
	budgeted.Memory = "1GiB"
	warns = scenarioRequirements(budgeted, []plan.Options{{Phase: "tr", A: "int", Memory: "1GiB", Bursts: 2}}) // in-process bursts
	var foundMix, foundTrans bool
	for _, w := range warns {
		if strings.Contains(w, "engine will NOT allocate") {
			foundMix = true
		}
		if strings.Contains(w, "need per-process") {
			foundTrans = true
		}
	}
	if !foundMix || !foundTrans {
		t.Errorf("warnings = %v, want both the engine-not-applied and the bursts-need-per-process refusals", warns)
	}
}

// The scenario summary must state coverage (cores named, transitions
// commanded) and a reproduction hint, separating what ran from how to
// re-run it.
func TestScenarioSummaryCoverage(t *testing.T) {
	opts := []plan.Options{
		{Phase: "cpu", A: "int", Cores: []int{0, 1}},
		{Phase: "tr", A: "ram-constant", Cores: []int{2}, PerProcess: true, Bursts: 3},
	}
	results := []plan.Result{{Exit: 0, Validation: plan.ValVerified}, {Exit: 0, Validation: plan.ValExternalConfirmed}}
	ran := []bool{true, true}
	var buf bytes.Buffer
	writeScenarioSummary(&buf, opts, results, ran, -1, "", nil)
	out := buf.String()
	if !strings.Contains(out, "coverage: 3 distinct core(s) named across phases (0,1,2); 1 phase(s) command transitions") {
		t.Errorf("summary missing the coverage line:\n%s", out)
	}
	if !strings.Contains(out, "reproduce: cpu-stability repeat (saved plan: 2 phase(s), 2 ran)") {
		t.Errorf("summary missing the reproduction hint:\n%s", out)
	}

	// Unpinned core set is said so, not counted as zero cores.
	var buf2 bytes.Buffer
	writeScenarioSummary(&buf2, []plan.Options{{Phase: "all", A: "int"}}, []plan.Result{{Exit: 0}}, []bool{true}, -1, "", nil)
	if !strings.Contains(buf2.String(), "coverage: phases target all cores (core set not pinned)") {
		t.Errorf("unpinned cores must be stated, not zero:\n%s", buf2.String())
	}
}

// A02: exit 0 alone never earns "OK" in the summary: a phase whose
// structured verification is not confirmed is said out loud, and the
// closing line counts the unconfirmed phases instead of claiming a
// clean scenario.
func TestScenarioSummaryNeverPassesOnExitAlone(t *testing.T) {
	for _, v := range []plan.Validation{plan.ValPartial, plan.ValUnknown, plan.ValNotPerformed, plan.Validation("")} {
		opts := []plan.Options{{Phase: "cpu", A: "int", Cores: []int{0}}}
		results := []plan.Result{{Exit: 0, Validation: v}}
		var buf bytes.Buffer
		writeScenarioSummary(&buf, opts, results, []bool{true}, -1, "", nil)
		out := buf.String()
		if strings.Contains(out, "completed OK") || strings.Contains(out, "RESULT: PASS") {
			t.Errorf("validation %q: summary claims a clean scenario:\n%s", v, out)
		}
		if !strings.Contains(out, "UNVERIFIED") {
			t.Errorf("validation %q: phase line must say the clean end did not verify:\n%s", v, out)
		}
		if !strings.Contains(out, "1 without confirmed verification") {
			t.Errorf("validation %q: closing line must count the unconfirmed phase:\n%s", v, out)
		}
	}
	// A confirmed claim still earns the OK wording.
	var buf bytes.Buffer
	writeScenarioSummary(&buf, []plan.Options{{Phase: "cpu", A: "int", Cores: []int{0}}},
		[]plan.Result{{Exit: 0, Validation: plan.ValVerified}}, []bool{true}, -1, "", nil)
	if !strings.Contains(buf.String(), "RESULT: PASS") || !strings.Contains(buf.String(), "completed OK") {
		t.Errorf("confirmed verification must render as PASS:\n%s", buf.String())
	}
}

func fmtItoa(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}
