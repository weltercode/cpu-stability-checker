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
	// SuggestCycles is the card's own default answer for the Cycles
	// question (0 = the generic default of 1). It is only a default:
	// the operator types any number, and the question is still asked
	// - a preset may pre-fill a knob but never remove it.
	SuggestCycles int
	Phases        func(cfg config.Config) []config.Phase
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

// presetRamExtremeRough (R31, owner 2026-11-12: "можем повторить этот
// алгоритм, хотя бы грубо" - can we repeat this algorithm, at least
// roughly). Unlike ram-sustained (structure INSPIRED by the public
// TM5 config), this preset reproduces the DISCLOSED algorithm of
// "extreme@anta777" as literally as an honest reproduction gets: the
// 31-slot test sequence is copied VERBATIM from the public config
// (Test Sequence=4,6,1,12,2,...), and each numbered test is mapped to
// one of our versioned kernels by ROLE:
//
//	T1 MirrorMove (Time 20%, slots 3,11,19,27)  -> ram-block4, fixed short window
//	T2 MirrorMove128 (slots 5,8,13,16,21,24,29) -> ram-copy (full operator duration)
//	4 MB-block SimpleTests (11 slots)           -> ram-block4 at the asked duration
//	window SimpleTests Mode0 (T5,T7,T13)        -> ram-address
//	window SimpleTests Mode2 (T10,T11,T15)      -> ram-random
//	T0 RefreshStable                            -> excluded, exactly as the public
//	                                              config excludes it (defined but
//	                                              never scheduled)
//
// What is NOT reproduced (and the card says so): MirrorMove's move
// semantics (closed MT0.dll internals - the T1 slots reproduce the
// SHORT-TIME ROLE only, via per-block verification, not the mirrored
// copy); the SimpleTest pattern modes (Mode 0/1/2 collapse into one
// data contract each); the "Parameter" values (their semantics are
// undocumented public numbers); per-thread windows (our engine
// partitions one budget across pinned workers). The fixed 6s on the
// T1-role slots is a NOMINAL DEADLINE, not a coverage guarantee: it
// keeps the disclosed 20%-of-100% ratio at the default asked 30s, and
// the block kernel credits whole verified units inside it no matter
// how large the regions are - while a window that closes zero units
// (starvation, forced kill) still reports an honest zero.
//
// Cycles=3 matches the public config's Cycles and is only the
// suggested answer (the operator's typed number still rules).
func presetRamExtremeRough(cfg config.Config) []config.Phase {
	// slot -> kernel, reading the public sequence 4,6,1,12,2,4,12,2,
	// 5,7,1,13,2,5,13,2,8,9,1,14,2,8,14,2,10,11,1,15,2,10,3 in order.
	seq := []struct {
		test int
		a    string
		dur  string
	}{
		{4, "ram-block4", ""},   // slot 1: 4 MB-block SimpleTest
		{6, "ram-block4", ""},   // slot 2
		{1, "ram-block4", "6s"}, // slot 3: MirrorMove ROLE: short fixed window
		{12, "ram-block4", ""},  // slot 4
		{2, "ram-copy", ""},     // slot 5: MirrorMove128: move role -> paired copy/verify
		{4, "ram-block4", ""},   // slot 6
		{12, "ram-block4", ""},  // slot 7
		{2, "ram-copy", ""},     // slot 8
		{5, "ram-address", ""},  // slot 9: window test, pattern Mode 0
		{7, "ram-address", ""},  // slot 10
		{1, "ram-block4", "6s"}, // slot 11: MirrorMove role
		{13, "ram-address", ""}, // slot 12
		{2, "ram-copy", ""},     // slot 13
		{5, "ram-address", ""},  // slot 14
		{13, "ram-address", ""}, // slot 15
		{2, "ram-copy", ""},     // slot 16
		{8, "ram-block4", ""},   // slot 17: 4 MB-block
		{9, "ram-block4", ""},   // slot 18
		{1, "ram-block4", "6s"}, // slot 19: MirrorMove role
		{14, "ram-block4", ""},  // slot 20
		{2, "ram-copy", ""},     // slot 21
		{8, "ram-block4", ""},   // slot 22
		{14, "ram-block4", ""},  // slot 23
		{2, "ram-copy", ""},     // slot 24
		{10, "ram-random", ""},  // slot 25: window test, pattern Mode 2
		{11, "ram-random", ""},  // slot 26
		{1, "ram-block4", "6s"}, // slot 27: MirrorMove role
		{15, "ram-random", ""},  // slot 28
		{2, "ram-copy", ""},     // slot 29
		{10, "ram-random", ""},  // slot 30
		{3, "ram-block4", ""},   // slot 31: 4 MB-block (the cycle's last test)
	}
	phases := make([]config.Phase, 0, len(seq))
	for i, s := range seq {
		phases = append(phases, config.Phase{
			Name:       fmt.Sprintf("ext-%02d-t%02d", i+1, s.test), // slot + the public config's test number
			Test:       "general",
			A:          s.a,
			Duration:   s.dur,
			PerProcess: true,
		})
	}
	return phases
}

// presetRamSustained (R30, owner question 2026-11-12: "can we make a
// test at the TestMem5 level?"). The disclosed structure of the public
// TestMem5 config "extreme@anta777" (its sequence of numbered tests,
// repeated interleaves of short verified-transfer kernels between data
// phases, locality-contrast variants, repeated tests inside one cycle)
// is mirrored here SHAPE-WISE with OUR OWN versioned kernels. What is
// NOT borrowed: MT0.dll's pattern internals (closed; the public config
// exposes only names, parameters and window geometry as data), and
// TM5's per-thread testing windows (our engine partitions one budget
// across pinned workers). No detection parity is claimed either way -
// the strength question is an open measurement with destructive-level
// constraints (ROADMAP P-note), and this card promises only its own
// phase list.
//
// Why the interludes are NOT short fixed-time phases here: our kernels
// verify per COMPLETE pass (ram-copy counts nothing for a partial pass;
// the mix-trans incident proved a short window on GiB regions closes
// zero verified checks). A fixed "20% time" interlude at max-budget
// scale would therefore honestly self-report every run as partial. The
// interlude's character in THIS preset is its kernel (a verified copy
// transfer) and its deliberate sequence position; the duration of
// every phase is the operator's asked duration, and a phase that
// closes no full pass in the chosen window says so instead of faking
// coverage.
//
// Eighteen phases per cycle, four kernels repeated inside the cycle
// (as Extreme1 repeats its tests), five copy interludes at hand-chosen
// positions. The sequence is static, not shuffled: a detection
// sequence's value is the adjacency someone chose on purpose, and a
// static list stays byte-reproducible through planhash and replay.
func presetRamSustained(cfg config.Config) []config.Phase {
	return []config.Phase{
		{Name: "sust-01-constant", Test: "general", A: "ram-constant", PerProcess: true},
		{Name: "sust-02-address", Test: "general", A: "ram-address", PerProcess: true},
		{Name: "sust-03-copy", Test: "general", A: "ram-copy", PerProcess: true},
		{Name: "sust-04-walking", Test: "general", A: "ram-walking", PerProcess: true},
		{Name: "sust-05-perm", Test: "general", A: "ram-perm", PerProcess: true}, // locality contrast: scattered 64KiB blocks
		{Name: "sust-06-copy", Test: "general", A: "ram-copy", PerProcess: true},
		{Name: "sust-07-moving", Test: "general", A: "ram-moving", PerProcess: true},
		{Name: "sust-08-stride", Test: "general", A: "ram-stride", PerProcess: true}, // locality contrast: stride residue classes
		{Name: "sust-09-reverse", Test: "general", A: "ram-reverse", PerProcess: true},
		{Name: "sust-10-copy", Test: "general", A: "ram-copy", PerProcess: true},
		{Name: "sust-11-random", Test: "general", A: "ram-random", PerProcess: true},
		{Name: "sust-12-constant", Test: "general", A: "ram-constant", PerProcess: true},
		{Name: "sust-13-copy", Test: "general", A: "ram-copy", PerProcess: true},
		{Name: "sust-14-address", Test: "general", A: "ram-address", PerProcess: true},
		{Name: "sust-15-perm", Test: "general", A: "ram-perm", PerProcess: true},
		{Name: "sust-16-dwell", Test: "general", A: "ram-dwell", PerProcess: true}, // pause-survival contrast (data survival, not a retention promise)
		{Name: "sust-17-copy", Test: "general", A: "ram-copy", PerProcess: true},
		{Name: "sust-18-moving", Test: "general", A: "ram-moving", PerProcess: true},
	}
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
	{Name: "ram-extended", Description: "the full extended set so far: standard plus walking bits, copy/verify between paired regions, fill-pause-verify dwell (needs memory budget; inter-core block handoff not yet included; dwell is data survival after a pause, not a DRAM retention promise)", NeedsMemory: true, Phases: presetRamExtended},
	{Name: "ram-sustained", Description: "a sustained 18-phase RAM sequence with verified-copy interludes at chosen positions, locality-contrast kernels and four kernels repeated inside the cycle; sequence STRUCTURE inspired by the public TestMem5 'extreme@anta777' config, using our own versioned kernels - TM5's pattern internals are closed and not reproduced, its per-thread windows differ from our partitioned budget, and whether this finds what TM5 finds is UNMEASURED (no parity claimed either way). Needs a memory budget; every phase runs your asked duration, and a phase that closes no full verified pass in its window says so. Suggested cycles: 3.", NeedsMemory: true, SuggestCycles: 3, Phases: presetRamSustained},
	{Name: "ram-extreme-rough", Description: "a rough reproduction of the PUBLIC TestMem5 'extreme@anta777' Extreme1 algorithm: the 31-slot test sequence copied verbatim from the published config, each test mapped by role to one of our kernels (short MirrorMove-role slots run the per-block 4 MB verified kernel in a fixed short window; MirrorMove128 slots run our verified copy; 4 MB-block and window SimpleTests map to block4/address/random with their modes collapsed). What is NOT reproduced: the closed MT0.dll pattern internals, MirrorMove's move semantics, the 'Parameter' values (semantics undocumented), and TM5's per-thread windows. Whether this finds what TM5 finds is UNMEASURED - no equivalence is claimed either way. Needs a memory budget; the fixed short slots are nominal deadlines, a slot that closes no verified unit reports zero honestly. Suggested cycles: 3.", NeedsMemory: true, SuggestCycles: 3, Phases: presetRamExtremeRough},
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
