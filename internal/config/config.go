// Package config reads and writes the durable config.json defaults.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// DefaultPath is where every command looks for the configuration.
const DefaultPath = "config.json"

// Burst generates a sequence of phases that drive one core at a time from
// idle straight into load — the wake transient the whole park/wake idea
// rests on. It is compiled into ordinary phases (see the scenario
// command), so there is no separate execution path for generators.
type Burst struct {
	Cores        []int  `json:"cores"`                   // logical CPUs to burst, one at a time
	Repeat       int    `json:"repeat,omitempty"`        // cycles per core (0 = 1)
	Load         string `json:"load,omitempty"`          // load workload (empty = config workload_a)
	LoadDuration string `json:"load_duration,omitempty"` // empty = config duration
	IdleDuration string `json:"idle_duration,omitempty"` // settle before each burst (empty = 2s)
}

// Pairs is a scenario generator: it compiles in place into one directed
// (aggressor, observer) phase per ordered core pair. The aggressor hammers
// one core while the observer runs its own workload on another; a fault is
// attributed to whichever participant reported it (the journal keys faults
// by logical CPU).
type Pairs struct {
	Cores     []int  `json:"cores"`               // logical CPUs to pair
	Scope     string `json:"scope,omitempty"`     // "ccx" (default): same L3 domain; "smt": SMT siblings of one core; "all": every ordered pair
	Aggressor string `json:"aggressor,omitempty"` // aggressor workload (empty = config workload_a)
	Observer  string `json:"observer,omitempty"`  // observer workload (empty = config workload_b)
	Duration  string `json:"duration,omitempty"`  // per-pair run duration (empty = config duration)
	Settle    string `json:"settle,omitempty"`    // optional idle before each pair (wake transient)
}

// Curve is a scenario generator: it compiles in place into one phase per
// step of a load ramp — the first k cores of the pool hammering together,
// k growing step by step. Each step starts from a common idle settle, so
// the step is a simultaneous wake+load on k cores: the voltage-droop step
// test that finds the load level where a core (or a CCD) stops being
// stable — the Curve Optimizer margin probe. Pool order matters: a step
// takes a prefix, so interleaving CCDs in the pool spreads droop across
// domains early.
type Curve struct {
	Cores    []int  `json:"cores"`              // pool; each step loads a prefix of it
	Steps    []int  `json:"steps,omitempty"`    // loaded-core counts (empty = doubling: 1,2,4,...,pool size)
	Load     string `json:"load,omitempty"`     // load workload (empty = config workload_a)
	Duration string `json:"duration,omitempty"` // per-step run duration (empty = config duration)
	Settle   string `json:"settle,omitempty"`   // common idle before each step (empty = 2s)
}

// Cycle is a scenario generator: it compiles in place into one phase per
// PHYSICAL core, walking the cores one at a time — the sequential core
// cycle. Each phase pins that core's logical CPUs, idles them (settle,
// which measures the wake), then loads them. smt "one" tests a single
// logical CPU per physical core while its siblings stay idle (a clean
// single-thread fault signature); smt "both" loads every SMT sibling of
// that core together as one indivisible group (all_or_none: half a core
// is not that core). order walks the cores "sequential" (physical index),
// "seeded" (deterministic from seed), or "randomized" (a seed is drawn
// once, written back into the generator, and saved with the plan, so the
// realized order replays without depending on any PRNG version).
type Cycle struct {
	SMT      string `json:"smt,omitempty"`      // "one" (default) | "both": SMT siblings load as one group
	Order    string `json:"order,omitempty"`    // "sequential" (default) | "seeded" | "randomized"
	Seed     *int64 `json:"seed,omitempty"`     // shuffle seed for "seeded"; "randomized" draws one and stores it here
	Load     string `json:"load,omitempty"`     // load workload (empty = config workload_a)
	Duration string `json:"duration,omitempty"` // per-core run duration (empty = config duration)
	Settle   string `json:"settle,omitempty"`   // idle on the pinned core before its load (empty = 2s)
	Cores    []int  `json:"cores,omitempty"`    // restrict to the physical cores owning these logical CPUs (empty = all)
}

// Phase is one step of a scenario: a complete run with its own layout.
// Every empty field inherits the corresponding top-level config default,
// so a phase usually names only what differs. Repeat runs the same step
// N times in a row (0 or 1 = once). A phase with a Burst, Pairs, Curve or
// Cycle is not a run itself: it expands in place into ordinary phases.
type Phase struct {
	Name        string `json:"name,omitempty"`
	Repeat      int    `json:"repeat,omitempty"`
	Burst       *Burst `json:"burst,omitempty"`
	Pairs       *Pairs `json:"pairs,omitempty"`
	Curve       *Curve `json:"curve,omitempty"`
	Cycle       *Cycle `json:"cycle,omitempty"`
	Test        string `json:"test,omitempty"`
	A           string `json:"a,omitempty"`
	B           string `json:"b,omitempty"`
	Cores       []int  `json:"cores,omitempty"`
	Workers     int    `json:"workers,omitempty"`
	Duration    string `json:"duration,omitempty"`
	StopOnFault *bool  `json:"stop_on_fault,omitempty"` // nil = config default
	PerProcess  bool   `json:"per_process,omitempty"`
	Rotate      *bool  `json:"rotate,omitempty"`      // nil = config default; per-process only
	Settle      string `json:"settle,omitempty"`      // per-process: idle this long before load (wake transient)
	AllOrNone   bool   `json:"all_or_none,omitempty"` // skip the whole phase if any of its cores is fault-blacklisted (pair semantics)
	// P08 transition mode: this phase runs prepared per-core sessions
	// through N verified bursts on a prepared schedule instead of one
	// continuous window. Requires per_process, refuses rotate; the
	// schedule (not duration) defines the phase's time.
	Bursts       int    `json:"bursts,omitempty"`        // verified bursts per core (0 = ordinary phase)
	BurstGap     string `json:"burst_gap,omitempty"`     // idle gap before each burst (empty = 200ms)
	BurstRun     string `json:"burst_run,omitempty"`     // burst load window (empty = 500ms)
	BurstJitter  string `json:"burst_jitter,omitempty"`  // +/- spread on the gap (empty = fixed windows)
	BurstSeed    uint64 `json:"burst_seed,omitempty"`    // jitter seed; only consumed with jitter, 0 is a valid seed
	BurstStagger string `json:"burst_stagger,omitempty"` // wave shift between cores (empty = all together)
	// Q04 parking evidence policy for this phase's prepared sessions:
	// empty = inherit the global config default, "off" = say the
	// default away, "try" = observe and record without touching the
	// verdict, "required" = a full success needs observed evidence.
	// Only a bursts or a pair-switch phase can carry it: the
	// observation rides MEASURED idle windows (burst gaps, switch
	// turn gaps), and an ordinary phase has none - naming the word
	// there is a visible compile error, never a silently dropped
	// promise.
	ParkingPolicy string `json:"parking_policy,omitempty"`
	// P07e handoff phase (S5): parent-arbitrated block-ownership leases
	// over ONE shared section, driven by the coordinator instead of a
	// duration window. Requires a = "ram-handoff" and per-process mode;
	// the geometry (blocks x block size x rounds) is both the promise
	// and the budget - the shared section passes the same admission the
	// memory engine uses before anything maps, and the size lives HERE,
	// not in "memory". all_or_none is forced: the block-to-core
	// partition makes every participant load-bearing, so a faulted core
	// skips the phase whole instead of quietly re-dividing blocks.
	HandoffBlocks    int    `json:"handoff_blocks,omitempty"`     // blocks in the shared section (0 = ordinary phase; must divide evenly across the selected cores)
	HandoffBlockSize string `json:"handoff_block_size,omitempty"` // uniform block size (empty = 64KiB; a whole number of 4-byte words)
	HandoffRounds    int    `json:"handoff_rounds,omitempty"`     // every block published and independently re-read this many times (empty/0 = 1)
	HandoffSeed      uint64 `json:"handoff_seed,omitempty"`       // seed for the lease schedule order and the ram-handoff image (0 is a valid seed)
	// Q05 prepared pair-switch phase: groups of prepared participants
	// (each [aggressor, observer]) alternate the load on a prepared turn
	// schedule WITHOUT process restart - every participant is spawned and
	// PREPAREd before the first switch, so the measured transition holds
	// no spawn and no allocation. Like a bursts phase, the schedule (not
	// duration) defines the phase; per-process mode is part of the shape
	// (a prepared participant IS a pinned process), rotate contradicts it.
	// Presence of ANY switch_* word demands the whole honest shape:
	// switch_rounds plus at least two groups of exactly two distinct
	// cores, named by hand (switch_pairs) or paired off the detected
	// topology by the scope fields below (plan.ValidateSwitch names every
	// refusal) - a half-specified shape is a compile error, never a
	// defaulted-around experiment. all_or_none is forced: each
	// participant is load-bearing for its group's turns, so a
	// fault-blacklisted core skips the phase whole.
	SwitchPairs  [][]int `json:"switch_pairs,omitempty"`  // group -> [aggressor, observer] logical CPUs (the manual source; the scope fields generate instead)
	SwitchRounds int     `json:"switch_rounds,omitempty"` // cycles over the groups; the phase's end criterion (0 = ordinary phase)
	SwitchGap    string  `json:"switch_gap,omitempty"`    // idle gap before each turn, also the parking-observation window (empty = 600ms)
	SwitchRun    string  `json:"switch_run,omitempty"`    // load window of each turn (empty = 200ms)
	SwitchJitter string  `json:"switch_jitter,omitempty"` // +/- spread on the gap (empty = fixed turns)
	SwitchOrder  string  `json:"switch_order,omitempty"`  // "" = fixed alternation; "seeded" per-round shuffle
	SwitchSeed   uint64  `json:"switch_seed,omitempty"`   // consumed by seeded order or jitter (0 is a valid seed)
	// Q05 scope matrix — the increment ValidateSwitch's A1 wording named
	// (and now lands): switch_scope/switch_cores GENERATE the groups from
	// the detected topology instead of naming them by hand. Presence of
	// either word selects generation; explicit switch_pairs beside them
	// is a named refusal - one source of groups. The scope vocabulary
	// mirrors the pairs generator: "ccx" (the default when only a pool
	// is given) pairs both members inside one L3 domain, "smt" pairs the
	// SMT siblings of one physical core, "all" pairs any two distinct
	// CPUs - which domains those pairs cross is pool ORDER, data, and
	// the run never claims a domain relationship the scope did not
	// enforce. An empty pool is every group-0 logical CPU this system
	// reports, sorted; every named member must exist in the detected
	// inventory (a core invented at compile time is a compile error),
	// and a core the scope cannot pair is a named refusal, never a
	// silently dropped participant.
	SwitchScope string `json:"switch_scope,omitempty"` // "ccx" | "smt" | "all" (pool without scope = "ccx"; no scope and no pool = the manual switch_pairs list)
	SwitchCores []int  `json:"switch_cores,omitempty"` // pool the groups are paired from (empty = all detected logical CPUs)
}

// Config holds durable defaults. Anything left zero falls back to a built-in
// default so an empty or partial file is always usable.
type Config struct {
	Cores       []int  `json:"cores"`         // logical cores to test; empty = all
	Test        string `json:"test"`          // default test for the menu
	WorkloadA   string `json:"workload_a"`    // default workload slot A
	WorkloadB   string `json:"workload_b"`    // default workload slot B
	Duration    string `json:"duration"`      // default duration, e.g. "30s"
	Workers     int    `json:"workers"`       // workers per core
	LogFile     string `json:"log_file"`      // execution log path
	StopOnFault bool   `json:"stop_on_fault"` // abort the run when a fault is seen
	ShowMetrics bool   `json:"show_metrics"`  // show memory/OS stats in the live UI
	// CoTool is the explicit path to the Curve Optimizer backend
	// executable (P10). Empty means no CO operations configured: the
	// tool never searches PATH for it silently.
	CoTool string `json:"co_tool,omitempty"`
	Rotate bool   `json:"rotate,omitempty"` // per-process mode: one core runs at a time
	// Execution is the worker-mode POLICY (menu plan section 4):
	// "" / "auto" lets the resolver pick per workload from its hard
	// requirements; the explicit values are a settings-level override,
	// and an override that contradicts a requirement is refused before
	// any load (plan.ResolveExecution), never quietly switched.
	Execution   string `json:"execution,omitempty"`
	ControlCore *int   `json:"control_core,omitempty"` // helper threads' CPU: null = first core no worker occupies, -1 = off, N = explicit
	// ReserveControl is the helper-headroom POLICY: "" / "none" never
	// touches the requested target set (auto honestly gives up and
	// lets helpers float when every visible CPU is a target);
	// "logical" consents to a coverage loss: when the auto control
	// core would have no free CPU, the highest-numbered logical CPU
	// is removed from the worker set and handed to the helper threads
	// (watchdog, journal, panel) - an explicit answer to "leave the
	// console and the safety plane some headroom". It is only
	// meaningful with auto control (an explicit control core answers
	// for itself, -1 asks for floating helpers); the reduced target
	// set IS the experiment: plan hash, journal and every summary
	// state the coverage and name the reserved CPU.
	ReserveControl string                           `json:"reserve_control,omitempty"`
	MaxTemp        float64                          `json:"max_temp,omitempty"`      // shared panel sensor limit; stops on missing/stale data (0 = off)
	Telemetry      bool                             `json:"telemetry,omitempty"`     // log per-second clock/idle lines during runs
	BeatEvery      string                           `json:"beat_interval,omitempty"` // how often core_beat records hit the journal (empty = 1s); live UI is unaffected
	External       map[string]workload.ExternalSpec `json:"external,omitempty"`      // third-party stress programs
	Memory         string                           `json:"memory,omitempty"`        // memory-engine budget: "auto" or exact size ("2GiB"); empty = engine stays off
	MemoryLock     string                           `json:"memory_lock,omitempty"`   // pin engine pages in RAM: "" / "off" = never, "try" = attempt and report honestly, "required" = setup error if the OS refuses
	// ParkingPolicy is the GLOBAL default parking-evidence word for
	// prepared transition phases (Q04): "" / "off" = never observe,
	// "try" = observe and journal it without changing the verdict,
	// "required" = a full success requires the evidence. It shapes
	// only phases that actually drive prepared sessions (bursts): an
	// ordinary phase has no measured session idle windows to observe,
	// and there the inherited word is cleared - nothing that shapes
	// no run belongs in a plan hash. An unknown word is a visible
	// setup error naming the vocabulary, same rule as memory_lock.
	ParkingPolicy string  `json:"parking_policy,omitempty"`
	RamSeed       uint64  `json:"ram_seed,omitempty"` // seed for the ram-* memory-test family: expected data = f(seed, region, pass, offset); 0 is a valid seed (the default), not "unset"
	Phases        []Phase `json:"phases,omitempty"`   // scenario steps for the 'scenario' command
}

// Default returns the built-in defaults used for every missing key.
func Default() Config {
	return Config{
		Test:        "general",
		WorkloadA:   "float",
		WorkloadB:   "mem",
		Duration:    "30s",
		Workers:     1,
		LogFile:     "cpu-stability.log",
		StopOnFault: true,
		ShowMetrics: true,
		Execution:   ExecutionAuto,
	}
}

// Execution policy vocabulary (config "execution"): how the workers
// of a run are realized. The user picks test GOALS in the menu; the
// mechanism is policy, resolved per workload by plan.ResolveExecution
// and recorded in the request. An unknown value is a visible config
// error at resolve time, never a silent "auto".
const (
	ExecutionAuto       = "auto"       // resolver picks per workload requirement
	ExecutionThreaded   = "threaded"   // worker threads inside this process
	ExecutionSubprocess = "subprocess" // one pinned subprocess per CPU
	ExecutionRotate     = "rotate"     // subprocess mode with exclusive rotating windows
)

// MemoryRequest parses the configured memory-engine budget: "auto"
// or an exact size like "2GiB". A malformed value is a setup error
// the caller must surface (exit 2), never a silent fallback to auto.
// The engine itself is gated on the raw string: empty means the
// engine stays off and this parser is never reached (ParseRequest
// treats empty as auto, so callers must check for empty first).
func (c Config) MemoryRequest() (memtest.Request, error) {
	return memtest.ParseRequest(c.Memory)
}

// LoadOrDefault is the only general load primitive, and it is honest
// about the difference between the two "no configuration" states
// (R15, owner queue 2026-11-09):
//
//	absent file  -> Default() with a nil error. Defaults are a
//	                legitimate state (the config is only a saved
//	                preference), but the CALLER must disclose that
//	                defaults are in effect - the silent version of
//	                this rule is what made the old Load dangerous.
//	broken file  -> empty Config with the error. A file that exists
//	                but cannot be read or parsed holds this machine's
//	                deliberate plan (cores, memory budget, durations,
//	                registered tools). Falling back to defaults there
//	                silently replaces the user's intended test with a
//	                different one, and the journal, the report and
//	                'repeat' then describe a run that never was the
//	                planned one - a pass becomes a lie about a test
//	                that never existed. Refusing is the honest action.
//
// The old silent Load did exactly that forbidden fallback for EVERY
// error kind, at eleven call sites including the menu and subprocess
// workers; it is deleted, and the package tests pin the new shape.
func LoadOrDefault(path string) (Config, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	return LoadStrict(path)
}

// LoadStrict is used by config-first commands. Unlike Load it reports
// missing or malformed configuration instead of silently falling back.
func LoadStrict(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	// Decode over defaults so omitted keys retain usable values.
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes a human-editable sample config.
func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ParsedDuration parses the configured duration, falling back to 30s.
func (c Config) ParsedDuration() time.Duration {
	if d, err := time.ParseDuration(c.Duration); err == nil && d > 0 {
		return d
	}
	return 30 * time.Second
}

// workers returns the configured worker count with a floor of 1.
func (c Config) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	return 1
}

// Sample returns a config with the given core list for `config init`.
func Sample(cores []int) Config {
	c := Default()
	c.Cores = cores
	return c
}
