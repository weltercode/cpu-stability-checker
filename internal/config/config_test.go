package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// R15 source pin: the silent fallback is not allowed to come back.
// This walks the module's Go sources and refuses any call to a bare
// package-level config.Load - the primitive itself is deleted, and a
// reintroduced silent-defaults path at even one entry point would
// re-open the hole where a broken config.json replaces the user's
// intended test with defaults and the journal then describes a run
// that never was the planned one. .Load( call shapes on other types
// (atomic .Load(), Store.Load()) are method calls and stay allowed.
func TestNoSilentConfigFallbackAnywhere(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := wd
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("no go.mod above the test's working directory")
		}
		root = parent
	}

	var offenders []string
	err = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// The pin's own source quotes the forbidden pattern in its
		// detection rule; skipping this one file is stated, not hidden.
		if strings.HasSuffix(path, "config_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "config.Load(") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", strings.TrimPrefix(path, root+string(os.PathSeparator)), i+1, strings.TrimSpace(line)))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("silent config.Load call sites found (use LoadOrDefault + refusal, or LoadStrict):\n%s",
			strings.Join(offenders, "\n"))
	}
}

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Test != "general" {
		t.Fatalf("default test = %q, want general", cfg.Test)
	}
	if cfg.WorkloadA != "float" || cfg.WorkloadB != "mem" {
		t.Fatalf("default workloads = %q/%q, want float/mem", cfg.WorkloadA, cfg.WorkloadB)
	}
	if cfg.Duration != "30s" {
		t.Fatalf("default duration = %q, want 30s", cfg.Duration)
	}
	if cfg.Workers != 1 {
		t.Fatalf("default workers = %d, want 1", cfg.Workers)
	}
	if !cfg.StopOnFault || !cfg.ShowMetrics {
		t.Fatalf("default flags = stopOnFault:%v showMetrics:%v, want both true", cfg.StopOnFault, cfg.ShowMetrics)
	}
	if cfg.LogFile != "cpu-stability.log" {
		t.Fatalf("default log file = %q", cfg.LogFile)
	}
	if len(cfg.Cores) != 0 {
		t.Fatalf("default cores = %v, want empty (all cores)", cfg.Cores)
	}
}

// R15 shape, part 1: an ABSENT config is defaults with no error - the
// file is a saved preference, not a precondition.
func TestLoadOrDefaultMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-config.json")

	cfg, err := LoadOrDefault(path)
	if err != nil {
		t.Fatalf("missing file must be defaults without error, got %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("config = %+v, want defaults", cfg)
	}

	if _, err := LoadStrict(path); err == nil {
		t.Fatal("LoadStrict: expected error for missing file")
	}
}

// R15 shape, part 2: a BROKEN config is an error, never defaults. The
// old Load returned Default() here, silently discarding the machine's
// intended plan; any caller that then started a run reported a pass
// for a test that never was the planned one. The expectation is
// inverted on purpose (owner queue 2026-11-09): the fallback WAS the
// bug.
func TestLoadOrDefaultBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadOrDefault(path)
	if err == nil {
		t.Fatalf("broken file must refuse, got config %+v", cfg)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("refusal must carry an empty Config, got %+v", cfg)
	}
	// The wrapped message names the file and the parse failure, so
	// every refusal a user sees points at the thing to fix.
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "parse") {
		t.Errorf("refusal must name the file and the parse failure, got %v", err)
	}
	if _, err := LoadStrict(path); err == nil {
		t.Fatal("LoadStrict: expected error for malformed json")
	}
}

// R15 shape, part 3: unreadable-but-present (here: a directory named
// config.json) is also a refusal, not a fallback.
func TestLoadOrDefaultUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultPath)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrDefault(path); err == nil {
		t.Fatal("unreadable config must refuse")
	}
}

func TestLoadKeepsDefaultsForOmittedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)
	if err := os.WriteFile(path, []byte(`{"test":"custom","duration":"1m30s"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}

	if cfg.Test != "custom" {
		t.Fatalf("test = %q, want custom", cfg.Test)
	}
	if cfg.Duration != "1m30s" {
		t.Fatalf("duration = %q, want 1m30s", cfg.Duration)
	}
	cfg.Test, cfg.Duration = "", ""
	base := Default()
	base.Test, base.Duration = "", ""
	if !reflect.DeepEqual(cfg, base) {
		t.Fatalf("omitted keys = %+v, want defaults %+v", cfg, base)
	}
}

func TestLoadIgnoresUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)
	body := `{"test":"general","workers_per_core":4,"future_option":{"enabled":true}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("config = %+v, want defaults", cfg)
	}
}

func TestLoadControlCore(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)
	body := `{"control_core": 7}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if cfg.ControlCore == nil || *cfg.ControlCore != 7 {
		t.Fatalf("ControlCore = %v, want pointer to 7", cfg.ControlCore)
	}

	if err := os.WriteFile(path, []byte(`{"control_core": -1}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err = LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if cfg.ControlCore == nil || *cfg.ControlCore != -1 {
		t.Fatalf("ControlCore = %v, want pointer to -1 (off)", cfg.ControlCore)
	}

	// Omitted means auto, which is a nil pointer, not core 0. The
	// absent-file branch of LoadOrDefault is the honest defaults path.
	cfg, err = LoadOrDefault(DefaultPath + ".does-not-exist")
	if err != nil {
		t.Fatalf("LoadOrDefault(missing): %v", err)
	}
	if cfg.ControlCore != nil {
		t.Errorf("default ControlCore = %v, want nil (auto)", cfg.ControlCore)
	}
}

func TestLoadPhases(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)
	body := `{"phases":[` +
		`{"name":"warm","test":"general","duration":"10s","repeat":2},` +
		`{"test":"isolate","a":"sha256","cores":[0,3],"workers":2,"stop_on_fault":false,"per_process":true},` +
		`{"name":"bursts","burst":{"cores":[2,5],"repeat":3,"load":"int","load_duration":"5s","idle_duration":"1s"}},` +
		`{"name":"rot","test":"general","per_process":true,"rotate":true},` +
		`{"name":"walk","cycle":{"smt":"both","order":"seeded","seed":0,"load":"int","duration":"20s","settle":"1s","cores":[0,9]}}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if len(cfg.Phases) != 5 {
		t.Fatalf("len(Phases) = %d, want 5", len(cfg.Phases))
	}

	ph := cfg.Phases[0]
	if ph.Name != "warm" || ph.Test != "general" || ph.Duration != "10s" || ph.Repeat != 2 {
		t.Errorf("phase 0 = %+v, want name=warm test=general duration=10s repeat=2", ph)
	}
	if ph.A != "" || ph.StopOnFault != nil || ph.PerProcess {
		t.Errorf("phase 0 = %+v, want unset fields to stay unset (nil, false)", ph)
	}

	ph = cfg.Phases[1]
	if ph.Name != "" || ph.A != "sha256" || ph.Workers != 2 || !ph.PerProcess {
		t.Errorf("phase 1 = %+v, want a=sha256 workers=2 per_process=true", ph)
	}
	if ph.StopOnFault == nil || *ph.StopOnFault {
		t.Errorf("phase 1 stop_on_fault = %v, want explicitly false", ph.StopOnFault)
	}
	if !reflect.DeepEqual(ph.Cores, []int{0, 3}) {
		t.Errorf("phase 1 cores = %v, want [0 3]", ph.Cores)
	}

	ph = cfg.Phases[2]
	if ph.Name != "bursts" || ph.Burst == nil {
		t.Fatalf("phase 2 = %+v, want name=bursts with a burst spec", ph)
	}
	b := ph.Burst
	if !reflect.DeepEqual(b.Cores, []int{2, 5}) || b.Repeat != 3 || b.Load != "int" ||
		b.LoadDuration != "5s" || b.IdleDuration != "1s" {
		t.Errorf("burst = %+v, want cores [2 5] repeat 3 load int 5s idle 1s", b)
	}

	ph = cfg.Phases[3]
	if ph.Name != "rot" || !ph.PerProcess || ph.Rotate == nil || !*ph.Rotate {
		t.Errorf("phase 3 = %+v, want per_process with rotate explicitly true", ph)
	}

	ph = cfg.Phases[4]
	if ph.Name != "walk" || ph.Cycle == nil {
		t.Fatalf("phase 4 = %+v, want name=walk with a cycle spec", ph)
	}
	cy := ph.Cycle
	if cy.SMT != "both" || cy.Order != "seeded" || cy.Load != "int" || cy.Duration != "20s" || cy.Settle != "1s" {
		t.Errorf("cycle = %+v, want smt=both order=seeded load=int duration=20s settle=1s", cy)
	}
	// Seed 0 is a valid seed, not "unset": the pointer must survive.
	if cy.Seed == nil || *cy.Seed != 0 {
		t.Errorf("cycle seed = %v, want an explicit 0", cy.Seed)
	}
	if !reflect.DeepEqual(cy.Cores, []int{0, 9}) {
		t.Errorf("cycle cores = %v, want [0 9]", cy.Cores)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultPath)

	want := Default()
	want.Cores = []int{0, 2, 3}
	want.Test = "custom"
	want.WorkloadA = "sha256"
	want.WorkloadB = "int"
	want.Duration = "90s"
	want.Workers = 8
	want.LogFile = "custom.log"
	want.StopOnFault = false
	want.ShowMetrics = false

	if err := want.Save(path); err != nil {
		t.Fatalf("save config: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("saved config is not valid json: %v", err)
	}
	for _, key := range []string{"cores", "test", "workload_a", "workload_b", "duration", "workers", "log_file", "stop_on_fault", "show_metrics"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("saved config is missing field %q", key)
		}
	}

	got, err := LoadStrict(path)
	if err != nil {
		t.Fatalf("LoadStrict: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip config = %+v, want %+v", got, want)
	}
}

func TestSample(t *testing.T) {
	cfg := Sample([]int{1, 5})

	if !reflect.DeepEqual(cfg.Cores, []int{1, 5}) {
		t.Fatalf("sample cores = %v, want [1 5]", cfg.Cores)
	}
	cfg.Cores = nil
	base := Default()
	base.Cores = nil
	if !reflect.DeepEqual(cfg, base) {
		t.Fatalf("sample config = %+v, want defaults %+v", cfg, base)
	}
}

func TestParsedDurationFallback(t *testing.T) {
	tests := []struct {
		name string
		have string
		want time.Duration
	}{
		{name: "empty", have: "", want: 30 * time.Second},
		{name: "garbage", have: "thirty seconds", want: 30 * time.Second},
		{name: "zero", have: "0s", want: 30 * time.Second},
		{name: "negative", have: "-5s", want: 30 * time.Second},
		{name: "seconds", have: "15s", want: 15 * time.Second},
		{name: "minutes", have: "2m", want: 2 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Duration = tt.have
			if got := cfg.ParsedDuration(); got != tt.want {
				t.Fatalf("ParsedDuration() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkersFallback(t *testing.T) {
	tests := []struct {
		name string
		have int
		want int
	}{
		{name: "zero", have: 0, want: 1},
		{name: "negative", have: -4, want: 1},
		{name: "one", have: 1, want: 1},
		{name: "many", have: 64, want: 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Workers = tt.have
			if got := cfg.workers(); got != tt.want {
				t.Fatalf("workers() = %d, want %d", got, tt.want)
			}
		})
	}
}

// The memory-engine budget knob is parsed at config level: empty =
// auto, exact sizes honored, malformed values a visible setup error -
// never a silent fallback that changes what the run will allocate.
func TestMemoryRequest(t *testing.T) {
	c := Config{}
	if r, err := c.MemoryRequest(); err != nil || !r.Auto {
		t.Errorf("empty memory setting should be auto: %+v, %v", r, err)
	}
	c.Memory = "512MiB"
	if r, err := c.MemoryRequest(); err != nil || r.Auto || r.Bytes != 512<<20 {
		t.Errorf(`"512MiB": got %+v, %v; want fixed 512MiB`, r, err)
	}
	c.Memory = "banana"
	if _, err := c.MemoryRequest(); err == nil {
		t.Error("malformed memory setting must be an error, not a silent auto")
	}
}
