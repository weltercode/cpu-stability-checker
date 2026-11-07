package cli

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestSessionSettingsSurviveRecipeAndReachExecutor(t *testing.T) {
	for _, control := range []int{-1, 1} {
		if control >= runtime.NumCPU() {
			continue
		}
		wantControl := control
		cfg := config.Default()
		cfg.ControlCore, cfg.MaxTemp, cfg.Telemetry, cfg.BeatEvery = &control, 83, true, "7s"
		s := sessionDefaults(cfg)
		s.Workload, s.Duration, s.PerProcess, s.Rotate = "int", time.Second, true, true
		// The same JSON envelope is persisted by saveLastRun and read on repeat.
		data, err := json.Marshal(lastRun{Version: lastRunVersion, Kind: lastRunSession, Session: &s})
		if err != nil {
			t.Fatal(err)
		}
		var saved lastRun
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		*cfg.ControlCore, cfg.MaxTemp, cfg.Telemetry, cfg.BeatEvery = 0, 0, false, ""
		opt, err := sessionRunOptions(*saved.Session, sessionRun{logicalCPUs: []int{0}}, "test.log")
		if err != nil {
			t.Fatal(err)
		}
		if opt.MaxTempC != 83 || !opt.Telemetry || opt.BeatEvery != 7*time.Second ||
			opt.ControlCore != wantControl || *s.ControlCore != wantControl || !opt.PerProcess || !opt.Rotate || !opt.StopOnFault {
			t.Fatalf("saved settings lost on dispatch: %+v", opt)
		}
	}
}

// The headroom policy travels exactly like the control core itself:
// what the config said at save time is what the dispatch sees, even
// if config.json changed since, and it only combines with AUTO control
// (explicit control_core in a saved session alongside the consent is a
// contradiction the plan gate names).
func TestSessionReserveControlSurvivesRecipe(t *testing.T) {
	num := runtime.NumCPU()
	cfg := config.Default()
	cfg.ReserveControl = "logical"
	s := sessionDefaults(cfg)
	s.Workload, s.Duration, s.PerProcess = "int", time.Second, true
	if s.ReserveControl != "logical" {
		t.Fatalf("sessionDefaults lost the reserve policy: %+v", s)
	}
	data, err := json.Marshal(lastRun{Version: lastRunVersion, Kind: lastRunSession, Session: &s})
	if err != nil {
		t.Fatal(err)
	}
	var saved lastRun
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Session.ReserveControl != "logical" {
		t.Fatalf("the saved recipe lost the reserve policy: %+v", saved.Session)
	}
	cfg.ReserveControl = "" // a later config edit must not reach the saved request

	all := make([]int, num)
	for i := range all {
		all[i] = i
	}
	opt, err := sessionRunOptions(*saved.Session, sessionRun{logicalCPUs: all}, "test.log")
	if err != nil {
		t.Fatal(err)
	}
	if opt.ControlCore != num-1 || len(opt.Cores) != num-1 || len(opt.ReservedCPUs) != 1 {
		t.Errorf("saved consent did not reach the executor: control %d, %d cores, reserved %v", opt.ControlCore, len(opt.Cores), opt.ReservedCPUs)
	}

	saved.Session.ControlCore = func() *int { v := -1; return &v }()
	if _, err := sessionRunOptions(*saved.Session, sessionRun{logicalCPUs: []int{0}}, "test.log"); err == nil || !strings.Contains(err.Error(), "needs auto control") {
		t.Errorf("explicit control_core with the consent passed the gate: %v", err)
	}
}

func TestSessionRAMSettingsSurviveRecipeAndReachExecutor(t *testing.T) {
	// Audit A03: the memory budget, lock policy and ram-* seed are
	// part of the session, not of whatever config.json says later.
	// They must survive the recipe round-trip and land in the
	// executor's options - including the valid-but-zero seed.
	for _, tc := range []struct {
		name, memory, lock string
		seed               uint64
	}{
		{"auto budget", "auto", "try", 0},
		{"fixed budget with seed", "64MiB", "required", 123},
		{"seed zero is a seed", "2GiB", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Memory, cfg.MemoryLock, cfg.RamSeed = tc.memory, tc.lock, tc.seed
			s := sessionDefaults(cfg)
			s.Workload, s.Duration, s.PerProcess = "int", time.Second, true
			data, err := json.Marshal(lastRun{Version: lastRunVersion, Kind: lastRunSession, Session: &s})
			if err != nil {
				t.Fatal(err)
			}
			var saved lastRun
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			// config.json drifting afterwards must not change the run.
			opt, err := sessionRunOptions(*saved.Session, sessionRun{logicalCPUs: []int{0}}, "test.log")
			if err != nil {
				t.Fatal(err)
			}
			if opt.Memory != tc.memory || opt.MemLock != tc.lock || opt.RamSeed != tc.seed {
				t.Fatalf("RAM settings lost on dispatch: %+v", opt)
			}
		})
	}

	t.Run("ram workload reaches preflight with its budget", func(t *testing.T) {
		cfg := config.Default()
		cfg.Memory, cfg.RamSeed = "64MiB", 7
		s := sessionDefaults(cfg)
		s.Workload, s.Duration, s.PerProcess = "ram-constant", time.Second, true
		opt, err := sessionRunOptions(s, sessionRun{logicalCPUs: []int{0}}, "test.log")
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.PreflightMemoryWorkloads(opt); err != nil {
			t.Fatalf("configured budget did not reach the preflight: %v", err)
		}
	})

	t.Run("ram workload without budget is refused before load", func(t *testing.T) {
		s := sessionDefaults(config.Default()) // no memory in the default config
		s.Workload, s.Duration, s.PerProcess = "ram-constant", time.Second, true
		opt, err := sessionRunOptions(s, sessionRun{logicalCPUs: []int{0}}, "test.log")
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.PreflightMemoryWorkloads(opt); err == nil {
			t.Fatal("memory-less ram workload accepted: the quiet reduction A03 forbids")
		}
	})
}

func TestSessionRejectsInvalidSafetySettings(t *testing.T) {
	for _, name := range []string{"temperature", "beat", "control collision"} {
		t.Run(name, func(t *testing.T) {
			s := sessionDefaults(config.Default())
			s.Workload, s.Duration = "int", time.Second
			switch name {
			case "temperature":
				s.MaxTemp = -1
			case "beat":
				s.BeatEvery = "bad"
			case "control collision":
				core := 0
				s.ControlCore = &core
			}
			if _, err := sessionRunOptions(s, sessionRun{logicalCPUs: []int{0}}, "test.log"); err == nil {
				t.Fatal("invalid safety setting accepted")
			}
		})
	}
}

func TestScenarioSetupErrorNeverContinuesOrBlacklists(t *testing.T) {
	for _, faults := range []map[int]string{nil, {0: "another worker faulted"}} {
		calls := 0
		exec := func(plan.Options) plan.Result {
			calls++
			return plan.Result{Exit: 2, Aborted: true, Faults: faults}
		}
		blacklist := map[int]bool{}
		_, ran, stopped, _ := runCycle([]plan.Options{{Cores: []int{0}}, {Cores: []int{0}}}, &bytes.Buffer{}, exec, true, blacklist)
		if calls != 1 || stopped != 0 || ran[1] || len(blacklist) != 0 {
			t.Fatalf("setup error continued/blacklisted: calls=%d stopped=%d ran=%v blacklist=%v", calls, stopped, ran, blacklist)
		}
	}
}

func TestReportJSONPreservesIncompleteOutcomes(t *testing.T) {
	sum := journal.Summary{Runs: 1, Cores: []journal.CoreSummary{{Core: 2, Runs: 3,
		Interrupted: 1, Skipped: 1, SetupErrors: 1, Exposure: 125 * time.Millisecond}}}
	data, err := json.Marshal(summaryToJSON("test.jsonl", sum, nil, &journal.Issues{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var decoded reportJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	c := decoded.Cores[0]
	if c.Interrupted != 1 || c.Skipped != 1 || c.SetupErrors != 1 || c.ExposureMS != 125 || c.Faults != 0 {
		t.Fatalf("incomplete result lost in JSON: %s", data)
	}
}
