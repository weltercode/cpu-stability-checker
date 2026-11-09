package plan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func TestOptionsFromConfig(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Test = "abab"
		cfg.Cores = []int{0, 1}
		cfg.WorkloadA = "float"
		cfg.WorkloadB = "mem"
		cfg.Duration = "5s"
		cfg.Workers = 2
		cfg.StopOnFault = true
		cfg.LogFile = "custom.log"
		cfg.ShowMetrics = false
		return cfg
	}

	t.Run("maps config to run options", func(t *testing.T) {
		cfg := base()

		opt, err := OptionsFromConfig(cfg)
		if err != nil {
			t.Fatalf("OptionsFromConfig() error = %v", err)
		}
		if opt.TestName != cfg.Test || opt.A != cfg.WorkloadA || opt.B != cfg.WorkloadB {
			t.Fatalf("OptionsFromConfig() = %+v, want test=%q a=%q b=%q", opt, cfg.Test, cfg.WorkloadA, cfg.WorkloadB)
		}
		if opt.Duration != 5*time.Second || opt.Workers != 2 || !opt.StopOnFault || opt.ShowMetrics {
			t.Fatalf("OptionsFromConfig() = %+v, want duration=5s workers=2 stopOnFault=true showMetrics=false", opt)
		}
		if opt.LogFile != "custom.log" {
			t.Fatalf("OptionsFromConfig() log file = %q, want %q", opt.LogFile, "custom.log")
		}
		if !reflect.DeepEqual(opt.Cores, []int{0, 1}) {
			t.Fatalf("OptionsFromConfig() cores = %v, want [0 1]", opt.Cores)
		}

		// cores must be copied, not aliased: mutating the config must not change the plan
		cfg.Cores[0] = 7
		if opt.Cores[0] != 0 {
			t.Fatalf("OptionsFromConfig() aliases the config cores slice")
		}
	})

	t.Run("cyclic test ignores workloads", func(t *testing.T) {
		cfg := config.Default()
		cfg.Test = "cyclic"
		cfg.WorkloadA = ""
		cfg.WorkloadB = "no-such-workload"

		opt, err := OptionsFromConfig(cfg)
		if err != nil {
			t.Fatalf("OptionsFromConfig() error = %v", err)
		}
		if opt.TestName != "cyclic" {
			t.Fatalf("OptionsFromConfig() test = %q, want cyclic", opt.TestName)
		}
	})

	t.Run("registered external names pass validation", func(t *testing.T) {
		workload.Register(&workload.Workload{
			Name:        "fake-ok",
			Description: "fake passing tool",
			Run:         func(ctx context.Context, rep workload.Reporter) (uint64, error) { return 0, nil },
		})
		t.Cleanup(func() { workload.Delete("fake-ok") })

		cfg := base()
		cfg.WorkloadA = "fake-ok"
		if _, err := OptionsFromConfig(cfg); err != nil {
			t.Fatalf("OptionsFromConfig() error = %v, want external names accepted as workloads", err)
		}
	})

	t.Run("idle is an accepted workload name", func(t *testing.T) {
		cfg := base()
		cfg.WorkloadA = "idle"
		if _, err := OptionsFromConfig(cfg); err != nil {
			t.Fatalf("OptionsFromConfig() error = %v, want idle accepted (settle phases)", err)
		}
	})

	t.Run("rotate inherits and overrides", func(t *testing.T) {
		cfg := base()
		cfg.Rotate = true
		opt, err := OptionsFromConfig(cfg)
		if err != nil || !opt.Rotate {
			t.Fatalf("OptionsFromConfig() = %+v, %v, want Rotate true", opt, err)
		}
		off := false
		opt, err = OptionsFromPhase(cfg, config.Phase{Name: "x", Rotate: &off})
		if err != nil || opt.Rotate {
			t.Fatalf("OptionsFromPhase() = %+v, %v, want phase rotate override to false", opt, err)
		}
	})

	t.Run("beat_interval parses and flows into phases", func(t *testing.T) {
		cfg := base()
		cfg.BeatEvery = "5s"
		opt, err := OptionsFromConfig(cfg)
		if err != nil || opt.BeatEvery != 5*time.Second {
			t.Fatalf("OptionsFromConfig() = %+v, %v, want BeatEvery 5s", opt, err)
		}
		opt, err = OptionsFromPhase(cfg, config.Phase{Name: "x"})
		if err != nil || opt.BeatEvery != 5*time.Second {
			t.Fatalf("OptionsFromPhase() = %+v, %v, want BeatEvery inherited", opt, err)
		}
		opt, err = OptionsFromConfig(base())
		if err != nil || opt.BeatEvery != 0 {
			t.Fatalf("OptionsFromConfig() = %+v, %v, want BeatEvery 0 (default 1s)", opt, err)
		}
	})

	t.Run("settle is parsed for per-process phases", func(t *testing.T) {
		opt, err := OptionsFromPhase(base(), config.Phase{Name: "w", PerProcess: true, Settle: "2s"})
		if err != nil || opt.Settle != 2*time.Second {
			t.Fatalf("OptionsFromPhase() = %+v, %v, want Settle 2s", opt, err)
		}
		if _, err := OptionsFromPhase(base(), config.Phase{Settle: "-1s"}); err == nil {
			t.Error("OptionsFromPhase() error = nil, want a negative settle rejected")
		}
		if _, err := OptionsFromPhase(base(), config.Phase{Settle: "banana"}); err == nil {
			t.Error("OptionsFromPhase() error = nil, want an unparsable settle rejected")
		}
	})

	t.Run("all_or_none passes through for pair phases", func(t *testing.T) {
		opt, err := OptionsFromPhase(base(), config.Phase{Name: "p", AllOrNone: true})
		if err != nil || !opt.AllOrNone {
			t.Fatalf("OptionsFromPhase() = %+v, %v, want AllOrNone true", opt, err)
		}
		opt, err = OptionsFromPhase(base(), config.Phase{Name: "p"})
		if err != nil || opt.AllOrNone {
			t.Fatalf("OptionsFromPhase() = %+v, %v, want AllOrNone false by default", opt, err)
		}
	})

	invalid := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{name: "unknown test", mutate: func(c *config.Config) { c.Test = "no-such-test" }, wantErr: "unknown test"},
		{name: "empty test", mutate: func(c *config.Config) { c.Test = "" }, wantErr: "unknown test"},
		{name: "unknown primary workload", mutate: func(c *config.Config) { c.WorkloadA = "no-such-workload" }, wantErr: `unknown primary workload "no-such-workload"`},
		{name: "empty primary workload", mutate: func(c *config.Config) { c.WorkloadA = "" }, wantErr: "unknown primary workload"},
		{name: "unknown secondary workload", mutate: func(c *config.Config) { c.WorkloadB = "no-such-workload" }, wantErr: `unknown secondary workload "no-such-workload"`},
		{name: "empty duration", mutate: func(c *config.Config) { c.Duration = "" }, wantErr: "duration"},
		{name: "zero duration", mutate: func(c *config.Config) { c.Duration = "0s" }, wantErr: "duration"},
		{name: "negative duration", mutate: func(c *config.Config) { c.Duration = "-5s" }, wantErr: "duration"},
		{name: "not a duration", mutate: func(c *config.Config) { c.Duration = "five minutes" }, wantErr: "duration"},
		{name: "zero workers", mutate: func(c *config.Config) { c.Workers = 0 }, wantErr: "workers must be at least 1"},
		{name: "negative workers", mutate: func(c *config.Config) { c.Workers = -1 }, wantErr: "workers must be at least 1"},
		{name: "empty log file", mutate: func(c *config.Config) { c.LogFile = "" }, wantErr: "log_file must not be empty"},
		{name: "blank log file", mutate: func(c *config.Config) { c.LogFile = "   " }, wantErr: "log_file must not be empty"},
		{name: "core outside range", mutate: func(c *config.Config) { c.Cores = []int{999999} }, wantErr: "outside the available range"},
		{name: "duplicate cores", mutate: func(c *config.Config) { c.Cores = []int{0, 0} }, wantErr: "listed more than once"},
		{name: "negative core", mutate: func(c *config.Config) { c.Cores = []int{-1} }, wantErr: "outside the available range"},
		{name: "beat not a duration", mutate: func(c *config.Config) { c.BeatEvery = "5" }, wantErr: "beat_interval"},
		{name: "beat zero", mutate: func(c *config.Config) { c.BeatEvery = "0s" }, wantErr: "beat_interval"},
		{name: "beat negative", mutate: func(c *config.Config) { c.BeatEvery = "-1s" }, wantErr: "beat_interval"},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)

			_, err := OptionsFromConfig(cfg)
			if err == nil {
				t.Fatalf("OptionsFromConfig() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("OptionsFromConfig() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveControlCore(t *testing.T) {
	num := runtime.NumCPU()
	if num < 4 {
		t.Skipf("test assumes at least 4 CPUs, machine has %d", num)
	}
	ptr := func(n int) *int { return &n }
	all := make([]int, num)
	for i := range all {
		all[i] = i
	}

	cases := []struct {
		name     string
		explicit *int
		cores    []int
		want     int
		wantErr  string
	}{
		{"auto takes the first free core", nil, []int{0, 1}, 2, ""},
		{"auto with every core taken is none", nil, all, -1, ""},
		{"auto with no core list (all targets) is none", nil, nil, -1, ""},
		{"explicit core outside the targets is used", ptr(num - 1), []int{0}, num - 1, ""},
		{"explicit -1 turns helpers loose", ptr(-1), nil, -1, ""},
		{"explicit target core is refused", ptr(1), []int{1, 2}, 0, "also a target core"},
		{"explicit core out of range is refused", ptr(num + 5), nil, 0, "outside the available range"},
		{"below -1 is refused", ptr(-2), nil, 0, "must be -1"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveControlCore(tt.explicit, tt.cores)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveControlCore() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveControlCore() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveControlCore() = %d, want %d", got, tt.want)
			}
		})
	}
}

// The reserve policy is an explicit consent to headroom: it acts on
// saturated AUTO plans only, hands the highest-numbered logical CPU to
// the helpers by a fixed rule (no topology guessing), names what it
// removed, and pays nothing when a CPU was already free.
func TestResolveControlReserve(t *testing.T) {
	num := runtime.NumCPU()
	if num < 4 {
		t.Skipf("test assumes at least 4 CPUs, machine has %d", num)
	}
	ptr := func(n int) *int { return &n }
	all := make([]int, num)
	for i := range all {
		all[i] = i
	}

	t.Run("none keeps today's answers identical", func(t *testing.T) {
		for _, res := range []string{"", "none"} {
			c, eff, rm, err := ResolveControl(nil, all, res)
			if err != nil || c != -1 || rm != nil || len(eff) != num {
				t.Fatalf("reserve=%q = (%d, %d cpus, %v, %v), want today's (-1, all, nil, nil)", res, c, len(eff), rm, err)
			}
		}
	})
	t.Run("logical hands the highest logical cpu to helpers", func(t *testing.T) {
		c, eff, rm, err := ResolveControl(nil, all, "logical")
		if err != nil {
			t.Fatal(err)
		}
		if c != num-1 || len(rm) != 1 || rm[0] != num-1 || len(eff) != num-1 {
			t.Fatalf("control %d, effective %d cpus, removed %v: want control+removed %d, effective %d cpus", c, len(eff), rm, num-1, num-1)
		}
		for _, cpu := range eff {
			if cpu == num-1 {
				t.Fatalf("the removed cpu is still in the effective set: %v", eff)
			}
		}
	})
	t.Run("implicit all-cores materializes the reduced set", func(t *testing.T) {
		c, eff, rm, err := ResolveControl(nil, nil, "logical")
		if err != nil || c != num-1 || len(rm) != 1 || len(eff) != num-1 {
			t.Fatalf("implicit all-cores = (control %d, %d cpus, removed %v, err %v)", c, len(eff), rm, err)
		}
	})
	t.Run("a free cpu pays no coverage", func(t *testing.T) {
		c, eff, rm, err := ResolveControl(nil, []int{0}, "logical")
		if err != nil || c != 1 || rm != nil || len(eff) != 1 {
			t.Fatalf("free cpu = (control %d, %v, removed %v, err %v), want (1, untouched, nil, nil)", c, eff, rm, err)
		}
	})
	t.Run("explicit and off refuse the consent", func(t *testing.T) {
		for _, e := range []*int{ptr(-1), ptr(2)} {
			if _, _, _, err := ResolveControl(e, all, "logical"); err == nil || !strings.Contains(err.Error(), "needs auto control") {
				t.Fatalf("control_core=%d with reserve: got err %v, want the named contradiction", *e, err)
			}
		}
	})
	t.Run("unknown policy is refused (physical units are not promised)", func(t *testing.T) {
		if _, _, _, err := ResolveControl(nil, all, "physical"); err == nil || !strings.Contains(err.Error(), "reserve_control") {
			t.Fatalf("unknown reserve policy: %v", err)
		}
	})
}

// OptionsFromConfig carries the consent end into the resolved plan:
// the effective Cores shrink (that IS the plan identity), ReservedCPUs
// names the loss for the displays, and the config-level gates refuse
// the contradictions before any load.
func TestOptionsReserveControl(t *testing.T) {
	num := runtime.NumCPU()
	if num < 4 {
		t.Skipf("test assumes at least 4 CPUs, machine has %d", num)
	}
	cfg := config.Default()
	cfg.Test, cfg.WorkloadA, cfg.WorkloadB = "general", "float", "mem"
	cfg.Duration, cfg.Workers, cfg.LogFile = "5s", 1, "reserve.log"
	all := make([]int, num)
	for i := range all {
		all[i] = i
	}
	cfg.Cores = all

	cfg.ReserveControl = ""
	opt, err := OptionsFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if opt.ControlCore != -1 || len(opt.Cores) != num || len(opt.ReservedCPUs) != 0 {
		t.Errorf("without the consent: control %d, %d cores, %d reserved - want today's -1/%d/0", opt.ControlCore, len(opt.Cores), len(opt.ReservedCPUs), num)
	}

	cfg.ReserveControl = "logical"
	opt, err = OptionsFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if opt.ControlCore != num-1 || len(opt.Cores) != num-1 || len(opt.ReservedCPUs) != 1 || opt.ReservedCPUs[0] != num-1 {
		t.Errorf("with the consent: control %d, %d cores, reserved %v - want control %d, %d cores, [%d]", opt.ControlCore, len(opt.Cores), opt.ReservedCPUs, num-1, num-1, num-1)
	}

	cfg.ReserveControl = "maybe"
	if _, err := OptionsFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "reserve_control") {
		t.Errorf("unknown policy passed the gate: %v", err)
	}
	cfg.ReserveControl = "logical"
	off := -1
	cfg.ControlCore = &off
	if _, err := OptionsFromConfig(cfg); err == nil || !strings.Contains(err.Error(), "needs auto control") {
		t.Errorf("consent alongside control_core=-1 passed the gate: %v", err)
	}
}

func TestOptionsFromPhase(t *testing.T) {
	base := func() config.Config {
		cfg := config.Default()
		cfg.Test = "general"
		cfg.Cores = []int{0, 1}
		cfg.WorkloadA = "float"
		cfg.WorkloadB = "mem"
		cfg.Duration = "5s"
		cfg.Workers = 2
		cfg.StopOnFault = true
		cfg.LogFile = "custom.log"
		return cfg
	}

	t.Run("empty phase inherits every config default", func(t *testing.T) {
		opt, err := OptionsFromPhase(base(), config.Phase{Name: "warmup"})
		if err != nil {
			t.Fatalf("OptionsFromPhase() error = %v", err)
		}
		if opt.TestName != "general" || opt.A != "float" || opt.B != "mem" {
			t.Fatalf("OptionsFromPhase() = %+v, want the config defaults", opt)
		}
		if opt.Phase != "warmup" || opt.Duration != 5*time.Second || opt.Workers != 2 {
			t.Fatalf("OptionsFromPhase() = %+v, want phase=warmup duration=5s workers=2", opt)
		}
		if !reflect.DeepEqual(opt.Cores, []int{0, 1}) || !opt.StopOnFault || opt.PerProcess {
			t.Fatalf("OptionsFromPhase() = %+v, want cores=[0 1] stopOnFault=true perProcess=false", opt)
		}
	})

	t.Run("phase fields override the defaults", func(t *testing.T) {
		if n := runtime.NumCPU(); n < 4 {
			t.Skipf("fixture names core 3: needs 4 logical CPUs to stay in range, this box has %d", n)
		}
		off := false
		ph := config.Phase{
			Name:        "heavy",
			Test:        "isolate",
			A:           "sha256",
			Cores:       []int{3},
			Workers:     4,
			Duration:    "1m",
			StopOnFault: &off,
			PerProcess:  true,
		}

		opt, err := OptionsFromPhase(base(), ph)
		if err != nil {
			t.Fatalf("OptionsFromPhase() error = %v", err)
		}
		if opt.TestName != "isolate" || opt.A != "sha256" || opt.B != "mem" {
			t.Fatalf("OptionsFromPhase() = %+v, want test=isolate a=sha256 with b inherited", opt)
		}
		if opt.Duration != time.Minute || opt.Workers != 4 || opt.StopOnFault || !opt.PerProcess {
			t.Fatalf("OptionsFromPhase() = %+v, want duration=1m workers=4 stopOnFault=false perProcess=true", opt)
		}
		if !reflect.DeepEqual(opt.Cores, []int{3}) {
			t.Fatalf("OptionsFromPhase() cores = %v, want [3]", opt.Cores)
		}
	})

	t.Run("invalid phase fields are rejected like config fields", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			phase   config.Phase
			wantErr string
		}{
			{"unknown test", config.Phase{Test: "nope"}, "unknown test"},
			{"unknown workload", config.Phase{A: "nope"}, "unknown primary workload"},
			{"bad duration", config.Phase{Duration: "banana"}, "duration"},
			{"core out of range", config.Phase{Cores: []int{999999}}, "outside the available range"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				_, err := OptionsFromPhase(base(), tt.phase)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("OptionsFromPhase() error = %v, want it to contain %q", err, tt.wantErr)
				}
			})
		}
	})
	t.Run("transition-mode phases map and validate their schedule", func(t *testing.T) {
		opt, err := OptionsFromPhase(base(), config.Phase{Name: "tr", PerProcess: true, Bursts: 3})
		if err != nil {
			t.Fatalf("OptionsFromPhase() error = %v", err)
		}
		if opt.Bursts != 3 || opt.BurstGap != DefaultBurstGap || opt.BurstRun != DefaultBurstRun ||
			opt.BurstJitter != 0 || opt.BurstStagger != 0 || opt.BurstSeed != 0 {
			t.Fatalf("transition defaults = %+v, want 3 bursts gap=%s run=%s, no jitter/stagger",
				opt, DefaultBurstGap, DefaultBurstRun)
		}

		opt, err = OptionsFromPhase(base(), config.Phase{
			PerProcess: true, Bursts: 2, BurstGap: "100ms", BurstRun: "300ms",
			BurstJitter: "20ms", BurstSeed: 7, BurstStagger: "50ms",
		})
		if err != nil {
			t.Fatalf("OptionsFromPhase() error = %v", err)
		}
		if opt.BurstGap != 100*time.Millisecond || opt.BurstRun != 300*time.Millisecond ||
			opt.BurstJitter != 20*time.Millisecond || opt.BurstSeed != 7 || opt.BurstStagger != 50*time.Millisecond {
			t.Fatalf("explicit transition knobs = %+v, want the parsed schedule", opt)
		}

		rotate := true
		for _, tt := range []struct {
			name    string
			phase   config.Phase
			wantErr string
		}{
			{"bursts with rotate", config.Phase{PerProcess: true, Rotate: &rotate, Bursts: 2}, "do not mix"},
			{"zero burst run", config.Phase{PerProcess: true, Bursts: 2, BurstRun: "0s"}, "must be positive"},
			{"negative gap", config.Phase{PerProcess: true, Bursts: 2, BurstGap: "-1s"}, "must not be negative"},
			{"unparsable run", config.Phase{PerProcess: true, Bursts: 2, BurstRun: "banana"}, "Go duration"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				_, err := OptionsFromPhase(base(), tt.phase)
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("OptionsFromPhase() error = %v, want it to contain %q", err, tt.wantErr)
				}
			})
		}

		// U01 auto-execution (menu plan section 4): a burst phase that
		// does not name its worker mode is no longer refused - the
		// resolver picks the subprocess mode the prepared lifecycle
		// needs. The refusal only survives as an override contradiction.
		opt, err = OptionsFromPhase(base(), config.Phase{Bursts: 2})
		if err != nil {
			t.Fatalf("auto resolver refused a burst phase: %v", err)
		}
		if !opt.PerProcess {
			t.Errorf("burst phase PerProcess = false, want the resolver's subprocess answer")
		}
		threaded := base()
		threaded.Execution = config.ExecutionThreaded
		if _, err := OptionsFromPhase(threaded, config.Phase{Bursts: 2}); err == nil ||
			!strings.Contains(err.Error(), "contradicts bursts") {
			t.Errorf("threaded override + bursts error = %v, want a refusal naming the contradiction", err)
		}
	})
}

// Provenance plumbing: the config hash is deterministic and unknown
// (never a guess) when the file cannot be read; the seed pointer must
// distinguish "seed 0 configured" from "no seed carried".
func TestRunProvenance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"duration":"10s"}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h1 := ConfigHashAt(path)
	if h1 == "" || h1 != ConfigHashAt(path) {
		t.Fatalf("ConfigHashAt = %q, want a stable non-empty hash", h1)
	}
	if len(h1) != 64 {
		t.Errorf("ConfigHashAt length = %d, want 64 hex chars", len(h1))
	}
	if got := ConfigHashAt(filepath.Join(dir, "missing.json")); got != "" {
		t.Errorf("ConfigHashAt(missing) = %q, want empty (unknown)", got)
	}

	opt := Options{RamSeed: 0} // seed 0 is a valid configured seed
	m := RunProvenance(opt)
	if m.Seed == nil || *m.Seed != 0 {
		t.Errorf("provenance seed = %v, want an explicit 0", m.Seed)
	}
	// The test binary runs from the package dir: no config.json there,
	// so the hash must read as unknown, not as a hash of nothing.
	if m.ConfigHash != "" && m.ConfigHash != ConfigHashAt(config.DefaultPath) {
		t.Errorf("provenance config hash %q does not match the file it claims", m.ConfigHash)
	}

	// A11: the plan hash is the ENTRY POINT's claim, journaled verbatim
	// ("" stays unknown); the topology fingerprint is the engine's own
	// observation of the layout the run resolved against.
	opt.PlanHash = "deadbeef"
	m = RunProvenance(opt)
	if m.PlanHash != "deadbeef" {
		t.Errorf("PlanHash = %q, want the entry point's value passed through", m.PlanHash)
	}
	if _, terr := sysinfo.DiscoverTopology(); terr == nil {
		if m.TopologyFP == "" {
			t.Error("TopologyFP empty although the topology is observable")
		}
	}

	t.Run("topology fingerprint is stable and sensitive", func(t *testing.T) {
		topo := sysinfo.Topology{Source: "test", PhysicalCores: []sysinfo.PhysicalCore{
			{Index: 0, Group: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}, {Number: 16}}},
			{Index: 1, Group: 0, CCXIndex: 0, CCXKnown: true, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 1}}},
		}}
		fp := TopologyFingerprint(topo)
		if fp == "" || fp != TopologyFingerprint(topo) {
			t.Fatalf("TopologyFingerprint must be a stable non-empty hash, got %q", fp)
		}
		if len(fp) != 64 {
			t.Errorf("TopologyFingerprint length = %d, want 64 hex chars", len(fp))
		}
		// SMT width change = different experiment = different fingerprint.
		topo.PhysicalCores[0].LogicalCPUs = topo.PhysicalCores[0].LogicalCPUs[:1]
		if TopologyFingerprint(topo) == fp {
			t.Error("fingerprint ignored an SMT layout change")
		}
		if got := TopologyFingerprint(sysinfo.Topology{Source: "test"}); got != "" {
			t.Errorf("empty topology fingerprint = %q, want empty (unknown)", got)
		}
	})
}

// The reproduction note must list the resolved parameters and mark the
// config-sourced ones (memory/lock/seed) honestly, never pretend they
// are command-line flags.
func TestReproductionParameters(t *testing.T) {
	t.Run("plain per-process run", func(t *testing.T) {
		line := ReproductionParameters(Options{
			TestName: "general", A: "int", B: "float", Cores: []int{0, 1}, Workers: 2,
			PerProcess: true, Duration: 30 * time.Second, ControlCore: 3, MaxTempC: 90,
		})
		for _, want := range []string{"test=general", "a=int", "b=float", "cores=0,1", "workers=2", "mode=per-process", "duration=30s", "control=core3", "max-temp=90C"} {
			if !strings.Contains(line, want) {
				t.Errorf("line %q missing %q", line, want)
			}
		}
	})

	t.Run("transition schedule and config-sourced memory", func(t *testing.T) {
		line := ReproductionParameters(Options{
			TestName: "general", A: "ram-constant", PerProcess: true,
			Bursts: 3, BurstGap: 200 * time.Millisecond, BurstRun: 500 * time.Millisecond,
			BurstJitter: 20 * time.Millisecond, BurstSeed: 7,
			Memory: "1GiB", MemLock: "try", RamSeed: 4242, ControlCore: -1,
		})
		if !strings.Contains(line, "schedule=3bursts/gap200ms/run500ms/jit±20ms/seed7") {
			t.Errorf("line %q missing the schedule", line)
		}
		if !strings.Contains(line, "memory=1GiB/lock=try/ram-seed=4242 (from config.json)") {
			t.Errorf("line %q must mark memory as config-sourced", line)
		}
		if !strings.Contains(line, "control=off") {
			t.Errorf("line %q missing control=off", line)
		}
		// A duration must NOT appear: the schedule defines the time.
		if strings.Contains(line, "duration=") {
			t.Errorf("transition run must not print a duration: %q", line)
		}
	})

	t.Run("all-cores in-process", func(t *testing.T) {
		line := ReproductionParameters(Options{TestName: "general", A: "int", Duration: time.Second})
		if !strings.Contains(line, "cores=all") || !strings.Contains(line, "mode=in-process") {
			t.Errorf("line %q, want cores=all mode=in-process", line)
		}
	})
}

func TestValidateLogicalCPUs(t *testing.T) {
	numCPU := runtime.NumCPU()

	tests := []struct {
		name    string
		cpus    []int
		wantErr string
	}{
		{name: "empty is valid", cpus: nil},
		{name: "all available CPUs", cpus: rangeInts(0, numCPU)},
		{name: "out of range", cpus: []int{numCPU}, wantErr: "outside the available range"},
		{name: "negative", cpus: []int{-1}, wantErr: "outside the available range"},
		{name: "duplicate", cpus: []int{0, 1, 0}, wantErr: "listed more than once"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLogicalCPUs(tt.cpus)
			if (err != nil) != (tt.wantErr != "") {
				t.Fatalf("validateLogicalCPUs(%v) error = %v, wantErr %q", tt.cpus, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLogicalCPUs(%v) error = %v, want it to contain %q", tt.cpus, err, tt.wantErr)
			}
		})
	}
}

func TestLoadResolvedPlan(t *testing.T) {
	t.Run("missing config file", func(t *testing.T) {
		_, _, err := LoadResolvedPlan(filepath.Join(t.TempDir(), "missing.json"))
		if err == nil {
			t.Fatal("LoadResolvedPlan() error = nil, want missing file error")
		}
	})

	t.Run("invalid config is rejected before topology lookup", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(`{"test": "no-such-test"}`), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}

		_, _, err := LoadResolvedPlan(path)
		if err == nil || !strings.Contains(err.Error(), "unknown test") {
			t.Fatalf("LoadResolvedPlan() error = %v, want unknown test error", err)
		}
	})
}

func TestSelectedLogicalCPUs(t *testing.T) {
	t.Run("configured CPUs are used as-is", func(t *testing.T) {
		configured := []int{3, 1, 2}

		got := SelectedLogicalCPUs(configured, 8)
		if !reflect.DeepEqual(got, []int{3, 1, 2}) {
			t.Fatalf("SelectedLogicalCPUs() = %v, want [3 1 2] in configured order", got)
		}

		// the result must be a copy: mutating the input must not change the selection
		configured[0] = 0
		if got[0] != 3 {
			t.Fatalf("SelectedLogicalCPUs() aliases the input slice")
		}
	})

	t.Run("empty selection means all CPUs", func(t *testing.T) {
		for _, configured := range [][]int{nil, {}} {
			got := SelectedLogicalCPUs(configured, 4)
			if !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
				t.Fatalf("SelectedLogicalCPUs(%v, 4) = %v, want [0 1 2 3]", configured, got)
			}
		}
	})

	t.Run("zero total yields empty selection", func(t *testing.T) {
		got := SelectedLogicalCPUs(nil, 0)
		if len(got) != 0 {
			t.Fatalf("SelectedLogicalCPUs(nil, 0) = %v, want empty", got)
		}
	})
}

func TestGroupAssignments(t *testing.T) {
	tests := []struct {
		name string
		cpus []int
		plan []string
		want []workloadAssignment
	}{
		{
			name: "empty plan yields no groups",
			cpus: nil,
			plan: nil,
			want: nil,
		},
		{
			name: "single workload gets all CPUs",
			cpus: []int{0, 1, 2},
			plan: []string{"matrix", "matrix", "matrix"},
			want: []workloadAssignment{{workload: "matrix", cpus: []int{0, 1, 2}}},
		},
		{
			name: "alternating plan is grouped per workload",
			cpus: []int{0, 1, 2, 3},
			plan: []string{"matrix", "sorting", "matrix", "sorting"},
			want: []workloadAssignment{
				{workload: "matrix", cpus: []int{0, 2}},
				{workload: "sorting", cpus: []int{1, 3}},
			},
		},
		{
			name: "groups keep first-appearance order",
			cpus: []int{4, 5, 6, 7},
			plan: []string{"sorting", "matrix", "sorting", "matrix"},
			want: []workloadAssignment{
				{workload: "sorting", cpus: []int{4, 6}},
				{workload: "matrix", cpus: []int{5, 7}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := groupAssignments(tt.cpus, tt.plan)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("groupAssignments(%v, %v) = %v, want %v", tt.cpus, tt.plan, got, tt.want)
			}
		})
	}
}

func rangeInts(start, count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = start + i
	}
	return out
}
