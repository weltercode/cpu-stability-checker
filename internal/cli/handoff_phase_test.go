package cli

// S5 scenario integration for handoff phases: the up-front resource
// gate admits the geometry (the same rule the executor applies before
// mapping, B09), the plan line and the phase header state the geometry
// instead of a clock, the experiment identity covers every handoff
// knob, and the fault blacklist skips a handoff phase WHOLE (its
// block-to-core partition is load-bearing - never silently re-divided).

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func handoffPhaseOpt() plan.Options {
	return plan.Options{
		TestName: "s5", Phase: "ho", Cores: []int{0, 1}, A: "ram-handoff",
		PerProcess: true, AllOrNone: true, StopOnFault: true,
		HandoffBlocks: 4, HandoffBlockBytes: 16 * 1024,
		HandoffRounds: 1, HandoffSeed: 3,
		Duration: 30 * time.Second, // inherited, never used by this phase
	}
}

func TestHandoffScenarioCompileFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA, cfg.WorkloadB = "ram-handoff", ""
	cfg.Phases = []config.Phase{{
		Name: "ho", Cores: []int{0, 1},
		HandoffBlocks: 4, HandoffBlockSize: "16KiB", HandoffRounds: 1, HandoffSeed: 3,
	}}
	opts, _, err := compileScenario(cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(opts) != 1 {
		t.Fatalf("phases = %d, want 1", len(opts))
	}
	o := opts[0]
	if o.HandoffBlocks != 4 || o.HandoffBlockBytes != 16*1024 || o.HandoffRounds != 1 || o.HandoffSeed != 3 {
		t.Errorf("compiled geometry = %+v, want 4 x 16384 x 1 round seed 3", o)
	}
	if !o.AllOrNone {
		t.Error("compiled handoff phase lacks the forced all_or_none")
	}
}

func TestHandoffScenarioGate(t *testing.T) {
	// Small geometry: no refusal even with the engine budget unset and
	// an inherited slot B that this shape never loads.
	opt := handoffPhaseOpt()
	opt.B = "ram-constant"
	if errs := scenarioMemoryRefusals(nil, []plan.Options{opt}); len(errs) != 0 {
		t.Errorf("small handoff phase refused: %v", errs)
	}
	// Oversized section: named refusal BEFORE any save or load, with
	// the phase label on it.
	huge := handoffPhaseOpt()
	huge.HandoffBlocks = 400_000_000
	huge.HandoffBlockBytes = 4 << 20 // 1.6 PiB: no machine honors this
	errs := scenarioMemoryRefusals(nil, []plan.Options{huge})
	if len(errs) == 0 || !strings.Contains(errs[0], "ho:") ||
		!strings.Contains(errs[0], "handoff shared section admission") {
		t.Errorf("oversized geometry not named-refused: %v", errs)
	}
	// Shape errors answer at the gate too (defense in depth beside the
	// compile), never as a mid-run surprise.
	broken := handoffPhaseOpt()
	broken.A = "float"
	errs = scenarioMemoryRefusals(nil, []plan.Options{broken})
	if len(errs) == 0 || !strings.Contains(errs[0], "ram-handoff") {
		t.Errorf("broken handoff shape not refused at the gate: %v", errs)
	}
}

func TestHandoffScenarioPlanLine(t *testing.T) {
	var buf bytes.Buffer
	printScenarioPlan(&buf, config.Default(), []plan.Options{handoffPhaseOpt()}, 1)
	out := buf.String()
	for _, want := range []string{"mode=handoff(4 blocks x", "seed 3", "all_or_none"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan line lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "duration=30s") {
		t.Errorf("handoff phase advertised a clock criterion it never uses:\n%s", out)
	}
}

func TestHandoffPhaseHashIdentity(t *testing.T) {
	a := handoffPhaseOpt()
	base := planHash(lastRun{}, a)
	b := a
	b.HandoffSeed = 4 // a different lease schedule is a different experiment
	if planHash(lastRun{}, b) == base {
		t.Error("handoff seed is not part of the experiment identity")
	}
	c := a
	c.HandoffBlockBytes = 32 * 1024
	if planHash(lastRun{}, c) == base {
		t.Error("handoff block size is not part of the experiment identity")
	}
	d := a
	d.HandoffRounds = 2
	if planHash(lastRun{}, d) == base {
		t.Error("handoff rounds are not part of the experiment identity")
	}
	e := a
	e.B = "anything" // slot B never loads in this shape: it must not split one identity in two
	if planHash(lastRun{}, e) != base {
		t.Error("inherited slot B changed a handoff phase's identity it has no part in")
	}
}

func TestHandoffRunCycleHeaderAndSkip(t *testing.T) {
	var ran []plan.Options
	execute := func(o plan.Options) plan.Result {
		ran = append(ran, o)
		return plan.Result{Exit: 0}
	}
	opt := handoffPhaseOpt()

	// A blacklisted participant skips the WHOLE phase: re-dividing 4
	// blocks across the survivors would be a smaller, differently
	// claimed experiment.
	var buf bytes.Buffer
	if _, ranFlags, stopped, _ := runCycle([]plan.Options{opt}, &buf, execute, true, map[int]bool{1: true}); len(ran) != 0 {
		t.Errorf("handoff phase ran with a faulted participant: %v", ranFlags)
	} else if stopped != -1 {
		t.Errorf("skip reported as stop (%d)", stopped)
	}
	if out := buf.String(); !strings.Contains(out, "skipped (a participant core faulted earlier)") {
		t.Errorf("skip wording missing:\n%s", out)
	}

	// Clean pass: the header states the geometry, never the inherited
	// duration this phase does not use.
	buf.Reset()
	runCycle([]plan.Options{opt}, &buf, execute, false, nil)
	if len(ran) != 1 {
		t.Fatalf("phase ran %d times, want 1", len(ran))
	}
	out := buf.String()
	for _, want := range []string{"handoff: cores 0,1", "4 block(s)", "seed 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("phase header lacks %q:\n%s", want, out)
		}
	}
}

func TestHandoffJournalEstimate(t *testing.T) {
	// 2 run records + 2 worker fragments + 2 lease records per block
	// (write + read) - the estimate must account for the lease-heavy
	// journal or a dry-run under-promises the disk footprint.
	records, _, size := journalEstimate([]plan.Options{handoffPhaseOpt()}, 1, 2)
	if want := 2 + 2 + 2*4; records < want {
		t.Errorf("records = %d, want at least %d (two leases per block)", records, want)
	}
	if size <= 0 {
		t.Errorf("size estimate = %d", size)
	}
}
