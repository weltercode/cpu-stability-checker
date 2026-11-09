// Package plan defines the pluggable stability tests (which workload runs on
// which core) and resolves a config into concrete run options.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Options is a fully-resolved request to run one test.
type Options struct {
	TestName    string
	Phase       string // scenario phase label (empty for single runs)
	Cores       []int  // logical cores to test; empty = all
	A, B        string
	Workers     int
	Duration    time.Duration
	StopOnFault bool
	PerProcess  bool          // one pinned subprocess per CPU instead of threads
	Rotate      bool          // per-process mode: freeze all children but one, rotating
	Settle      time.Duration // per-process mode: idle before load, then measure the wake
	AllOrNone   bool          // with the fault blacklist: skip the whole phase if any core is faulted
	ControlCore int           // CPU the helper threads live on; -1 = none (helpers float over the targets)
	// ReservedCPUs names the target CPUs the reserve_control policy
	// removed from the workers and gave to the helpers (nil/empty =
	// nothing was removed). It is transport for the displays: the
	// reduced Cores already carry the loss into the plan identity, so
	// this itself stays out of the hash (json:"-") the way LogFile
	// and HandoffJournal do.
	ReservedCPUs []int         `json:"-"`
	MaxTempC     float64       // shared sensor cutoff; missing/stale readings also stop the run (0 = off)
	Telemetry    bool          // log a compact per-second clock/idle line during the run
	BeatEvery    time.Duration // core_beat journal cadence (0 = default 1s); live sampling is unaffected
	Memory       string        // memory-engine budget: "" = engine off, "auto"/"2GiB" = allocate + first-touch before measurement
	MemLock      string        // page-lock policy for engine regions: "" = off, "off"/"try"/"required"
	RamSeed      uint64        // seed for the ram-* family: expected data = f(seed, region, pass, offset); 0 is a valid seed
	// P08 transition mode (prepared sessions): N verified bursts per
	// core on a prepared schedule instead of one continuous window.
	// Bursts == 0 keeps the ordinary run path.
	Bursts       int           // verified bursts per core (0 = transition mode off)
	BurstGap     time.Duration // prepared idle gap before each burst
	BurstRun     time.Duration // each burst's load window
	BurstJitter  time.Duration // +/- spread on the gap (0 = fixed windows)
	BurstSeed    uint64        // jitter seed (only consumed when BurstJitter > 0; 0 is a valid seed)
	BurstStagger time.Duration // wave shift between cores' sessions (0 = all cores jump together)
	// Q04 parking evidence policy for PREPARED transition sessions
	// (Bursts > 0): the raw config word ""|off|try|required, parsed at
	// the session entry (an unknown word is a visible setup error,
	// never a silent "auto" - the execution-vocabulary rule). off (the
	// zero value) observes nothing: today's run, bit-identical.
	// The json tag + omitempty are a machine contract, not style:
	// plan.Options is marshaled wholesale into planHash, and this field
	// being INVISIBLE at its zero value is what keeps every pre-Q04
	// saved recipe's hash byte-identical. Renaming or retagging it
	// forks experiment identity - schema bump territory, never a quiet
	// edit.
	Parking string `json:"Parking,omitempty"`
	// P07e handoff phase (S5): parent-arbitrated block-ownership leases
	// over one shared section. HandoffBlocks == 0 keeps the ordinary
	// path; otherwise the geometry defines the phase, its budget and
	// its end criterion - never the clock. The section is admitted
	// like the memory engine (HandoffAdmission) before any mapping.
	HandoffBlocks     int    // blocks in the shared section (0 = handoff phase off)
	HandoffBlockBytes uint64 // uniform block size in bytes (multiple of the 4-byte word)
	HandoffRounds     int    // publish + independent re-read rounds per block
	HandoffSeed       uint64 // schedule order + image seed (0 is a valid seed)
	// HandoffJournal is a TRANSPORT override naming the journal FILE
	// itself for a handoff run: the durable handoff manifest stores
	// the journal file directly, and journal.Path appends a suffix
	// (it is not idempotent), so a replay must name the file rather
	// than rebuild a base log path from it. Scenario phases leave it
	// empty (their file comes from the phase's own log). It says
	// WHERE the evidence goes, never WHICH experiment runs, so it
	// stays out of the plan identity (json:"-" mirrors how planHash
	// excludes LogFile).
	HandoffJournal string `json:"-"`
	// Q05 prepared pair-switch (minimal honest core): ONE prepared
	// session in which two prepared groups (each [aggressorCore,
	// observerCore], slot order mirroring abab) alternate turns on a
	// commanded schedule - every participant prepared before the first
	// switch, so the measured transition contains no spawn and no
	// allocation. Like Parking, the json tag + omitempty is a machine
	// contract, not style: the WHOLE mode is invisible to the marshaled
	// plan at its zero values, so every pre-Q05 saved recipe replays
	// with a byte-identical plan hash; any visible field would fork
	// experiment identity (schema-bump territory).
	SwitchPairs  [][]int       `json:"SwitchPairs,omitempty"`  // group index -> [aggressor, observer] (>=2 groups: manual list or scope-generated)
	SwitchRounds int           `json:"SwitchRounds,omitempty"` // cycles over the groups; the phase's end criterion - never the clock (0 = mode absent)
	SwitchGap    time.Duration `json:"SwitchGap,omitempty"`    // prepared idle gap before each turn (switch windows are also parking-observation windows)
	SwitchRun    time.Duration `json:"SwitchRun,omitempty"`    // each turn's load window
	SwitchJitter time.Duration `json:"SwitchJitter,omitempty"` // +/- spread on the gap (0 = fixed windows)
	SwitchSeed   uint64        `json:"SwitchSeed,omitempty"`   // consumed by jitter or a seeded order (0 is a valid seed)
	SwitchOrder  string        `json:"SwitchOrder,omitempty"`  // "" = fixed alternation; "seeded" per the pinned word spec
	LogFile      string
	ShowMetrics  bool
	// PlanHash is the experiment identity computed by the ENTRY
	// POINT from the normalized request plus this resolved plan
	// (see cli.planHash): CLI overrides, menu choices and saved
	// recipes all land in it, which the config-file hash cannot do.
	// The engines journal it verbatim into run_meta; "" means the
	// entry point did not compute one (unknown, never a claim).
	PlanHash string
	// SessionID is the unique id of the user session this run
	// belongs to (U02 history): the entry point mints it before
	// any load and the engines journal it verbatim into run_meta,
	// exactly like PlanHash. "" means the entry point started no
	// tracked session (unknown, never a claim).
	SessionID string
}

// Default burst windows: one source of truth for the CLI flags and for
// scenario phases, so the two entry paths can never drift apart.
const (
	DefaultBurstGap = 200 * time.Millisecond
	DefaultBurstRun = 500 * time.Millisecond
)

// RunSummary describes what is running in a few honest characters: the
// workloads, how many slots, and the mode when it is not the default
// in-process one. The panel footer uses it; the phase tag alone
// ("session-c3-r1") never explained itself to anyone watching.
func RunSummary(opt Options, slots int) string {
	run := opt.A
	if opt.B != "" && opt.B != opt.A {
		run += " vs " + opt.B
	}
	run += fmt.Sprintf(" ×%d", slots)
	switch {
	case opt.Rotate:
		run += " rotate"
	case opt.PerProcess:
		run += " per-core"
	}
	return run
}

// OptionsFromConfig validates a config and maps it to run options.
func OptionsFromConfig(cfg config.Config) (Options, error) {
	test, ok := GetTest(cfg.Test)
	if !ok {
		return Options{}, fmt.Errorf("unknown test %q; run 'cpu-stability list'", cfg.Test)
	}
	if test.workloadSlots() >= 1 {
		if _, ok := workload.Get(cfg.WorkloadA); !ok && cfg.WorkloadA != workload.Idle {
			return Options{}, fmt.Errorf("unknown primary workload %q", cfg.WorkloadA)
		}
	}
	if test.workloadSlots() >= 2 {
		if _, ok := workload.Get(cfg.WorkloadB); !ok && cfg.WorkloadB != workload.Idle {
			return Options{}, fmt.Errorf("unknown secondary workload %q", cfg.WorkloadB)
		}
	}
	if err := validateLogicalCPUs(cfg.Cores); err != nil {
		return Options{}, err
	}

	duration, err := time.ParseDuration(cfg.Duration)
	if err != nil || duration <= 0 {
		return Options{}, fmt.Errorf("duration %q must be a positive Go duration such as 30s or 5m", cfg.Duration)
	}
	if cfg.Workers < 1 {
		return Options{}, fmt.Errorf("workers must be at least 1")
	}
	if strings.TrimSpace(cfg.LogFile) == "" {
		return Options{}, fmt.Errorf("log_file must not be empty")
	}
	control, effCores, removed, err := ResolveControl(cfg.ControlCore, cfg.Cores, cfg.ReserveControl)
	if err != nil {
		return Options{}, err
	}
	if cfg.MaxTemp < 0 || math.IsNaN(cfg.MaxTemp) || math.IsInf(cfg.MaxTemp, 0) {
		return Options{}, fmt.Errorf("max_temp must be 0 (off) or a positive Celsius limit")
	}
	var beat time.Duration
	if s := strings.TrimSpace(cfg.BeatEvery); s != "" {
		var err error
		beat, err = time.ParseDuration(s)
		if err != nil || beat <= 0 {
			return Options{}, fmt.Errorf("beat_interval %q must be a positive Go duration such as 5s", s)
		}
	}
	if err := ValidateMemorySettings(cfg.Memory, cfg.MemoryLock); err != nil {
		return Options{}, err
	}
	// U01 auto-execution (menu plan section 4): the worker mode is
	// policy, not a user question. The resolver decides from the
	// workloads' hard requirements; a contradicting settings override
	// is refused HERE (resolve time, exit 2 before any load), never
	// quietly switched and never left to the executor's preflight.
	dec, err := ResolveExecution(cfg.Execution, cfg.WorkloadA, cfg.WorkloadB, 0)
	if err != nil {
		return Options{}, err
	}
	// Q04: the parking word is validated at resolve time, exactly like
	// the execution and memory-lock vocabularies: an unknown value is
	// a visible setup error naming itself before any load, never a
	// silent "off". ParseParkingPolicy normalizes "off"/"OFF"/spaces
	// to the same empty value, so spelling the default out loud never
	// forks a run's identity.
	parking, err := transition.ParseParkingPolicy(cfg.ParkingPolicy)
	if err != nil {
		return Options{}, err
	}

	return Options{
		TestName:     cfg.Test,
		Cores:        append([]int(nil), effCores...),
		A:            cfg.WorkloadA,
		B:            cfg.WorkloadB,
		Workers:      cfg.Workers,
		Duration:     duration,
		StopOnFault:  cfg.StopOnFault,
		PerProcess:   dec.PerProcess,
		Rotate:       cfg.Rotate || dec.Rotate,
		ControlCore:  control,
		ReservedCPUs: removed,
		MaxTempC:     cfg.MaxTemp,
		Telemetry:    cfg.Telemetry,
		BeatEvery:    beat,
		Memory:       strings.TrimSpace(cfg.Memory),
		MemLock:      strings.TrimSpace(cfg.MemoryLock),
		RamSeed:      cfg.RamSeed,
		LogFile:      cfg.LogFile,
		ShowMetrics:  cfg.ShowMetrics,
		Parking:      string(parking),
	}, nil
}

// ReproductionParameters renders one compact line listing the resolved
// parameters that define this run, so a result can be reproduced. It
// is deliberately a parameter list, not a reconstructed command line:
// the memory budget, lock policy and seed come from config.json (there
// is no -memory flag), and saying which parts are config-sourced is
// part of an honest reproduction note. It never claims to reproduce
// machine state (thermals, other load, the exact silicon).
func ReproductionParameters(opt Options) string {
	parts := []string{fmt.Sprintf("test=%s", opt.TestName)}
	if opt.A != "" {
		parts = append(parts, "a="+opt.A)
	}
	if opt.B != "" {
		parts = append(parts, "b="+opt.B)
	}
	if len(opt.Cores) > 0 {
		parts = append(parts, "cores="+human.Ints(opt.Cores))
	} else {
		parts = append(parts, "cores=all")
	}
	if opt.Workers > 0 {
		parts = append(parts, fmt.Sprintf("workers=%d", opt.Workers))
	}
	mode := "in-process"
	if opt.PerProcess {
		mode = "per-process"
		if opt.Rotate {
			mode += "+rotate"
		}
		if opt.Settle > 0 {
			mode += fmt.Sprintf("+settle%s", opt.Settle)
		}
	}
	parts = append(parts, "mode="+mode)
	if opt.Bursts > 0 {
		sched := fmt.Sprintf("%dbursts/gap%s/run%s", opt.Bursts, opt.BurstGap, opt.BurstRun)
		if opt.BurstJitter > 0 {
			sched += fmt.Sprintf("/jit±%s/seed%d", opt.BurstJitter, opt.BurstSeed)
		}
		if opt.BurstStagger > 0 {
			sched += fmt.Sprintf("/stagger%s", opt.BurstStagger)
		}
		if opt.Parking != "" {
			sched += "/parking=" + opt.Parking // Q04: the word that decides whether parking evidence was gathered at all
		}
		parts = append(parts, "schedule="+sched)
	} else if opt.Duration > 0 {
		parts = append(parts, "duration="+opt.Duration.String())
	}
	if opt.SwitchRounds > 0 {
		sw := fmt.Sprintf("%dgroups x %drounds/gap%s/run%s", len(opt.SwitchPairs), opt.SwitchRounds, opt.SwitchGap, opt.SwitchRun)
		if opt.SwitchJitter > 0 {
			sw += fmt.Sprintf("/jit±%s/seed%d", opt.SwitchJitter, opt.SwitchSeed)
		}
		if opt.SwitchOrder != "" {
			sw += "/order=" + opt.SwitchOrder
		}
		parts = append(parts, "switch="+sw) // Q05: the turn-taking shape the run actually commands
	}
	if opt.Memory != "" {
		lock := opt.MemLock
		if lock == "" {
			lock = "off"
		}
		parts = append(parts, fmt.Sprintf("memory=%s/lock=%s/ram-seed=%d (from config.json)", opt.Memory, lock, opt.RamSeed))
	}
	if opt.ControlCore >= -1 {
		switch {
		case opt.ControlCore < 0:
			parts = append(parts, "control=off")
		case len(opt.ReservedCPUs) > 0:
			// The reserved CPU is a coverage loss the user opted into:
			// the reproduce line names it, so a 31-of-32 experiment can
			// never be compared against a full-coverage one by accident.
			parts = append(parts, fmt.Sprintf("control=core%d (reserved from workers: %s)", opt.ControlCore, human.Ints(opt.ReservedCPUs)))
		default:
			parts = append(parts, fmt.Sprintf("control=core%d", opt.ControlCore))
		}
	}
	if opt.MaxTempC > 0 {
		parts = append(parts, fmt.Sprintf("max-temp=%.0fC", opt.MaxTempC))
	}
	return "reproduce: " + strings.Join(parts, " ")
}

// ValidateMemorySettings checks the memory-engine knobs before any
// run starts: a malformed budget or lock mode is a visible setup
// error, never a silent fallback to auto or off. Empty memory means
// the engine stays off; empty lock means never lock.
func ValidateMemorySettings(memory, lock string) error {
	if _, err := memtest.ParseLockMode(lock); err != nil {
		return err
	}
	if m := strings.TrimSpace(memory); m != "" {
		if _, err := memtest.ParseRequest(m); err != nil {
			return fmt.Errorf("memory %q: %w", m, err)
		}
	}
	return nil
}

// PreflightMemoryWorkloads refuses, BEFORE anything starts, a run whose
// selected workloads need the memory engine but cannot get it: no
// budget configured, or a mode where the engine is not applied
// (in-process). A silent fallback would run the workload without the
// regions it promises to verify - the quiet reduction this project
// forbids - so the refusal is a visible setup error (exit 2) instead.
func PreflightMemoryWorkloads(opt Options) error {
	var need []string
	names := []string{opt.A, opt.B}
	// A handoff phase is judged by its own contract: the coordinator
	// drives slot A (the ram-handoff kernel) only, and the phase's
	// budget is the shared section geometry ITSELF - admitted at the
	// dry-run gate (HandoffAdmission) and again before mapping at
	// execution (B09). The "memory" engine knob neither grants nor
	// bounds that section, so requiring it here would refuse an honest
	// phase while adding no safety; shape errors, though, refuse right
	// here instead of surfacing as a mid-run setup error.
	if opt.HandoffBlocks > 0 {
		if err := ValidateHandoff(opt); err != nil {
			return err
		}
		names = nil // ram-handoff's need is met by the section; slot B never loads in this shape
	}
	for _, name := range names {
		if wl, ok := workload.Get(name); ok && wl.RequiresMemory && !contains(need, name) {
			need = append(need, name)
		}
	}
	// Audit C01 closeout: per-thread grant ownership landed for the
	// one-shot per-process path (the parent re-splits each child's
	// data envelope at thread granularity; every thread owns a
	// disjoint set, the child re-validates the map from argv). The
	// session shape is a DIFFERENT contract, not an unproven one: a
	// session worker is EXACTLY ONE prepared engine per pinned core
	// (P08 sessions serve bursts one at a time; P07e handoff
	// ownership is the per-block grant plus the parent-arbitrated
	// lease, proven end to end with the corruption probe). A
	// workers>1 request here would be parsed and silently never run
	// - the quiet reduction this project forbids - so ANY
	// workers>1 in a prepared-burst run is refused as a corrupted
	// contract before anything loads, one-shot threads stay legal by
	// ownership, and no silent worker reduction happens anywhere.
	if opt.Bursts > 0 && opt.Workers > 1 {
		return fmt.Errorf("workers=%d with prepared bursts: a session worker is one prepared engine per pinned core - extra threads would be parsed but never run (audit C01 shape: silently ignored concurrency). Keep workers=1 for transitions, or use one-shot per-process windows where per-thread grant ownership is proven", opt.Workers)
	}
	engineConfigured := strings.TrimSpace(opt.Memory) != ""
	if len(need) == 0 {
		return nil
	}
	if !engineConfigured {
		return fmt.Errorf("workload(s) %v need the memory engine: set memory in config (\"auto\" or an exact size like \"2GiB\")", need)
	}
	if !opt.PerProcess {
		return fmt.Errorf("workload(s) %v need the memory engine, which is applied in per-process mode only: run with -per-process", need)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ConfigHashAt returns the sha256 hex of the file's bytes, or "" when
// it cannot be read: unknown, never a guess.
func ConfigHashAt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TopologyFingerprint is a stable sha256 over the CPU topology a plan
// resolved against: source, per physical core (index, group, NUMA
// node, efficiency class, CCX when known) and its logical CPU numbers.
// Two runs with the same plan on different layouts are different
// experiments, and this is how a report can tell. An empty topology
// fingerprints to "" (unknown, never a hash of nothing).
func TopologyFingerprint(t sysinfo.Topology) string {
	if len(t.PhysicalCores) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "topology/1|%s|est=%t|%d cores\n", t.Source, t.Estimated, len(t.PhysicalCores))
	for _, c := range t.PhysicalCores {
		fmt.Fprintf(&b, "c%d:g%d:n%d:e%d:", c.Index, c.Group, c.NUMANode, c.EfficiencyClass)
		if c.CCXKnown {
			fmt.Fprintf(&b, "ccx%d", c.CCXIndex)
		} else {
			b.WriteString("ccx?")
		}
		for _, l := range c.LogicalCPUs {
			fmt.Fprintf(&b, "|l%d.%d", l.Group, l.Number)
		}
		b.WriteString("\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// RunProvenance assembles the run_meta record: the build stamp Go
// embeds, the config file as present at run start, the configured seed
// and the machine identity the platform offers. Every part is
// independently unknown-safe — an executor never invents an identity
// it could not observe.
func RunProvenance(opt Options) journal.RunMetaFields {
	m := journal.BuildProvenance()
	m.ConfigHash = ConfigHashAt(config.DefaultPath)
	m.PlanHash = opt.PlanHash
	m.SessionID = opt.SessionID
	if t, err := sysinfo.DiscoverTopology(); err == nil {
		m.TopologyFP = TopologyFingerprint(t)
	}
	seed := opt.RamSeed
	m.Seed = &seed
	id := sysinfo.DiscoverMachineIdentity()
	m.CPUName, m.OSVersion, m.BIOSVersion = id.CPUName, id.OSVersion, id.BIOSVersion
	m.Memory = CollectMemoryProvenance()
	return m
}

// CollectMemoryProvenance performs the one read-only firmware lookup
// provenance needs and maps the answer, recording a refusal as a named
// UnknownReason instead of silence: a run that knows nothing about the
// machine's memory must be able to say why. It never returns nil and
// it never starts a load. Both the journal's run_meta and the
// read-only 'version' command go through here, so the evidence a run
// leaves and the evidence a user reads are the same values.
func CollectMemoryProvenance() *journal.RunMemoryFields {
	prof, err := sysinfo.CollectMemoryProfile()
	if err != nil {
		return &journal.RunMemoryFields{UnknownReason: err.Error()}
	}
	return MemoryProvenance(prof)
}

// MemoryProvenance mirrors the sysinfo SMBIOS memory observations into
// journal fields one-to-one: pointers stay pointers (nil = not
// reported), no totals, no "effective running speed", no
// above/below-rating claim is computed here. Provenance is evidence
// other humans read; the mapping must not smuggle in an
// interpretation the collector refused to make.
//
// It never returns nil for a profile that was actually observed - not
// even an empty one: an omitted field reads as "nobody looked", while
// "looked, firmware published no device records" is itself an
// observation worth recording. A collector that refused is recorded by
// the caller with UnknownReason instead.
//
// Exported because the read-only 'version' command answers with the
// same mapping a journal records: what the user sees printed and what
// gets journaled can never drift apart.
func MemoryProvenance(p sysinfo.MemoryProfile) *journal.RunMemoryFields {
	f := &journal.RunMemoryFields{
		SMBIOSVersion:      p.SMBIOSVersion,
		RejectedCandidates: p.RejectedCandidates,
	}
	if p.ArrayMaxCapacityMB != nil {
		v := *p.ArrayMaxCapacityMB
		f.ArrayMaxCapacityMB = &v
	}
	for _, d := range p.Devices {
		e := journal.RunMemoryDevice{
			Locator:      d.Locator,
			BankLocator:  d.BankLocator,
			Manufacturer: d.Manufacturer,
			PartNumber:   d.PartNumber,
		}
		for _, cp := range []struct{ src, dst **int }{
			{&d.SizeMB, &e.SizeMB},
			{&d.SPDMaxMTs, &e.SPDMaxMTs},
			{&d.MaxCapMTs, &e.MaxCapMTs},
			{&d.ConfiguredMTs, &e.ConfiguredMTs},
		} {
			if *cp.src != nil {
				v := **cp.src
				*cp.dst = &v
			}
		}
		f.Devices = append(f.Devices, e)
	}
	return f
}

// ResolveControlCore keeps the pre-reserve signature for callers that
// carry no headroom policy: auto/off/explicit only, targets never
// touched, the exact answers and refusals this resolver has always
// given.
func ResolveControlCore(explicit *int, cores []int) (int, error) {
	control, _, _, err := ResolveControl(explicit, cores, "")
	return control, err
}

// ResolveControl is the SINGLE control-core logic. Explicit policy:
// nil = auto (the first CPU no worker occupies, -1 when every visible
// CPU is a target), -1 = off (helpers float over the targets), N =
// that core - which must exist and must not be a target core. The
// reserve policy is an explicit, opt-in headroom consent: "logical"
// means that when AUTO has no free CPU, the highest-numbered logical
// CPU of the effective target set leaves the worker set and becomes
// the control CPU, so the watchdog, the journal writer and the panel
// get a CPU of their own instead of competing with spinning workers.
// That consent changes coverage and says so: the returned effective
// set (a removal materializes "all cores" into explicit CPUs) is what
// hashes, journals and runs; the removed CPUs come back named. Reserve
// belongs to auto alone - an explicit core already answers for the
// helpers, and -1 asks for floating helpers by name. "none"/"" keeps
// every historical behavior byte-identical.
func ResolveControl(explicit *int, cores []int, reserve string) (control int, effective, removed []int, err error) {
	switch reserve {
	case "", "none":
	case "logical":
		if explicit != nil {
			return 0, nil, nil, fmt.Errorf(`reserve_control="logical" needs auto control (omit control_core): control_core=%d already answers for the helpers itself`, *explicit)
		}
	default:
		return 0, nil, nil, fmt.Errorf(`reserve_control must be "" / "none" (never shrink) or "logical" (hand the highest free-of-nobody logical CPU to helpers), got %q`, reserve)
	}
	if explicit != nil && *explicit < -1 {
		return 0, nil, nil, fmt.Errorf("control_core must be -1 (off), a logical CPU number, or omitted (auto)")
	}
	occupied := make(map[int]bool, len(cores))
	for _, c := range cores {
		occupied[c] = true
	}
	if len(cores) == 0 {
		for c := range runtime.NumCPU() {
			occupied[c] = true
		}
	}
	if explicit == nil {
		for c := range runtime.NumCPU() {
			if !occupied[c] {
				return c, cores, nil, nil
			}
		}
		if reserve != "logical" {
			return -1, cores, nil, nil
		}
		// Saturated auto with consent: the highest-numbered logical
		// CPU is removed by a fixed, explainable rule (no topology
		// guessing about SMT pairs - the OS scheduler shares the
		// remaining pair of that physical core with its worker, and
		// the plan says the coverage out loud).
		highest := runtime.NumCPU() - 1
		eff := make([]int, 0, len(occupied))
		if len(cores) == 0 {
			for c := 0; c < highest; c++ {
				eff = append(eff, c)
			}
		} else {
			for _, c := range cores {
				if c != highest {
					eff = append(eff, c)
				}
			}
		}
		if len(eff) == 0 {
			return 0, nil, nil, fmt.Errorf("reserve_control=logical would leave no target CPU to test (this machine exposes one logical CPU): refusing to run an empty experiment instead")
		}
		return highest, eff, []int{highest}, nil
	}
	n := *explicit
	if n == -1 {
		return -1, cores, nil, nil
	}
	if n < 0 || n >= runtime.NumCPU() {
		return 0, nil, nil, fmt.Errorf("control core %d is outside the available range 0..%d", n, runtime.NumCPU()-1)
	}
	if occupied[n] {
		return 0, nil, nil, fmt.Errorf("control core %d is also a target core; helpers need a CPU of their own", n)
	}
	return n, cores, nil, nil
}

// OptionsFromPhase merges one scenario phase over the config defaults and
// validates the result exactly like OptionsFromConfig does. Phases are
// resolved up front, before any load starts, so a typo in phase 5 fails
// the whole scenario instead of surprising us mid-run.
func OptionsFromPhase(cfg config.Config, ph config.Phase) (Options, error) {
	merged := cfg
	merged.Phases = nil // the phase list itself is not inherited
	if ph.Test != "" {
		merged.Test = ph.Test
	}
	if ph.A != "" {
		merged.WorkloadA = ph.A
	}
	if ph.B != "" {
		merged.WorkloadB = ph.B
	}
	if len(ph.Cores) > 0 {
		merged.Cores = ph.Cores
	}
	if ph.Workers > 0 {
		merged.Workers = ph.Workers
	}
	if ph.Duration != "" {
		merged.Duration = ph.Duration
	}
	if ph.StopOnFault != nil {
		merged.StopOnFault = *ph.StopOnFault
	}
	if ph.Rotate != nil {
		merged.Rotate = *ph.Rotate
	}
	if ph.ParkingPolicy != "" {
		merged.ParkingPolicy = ph.ParkingPolicy
	}
	opt, err := OptionsFromConfig(merged)
	if err != nil {
		return Options{}, err
	}
	opt.Phase = ph.Name
	opt.PerProcess = ph.PerProcess
	// U01 auto-execution: a phase that did not name its worker mode
	// explicitly gets the resolver's answer for THIS phase - workloads
	// and burst schedule included (OptionsFromConfig resolved without
	// seeing the bursts). An explicit phase per_process stays the
	// author's word.
	if !opt.PerProcess {
		pdec, err := ResolveExecution(merged.Execution, opt.A, opt.B, ph.Bursts)
		if err != nil {
			return Options{}, err
		}
		opt.PerProcess = pdec.PerProcess
		opt.Rotate = opt.Rotate || pdec.Rotate
	}
	opt.AllOrNone = ph.AllOrNone
	// Q05: does this phase name a pair-switch shape at all? The full
	// all-or-nothing validation runs at the end of this function, but
	// the answer is needed early: the parking word rule and the
	// execution mode below both depend on it.
	swPhase := ph.SwitchRounds > 0 || len(ph.SwitchPairs) > 0 || ph.SwitchOrder != "" ||
		ph.SwitchGap != "" || ph.SwitchRun != "" || ph.SwitchJitter != "" || ph.SwitchSeed != 0
	if ph.Settle != "" {
		settle, err := time.ParseDuration(ph.Settle)
		if err != nil || settle < 0 {
			return Options{}, fmt.Errorf("settle %q must be a non-negative Go duration such as 2s", ph.Settle)
		}
		opt.Settle = settle
	}
	if ph.Bursts > 0 {
		opt.Bursts = ph.Bursts
		opt.BurstGap, opt.BurstRun = DefaultBurstGap, DefaultBurstRun
		if err := parseBurstDuration("burst_gap", ph.BurstGap, &opt.BurstGap, false); err != nil {
			return Options{}, err
		}
		if err := parseBurstDuration("burst_run", ph.BurstRun, &opt.BurstRun, false); err != nil {
			return Options{}, err
		}
		if err := parseBurstDuration("burst_jitter", ph.BurstJitter, &opt.BurstJitter, true); err != nil {
			return Options{}, err
		}
		if err := parseBurstDuration("burst_stagger", ph.BurstStagger, &opt.BurstStagger, true); err != nil {
			return Options{}, err
		}
		opt.BurstSeed = ph.BurstSeed
		if err := ValidateTransition(opt); err != nil {
			return Options{}, err
		}
	}
	// Q04 shape rule: parking evidence rides the MEASURED idle windows
	// of a prepared session (burst gaps, switch turn gaps), so the word
	// only makes sense on a prepared-schedule phase. A phase that NAMED
	// the word without bursts or a switch shape is a promise the phase
	// cannot keep - a visible compile error naming the phase and the
	// fix. The inherited global default needs no error: it simply does
	// not apply here, and clearing it keeps the word out of plan hash of
	// runs it would not have shaped (nothing that shapes no run belongs
	// in an experiment identity).
	if ph.ParkingPolicy != "" && ph.Bursts <= 0 && !swPhase {
		return Options{}, fmt.Errorf("phase %q sets parking_policy %q but has no bursts or pair-switch schedule: parking evidence only rides prepared idle windows (burst or switch phases); remove the word or give the phase a prepared schedule", ph.Name, ph.ParkingPolicy)
	}
	if ph.Bursts <= 0 && !swPhase {
		opt.Parking = ""
	}
	// P07e handoff phase (S5): any handoff_* presence demands the
	// whole dedicated shape - a stray handoff_block_size without
	// handoff_blocks is a malformed phase, not a quiet default. The
	// geometry is compiled once HERE (sizes, defaults, rounds, seed)
	// so the plan line, the resource gate, the hash and the executor
	// all read the same resolved values.
	if ph.HandoffBlocks > 0 || ph.HandoffBlockSize != "" || ph.HandoffRounds > 0 || ph.HandoffSeed != 0 {
		if ph.HandoffBlocks <= 0 {
			return Options{}, fmt.Errorf("handoff phase needs handoff_blocks: the shared section is blocks x block size, and the phase's end criterion is blocks x rounds - handoff_* fields without it define nothing")
		}
		opt.HandoffBlocks = ph.HandoffBlocks
		size := ph.HandoffBlockSize
		if size == "" {
			size = DefaultHandoffBlockSize
		}
		bs, err := memtest.ParseSize(size)
		if err != nil {
			return Options{}, fmt.Errorf("handoff_block_size: %w", err)
		}
		opt.HandoffBlockBytes = bs
		if ph.HandoffRounds > 0 {
			opt.HandoffRounds = ph.HandoffRounds
		} else {
			opt.HandoffRounds = 1
		}
		opt.HandoffSeed = ph.HandoffSeed
		// The block-to-core partition makes every participant
		// load-bearing: a handoff phase is skipped whole when any of
		// its cores sits in the fault blacklist, never re-divided into
		// a smaller - and differently claimed - experiment.
		opt.AllOrNone = true
		if err := ValidateHandoff(opt); err != nil {
			return Options{}, err
		}
	}
	// Q05 prepared pair-switch phase: same all-or-nothing geometry rule
	// as handoff_* (a stray switch_gap without switch_rounds is a
	// malformed phase, not a quiet default), and the geometry is
	// compiled ONCE here (durations, defaults, seed) so the plan line,
	// the hash, the dry-run and the executor all read the same resolved
	// values. ValidateSwitch then enforces the shape exclusivity
	// (bursts/handoff/rotate/workers/per-process) and builds the
	// prepared turn sequence - a phase that cannot build its schedule
	// never reaches load.
	if swPhase {
		// switch_scope/switch_cores are scenario-COMPILER words: they
		// generate groups by pairing the detected topology, and the
		// scenario pipeline resolves them into SwitchPairs before this
		// function ever runs (mirroring how expandPairs/expandCycle
		// resolve their generators first). Generation words arriving at
		// the plan with no generated groups means a path that skipped
		// the compiler - those words would then silently define nothing,
		// so they are refused by name instead.
		if (ph.SwitchScope != "" || len(ph.SwitchCores) > 0) && len(ph.SwitchPairs) == 0 {
			return Options{}, fmt.Errorf("switch_scope/switch_cores reached the plan without generated groups: this shape is paired off the DETECTED topology by the scenario compiler - compile the phase through it (or name the groups explicitly with switch_pairs)")
		}
		if ph.SwitchRounds <= 0 {
			return Options{}, fmt.Errorf("pair-switch phase needs switch_rounds: the phase's end criterion is rounds x groups - switch_* fields without it define nothing (the handoff geometry rule again)")
		}
		opt.SwitchPairs = ph.SwitchPairs
		opt.SwitchRounds = ph.SwitchRounds
		opt.SwitchGap, opt.SwitchRun = DefaultSwitchGap, DefaultSwitchRun
		if err := parseBurstDuration("switch_gap", ph.SwitchGap, &opt.SwitchGap, false); err != nil {
			return Options{}, err
		}
		if err := parseBurstDuration("switch_run", ph.SwitchRun, &opt.SwitchRun, false); err != nil {
			return Options{}, err
		}
		if err := parseBurstDuration("switch_jitter", ph.SwitchJitter, &opt.SwitchJitter, true); err != nil {
			return Options{}, err
		}
		opt.SwitchOrder = ph.SwitchOrder
		opt.SwitchSeed = ph.SwitchSeed
		// A prepared participant IS a pinned prepared process: the
		// switch shape defines its own execution mode (the bursts
		// resolver never offers in-process either), and the journal,
		// coverage and blacklist skip must see the participant list,
		// not the config's default cores: the groups name exactly the
		// CPUs this phase loads.
		opt.PerProcess = true
		var pcores []int
		for _, group := range ph.SwitchPairs {
			pcores = append(pcores, group...)
		}
		if len(pcores) > 0 {
			opt.Cores = pcores
		}
		// Every participant is load-bearing for its group's turns: a
		// fault-blacklisted core skips the phase whole instead of
		// leaving one group half-loaded (the pairs/handoff rule).
		opt.AllOrNone = true
		if err := ValidateSwitch(opt); err != nil {
			return Options{}, err
		}
	}
	return opt, nil
}

// parseBurstDuration reads one transition knob; empty keeps the default,
// and only gap/run reject an unparseable value strictly (jitter and
// stagger may not be zero but must still parse).
func parseBurstDuration(field, raw string, dst *time.Duration, allowEmptyDefault bool) error {
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s %q must be a Go duration such as 300ms", field, raw)
	}
	if d < 0 {
		return fmt.Errorf("%s %q must not be negative", field, raw)
	}
	*dst = d
	return nil
}

// ValidateTransition checks the P08 transition-mode knobs on a resolved
// Options: prepared sessions exist only in per-process mode, and the
// schedule commands the bursts, so rotate (which freezes whole windows)
// contradicts them. Called for scenario phases; the CLI run path applies
// the same contract at flag validation.
func ValidateTransition(opt Options) error {
	if opt.Bursts <= 0 {
		return nil
	}
	if !opt.PerProcess {
		return fmt.Errorf("bursts need per-process mode: transition mode drives prepared worker processes")
	}
	if opt.Rotate {
		return fmt.Errorf("bursts and rotate do not mix: the schedule commands the bursts, rotate freezes whole windows")
	}
	if opt.BurstRun <= 0 {
		return fmt.Errorf("burst run window must be positive")
	}
	return nil
}

func validateLogicalCPUs(cpus []int) error {
	seen := make(map[int]struct{}, len(cpus))
	for _, cpu := range cpus {
		if cpu < 0 || cpu >= runtime.NumCPU() {
			return fmt.Errorf("logical CPU %d is outside the available range 0..%d", cpu, runtime.NumCPU()-1)
		}
		if _, duplicate := seen[cpu]; duplicate {
			return fmt.Errorf("logical CPU %d is listed more than once", cpu)
		}
		seen[cpu] = struct{}{}
	}
	return nil
}

// LoadResolvedPlan reads the config, registers external workloads, validates
// everything and discovers the CPU topology.
func LoadResolvedPlan(configPath string) (Options, sysinfo.Topology, error) {
	cfg, err := config.LoadStrict(configPath)
	if err != nil {
		return Options{}, sysinfo.Topology{}, err
	}
	workload.RegisterExternal(cfg.External)
	opt, err := OptionsFromConfig(cfg)
	if err != nil {
		return Options{}, sysinfo.Topology{}, fmt.Errorf("validate %s: %w", configPath, err)
	}
	topology, err := sysinfo.DiscoverTopology()
	if err != nil {
		return Options{}, sysinfo.Topology{}, fmt.Errorf("discover CPU topology: %w", err)
	}
	return opt, topology, nil
}

// WriteRunPlan renders the resolved plan for the 'plan' command.
func WriteRunPlan(w io.Writer, configPath string, opt Options, topology sysinfo.Topology) {
	test, _ := GetTest(opt.TestName)
	physicalCount := len(topology.PhysicalCores)
	logicalCount := topologyLogicalCount(topology)
	smt := topologySMTLabel(topology)
	selected := SelectedLogicalCPUs(opt.Cores, logicalCount)
	plan := test.Plan(len(selected), opt.A, opt.B)

	absLog, err := filepath.Abs(opt.LogFile)
	if err != nil {
		absLog = opt.LogFile
	}

	fmt.Fprintln(w, "==============================================================================")
	fmt.Fprintln(w, " CPU Stability Checker — resolved run plan")
	fmt.Fprintln(w, "==============================================================================")
	fmt.Fprintf(w, "Configuration file: ................. %s\n", configPath)
	fmt.Fprintf(w, "Topology source: .................... %s\n", topology.Source)
	fmt.Fprintf(w, "Logical / physical CPUs: ............ %d logical / %d physical\n", logicalCount, physicalCount)
	fmt.Fprintf(w, "SMT: ................................ %s\n", smt)
	fmt.Fprintf(w, "Test layout: ........................ %s (%s)\n", opt.TestName, test.Description)
	if test.workloadSlots() >= 1 {
		a, _ := workload.Get(opt.A)
		fmt.Fprintf(w, "Primary workload: ................... %s — %s\n", opt.A, a.Description)
	}
	if test.workloadSlots() >= 2 {
		b, _ := workload.Get(opt.B)
		fmt.Fprintf(w, "Secondary workload: ................. %s — %s\n", opt.B, b.Description)
	}
	if test.workloadSlots() == 0 {
		fmt.Fprintln(w, "Workloads: .......................... all registered built-in workloads")
	}
	fmt.Fprintf(w, "Selected logical CPUs: .............. %s\n", logicalCPUListLabel(opt.Cores, logicalCount))
	if len(opt.ReservedCPUs) > 0 {
		fmt.Fprintf(w, "Reserved for helpers: ............. %s (removed from the target set so watchdog/journal/panel get a CPU of their own)\n", human.Ints(opt.ReservedCPUs))
	}
	fmt.Fprintf(w, "Workers per CPU slot: ............... %d\n", opt.Workers)
	fmt.Fprintf(w, "Total worker goroutines: ............ %d\n", len(selected)*opt.Workers)
	fmt.Fprintf(w, "Run duration: ....................... %s\n", opt.Duration)
	if opt.ControlCore >= 0 {
		fmt.Fprintf(w, "Control CPU (helper threads): ....... CPU %d\n", opt.ControlCore)
	}
	if opt.MaxTempC > 0 {
		fmt.Fprintf(w, "Temperature cutoff: ................... %.1f°C (shared panel sensor; stops if data is lost)\n", opt.MaxTempC)
	} else {
		fmt.Fprintf(w, "Control CPU (helper threads): ....... none (every visible core is a target)\n")
	}
	fmt.Fprintf(w, "Stop on workload fault: ............. %s\n", human.Enabled(opt.StopOnFault))
	fmt.Fprintf(w, "Live metrics: ....................... %s\n", human.Enabled(opt.ShowMetrics))
	fmt.Fprintf(w, "Log file: ........................... %s\n", absLog)

	fmt.Fprintln(w, "\nWorkload assignment (intended static layout):")
	for _, assignment := range groupAssignments(selected, plan) {
		fmt.Fprintf(w, "  %-8s -> logical CPU %s\n", assignment.workload, human.Ints(assignment.cpus))
	}

	fmt.Fprintln(w, "\nWorker pinning:")
	fmt.Fprintln(w, "  Each worker goroutine is locked to an OS thread and pinned to its")
	fmt.Fprintln(w, "  assigned logical CPU (where the OS supports thread affinity), so the")
	fmt.Fprintln(w, "  layout above is a scheduling guarantee, not an intention. Helper")
	fmt.Fprintln(w, "  threads (watchdog, UI, journal, signals, WHEA) sit on the control CPU")
	fmt.Fprintln(w, "  when one exists; GC and sysmon cannot be pinned from Go. Use the")
	fmt.Fprintln(w, "  'topology' command to inspect SMT mapping.")
	fmt.Fprintln(w, "==============================================================================")
}

func topologyLogicalCount(topology sysinfo.Topology) int {
	count := 0
	for _, core := range topology.PhysicalCores {
		count += len(core.LogicalCPUs)
	}
	return count
}

func topologySMTLabel(topology sysinfo.Topology) string {
	for _, core := range topology.PhysicalCores {
		if len(core.LogicalCPUs) > 1 {
			return "ENABLED"
		}
	}
	return "DISABLED OR NOT DETECTED"
}

// SelectedLogicalCPUs returns the configured CPUs, or all CPUs 0..total-1
// when none were configured.
func SelectedLogicalCPUs(configured []int, total int) []int {
	if len(configured) > 0 {
		return append([]int(nil), configured...)
	}
	out := make([]int, total)
	for i := range out {
		out[i] = i
	}
	return out
}

func logicalCPUListLabel(configured []int, total int) string {
	if len(configured) == 0 {
		return fmt.Sprintf("ALL (0..%d)", total-1)
	}
	return human.Ints(configured)
}

type workloadAssignment struct {
	workload string
	cpus     []int
}

func groupAssignments(cpus []int, plan []string) []workloadAssignment {
	byWorkload := make(map[string][]int)
	var order []string
	for i, workload := range plan {
		if _, exists := byWorkload[workload]; !exists {
			order = append(order, workload)
		}
		byWorkload[workload] = append(byWorkload[workload], cpus[i])
	}
	out := make([]workloadAssignment, 0, len(order))
	for _, workload := range order {
		out = append(out, workloadAssignment{workload: workload, cpus: byWorkload[workload]})
	}
	return out
}
