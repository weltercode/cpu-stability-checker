package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// chdirTemp switches the working directory to a private temp dir so that the
// fixed config.DefaultPath ("config.json") resolves inside it, and restores
// it when the test finishes.
func chdirTemp(t *testing.T) string {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("restore chdir: %v", err)
		}
	})
	return dir
}

func writeDefaultConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(config.DefaultPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", config.DefaultPath, err)
	}
}

func TestRunConfiguredIOExitCodes(t *testing.T) {
	const validConfig = `{
		"test": "general",
		"workload_a": "float",
		"workload_b": "mem",
		"cores": [],
		"duration": "30s",
		"workers": 1,
		"log_file": "run.log",
		"stop_on_fault": true,
		"show_metrics": false
	}`

	t.Run("missing config file exits 2", func(t *testing.T) {
		chdirTemp(t)

		if code := runConfiguredIO(false, strings.NewReader(""), os.Stdout); code != 2 {
			t.Fatalf("runConfiguredIO() exit code = %d, want 2", code)
		}
	})

	t.Run("invalid config exits 2", func(t *testing.T) {
		chdirTemp(t)
		writeDefaultConfig(t, `{"test": "no-such-test"}`)

		if code := runConfiguredIO(false, strings.NewReader(""), os.Stdout); code != 2 {
			t.Fatalf("runConfiguredIO() exit code = %d, want 2", code)
		}
	})

	t.Run("plan without confirmation exits 0", func(t *testing.T) {
		chdirTemp(t)
		if _, err := sysinfo.DiscoverTopology(); err != nil {
			t.Skipf("topology discovery unavailable: %v", err)
		}
		writeDefaultConfig(t, validConfig)

		var out strings.Builder
		if code := runConfiguredIO(false, strings.NewReader(""), &out); code != 0 {
			t.Fatalf("runConfiguredIO() exit code = %d, want 0", code)
		}
		if out.Len() == 0 {
			t.Fatal("runConfiguredIO() printed no run plan, want a plan")
		}
	})

	t.Run("declining confirmation exits 0 without running", func(t *testing.T) {
		chdirTemp(t)
		if _, err := sysinfo.DiscoverTopology(); err != nil {
			t.Skipf("topology discovery unavailable: %v", err)
		}
		writeDefaultConfig(t, validConfig)

		var out strings.Builder
		if code := runConfiguredIO(true, strings.NewReader("n\n"), &out); code != 0 {
			t.Fatalf("runConfiguredIO() exit code = %d, want 0", code)
		}
		if _, err := os.Stat("run.log"); !os.IsNotExist(err) {
			t.Fatalf("cancelled run created %s, want no log file", "run.log")
		}
	})
}

func TestRunCmdExitCodes(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})
	tests := []struct {
		name   string
		args   []string
		want   int
		config string
	}{
		{name: "unknown test", args: []string{"-test", "no-such-test"}, want: 2},
		{name: "empty test", args: []string{"-test", ""}, want: 2},
		{name: "unknown workload A", args: []string{"-test", "general", "-a", "no-such-workload"}, want: 2},
		{name: "unknown workload B", args: []string{"-test", "abab", "-a", "int", "-b", "no-such-workload"}, want: 2},
		{name: "garbage in -cores", args: []string{"-test", "general", "-cores", "0,x"}, want: 2},
		{name: "core out of range", args: []string{"-test", "general", "-cores", "99999"}, want: 2},
		// Auto now selects subprocesses for bursts. Only an explicit
		// threaded override is a validation error; the old auto case
		// accidentally launched this test binary recursively.
		{name: "-bursts with threaded override", args: []string{"-test", "general", "-bursts", "2"}, config: `{"execution":"threaded"}`, want: 2},
		{name: "-bursts with -rotate", args: []string{"-test", "general", "-bursts", "2", "-per-process", "-rotate"}, want: 2},
		{name: "garbage in -burst-run", args: []string{"-test", "general", "-bursts", "2", "-per-process", "-burst-run", "nope"}, want: 2},
		{name: "garbage in -burst-gap", args: []string{"-test", "general", "-bursts", "2", "-per-process", "-burst-gap", "nope"}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chdirTemp(t)
			if tt.config != "" {
				writeDefaultConfig(t, tt.config)
			}

			if code := runCmd(tt.args); code != tt.want {
				t.Fatalf("runCmd(%v) exit code = %d, want %d", tt.args, code, tt.want)
			}
		})
	}

	t.Run("unopenable log file exits 2", func(t *testing.T) {
		chdirTemp(t)

		args := []string{"-test", "general", "-a", "float", "-duration", "100ms",
			"-log", filepath.Join("no-such-dir", "run.log")}
		if code := runCmd(args); code != 2 {
			t.Fatalf("runCmd(%v) exit code = %d, want 2", args, code)
		}
	})

	// "idle" is a legal slot value through config, so the -a/-b
	// flags must accept it too: this run must pass validation and
	// finish, not exit 2 with "unknown workload". The duration is
	// generous on purpose: the full suite runs packages in parallel
	// and a saturated machine can starve a 100ms run to zero
	// checks - and zero confirmed checks honestly exits 1 (never a
	// fake "verified"). The test is about idle passing validation,
	// not about the machine being idle.
	t.Run("-b idle passes validation", func(t *testing.T) {
		chdirTemp(t)

		args := []string{"-test", "abab", "-a", "int", "-b", "idle", "-duration", "500ms",
			"-cores", "0,1", "-log", "run.log"}
		if code := runCmd(args); code != 0 {
			t.Fatalf("runCmd(%v) exit code = %d, want 0 (idle is a legal slot value)", args, code)
		}
	})

	t.Run("short successful run exits 0", func(t *testing.T) {
		dir := chdirTemp(t)

		// Long enough that the worker certainly starts even when the
		// rest of the test suite is hammering every core: a run whose
		// mandatory slot never started must exit 1 (incomplete
		// mandatory check), so a too-short "success" test is a lie.
		args := []string{"-test", "general", "-a", "float", "-duration", "1s",
			"-cores", "0", "-control", "-1", "-log", filepath.Join(dir, "ok.log")}
		if code := runCmd(args); code != 0 {
			t.Fatalf("runCmd(%v) exit code = %d, want 0", args, code)
		}
		if _, err := os.Stat(filepath.Join(dir, "ok.log")); err != nil {
			t.Fatalf("run did not create the log file: %v", err)
		}
	})
}

func TestConfigCmdExitCodes(t *testing.T) {
	t.Run("config init writes the file and exits 0", func(t *testing.T) {
		dir := chdirTemp(t)

		if code := configCmd([]string{"init"}); code != 0 {
			t.Fatalf("configCmd(init) exit code = %d, want 0", code)
		}
		cfg, err := config.LoadStrict(config.DefaultPath)
		if err != nil {
			t.Fatalf("config init did not write a valid config: %v", err)
		}
		if _, ok := plan.GetTest(cfg.Test); !ok {
			t.Fatalf("config init wrote unknown test %q", cfg.Test)
		}
		// A06 portability: the sample is written on the user's
		// machine but must never bake in machine-specific paths -
		// real tool registration stays an explicit tools detect or
		// install act (or a manual edit), never a guessed default,
		// and a template copied between machines stays valid. The
		// RAW bytes are checked, not the parsed view: a drive path
		// baked into the template is visible in the file itself.
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsRune(string(raw), '\\') {
			t.Errorf("config init wrote a backslash path into the sample:\n%s", raw)
		}
		doc := readConfigDoc(t, dir)
		if v, present := doc["external"]; present {
			if m, ok := v.(map[string]any); !ok || len(m) != 0 {
				t.Errorf("config init invented external entries: %v", v)
			}
		}
		if v, present := doc["co_tool"]; present && v != "" {
			t.Errorf("config init invented a co_tool path: %v", v)
		}
	})

	t.Run("config show without a file exits 0", func(t *testing.T) {
		chdirTemp(t)

		if code := configCmd(nil); code != 0 {
			t.Fatalf("configCmd() exit code = %d, want 0", code)
		}
	})
}

func TestJournalCmdExitCodes(t *testing.T) {
	t.Run("missing journal file exits 2", func(t *testing.T) {
		dir := chdirTemp(t)

		if code := journalCmd([]string{filepath.Join(dir, "missing.jsonl")}); code != 2 {
			t.Fatalf("journalCmd() exit code = %d, want 2", code)
		}
	})

	t.Run("empty journal exits 0", func(t *testing.T) {
		dir := chdirTemp(t)
		path := filepath.Join(dir, "journal.jsonl")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("write journal: %v", err)
		}

		if code := journalCmd([]string{path}); code != 0 {
			t.Fatalf("journalCmd() exit code = %d, want 0", code)
		}
	})

	t.Run("valid journal exits 0", func(t *testing.T) {
		dir := chdirTemp(t)
		path := filepath.Join(dir, "journal.jsonl")
		f, err := journal.OpenFile(path)
		if err != nil {
			t.Fatalf("open journal: %v", err)
		}
		jw := journal.NewWriter(f, "run-1")
		jw.RunStart("general", "", nil, 1, "1s", false)
		jw.CoreStart(0, "float")
		jw.CoreDone(0, journal.OK, "")
		jw.RunEnd(0)
		f.Close()

		if code := journalCmd([]string{path}); code != 0 {
			t.Fatalf("journalCmd() exit code = %d, want 0", code)
		}
	})
}

func TestReportCmdExitCodes(t *testing.T) {
	t.Run("missing journal file exits 2", func(t *testing.T) {
		dir := chdirTemp(t)

		if code := reportCmd([]string{filepath.Join(dir, "missing.jsonl")}); code != 2 {
			t.Fatalf("reportCmd() exit code = %d, want 2", code)
		}
	})

	t.Run("a journal with faults still exits 0 (reporting is read-only)", func(t *testing.T) {
		dir := chdirTemp(t)
		path := filepath.Join(dir, "journal.jsonl")
		f, err := journal.OpenFile(path)
		if err != nil {
			t.Fatalf("open journal: %v", err)
		}
		jw := journal.NewWriter(f, "run-1")
		jw.RunStart("general", "heavy", []int{4, 5}, 1, "1s", false)
		jw.CoreStart(0, "int")
		jw.CoreDone(0, journal.OK, "")
		jw.CoreStart(1, "int")
		jw.CoreDone(1, journal.Fault, "mismatch at iter 42")
		jw.RunEnd(1)
		f.Close()

		if code := reportCmd([]string{path}); code != 0 {
			t.Fatalf("reportCmd() exit code = %d, want 0", code)
		}
	})
}
