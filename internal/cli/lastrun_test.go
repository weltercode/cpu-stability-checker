package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func recipeSnapshot() *config.Config {
	cfg := config.Default()
	cfg.Memory, cfg.MemoryLock, cfg.RamSeed = "64MiB", "try", 123
	cfg.MaxTemp, cfg.Telemetry = 80, true
	snap := recipeConfig(cfg)
	return &snap
}

func TestLastRunRoundTrip(t *testing.T) {
	control := -1
	seed := int64(7)
	cases := []struct {
		name  string
		in    lastRun
		check func(*testing.T, *lastRun)
	}{
		{"session", lastRun{Kind: lastRunSession, Config: recipeSnapshot(), Session: &SessionOptions{
			Mode: sessionAllCore, PhysicalCores: []int{0, 1, 2}, Workload: "general",
			ThreadsPerCore: 1, Duration: 30 * time.Minute, Passes: 2,
			LogFile: "x.log", PerProcess: true, Rotate: true,
			ControlCore: &control, MaxTemp: 82, Telemetry: true, BeatEvery: "5s",
			Memory: "auto", MemoryLock: "required", RamSeed: 0,
		}}, func(t *testing.T, lr *lastRun) {
			s := lr.Session
			if s.Mode != sessionAllCore || s.Duration != 30*time.Minute || s.Passes != 2 ||
				len(s.PhysicalCores) != 3 || !s.PerProcess || !s.Rotate || s.Workload != "general" {
				t.Errorf("session recipe changed across save/load: %+v", s)
			}
			if s.ControlCore == nil || *s.ControlCore != -1 || s.MaxTemp != 82 || !s.Telemetry || s.BeatEvery != "5s" {
				t.Errorf("session safety settings changed across save/load: %+v", s)
			}
			if s.Memory != "auto" || s.MemoryLock != "required" || s.RamSeed != 0 {
				t.Errorf("session RAM settings changed across save/load: %+v", s)
			}
			if lr.Config == nil || lr.Config.Memory != "64MiB" || lr.Config.RamSeed != 123 {
				t.Errorf("config snapshot lost or altered: %+v", lr.Config)
			}
		}},
		{"scenario", lastRun{Kind: lastRunScenario, Config: recipeSnapshot(), Preset: "mixed-validation",
			Cycles: 3, Seed: &seed, ContinueOnFault: true,
			Phases: []config.Phase{
				{Name: "burst", Burst: &config.Burst{Cores: []int{0, 1}, Repeat: 3, LoadDuration: "10s"}},
				{Name: "soak", Duration: "1h"},
			}}, func(t *testing.T, lr *lastRun) {
			if len(lr.Phases) != 2 || lr.Phases[0].Burst == nil ||
				lr.Phases[0].Burst.Repeat != 3 || lr.Phases[1].Duration != "1h" {
				t.Errorf("scenario recipe changed across save/load: %+v", lr.Phases)
			}
			// A04: the execution parameters are part of the recipe -
			// losing them was losing the experiment.
			if lr.Cycles != 3 || lr.Seed == nil || *lr.Seed != 7 || !lr.ContinueOnFault {
				t.Errorf("scenario execution parameters lost: %+v", lr)
			}
			if lr.Preset != "mixed-validation" {
				t.Errorf("preset provenance lost: %+v", lr)
			}
		}},
		{"handoff", lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: &handoffRecipe{
			CoresInput: "auto", Cores: []int{0, 1, 16}, Blocks: 8, BlockBytes: 65536, BlockSize: "64KiB",
			Rounds: 2, Seed: 7, Lock: "try", Journal: "h.log.journal.jsonl",
			LLCDomainsSeen: 2, LLCByCore: map[int]int{0: 0, 1: 1},
		}}, func(t *testing.T, lr *lastRun) {
			h := lr.Handoff
			if h == nil {
				t.Fatalf("handoff manifest lost across save/load")
			}
			if h.CoresInput != "auto" || len(h.Cores) != 3 || h.Blocks != 8 || h.BlockBytes != 65536 ||
				h.BlockSize != "64KiB" || h.Rounds != 2 || h.Seed != 7 || h.Lock != "try" || h.Journal == "" {
				t.Errorf("handoff manifest changed across save/load: %+v", h)
			}
			if h.LLCDomainsSeen != 2 || len(h.LLCByCore) != 2 || h.LLCByCore[0] != 0 || h.LLCByCore[1] != 1 {
				t.Errorf("resolver provenance lost: %+v", h)
			}
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir := chdirTemp(t)
			if err := saveLastRun(tt.in); err != nil {
				t.Fatalf("saveLastRun: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "lastrun.json")); err != nil {
				t.Fatalf("saveLastRun wrote no file: %v", err)
			}
			got, err := loadLastRun()
			if err != nil {
				t.Fatalf("loadLastRun: %v", err)
			}
			if got.Version != lastRunVersion || got.Kind != tt.in.Kind {
				t.Errorf("recipe envelope wrong: %+v", got)
			}
			tt.check(t, got)
		})
	}
}

func TestLastRunRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not json", "this is not json"},
		{"newer version", `{"version":99,"kind":"scenario","phases":[{"name":"x"}],"config":{}}`},
		{"unknown kind", `{"version":2,"kind":"teleport","config":{}}`},
		{"session missing fields", `{"version":2,"kind":"session","config":{},"session":{"workload":"general"}}`},
		{"scenario without phases", `{"version":2,"kind":"scenario","config":{}}`},
		{"no config snapshot", `{"version":2,"kind":"scenario","phases":[{"name":"x"}]}`},
		{"handoff without a manifest", `{"version":2,"kind":"handoff","config":{}}`},
		{"handoff zero blocks", `{"version":2,"kind":"handoff","config":{},"handoff":{"cores_input":"0,1","cores":[0,1],"blocks":0,"block_bytes":65536,"rounds":1,"lock":"off","journal":"j"}}`},
		{"handoff duplicate cores", `{"version":2,"kind":"handoff","config":{},"handoff":{"cores_input":"1,1","cores":[1,1],"blocks":4,"block_bytes":65536,"rounds":1,"lock":"off","journal":"j"}}`},
		{"handoff invented lock policy", `{"version":2,"kind":"handoff","config":{},"handoff":{"cores_input":"0,1","cores":[0,1],"blocks":4,"block_bytes":65536,"rounds":1,"lock":"maybe","journal":"j"}}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			chdirTemp(t)
			if err := os.WriteFile(lastrunPath, []byte(tt.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if lr, err := loadLastRun(); err == nil {
				t.Fatalf("loadLastRun accepted %v", lr)
			}
		})
	}
}

// A version-1 recipe cannot be replayed faithfully (it lacks cycles,
// seeds, RAM settings and the external snapshot). Rebuilding those
// from today's defaults is the quiet substitution the audit forbids:
// the v1 file must read as "no previous run", with the reason.
func TestLastRunRejectsV1WithReason(t *testing.T) {
	chdirTemp(t)
	v1 := `{"version":1,"kind":"scenario","phases":[{"name":"old"}]}`
	if err := os.WriteFile(lastrunPath, []byte(v1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadLastRun()
	if err == nil {
		t.Fatal("a version-1 recipe must not be replayable")
	}
	if !strings.Contains(err.Error(), "version-1") || !strings.Contains(err.Error(), "faithful") {
		t.Errorf("rejection does not explain why: %v", err)
	}
}

// A failed save must be VISIBLE (the repeat promise is what breaks)
// and must never damage the recipe already on disk.
func TestSaveFailureKeepsOldRecipe(t *testing.T) {
	dir := chdirTemp(t)
	old := lastRun{Kind: lastRunScenario, Config: recipeSnapshot(), Phases: []config.Phase{{Name: "good"}}}
	if err := saveLastRun(old); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	// Point the path into a directory that does not exist: the temp
	// file cannot even be created.
	prev := lastrunPath
	lastrunPath = filepath.Join(dir, "gone", "lastrun.json")
	t.Cleanup(func() { lastrunPath = prev })
	if err := saveLastRun(lastRun{Kind: lastRunScenario, Config: recipeSnapshot(), Phases: []config.Phase{{Name: "new"}}}); err == nil {
		t.Fatal("save into a missing directory reported no error")
	}
	lastrunPath = filepath.Join(dir, "lastrun.json")
	got, err := loadLastRun()
	if err != nil {
		t.Fatalf("the old recipe vanished: %v", err)
	}
	if len(got.Phases) != 1 || got.Phases[0].Name != "good" {
		t.Errorf("failed save damaged the recipe: %+v", got.Phases)
	}
}

// A04 replay: repeat runs the SAVED request - cycles, continue policy
// and the snapshot's RAM settings - without reading config.json and
// without asking anything.
func TestRepeatReplaysSavedRequest(t *testing.T) {
	dir := chdirTemp(t)
	calls := 0
	workload.Register(&workload.Workload{Name: "repeat-fake",
		Description:  "counts its invocations",
		Verification: workload.VerifyRepeated,
		// The workload contract: run until ctx ends. Returning
		// early would honestly be reported as an interrupted
		// unit - not what a clean replay should look like.
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			calls++
			rep.Add(1)
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("repeat-fake")

	cfg := config.Default()
	cfg.LogFile = filepath.Join(dir, "repeat.log")
	snap := recipeConfig(cfg)
	// The saved request deliberately differs from anything on disk:
	// two cycles of one fake phase. config.json here is EMPTY (the
	// temp dir has none): replay must not care.
	lr := lastRun{Kind: lastRunScenario, Config: &snap, Cycles: 2,
		Phases: []config.Phase{{Name: "one", Test: "general", A: "repeat-fake",
			Cores: []int{0}, Duration: "250ms", Workers: 1}}}
	if err := saveLastRun(lr); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	if code := repeatLastRun(); code != 0 {
		t.Fatalf("repeat of a clean saved run = %d, want 0", code)
	}
	if calls != 2 {
		t.Errorf("saved Cycles=2 not honored: workload ran %d time(s)", calls)
	}
	// The replay journaled its runs like any other entry point.
	if _, err := os.Stat(filepath.Join(dir, "repeat.log.journal.jsonl")); err != nil {
		t.Errorf("replay wrote no journal: %v", err)
	}
	// A11: every journaled run carries the experiment identity, and
	// replaying the same saved request reproduces it exactly.
	hashes := journalPlanHashes(t, filepath.Join(dir, "repeat.log.journal.jsonl"))
	if len(hashes) < 2 {
		t.Fatalf("journal has %d run_meta plan_hash(es), want one per cycle", len(hashes))
	}
	for _, h := range hashes[1:] {
		if h != hashes[0] {
			t.Errorf("replay cycles of the same saved request hashed apart: %q vs %q", hashes[0], h)
		}
	}
}

// journalPlanHashes pulls the run_meta plan_hash values out of a
// journal file ("" entries skipped: unknown is not an identity).
func journalPlanHashes(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct {
			Type string `json:"type"`
			Meta *struct {
				PlanHash string `json:"plan_hash"`
			} `json:"meta"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Type != "run_meta" {
			continue
		}
		if rec.Meta != nil && rec.Meta.PlanHash != "" {
			out = append(out, rec.Meta.PlanHash)
		}
	}
	return out
}

// The saved snapshot's resource refusals surface BEFORE load: a recipe
// whose RAM phase can no longer be honored exits 2 without heating
// anything, and it never replaces the recipe with something else.
func TestRepeatResourceGateBeforeLoad(t *testing.T) {
	chdirTemp(t)
	calls := 0
	workload.Register(&workload.Workload{Name: "repeat-ram-fake", RequiresMemory: true,
		Description: "must never run without a budget",
		Run: func(context.Context, workload.Reporter) (uint64, error) {
			calls++
			return 0, nil
		}})
	defer workload.Delete("repeat-ram-fake")

	cfg := config.Default()
	snap := recipeConfig(cfg) // no memory in the snapshot
	lr := lastRun{Kind: lastRunScenario, Config: &snap,
		Phases: []config.Phase{{Name: "ram", Test: "general", A: "repeat-ram-fake",
			Cores: []int{0}, Duration: "10ms", PerProcess: true}}}
	if err := saveLastRun(lr); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	if code := repeatLastRun(); code != 2 {
		t.Errorf("budget-less saved RAM run = %d, want 2 before any load", code)
	}
	if calls != 0 {
		t.Errorf("the gate let the RAM workload run: %d call(s)", calls)
	}
}

// A04 replay: a saved external tool that moved or vanished is refused
// BEFORE load, naming the registered path - not a child dying in
// setup after other phases already heated the machine.
func TestRepeatExternalToolMissingRefusesBeforeLoad(t *testing.T) {
	chdirTemp(t)
	workload.RegisterExternal(map[string]workload.ExternalSpec{
		"ext-repeat-fake": {Command: `C:\does\not\exist\tool.exe`},
	})
	defer workload.Delete("ext-repeat-fake")

	cfg := config.Default()
	cfg.External = map[string]workload.ExternalSpec{"ext-repeat-fake": {Command: `C:\does\not\exist\tool.exe`}}
	snap := recipeConfig(cfg)
	lr := lastRun{Kind: lastRunScenario, Config: &snap,
		Phases: []config.Phase{{Name: "yc", Test: "general", A: "ext-repeat-fake",
			Cores: []int{0}, Duration: "10ms"}}}
	if err := saveLastRun(lr); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	code := repeatLastRun()
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if code != 2 {
		t.Fatalf("repeat with a missing external tool = %d, want 2 before any load:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "ext-repeat-fake") {
		t.Errorf("refusal does not name the external tool:\n%s", buf.String())
	}
	if _, err := os.Stat("cpu-stability.log.journal.jsonl"); !os.IsNotExist(err) {
		t.Errorf("the refused repeat wrote a journal anyway: %v", err)
	}
}

func TestRepeatSessionExternalToolMissing(t *testing.T) {
	chdirTemp(t)
	workload.RegisterExternal(map[string]workload.ExternalSpec{
		"ext-repeat-fake": {Command: `C:\does\not\exist\tool.exe`},
	})
	defer workload.Delete("ext-repeat-fake")

	cfg := config.Default()
	cfg.External = map[string]workload.ExternalSpec{"ext-repeat-fake": {Command: `C:\does\not\exist\tool.exe`}}
	snap := recipeConfig(cfg)
	s := sessionDefaults(cfg)
	s.Workload, s.Duration, s.Passes = "ext-repeat-fake", time.Second, 1
	s.PhysicalCores, s.ThreadsPerCore = []int{0}, 1
	lr := lastRun{Kind: lastRunSession, Config: &snap, Session: &s}
	if err := saveLastRun(lr); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	if code := repeatLastRun(); code != 2 {
		t.Errorf("session repeat with a missing external tool = %d, want 2", code)
	}
}

// The recipe format is JSON on disk: the snapshot must survive the
// marshal with the external specs intact (they are what an external
// phase replays against).
func TestRecipeSnapshotCarriesExternalSpecs(t *testing.T) {
	dir := chdirTemp(t)
	cfg := config.Default()
	cfg.External = map[string]workload.ExternalSpec{"ycruncher-v0.8": {Command: `C:\tools\yc.exe`}}
	snap := recipeConfig(cfg)
	// The live config changes AFTER the snapshot was taken (a later
	// config edit, a wizard write-back): the saved recipe must keep
	// the spec the run actually used.
	cfg.External["ycruncher-v0.8"] = workload.ExternalSpec{Command: `C:\mutated\later.exe`}
	data, err := json.Marshal(lastRun{Kind: lastRunScenario, Config: &snap, Phases: []config.Phase{{Name: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `C:\\tools\\yc.exe`) {
		t.Fatalf("snapshot did not freeze the external spec: %s", data)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var back lastRun
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Config.External["ycruncher-v0.8"].Command != `C:\tools\yc.exe` {
		t.Errorf("external spec lost in round-trip: %+v", back.Config.External)
	}
}

// With no recipe on disk, repeating must explain and return to the menu
// (exit 0), never crash and never invent a run.
func TestRepeatLastRunNoRecipe(t *testing.T) {
	chdirTemp(t)
	if _, err := loadLastRun(); err == nil {
		t.Fatal("missing lastrun.json must be an error")
	}
	if code := repeatLastRun(); code != 0 {
		t.Errorf("repeatLastRun(no recipe) = %d, want 0", code)
	}
}
