// Package catalog is the shared profile catalog the menu plan demands
// (MENU_CLI_GUI_PLAN section 3): one source of truth for what a
// user-facing test IS, which category it belongs to, where it may be
// offered, and whether this build can actually run it - as explicit
// data. The low-level kernel registry stays the executors' contract;
// classification here never guesses from a "ram-" name prefix or a
// description length.
//
// Display text is English (the product UI language, audit A05); machine
// IDs, enum values and JSON names are English too, so a future GUI maps
// them to any language without parsing menu text, and renaming a
// display string never breaks a saved recipe or a journal. Version is
// bumped only when a profile's EXECUTION content changes - display
// wording alone keeps the journal's id+version pair meaningful.
//
// The package is pure data: it imports nothing from the app, so the
// menu, the CLI and the future process protocol all read the same
// cards. Compiling a profile into an actual request stays in the CLI
// (the executors are unchanged).
package catalog

// Category groups the main-menu sections (menu plan section 2).
type Category string

const (
	CPU Category = "cpu" // CPU stability: compute paths of the pinned cores
	RAM Category = "ram" // memory verification inside engine regions
	MIX Category = "mix" // combined profiles
)

// Visibility decides which lists a profile may appear in. Diagnostic
// profiles (fault prober, leak soak, worker plumbing) never show up in
// a user list; they stay reachable through the CLI and the diagnostics
// submenu (menu plan section 3).
type Visibility string

const (
	Normal     Visibility = "normal"     // the ordinary CPU/RAM/MIX lists
	Extended   Visibility = "extended"   // "Extended profiles" and advanced choices
	Diagnostic Visibility = "diagnostic" // developer diagnostics, never a user list
)

// Availability is three-valued on purpose: "unknown" exists so a
// profile whose status cannot be observed is never rendered as ready
// (menu plan section 11: unknown availability never becomes "готов").
// The zero value is deliberately NOT a valid availability: every card
// states its status, and the package tests reject a silent one.
type Availability string

const (
	Available   Availability = "available"   // this build can run it right now
	Unavailable Availability = "unavailable" // known not runnable: the reason is required data
	Unknown     Availability = "unknown"     // cannot tell: never presented as ready
)

// Validation says what a COMPLETED run of this profile actually
// proves. The vocabulary is deliberately honest: stressing memory is
// not verifying data, a synthetic prober proves the reporting pipeline
// and not the hardware, and an external engine's verdict is only as
// good as its adapter's completion protocol.
type Validation string

const (
	ValidationCompute     Validation = "compute"     // computed values compared against exact expectations
	ValidationMemory      Validation = "memory"      // data compared inside memory-engine regions
	ValidationMemoryLoad  Validation = "memory-load" // stresses memory WITHOUT verifying data
	ValidationExternal    Validation = "external"    // the external tool's own protocol decides
	ValidationSynthetic   Validation = "synthetic"   // proves the reporting pipeline, not hardware
	ValidationMixed       Validation = "mixed"       // phases verify different things; see the plan
	ValidationObservation Validation = "observation" // measures behavior (parking, wake) without data checks
	ValidationUnknown     Validation = "unknown"     // cannot say what completion proves
)

// Shape names the execution form the CLI compiles this profile into.
// It describes the USER's goal ("all cores", "one core after
// another", "a prepared multi-phase plan"), not the in-process vs
// subprocess mechanism - the executor itself is chosen by the auto
// resolver (menu plan section 4) and recorded in the request.
type Shape string

const (
	ShapeSessionAll     Shape = "session-all"      // all selected physical cores at once
	ShapeSessionPerCore Shape = "session-per-core" // the selected physical cores one after another
	ShapePreset         Shape = "preset"           // a scenario preset: a multi-phase plan
	ShapeNone           Shape = "none"             // nothing compiles yet: only legal on not-available cards
)

// Profile is one catalog card. Every field is explicit data; the
// invariants (unique IDs, reason required when not available, shape
// consistency) are pinned by the package tests.
type Profile struct {
	ID           string       `json:"id"`      // stable machine ID, never translated
	Version      string       `json:"version"` // bumped whenever the profile's CONTENT changes: a journal's id+version pair stays meaningful forever
	Category     Category     `json:"category"`
	Visibility   Visibility   `json:"visibility"`
	Title        string       `json:"title"`             // short display name (UI language)
	Summary      string       `json:"summary"`           // one short list line: what it checks and how
	Details      string       `json:"details,omitempty"` // long description behind "Details"
	Validation   Validation   `json:"validation"`
	Shape        Shape        `json:"shape"`
	Workload     string       `json:"workload,omitempty"` // kernel registry name (session shapes)
	Preset       string       `json:"preset,omitempty"`   // preset ID (preset shapes)
	NeedsMemory  bool         `json:"needs_memory,omitempty"`
	External     string       `json:"external,omitempty"` // external tool registration name (external cards)
	Availability Availability `json:"availability"`       // explicit on every card; "" is rejected by the tests
	Reason       string       `json:"reason,omitempty"`   // required unless Available: "why not" is data, never a guess
	Order        int          `json:"order"`              // display order within a category
}

// builtIn is the frozen card set. Adding a profile means adding a
// card with an explicit category/visibility/availability - the test
// table pins that nothing else may sit in a normal list.
var builtIn = []Profile{
	// ---- CPU: the goals of menu plan section 2 ----
	{ID: "cpu-all", Version: "v1", Category: CPU, Visibility: Normal,
		Title:      "General CPU check",
		Summary:    "All selected cores under the same load; compute results compared exactly",
		Details:    "Every selected physical core receives the same compute load; every result is compared against an exact expectation. The execution mode is chosen automatically.",
		Validation: ValidationCompute, Shape: ShapeSessionAll, Workload: "int",
		Availability: Available, Order: 10},
	{ID: "cpu-per-core", Version: "v1", Category: CPU, Visibility: Normal,
		Title:      "Per-core check",
		Summary:    "Physical cores one after another: good for finding a bad core or an unstable CO",
		Details:    "Cores take the load sequentially, one after another: a mismatch lands on a concrete core, not \"somewhere among all of them\". Useful after Curve Optimizer edits.",
		Validation: ValidationCompute, Shape: ShapeSessionPerCore, Workload: "int",
		Availability: Available, Order: 20},
	{ID: "cpu-core-cycle", Version: "v1", Category: CPU, Visibility: Normal,
		Title:      "Every core separately (SMT cycle)",
		Summary:    "Each physical core first with one SMT thread, then with all threads at once",
		Details:    "Two-pass walk: first each SMT thread runs alone (its sibling idle), then the whole core under load. The different SMT modes are verified separately.",
		Validation: ValidationCompute, Shape: ShapePreset, Preset: "core-cycle",
		Availability: Available, Order: 30},
	{ID: "cpu-transitions", Version: "v1", Category: CPU, Visibility: Normal,
		Title:      "Prepared idle → load transitions",
		Summary:    "Prepared workers and short bursts: the command and the fact are verified separately",
		Details:    "Workers are prepared in advance and commanded to run idle, then short load bursts arrive on schedule. The journal keeps the commanded schedule and the verified facts apart: transitions appear as transitions. This profile claims only the commands and the acknowledgements - whether the OS really placed a core into a low-power state is a separate observation it does not make here: that evidence is gathered only under an explicit run policy, off by default.",
		Validation: ValidationCompute, Shape: ShapePreset, Preset: "transitions",
		Availability: Available, Order: 40},
	{ID: "cpu-all-float", Version: "v1", Category: CPU, Visibility: Extended,
		Title:      "General CPU check · floating point",
		Summary:    "The same all-core walk with floating-point computation as the load",
		Validation: ValidationCompute, Shape: ShapeSessionAll, Workload: "float",
		Availability: Available, Order: 50},
	{ID: "cpu-all-sha256", Version: "v1", Category: CPU, Visibility: Extended,
		Title:      "General CPU check · SHA-256",
		Summary:    "All-core walk with an integer-logic hash load",
		Validation: ValidationCompute, Shape: ShapeSessionAll, Workload: "sha256",
		Availability: Available, Order: 60},

	// ---- RAM: profile → volume → criterion (menu plan section 2) ----
	{ID: "ram-constant", Version: "v1", Category: RAM, Visibility: Normal,
		Title:      "Constant write patterns",
		Summary:    "Data verified inside memory-engine regions; writing and re-checking one pattern",
		Details:    "The classic integrity check: a pattern is written and re-checked inside memory-engine allocations. One shared budget per run, not a size per core.",
		Validation: ValidationMemory, Shape: ShapeSessionAll, Workload: "ram-constant", NeedsMemory: true,
		Availability: Available, Order: 10},
	{ID: "ram-address", Version: "v1", Category: RAM, Visibility: Normal,
		Title:      "Address patterns",
		Summary:    "Address-line check: the values depend on the region's address",
		Validation: ValidationMemory, Shape: ShapeSessionAll, Workload: "ram-address", NeedsMemory: true,
		Availability: Available, Order: 20},
	{ID: "ram-moving", Version: "v1", Category: RAM, Visibility: Normal,
		Title:      "Moving data",
		Summary:    "Content is copied between regions: the transfer is verified, not only the write",
		Validation: ValidationMemory, Shape: ShapeSessionAll, Workload: "ram-moving", NeedsMemory: true,
		Availability: Available, Order: 30},
	{ID: "ram-random", Version: "v1", Category: RAM, Visibility: Normal,
		Title:      "Independent data stream",
		Summary:    "Seeded pseudo-random data: every run gets its own stream, replay is reproducible by seed",
		Validation: ValidationMemory, Shape: ShapeSessionAll, Workload: "ram-random", NeedsMemory: true,
		Availability: Available, Order: 40},
	{ID: "ram-extended", Version: "v2", Category: RAM, Visibility: Normal,
		Title:      "Extended patterns",
		Summary:    "The full P07 set: base cycles plus stride, reverse passes, block swaps, walking bits, copies between region pairs and a dwell check",
		Details:    "Every phase is a separately versioned kernel with its own verification contract; one shared budget per phase. Cross-core block handoff is not part of the set yet; the dwell check verifies data survival across a pause, not DRAM retention; the set is not a Rowhammer test.",
		Validation: ValidationMemory, Shape: ShapePreset, Preset: "ram-extended", NeedsMemory: true,
		Availability: Available, Order: 50},
	{ID: "mem-quick", Version: "v1", Category: RAM, Visibility: Extended,
		Title:      "Quick small memory check",
		Summary:    "A short check of a small buffer: a fast smoke test, not overnight validation",
		Validation: ValidationMemory, Shape: ShapeSessionAll, Workload: "mem",
		Availability: Available, Order: 60},
	{ID: "mem-bandwidth", Version: "v1", Category: RAM, Visibility: Extended,
		Title:      "Memory load without data verification",
		Summary:    "Memory bandwidth: heat and stress, data is NOT verified - this is not RAM validation",
		Validation: ValidationMemoryLoad, Shape: ShapeSessionAll, Workload: "mem-bw",
		Availability: Available, Order: 70},

	// ---- MIX: sequential now, simultaneous honestly pending ----
	{ID: "mix-comprehensive", Version: "v1", Category: MIX, Visibility: Normal,
		Title:      "Comprehensive sequential check",
		Summary:    "CPU, then RAM, transitions and rising load; the phases run SEQUENTIALLY - this is not a simultaneous CPU+RAM load",
		Details:    "One plan with mandatory phases and a shared verdict. Phases run one after another; this profile has no simultaneous CPU+RAM load and will not promise one.",
		Validation: ValidationMixed, Shape: ShapePreset, Preset: "mixed-validation", NeedsMemory: true,
		Availability: Available, Order: 10},
	{ID: "mix-simultaneous", Version: "v1", Category: MIX, Visibility: Normal,
		Title:      "Simultaneous CPU + RAM load",
		Summary:    "Separate verifiable CPU and RAM workers in one window with a shared budget",
		Validation: ValidationUnknown, Shape: ShapeNone,
		Availability: Unavailable, Order: 20,
		Reason: "co-execution is not implemented yet: a shared budget and separate verifiable workers are needed; calling a phase sequence a simultaneous load would be false"},
	{ID: "mix-transitions-on-ram", Version: "v1", Category: MIX, Visibility: Normal,
		Title:      "CPU transitions during a RAM check",
		Summary:    "The RAM worker keeps verifying while selected CPUs take an idle/burst schedule",
		Validation: ValidationUnknown, Shape: ShapeNone,
		Availability: Unavailable, Order: 30,
		Reason: "combining the transition lifecycle with a continuous RAM check is not implemented yet"},

	// ---- diagnostics: explicit CLI/diagnostics territory only ----
	{ID: "diag-fault", Version: "v1", Category: CPU, Visibility: Diagnostic,
		Title:      "Synthetic prober",
		Summary:    "An intentionally wrong result: checks that the finding and reporting pipeline works",
		Validation: ValidationSynthetic, Shape: ShapeSessionAll, Workload: "fault",
		Availability: Available, Order: 900},
}

// All returns every card, built-in set only (external cards are built
// per machine by the CLI from the registered tool specs).
func All() []Profile {
	out := make([]Profile, len(builtIn))
	copy(out, builtIn)
	return out
}

// Get looks a card up by its stable ID.
func Get(id string) (Profile, bool) {
	for _, p := range builtIn {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// List returns the cards of one category whose visibility is at most
// as open as vis (Normal asks for Normal only; Extended includes
// Normal and Extended; Diagnostic includes everything). Diagnostic
// cards therefore never surface in a Normal or Extended list. The
// result is ordered by Order, then ID, and is deterministic.
func List(cat Category, vis Visibility) []Profile {
	var out []Profile
	for _, p := range builtIn {
		if p.Category != cat {
			continue
		}
		if !visibleAt(p.Visibility, vis) {
			continue
		}
		out = append(out, p)
	}
	sortProfiles(out)
	return out
}

func visibleAt(have, want Visibility) bool {
	rank := map[Visibility]int{Normal: 0, Extended: 1, Diagnostic: 2}
	return rank[have] <= rank[want]
}

func sortProfiles(ps []Profile) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && lessProfile(ps[j], ps[j-1]); j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}

func lessProfile(a, b Profile) bool {
	if a.Order != b.Order {
		return a.Order < b.Order
	}
	return a.ID < b.ID
}

// External builds the card for one registered external tool. The CLI
// knows the command path and whether it exists; the catalog keeps the
// honest three-valued availability: a registered tool whose binary is
// missing is Unavailable with the path in the reason, never "ready".
// An installed exe still does not prove the tool supports this CPU or
// that its completion protocol was seen - the card says what it can.
func External(name, command string, toolPresent bool) Profile {
	p := Profile{
		ID: "ext-" + name, Version: "v1", Category: CPU, Visibility: Extended,
		Title:      "External engine: " + name,
		Summary:    "The external tool schedules its own load; our worker count does not prove its CPU coverage",
		Details:    "Completion and findings follow the tool's own protocol (the adapter knows its format). An \"available\" status only means the registered command was found on disk.",
		Validation: ValidationExternal, Shape: ShapeSessionAll, Workload: name, External: name,
	}
	if toolPresent {
		p.Availability = Available
	} else {
		p.Availability = Unavailable
		p.Reason = "registered command not found on disk: " + command
	}
	return p
}
