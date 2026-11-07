package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// lastrun.json records what the tool ran most recently. A machine that
// crashes mid-test comes back to a menu that can replay the exact same
// settings without a single question — the crash-loop workflow this tool
// exists for. A test run is the moment to record, so we save before the
// first load is applied, not after a verdict that may never be printed.
//
// Version 2 (audit A04) is the FULL request, not a hint: the resolved
// config snapshot (memory budget/lock/seed, temperature, control core,
// telemetry, external tool specs, log path), the phase list, and the
// scenario execution parameters (cycles, shuffle seed, continue policy).
// Repeat reads every one of those from here — never from whatever
// config.json happens to say afterwards — so "repeat" means "the same
// experiment", not "something rebuilt from current defaults".

// lastrunPath is a var so tests can point it at a scratch directory.
var lastrunPath = "lastrun.json"

const (
	lastRunVersion  = 2
	lastRunSession  = "session"
	lastRunScenario = "scenario"
	lastRunHandoff  = "handoff"
)

// lastRun is the durable recipe: either an interactive session's choices
// or a scenario's phase list (generators included — replay re-expands
// them against the current topology), together with the resolved-config
// snapshot both resolve against and the execution parameters the entry
// point used.
type lastRun struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	// Config is the snapshot of the resolved defaults the plan was
	// compiled against. Phases inside it are always nil: the
	// kind-specific fields below carry the actual plan.
	Config  *config.Config  `json:"config,omitempty"`
	Session *SessionOptions `json:"session,omitempty"`
	// Handoff is the durable manifest of one P07e handoff run
	// (S2): what was asked, what the resolver actually chose, and
	// the exact block geometry. nil for every other kind.
	Handoff *handoffRecipe `json:"handoff,omitempty"`
	Phases  []config.Phase `json:"phases,omitempty"`
	// Scenario execution parameters: without these a repeat silently
	// ran one cycle in configured order with stop-on-fault, no matter
	// what the command line had asked for (audit A04).
	Cycles          int    `json:"cycles,omitempty"`            // whole-list repetitions (0/1 = once)
	Seed            *int64 `json:"seed,omitempty"`              // phase-order shuffle seed; nil = configured order
	ContinueOnFault bool   `json:"continue_on_fault,omitempty"` // scenario-wide: keep later phases, blacklist faulted cores
	// Preset records which built-in preset produced the phases
	// (provenance for the report; the phases themselves are what
	// replays, so a preset rename can never change a repeat).
	Preset string `json:"preset,omitempty"`
	// RecipeID stamps each saved request (C3): the outcome receipt
	// binds to this id, so a result produced for a recipe that was
	// later displaced can never be shown as THIS recipe's outcome.
	// "" = the recipe predates the field (unknown, never assumed).
	RecipeID string `json:"recipe_id,omitempty"`
	// ProfileID/ProfileVersion record which catalog profile this
	// request came from, where the entry point knew it (wizard with
	// an unambiguous card, preset with a card). Raw CLI runs and
	// pre-catalog recipes: "" = unknown. This is metadata, not
	// identity: planHash deliberately ignores it - the compiled
	// request is the experiment, the profile label is provenance.
	ProfileID      string `json:"profile_id,omitempty"`
	ProfileVersion string `json:"profile_version,omitempty"`
}

// recipeConfig snapshots the resolved defaults a recipe resolves
// against. The copy is deliberate: the request is immutable from the
// moment it is saved, and nothing that later mutates the live config
// (a wizard write-back, a config edit) may reach it.
func recipeConfig(cfg config.Config) config.Config {
	snap := cfg
	snap.Phases = nil
	snap.Cores = append([]int(nil), cfg.Cores...)
	if cfg.External != nil {
		snap.External = make(map[string]workload.ExternalSpec, len(cfg.External))
		for name, spec := range cfg.External {
			snap.External[name] = spec
		}
	}
	return snap
}

// newRecipeID stamps a saved request: second-resolution UTC time plus
// 8 random hex bytes. A collision would need the same second AND the
// same 64 random bits - and its only consequence would be one recipe's
// receipt being readable as another's; the receipts themselves stay
// single latest-writer files. If the random source ever fails the id
// stays empty: an unbound recipe means "outcome unknown", never a
// guessed binding.
func newRecipeID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// sessionRecipe is the wizard's save site (C3): the session choices
// plus the catalog profile they map back to, when the mapping is
// unique. The save itself stays in interactiveSession.
func sessionRecipe(cfg config.Config, session SessionOptions) lastRun {
	snap := recipeConfig(cfg)
	lr := lastRun{Kind: lastRunSession, Config: &snap, Session: &session, RecipeID: newRecipeID()}
	if p := profileForSession(session.Mode, session.Workload); p.ID != "" {
		lr.ProfileID, lr.ProfileVersion = p.ID, p.Version
	}
	return lr
}

// handoffRecipe is the durable manifest of one handoff run (P07e S2):
// what the user asked for (CoresInput), what the resolver actually
// produced (Cores - the effective set the run pinned), and the exact
// block geometry with seeds. The resolver's cpu-set facts are stored
// as PROVENANCE explaining the choice, never as replay input: a
// future replay that sees a different machine says so - it never
// re-picks cores behind the recorded experiment's back (S3 opens the
// replay slot on top of these fields).
type handoffRecipe struct {
	CoresInput string `json:"cores_input"` // as asked: "auto" or an explicit list
	Cores      []int  `json:"cores"`       // resolved effective set (what actually ran)
	Blocks     int    `json:"blocks"`
	BlockBytes uint64 `json:"block_bytes"`
	BlockSize  string `json:"block_size,omitempty"` // size as typed (honest echo)
	Rounds     int    `json:"rounds"`
	Seed       uint64 `json:"seed"`
	Lock       string `json:"lock"`
	Journal    string `json:"journal"` // the journal FILE the evidence went to
	// Resolver provenance, honestly absent (0/nil) when the core list
	// was explicit and the cpu-set view was never consulted:
	LLCDomainsSeen int         `json:"llc_domains_seen,omitempty"`
	LLCByCore      map[int]int `json:"llc_by_core,omitempty"` // selected core -> its llc domain at resolve time
}

// handoffRecipeOf is the handoff save site: the manifest built from
// the resolved inputs, immutable the moment it is saved, like every
// other recipe.
func handoffRecipeOf(cfg config.Config, coresInput string, res handoffCoreResolution, blocks int, blockSizeText string, blockBytes uint64, rounds int, seed uint64, lock, journalFile string) lastRun {
	snap := recipeConfig(cfg)
	return lastRun{Kind: lastRunHandoff, Config: &snap, RecipeID: newRecipeID(), Handoff: &handoffRecipe{
		CoresInput:     coresInput,
		Cores:          append([]int(nil), res.Cores...),
		Blocks:         blocks,
		BlockBytes:     blockBytes,
		BlockSize:      blockSizeText,
		Rounds:         rounds,
		Seed:           seed,
		Lock:           lock,
		Journal:        journalFile,
		LLCDomainsSeen: res.DomainsSeen,
		LLCByCore:      res.LLCByCore,
	}}
}

// saveLastRun writes the recipe atomically: a unique temp file, a full
// write, an explicit Sync (the crash must not eat the promise the save
// exists for) and a rename that replaces the old recipe in one step
// (Windows MoveFileEx REPLACE_EXISTING - never a delete-then-write gap).
//
// A failed save is NOT fatal - the run itself is still a valid test -
// but it is said out loud: the user must not expect a repeat of THIS
// test when the disk still holds the old one (audit A04: no promise
// may rest on a recipe that was never written).
func saveLastRun(lr lastRun) error {
	lr.Version = lastRunVersion
	if lr.RecipeID == "" {
		lr.RecipeID = newRecipeID()
	}
	b, err := json.MarshalIndent(lr, "", "  ")
	if err != nil {
		return err
	}
	dir, base := filepath.Split(lastrunPath)
	tmp, err := os.CreateTemp(dir, base+".tmp-*") // unique: two entry points in one directory cannot clobber each other
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, lastrunPath); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// noteSaveFailure states the repeat promise that just broke. The run
// continues; only the "press 2 after a crash" guarantee is gone, and
// saying so is the difference between a lost convenience and a silent
// lie about what the next repeat replays.
func noteSaveFailure(err error) {
	fmt.Fprintf(os.Stderr, "warning: this run will NOT be repeatable: cannot write %s: %v\n", lastrunPath, err)
}

// loadLastRun reads and validates the recipe. Anything incomplete,
// corrupt or from another era is reported as an error so the caller can
// say "no previous run" rather than launch something half-understood.
// A version-1 recipe is deliberately NOT migrated: it lacks the fields
// a faithful replay needs (cycles, seeds, RAM settings, external
// specs), and rebuilding them from today's defaults is exactly the
// quiet substitution this tool forbids.
func loadLastRun() (*lastRun, error) {
	b, err := os.ReadFile(lastrunPath)
	if err != nil {
		return nil, err
	}
	var lr lastRun
	if err := json.Unmarshal(b, &lr); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", lastrunPath, err)
	}
	if err := validateRecipe(&lr); err != nil {
		return nil, fmt.Errorf("%s: %w", lastrunPath, err)
	}
	return &lr, nil
}

// validateRecipe checks the version and per-kind shape that decide
// whether a faithful replay is possible. Shared by the slot reader
// and the strict headless request reader (G01): the run and the
// request contract can never drift apart on what "complete" means.
func validateRecipe(lr *lastRun) error {
	if lr.Version != lastRunVersion {
		if lr.Version < lastRunVersion {
			return fmt.Errorf("version-%d recipe: it lacks the parameters a faithful replay needs (cycles, seeds, RAM and external specs); configure a new run to replace it", lr.Version)
		}
		return fmt.Errorf("written by a newer tool (version %d)", lr.Version)
	}
	if lr.Config == nil {
		return fmt.Errorf("holds no resolved-config snapshot: replay would have to invent defaults")
	}
	switch lr.Kind {
	case lastRunSession:
		s := lr.Session
		if s == nil || s.Duration <= 0 || s.Passes <= 0 ||
			len(s.PhysicalCores) == 0 || s.Workload == "" {
			return fmt.Errorf("holds an incomplete session recipe")
		}
	case lastRunScenario:
		if len(lr.Phases) == 0 {
			return fmt.Errorf("holds no phases")
		}
	case lastRunHandoff:
		// Shape checks only: the manifest must be able to describe an
		// executed handoff. Machine validity (do these cpus exist
		// today, does the cpu-set view still agree) belongs to the
		// replay gate, not to the reader - a manifest is portable
		// evidence, and refusing to READ it would hide what ran.
		h := lr.Handoff
		if h == nil || h.CoresInput == "" || len(h.Cores) < 2 || h.Blocks <= 0 ||
			h.BlockBytes == 0 || h.Rounds <= 0 || h.Lock == "" || h.Journal == "" {
			return fmt.Errorf("holds an incomplete handoff manifest")
		}
		seen := map[int]bool{}
		for _, c := range h.Cores {
			if c < 0 || seen[c] {
				return fmt.Errorf("handoff cores %v are not a unique non-negative set", h.Cores)
			}
			seen[c] = true
		}
		switch h.Lock {
		case "off", "try", "required":
		default:
			return fmt.Errorf("handoff lock policy %q is not off, try or required", h.Lock)
		}
	default:
		return fmt.Errorf("has unknown kind %q", lr.Kind)
	}
	return nil
}

// repeatLastRun replays the most recent run with no questions: after a
// crash the user presses 2 and the same test is under load again. The
// one summary line before starting is what makes "the same" verifiable
// at a glance.
func repeatLastRun() int {
	return repeatLastRunWithUI(nil)
}

func repeatLastRunWithUI(ui *screenUI) int {
	lr, err := loadLastRun()
	if err != nil {
		fmt.Printf("No previous run to repeat (%v).\n", err)
		fmt.Println("Use 'Configure and start a test' once; the menu remembers it here afterwards.")
		return 0
	}
	return runLastRun(lr, ui)
}

// runLastRun executes an already-loaded request. The request is the
// in-memory object the caller validated: the repeat slot is a durable
// promise for the NEXT crash, never the transport for THIS run. A
// slot that failed to save, was replaced, or lies damaged cannot
// substitute a different workload (audit B06) - what runs is exactly
// what was asked for, or nothing.
func runLastRun(lr *lastRun, ui *screenUI) int {
	// The external tool specs travel inside the recipe: register them
	// before anything resolves workload names, so an external phase
	// repeats against the SAME spec that was saved, not against
	// whatever config.json registers today.
	workload.RegisterExternal(lr.Config.External)
	if lr.Kind == lastRunHandoff {
		// S6: the menu repeat of a handoff manifest is a real
		// settings-repeat through runHandoffTracked - the recorded
		// geometry, seed and journal, a NEW attempt under its own
		// session identity, through the single dispatch. Machine or
		// topology drift refuses there by name (never a re-pick), so
		// the recorded experiment is never approximated with today's
		// defaults.
		describeHandoff(lr)
		describeLastOutcome(lr)
		return runHandoffTracked(lr, ui)
	}
	if lr.Kind == lastRunSession {
		// A saved external workload whose tool moved or vanished is a
		// clear refusal BEFORE load, naming the path (audit A04): the
		// child would otherwise die in setup after the menu promised a
		// replay of the exact experiment.
		if msg := externalToolMissing(lr.Config.External, lr.Session.Workload); msg != "" {
			fmt.Fprintf(os.Stderr, "repeat: %s\n", msg)
			return 2
		}
		topology, err := sysinfo.DiscoverTopology()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot discover CPU topology: %v\n", err)
			return 2
		}
		describeSession(lr)
		describeLastOutcome(lr)
		return runSessionWithPanel(*lr.Session, topology, ui.monitor(), lr.Config.External, lr.RecipeID)
	}
	describeScenario(lr)
	describeLastOutcome(lr)
	return repeatScenarioWithUI(lr, ui)
}

// repeatScenarioWithUI replays the saved request exactly: the config
// snapshot (not config.json), the saved phase list, the saved cycles,
// shuffle seed and continue policy. The compile pipeline is the same
// one the 'scenario' command uses, so replay cannot drift from run.
func repeatScenarioWithUI(lr *lastRun, ui *screenUI) int {
	cfg := *lr.Config
	cfg.Phases = lr.Phases
	opts, notices, err := compileScenario(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario: %v\n", err)
		return 2
	}
	for _, n := range notices {
		fmt.Println(n)
	}
	// Resource refusals surface BEFORE load, from the saved snapshot -
	// a machine that can no longer honor the plan says so instead of
	// heating cores on the way to a mid-scenario exit 2.
	if errs := scenarioMemoryRefusals(cfg.External, opts); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "scenario: %s\n", e)
		}
		return 2
	}
	cycles := lr.Cycles
	if cycles < 1 {
		cycles = 1
	}
	seed := int64(-1)
	if lr.Seed != nil {
		seed = *lr.Seed
	}
	// The replay's plan_hash matches the original run's: same saved
	// request, same resolved plan. A report comparing the two sees
	// "same experiment" - and any drift (edited recipe, moved tool)
	// shows up as a hash difference, never as a silent match.
	return runScenarioRequest(*lr, opts, cycles, seed, lr.ContinueOnFault, ui, false)
}

func describeSession(lr *lastRun) {
	s := lr.Session
	exec := "in-process"
	if s.PerProcess {
		exec = "subprocess"
		if s.Rotate {
			exec = "rotating subprocess"
		}
	}
	profile := ""
	if lr.ProfileID != "" {
		profile = fmt.Sprintf(" · profile %s %s", lr.ProfileID, lr.ProfileVersion)
	}
	fmt.Printf("Repeating last run:%s %s · %s · physical cores %s · %d thread(s)/core · %s ×%d pass(es) · %s\n",
		profile, s.Mode, s.Workload, human.Ints(s.PhysicalCores), s.ThreadsPerCore,
		human.Dur(s.Duration), s.Passes, exec)
}

func describeScenario(lr *lastRun) {
	label := "config defaults"
	if lr.Phases[0].Name != "" {
		label = lr.Phases[0].Name
	}
	cycles := lr.Cycles
	if cycles < 1 {
		cycles = 1
	}
	extra := ""
	if lr.ProfileID != "" {
		extra += fmt.Sprintf(" · profile %s %s", lr.ProfileID, lr.ProfileVersion)
	}
	if lr.Preset != "" {
		extra += " · preset " + lr.Preset
	}
	if lr.Seed != nil {
		extra += fmt.Sprintf(" · shuffled seed %d", *lr.Seed)
	}
	if lr.ContinueOnFault {
		extra += " · continue after faults (blacklist)"
	}
	fmt.Printf("Repeating last scenario: %d phase(s), %d cycle(s), first: %s%s\n", len(lr.Phases), cycles, label, extra)
}
