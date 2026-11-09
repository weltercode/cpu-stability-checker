package cli

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// Built-in scenario presets (P09). A preset is NOT a second execution
// path: it only produces ordinary config.Phase values - generators and
// all - which then run through the exact same compileScenario pipeline
// as a hand-written phases list. That is deliberate: the dry-run, the
// journal, the fault blacklist and the saved-plan repeat all behave
// identically for preset and hand-written scenarios, and nothing here
// can drift from the real execution semantics.
//
// The presets separate sources on purpose (cpu-baseline vs ram-baseline
// vs their mixture): a fault in one family points somewhere different.
// They are starting points for a machine, not automatic localization of
// a defective component.

type preset struct {
	Name        string
	Description string
	// NeedsMemory marks presets whose promised composition verifies
	// data in memory-engine regions: the entry points refuse them
	// BEFORE any load with an actionable reason when no budget is
	// configured (audit A05 - silently dropping the RAM phases would
	// quietly change what the profile claims to have tested).
	NeedsMemory bool
	Phases      func(cfg config.Config) []config.Phase
}

// presetCPUBaseline: CPU workloads only, no memory engine. The all-core
// pass finds a global fault; the sequential one-SMT walk finds a fault
// that belongs to ONE core while its siblings idle.
func presetCPUBaseline(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "cpu-all", Test: "general", A: "int", PerProcess: true},
		{Name: "cpu-float", Test: "general", A: "float", PerProcess: true},
		{Name: "cpu-cycle", Cycle: &config.Cycle{SMT: "one", Order: "sequential", Load: "int"}},
	}
}

// presetRamBaseline: the memory engine's own verification family, one
// pattern pass and one independently-recoverable data stream. Needs a
// configured memory budget: without it the P06 preflight refuses the
// run up front (exit 2), never a memory-less "pass".
func presetRamBaseline(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "ram-constant", Test: "general", A: "ram-constant", PerProcess: true},
		{Name: "ram-random", Test: "general", A: "ram-random", PerProcess: true},
	}
}

// P07f: the versioned RAM profiles. Each phase is one versioned
// kernel with its own verification contract, so the profile's
// coverage story is the phase list itself - reproducible by seed and
// kernel versions, explainable without reading source. Durations
// come from config (the pass-count floors are P12 calibration
// territory, and pretending otherwise would promise unmeasured
// strength).
func presetRamQuick(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "ram-constant", Test: "general", A: "ram-constant", PerProcess: true},
	}
}

func presetRamStandard(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "ram-constant", Test: "general", A: "ram-constant", PerProcess: true},
		{Name: "ram-address", Test: "general", A: "ram-address", PerProcess: true},
		{Name: "ram-moving", Test: "general", A: "ram-moving", PerProcess: true},
		{Name: "ram-random", Test: "general", A: "ram-random", PerProcess: true},
		{Name: "ram-stride", Test: "general", A: "ram-stride", PerProcess: true},
		{Name: "ram-reverse", Test: "general", A: "ram-reverse", PerProcess: true},
		{Name: "ram-perm", Test: "general", A: "ram-perm", PerProcess: true},
	}
}

// presetRamExtended: the standard set plus the remaining P07 kernels
// implemented so far. Honest gaps stay in the description: inter-
// core block handoff (P07e) is not part of this set yet, and the
// dwell phase is a data-survival-after-pause check, not a DRAM
// retention promise.
func presetRamExtended(cfg config.Config) []config.Phase {
	std := presetRamStandard(cfg)
	return append(std,
		config.Phase{Name: "ram-walking", Test: "general", A: "ram-walking", PerProcess: true},
		config.Phase{Name: "ram-copy", Test: "general", A: "ram-copy", PerProcess: true},
		config.Phase{Name: "ram-dwell", Test: "general", A: "ram-dwell", PerProcess: true},
	)
}

// presetQuickCheck (R17): the bounded "is anything obviously wrong
// right now" window. CPU-only ON PURPOSE: it needs no memory budget,
// so it answers on any machine without config, and it names what it
// does NOT cover: no RAM-engine verification, no per-core walk, no
// soak. The three phases are the existing verified kernels with their
// own contracts (all-core int, all-core float, prepared idle->load
// transitions - the wake transient is where overclocked parts fail
// first); the durations are the preset's promise, ~21s of planned
// load in total. Worker preparation and teardown add time on top, so
// entry points describe the window as planned workload plus setup
// overhead - never as a wall-clock deadline nothing enforces.
func presetQuickCheck(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "quick-int", Test: "general", A: "int", PerProcess: true, Duration: "10s"},
		{Name: "quick-float", Test: "general", A: "float", PerProcess: true, Duration: "10s"},
		{Name: "quick-trans", Test: "general", A: "int", PerProcess: true,
			Bursts: 3, BurstGap: "200ms", BurstRun: "500ms"},
	}
}

// presetCoreCycle: every physical core alone, first one SMT thread
// (siblings idle: a clean single-thread signature), then the whole
// core with both siblings loading together (shared execution units).
func presetCoreCycle(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "cycle-one", Cycle: &config.Cycle{SMT: "one", Order: "sequential", Load: "int"}},
		{Name: "cycle-both", Cycle: &config.Cycle{SMT: "both", Order: "sequential", Load: "int"}},
	}
}

// presetTransitions: P08 prepared sessions - idle on the pinned core,
// then a commanded verified burst, N times per core on a prepared
// schedule. The wake transient is where overclocked parts often fail
// first. The load is always the CPU int workload: an unrelated memory
// budget in config must not silently turn this profile into a RAM
// transition test (audit A05). A RAM-carrying transition belongs to
// the mixed profile, where the budget is part of the promise.
func presetTransitions(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "trans", Test: "general", A: "int", PerProcess: true,
			Bursts: 5, BurstGap: "200ms", BurstRun: "500ms"},
	}
}

// presetMixedValidation: the SEQUENTIAL comprehensive profile - one
// resource plan (the parent computes the memory budget once per
// phase; control-core placement keeps helpers off the targets):
// CPU first, RAM second, then RAM under commanded transitions, then
// the ramp that loads growing core counts together - the interaction
// test the single-family baselines cannot show. It is NOT a
// simultaneous CPU+RAM load: no phase here applies CPU and memory
// verification at the same instant, and the description says so
// (audit A05). The RAM phases are never dropped: without a budget
// the entry points refuse the preset up front, so the mixture can
// never quietly shrink into a CPU-only run.
func presetMixedValidation(cfg config.Config) []config.Phase {
	phases := []config.Phase{
		{Name: "mix-cpu", Test: "general", A: "int", PerProcess: true},
		{Name: "mix-ram", Test: "general", A: "ram-constant", PerProcess: true},
		// The burst window must be able to CLOSE a verified pass:
		// ram-constant verifies per whole region, and on the incident
		// machine (20 workers, multi-hundred-MiB regions) a 500ms
		// burst completed zero verified checks on 59 of 60 bursts -
		// the phase then correctly refused verification, but the
		// profile as shipped could never earn it. 2s fits several
		// full passes per burst even under full-core memory
		// contention; the transitions themselves stay short
		// (gap 200ms of idle between bursts).
		config.Phase{Name: "mix-trans", Test: "general", A: "ram-constant", PerProcess: true,
			Bursts: 3, BurstGap: "200ms", BurstRun: "2s"},
		// The ramp needs an explicit pool: the preset takes every
		// logical CPU this machine exposes.
		config.Phase{Name: "mix-ramp", Curve: &config.Curve{Load: "int", Settle: "2s", Cores: allLogicals()}},
	}
	return phases
}

// allLogicals lists the logical CPUs this process sees (0..NumCPU-1):
// the machine-aware part a preset may know about.
func allLogicals() []int {
	n := runtime.NumCPU()
	cpus := make([]int, n)
	for i := range cpus {
		cpus[i] = i
	}
	return cpus
}

var presets = []preset{
	{Name: "quick-check", Description: "bounded sanity window (~21s planned load plus setup overhead): all-core int pass 10s, all-core float pass 10s, three prepared idle->load transition bursts. CPU-only: it needs no memory budget and NEVER verifies RAM-engine regions; no per-core walk, no soak. A clean word here covers only these loads in this window and is not a stability verdict.", Phases: presetQuickCheck},
	{Name: "cpu-baseline", Description: "CPU workloads only: all-core pass, then a sequential per-core walk", Phases: presetCPUBaseline},
	{Name: "ram-baseline", Description: "memory engine verification: pattern passes + independent data stream (needs memory budget)", NeedsMemory: true, Phases: presetRamBaseline},
	{Name: "ram-quick", Description: "one basic pattern cycle in engine memory (needs memory budget)", NeedsMemory: true, Phases: presetRamQuick},
	{Name: "ram-standard", Description: "basic pattern cycles plus movement: stride sweeps, reverse sweeps, seeded block permutation (needs memory budget)", NeedsMemory: true, Phases: presetRamStandard},
	{Name: "ram-extended", Description: "the full P07 set so far: standard plus walking bits, copy/verify between paired regions, fill-pause-verify dwell (needs memory budget; inter-core block handoff not yet included; dwell is data survival after a pause, not a DRAM retention promise)", NeedsMemory: true, Phases: presetRamExtended},
	{Name: "core-cycle", Description: "every physical core alone: one SMT thread, then the whole core", Phases: presetCoreCycle},
	{Name: "transitions", Description: "prepared sessions: commanded idle->load verified bursts per core (CPU int load)", Phases: presetTransitions},
	{Name: "mixed-validation", Description: "sequential comprehensive profile: CPU pass, RAM verify, RAM transitions, load ramp (needs memory budget; NOT simultaneous CPU+RAM)", NeedsMemory: true, Phases: presetMixedValidation},
}

func lookupPreset(name string) (preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return preset{}, false
}

func presetNames() []string {
	names := make([]string, 0, len(presets))
	for _, p := range presets {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func presetListText() string {
	var b strings.Builder
	for _, p := range presets {
		fmt.Fprintf(&b, "  %-18s %s\n", p.Name, p.Description)
	}
	return b.String()
}
