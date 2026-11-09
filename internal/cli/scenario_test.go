package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestExpandRepeats(t *testing.T) {
	tests := []struct {
		name string
		in   []config.Phase
		want []string // expanded phase names in order, "" = unnamed
	}{
		{"no repeat stays one phase", []config.Phase{{Name: "a"}}, []string{"a"}},
		{"repeat 0 or negative means once", []config.Phase{{Name: "a", Repeat: -3}}, []string{"a"}},
		{"named phase repeats get #k", []config.Phase{{Name: "a", Repeat: 3}}, []string{"a#1", "a#2", "a#3"}},
		{"unnamed repeats stay positional", []config.Phase{{Repeat: 2}}, []string{"", ""}},
		{"mixed list keeps order", []config.Phase{{Name: "a", Repeat: 2}, {Name: "b"}}, []string{"a#1", "a#2", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandRepeats(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("expandRepeats() produced %d phases, want %d", len(got), len(tt.want))
			}
			for i, ph := range got {
				if ph.Name != tt.want[i] {
					t.Errorf("phase %d name = %q, want %q", i, ph.Name, tt.want[i])
				}
				if ph.Repeat != 0 {
					t.Errorf("phase %d Repeat = %d, want 0 (already expanded)", i, ph.Repeat)
				}
			}
		})
	}
}

func TestRunScenario(t *testing.T) {
	phase := func(name string) plan.Options {
		return plan.Options{TestName: "general", Phase: name, A: "float", B: "mem",
			Workers: 1, Duration: 10 * time.Millisecond, StopOnFault: true, LogFile: "x.log"}
	}

	tests := []struct {
		name     string
		results  []plan.Result // outcome per executed phase
		wantCode int
		wantRun  int      // phases the fake executor must see
		wantOut  []string // substrings the summary must contain
	}{
		{
			name: "all phases complete", results: []plan.Result{{Validation: plan.ValVerified}, {Validation: plan.ValVerified}, {Validation: plan.ValVerified}},
			wantCode: 0, wantRun: 3,
			wantOut: []string{"Scenario summary:", "warmup", "RESULT: PASS", "all 3 phase(s) completed OK"},
		},
		{
			name: "stops at the first fault",
			results: []plan.Result{
				{},
				{Exit: 1, Aborted: true, Faults: map[int]string{3: "mismatch at iter 42"}},
			},
			wantCode: 1, wantRun: 2,
			wantOut: []string{"memory", "FAIL", "core(s) [cpu 3]: mismatch at iter 42",
				"not run", "stopped after phase 2 (memory)"},
		},
		{
			name: "propagates a setup error", results: []plan.Result{{Exit: 2}},
			wantCode: 2, wantRun: 1,
			wantOut: []string{"RESULT: INCOMPLETE", "stopped after phase 1 (warmup)"},
		},
		{
			name:     "propagates a force quit",
			results:  []plan.Result{{}, {Exit: 130}},
			wantCode: 130, wantRun: 2,
			wantOut: []string{"stopped after phase 2 (memory)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ran []string
			var out strings.Builder
			execute := func(opt plan.Options) plan.Result {
				ran = append(ran, opt.Phase)
				if len(ran) <= len(tt.results) {
					return tt.results[len(ran)-1]
				}
				return plan.Result{}
			}

			code := runScenario([]plan.Options{phase("warmup"), phase("memory"), phase("external")}, &out, execute, false)
			if code != tt.wantCode {
				t.Fatalf("runScenario() exit code = %d, want %d\n%s", code, tt.wantCode, out.String())
			}
			if len(ran) != tt.wantRun {
				t.Fatalf("runScenario() ran %v, want %d phases", ran, tt.wantRun)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output = %q, want it to contain %q", out.String(), want)
				}
			}
		})
	}
}

func TestRunScenarioContinueOnFault(t *testing.T) {
	// phase 1 faults on core 3; later phases must run without that core
	// and the scenario must still report the fault via exit code 1
	opts := []plan.Options{
		{TestName: "general", Phase: "first", A: "float", B: "mem", Workers: 1,
			Duration: time.Millisecond, StopOnFault: true, LogFile: "x.log", Cores: []int{0, 1, 2, 3}},
		{TestName: "general", Phase: "second", A: "int", B: "mem", Workers: 1,
			Duration: time.Millisecond, StopOnFault: true, LogFile: "x.log", Cores: []int{0, 1, 2, 3}},
	}

	var gotCores [][]int
	var out strings.Builder
	execute := func(opt plan.Options) plan.Result {
		gotCores = append(gotCores, opt.Cores)
		if len(gotCores) == 1 {
			return plan.Result{Exit: 1, Aborted: true, Faults: map[int]string{3: "mismatch"}}
		}
		return plan.Result{}
	}

	code := runScenario(opts, &out, execute, true)
	if code != 1 {
		t.Fatalf("runScenario() exit code = %d, want 1 (faults found)", code)
	}
	if len(gotCores) != 2 {
		t.Fatalf("executor ran %d phases, want 2 (scenario must continue past the fault)", len(gotCores))
	}
	if len(gotCores[1]) != 3 || gotCores[1][2] != 2 || gotCores[1][0] != 0 {
		t.Fatalf("phase 2 cores = %v, want [0 1 2] (faulted core 3 excluded)", gotCores[1])
	}
	for _, want := range []string{"excluded from later phases: [3]", "faults on 1 core(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

func TestRunScenarioNoUsableCoresLeft(t *testing.T) {
	opts := []plan.Options{
		{TestName: "isolate", Phase: "only", A: "float", B: "mem", Workers: 1,
			Duration: time.Millisecond, StopOnFault: true, LogFile: "x.log", Cores: []int{4}},
		{TestName: "isolate", Phase: "next", A: "int", B: "mem", Workers: 1,
			Duration: time.Millisecond, StopOnFault: true, LogFile: "x.log", Cores: []int{4}},
	}

	var out strings.Builder
	execute := func(opt plan.Options) plan.Result {
		return plan.Result{Exit: 1, Aborted: true, Faults: map[int]string{4: "mismatch"}}
	}

	code := runScenario(opts, &out, execute, true)
	if code != 1 {
		t.Fatalf("runScenario() exit code = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no usable cores left") || !strings.Contains(out.String(), "next") {
		t.Errorf("output = %q, want the no-cores stop reason and the skipped phase", out.String())
	}
}

func TestExpandBursts(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA = "float"
	cfg.Duration = "30s"

	t.Run("plain phases pass through", func(t *testing.T) {
		in := []config.Phase{{Name: "a"}, {Name: "b"}}
		got, err := expandBursts(in, cfg)
		if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
			t.Fatalf("expandBursts() = %+v, %v, want the input unchanged", got, err)
		}
	})

	t.Run("burst compiles in place to settle+load wake phases", func(t *testing.T) {
		in := []config.Phase{{Name: "warm"}, {Name: "bursts", Burst: &config.Burst{
			Cores: []int{2, 5}, Repeat: 2, Load: "int", LoadDuration: "3s", IdleDuration: "1s",
		}}}
		got, err := expandBursts(in, cfg)
		if err != nil {
			t.Fatalf("expandBursts() error = %v", err)
		}
		if len(got) != 1+4 {
			t.Fatalf("expandBursts() produced %d phases, want 1 + 4", len(got))
		}
		want := []string{"bursts-c2-r1", "bursts-c2-r2", "bursts-c5-r1", "bursts-c5-r2"}
		for i, name := range want {
			ph := got[i+1]
			if ph.Name != name || ph.A != "int" || ph.Settle != "1s" || !ph.PerProcess {
				t.Errorf("phase %d = %+v, want %s load int settle 1s per-process", i, ph, name)
			}
			if ph.Duration != "3s" {
				t.Errorf("phase %d Duration = %s, want load 3s", i, ph.Duration)
			}
		}
	})

	t.Run("burst fields default to the config", func(t *testing.T) {
		in := []config.Phase{{Burst: &config.Burst{Cores: []int{1}}}}
		got, err := expandBursts(in, cfg)
		if err != nil {
			t.Fatalf("expandBursts() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("produced %d phases, want 1", len(got))
		}
		if got[0].Name != "burst-c1-r1" || got[0].Settle != "2s" {
			t.Errorf("burst phase = %+v, want name burst-c1-r1 settle 2s", got[0])
		}
		if got[0].A != "float" || got[0].Duration != "30s" {
			t.Errorf("burst phase = %+v, want workload/duration from the config", got[0])
		}
	})

	t.Run("burst and pairs cannot share one entry", func(t *testing.T) {
		ph := config.Phase{Name: "x", Burst: &config.Burst{Cores: []int{1}}, Pairs: &config.Pairs{Cores: []int{1, 2}}}
		if _, err := expandBursts([]config.Phase{ph}, cfg); err == nil {
			t.Fatal("expandBursts() error = nil, want a generator conflict error")
		}
	})

	t.Run("burst without cores is a setup error", func(t *testing.T) {
		if _, err := expandBursts([]config.Phase{{Name: "x", Burst: &config.Burst{}}}, cfg); err == nil {
			t.Fatal("expandBursts() error = nil, want a cores error")
		}
	})
}

func TestExpandPairs(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA = "float"
	cfg.WorkloadB = "mem"
	cfg.Duration = "30s"

	t.Run("scope all emits every ordered pair", func(t *testing.T) {
		in := []config.Phase{{Name: "warm"}, {Name: "pp", Pairs: &config.Pairs{
			Cores: []int{5, 9}, Scope: "all", Aggressor: "int", Observer: "sha256", Duration: "2s", Settle: "1s",
		}}}
		got, err := expandPairs(in, cfg, nil, nil)
		if err != nil {
			t.Fatalf("expandPairs() error = %v", err)
		}
		if len(got) != 1+2 {
			t.Fatalf("expandPairs() produced %d phases, want 1 + 2", len(got))
		}
		want := []struct {
			name  string
			cores []int
		}{
			{"pp-c5-to-c9", []int{5, 9}},
			{"pp-c9-to-c5", []int{9, 5}},
		}
		for i, w := range want {
			ph := got[i+1]
			if ph.Name != w.name || ph.Test != "abab" || ph.A != "int" || ph.B != "sha256" {
				t.Errorf("phase %d = %+v, want %s abab int/sha256", i, ph, w.name)
			}
			if len(ph.Cores) != 2 || ph.Cores[0] != w.cores[0] || ph.Cores[1] != w.cores[1] {
				t.Errorf("phase %d cores = %v, want %v (aggressor first)", i, ph.Cores, w.cores)
			}
			if !ph.PerProcess || !ph.AllOrNone || ph.Settle != "1s" || ph.Duration != "2s" {
				t.Errorf("phase %d = %+v, want per-process all-or-none settle 1s duration 2s", i, ph)
			}
		}
	})

	t.Run("scope ccx is the default and keeps only same-domain pairs", func(t *testing.T) {
		ccx := map[int]int{2: 0, 5: 0, 7: 1}
		in := []config.Phase{{Name: "pp", Pairs: &config.Pairs{Cores: []int{2, 5, 7}}}}
		got, err := expandPairs(in, cfg, ccx, nil)
		if err != nil {
			t.Fatalf("expandPairs() error = %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expandPairs() produced %d phases, want 2 (cross-domain pairs excluded)", len(got))
		}
		if got[0].Name != "pp-c2-to-c5" || got[1].Name != "pp-c5-to-c2" {
			t.Errorf("phases = %s, %s, want the same-CCX pair in both directions", got[0].Name, got[1].Name)
		}
	})

	// scope smt pairs only SMT siblings of one physical core, both
	// directions: the classic CoreCycler aggressor/observer on shared
	// execution units.
	t.Run("scope smt keeps only sibling pairs", func(t *testing.T) {
		smt := map[int]int{0: 0, 16: 0, 1: 1, 17: 1}
		in := []config.Phase{{Name: "sp", Pairs: &config.Pairs{Cores: []int{0, 16, 1, 17}, Scope: "smt"}}}
		got, err := expandPairs(in, cfg, nil, smt)
		if err != nil {
			t.Fatalf("expandPairs() error = %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("expandPairs() produced %d phases, want 4 (two sibling pairs x two directions)", len(got))
		}
		want := []string{"sp-c0-to-c16", "sp-c16-to-c0", "sp-c1-to-c17", "sp-c17-to-c1"}
		for i, name := range want {
			if got[i].Name != name {
				t.Errorf("phase %d = %q, want %q", i, got[i].Name, name)
			}
		}
	})

	t.Run("workload and duration default to the config", func(t *testing.T) {
		in := []config.Phase{{Pairs: &config.Pairs{Cores: []int{0, 1}, Scope: "all"}}}
		got, err := expandPairs(in, cfg, nil, nil)
		if err != nil {
			t.Fatalf("expandPairs() error = %v", err)
		}
		if got[0].Name != "pairs-c0-to-c1" || got[0].A != "float" || got[0].B != "mem" || got[0].Duration != "30s" {
			t.Errorf("phase = %+v, want config defaults and generated name", got[0])
		}
	})

	t.Run("plain phases pass through", func(t *testing.T) {
		in := []config.Phase{{Name: "a"}, {Name: "b"}}
		got, err := expandPairs(in, cfg, nil, nil)
		if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
			t.Fatalf("expandPairs() = %+v, %v, want the input unchanged", got, err)
		}
	})

	bad := []struct {
		name   string
		phase  config.Phase
		ccx    map[int]int
		smt    map[int]int
		wantEr string
	}{
		{"fewer than two cores", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1}}}, nil, nil, "at least two cores"},
		{"duplicate core", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 1, 2}, Scope: "all"}}, nil, nil, "more than once"},
		{"negative core", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{-1, 2}, Scope: "all"}}, nil, nil, "negative"},
		{"unknown scope", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 2}, Scope: "ccd"}}, nil, nil, `must be "ccx", "smt" or "all"`},
		{"ccx without topology", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 2}}}, nil, nil, "cache topology"},
		{"ccx unknown for a core", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 9}}}, map[int]int{1: 0}, nil, "CCX unknown"},
		{"scope filters everything out", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 9}}}, map[int]int{1: 0, 9: 1}, nil, "no pairs"},
		{"smt without topology", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 2}, Scope: "smt"}}, nil, nil, "needs CPU topology"},
		{"smt unknown for a core", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{1, 9}, Scope: "smt"}}, nil, map[int]int{1: 0}, "physical core unknown"},
		{"smt no siblings in pool", config.Phase{Name: "p", Pairs: &config.Pairs{Cores: []int{0, 1}, Scope: "smt"}}, nil, map[int]int{0: 0, 1: 1}, "no pairs"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := expandPairs([]config.Phase{tt.phase}, cfg, tt.ccx, tt.smt)
			if err == nil || !strings.Contains(err.Error(), tt.wantEr) {
				t.Fatalf("expandPairs() error = %v, want it to contain %q", err, tt.wantEr)
			}
		})
	}
}

func TestExpandCurve(t *testing.T) {
	cfg := config.Default()
	cfg.WorkloadA = "float"
	cfg.Duration = "30s"

	t.Run("default steps double up to the pool size", func(t *testing.T) {
		pool := []int{0, 1, 2, 3, 4, 5, 6, 7}
		got, err := expandCurve([]config.Phase{{Name: "ramp", Curve: &config.Curve{Cores: pool}}}, cfg)
		if err != nil {
			t.Fatalf("expandCurve() error = %v", err)
		}
		wantSteps := []int{1, 2, 4, 8}
		if len(got) != len(wantSteps) {
			t.Fatalf("expandCurve() produced %d phases, want %d", len(got), len(wantSteps))
		}
		for i, k := range wantSteps {
			ph := got[i]
			if ph.Name != fmt.Sprintf("ramp-k%d", k) || ph.Test != "general" || ph.A != "float" {
				t.Errorf("phase %d = %+v, want ramp-k%d general float", i, ph, k)
			}
			if len(ph.Cores) != k || !reflect.DeepEqual(ph.Cores, pool[:k]) {
				t.Errorf("phase %d cores = %v, want the first %d pool cores", i, ph.Cores, k)
			}
			if !ph.PerProcess || !ph.AllOrNone || ph.Settle != "2s" || ph.Duration != "30s" {
				t.Errorf("phase %d = %+v, want per-process all-or-none settle 2s duration 30s", i, ph)
			}
		}
	})

	t.Run("explicit steps take pool-order prefixes", func(t *testing.T) {
		pool := []int{3, 1, 4, 0, 9}
		in := []config.Phase{{Name: "ramp", Curve: &config.Curve{
			Cores: pool, Steps: []int{2, 5, 3}, Load: "int", Duration: "2s", Settle: "1s",
		}}}
		got, err := expandCurve(in, cfg)
		if err != nil {
			t.Fatalf("expandCurve() error = %v", err)
		}
		want := [][]int{{3, 1}, {3, 1, 4, 0, 9}, {3, 1, 4}}
		for i, w := range want {
			if !reflect.DeepEqual(got[i].Cores, w) {
				t.Errorf("phase %d cores = %v, want %v", i, got[i].Cores, w)
			}
			if got[i].A != "int" || got[i].Duration != "2s" || got[i].Settle != "1s" {
				t.Errorf("phase %d = %+v, want explicit int/2s/1s", i, got[i])
			}
		}
	})

	t.Run("name and workloads default to the config", func(t *testing.T) {
		got, err := expandCurve([]config.Phase{{Curve: &config.Curve{Cores: []int{0, 1}, Steps: []int{2}}}}, cfg)
		if err != nil {
			t.Fatalf("expandCurve() error = %v", err)
		}
		if got[0].Name != "curve-k2" || got[0].A != "float" || got[0].Duration != "30s" {
			t.Errorf("phase = %+v, want config defaults and generated name", got[0])
		}
	})

	t.Run("plain phases pass through", func(t *testing.T) {
		in := []config.Phase{{Name: "a"}, {Name: "b"}}
		got, err := expandCurve(in, cfg)
		if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
			t.Fatalf("expandCurve() = %+v, %v, want the input unchanged", got, err)
		}
	})

	t.Run("one generator per entry", func(t *testing.T) {
		bc := config.Phase{Name: "x", Burst: &config.Burst{Cores: []int{1}}, Curve: &config.Curve{Cores: []int{1, 2}}}
		if _, err := expandBursts([]config.Phase{bc}, cfg); err == nil {
			t.Error("expandBursts() error = nil, want a generator conflict error")
		}
		pc := config.Phase{Name: "y", Pairs: &config.Pairs{Cores: []int{1, 2}}, Curve: &config.Curve{Cores: []int{1, 2}}}
		if _, err := expandPairs([]config.Phase{pc}, cfg, nil, nil); err == nil {
			t.Error("expandPairs() error = nil, want a generator conflict error")
		}
	})

	bad := []struct {
		name   string
		curve  *config.Curve
		wantEr string
	}{
		{"pool smaller than two", &config.Curve{Cores: []int{1}}, "at least two cores"},
		{"duplicate core", &config.Curve{Cores: []int{1, 1, 2}}, "more than once"},
		{"negative core", &config.Curve{Cores: []int{-1, 2}}, "negative"},
		{"step below one", &config.Curve{Cores: []int{1, 2}, Steps: []int{0}}, "outside 1..2"},
		{"step above the pool", &config.Curve{Cores: []int{1, 2}, Steps: []int{3}}, "outside 1..2"},
		{"duplicate step", &config.Curve{Cores: []int{1, 2, 3}, Steps: []int{2, 2}}, "more than once"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := expandCurve([]config.Phase{{Name: "p", Curve: tt.curve}}, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantEr) {
				t.Fatalf("expandCurve() error = %v, want it to contain %q", err, tt.wantEr)
			}
		})
	}
}

func TestRunCycles(t *testing.T) {
	phase := func(name string, cores ...int) plan.Options {
		return plan.Options{TestName: "general", Phase: name, A: "float", B: "mem",
			Workers: 1, Duration: time.Millisecond, StopOnFault: true, LogFile: "x.log", Cores: cores}
	}

	t.Run("cycles re-run the whole list and print markers", func(t *testing.T) {
		var count int
		var out strings.Builder
		execute := func(opt plan.Options) plan.Result {
			count++
			return plan.Result{}
		}
		code := runCycles([]plan.Options{phase("a"), phase("b")}, 3, -1, &out, execute, false, nil, nil, nil)
		if code != 0 {
			t.Fatalf("runCycles() exit code = %d, want 0\n%s", code, out.String())
		}
		if count != 6 {
			t.Errorf("executor ran %d phases, want 6 (2 phases x 3 cycles)", count)
		}
		for _, want := range []string{"=== cycle 1/3 ===", "=== cycle 3/3 ==="} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output = %q, want it to contain %q", out.String(), want)
			}
		}
	})

	t.Run("single cycle prints no cycle marker", func(t *testing.T) {
		var out strings.Builder
		execute := func(opt plan.Options) plan.Result { return plan.Result{} }
		if code := runScenario([]plan.Options{phase("a")}, &out, execute, false); code != 0 {
			t.Fatalf("runScenario() exit code = %d, want 0", code)
		}
		if strings.Contains(out.String(), "cycle") {
			t.Errorf("output = %q, want no cycle markers for a single pass", out.String())
		}
	})

	t.Run("seed reproduces the order, unseeded keeps it", func(t *testing.T) {
		opts := []plan.Options{phase("p1"), phase("p2"), phase("p3"), phase("p4"), phase("p5"), phase("p6")}
		order := func(seed int64) []string {
			var got []string
			execute := func(opt plan.Options) plan.Result {
				got = append(got, opt.Phase)
				return plan.Result{}
			}
			runCycles(opts, 2, seed, io.Discard, execute, false, nil, nil, nil)
			return got
		}

		if seq := order(-1); !reflect.DeepEqual(seq, []string{"p1", "p2", "p3", "p4", "p5", "p6", "p1", "p2", "p3", "p4", "p5", "p6"}) {
			t.Errorf("unseeded order = %v, want the configured order every cycle", seq)
		}
		first, second := order(42), order(42)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("same seed gave %v then %v, want a reproducible order", first, second)
		}
		if reflect.DeepEqual(first[:6], order(-1)[:6]) {
			t.Errorf("seeded order = %v, want it shuffled", first[:6])
		}
	})

	t.Run("blacklist carries across cycles", func(t *testing.T) {
		opts := []plan.Options{
			phase("first", 0, 1, 2, 3),
			phase("second", 2, 3),
		}
		var gotCores [][]int
		execute := func(opt plan.Options) plan.Result {
			gotCores = append(gotCores, opt.Cores)
			if opt.Phase == "first" {
				return plan.Result{Exit: 1, Aborted: true, Faults: map[int]string{3: "mismatch"}}
			}
			return plan.Result{}
		}
		code := runCycles(opts, 2, -1, io.Discard, execute, true, nil, nil, nil)
		if code != 1 {
			t.Fatalf("runCycles() exit code = %d, want 1 (faults found)", code)
		}
		want := [][]int{{0, 1, 2, 3}, {2}, {0, 1, 2}, {2}}
		if !reflect.DeepEqual(gotCores, want) {
			t.Errorf("core lists = %v, want %v (blacklist persists into cycle 2)", gotCores, want)
		}
	})

	t.Run("all_or_none pair phases are skipped when a member faulted", func(t *testing.T) {
		pair := func(name string, cores ...int) plan.Options {
			o := phase(name, cores...)
			o.AllOrNone = true
			return o
		}
		var executed []string
		var out strings.Builder
		execute := func(opt plan.Options) plan.Result {
			executed = append(executed, opt.Phase)
			if opt.Phase == "first" {
				return plan.Result{Exit: 1, Faults: map[int]string{4: "mismatch"}}
			}
			return plan.Result{}
		}
		opts := []plan.Options{pair("first", 4, 5), pair("second", 4, 6), pair("third", 5, 6)}
		code := runScenario(opts, &out, execute, true)
		if code != 1 {
			t.Fatalf("runScenario() exit code = %d, want 1 (fault found)", code)
		}
		if !reflect.DeepEqual(executed, []string{"first", "third"}) {
			t.Errorf("executed = %v, want first and third (second lost its aggressor)", executed)
		}
		if !strings.Contains(out.String(), "second — skipped") {
			t.Errorf("output = %q, want the skip reason for the broken pair", out.String())
		}
	})

	t.Run("a stop in one cycle ends the scenario", func(t *testing.T) {
		var count int
		var out strings.Builder
		execute := func(opt plan.Options) plan.Result {
			count++
			return plan.Result{Exit: 1, Aborted: true, AbortMsg: "user interrupt"}
		}
		code := runCycles([]plan.Options{phase("a"), phase("b")}, 3, -1, &out, execute, true, nil, nil, nil)
		if code != 1 {
			t.Fatalf("runCycles() exit code = %d, want 1", code)
		}
		if count != 1 {
			t.Errorf("executor ran %d phases, want 1 (abort without faults stops all cycles)", count)
		}
		if strings.Contains(out.String(), "cycle 2/3") {
			t.Errorf("output = %q, want no cycle 2 after the abort", out.String())
		}
	})
}

func TestScenarioCmdGuards(t *testing.T) {
	writeConfig := func(t *testing.T, body string) {
		t.Helper()
		dir := chdirTemp(t)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}

	t.Run(`no "phases" list is a setup error`, func(t *testing.T) {
		writeConfig(t, `{"test": "general"}`)
		if code := scenarioCmd(nil); code != 2 {
			t.Fatalf("scenarioCmd() exit code = %d, want 2", code)
		}
	})

	t.Run("an invalid phase fails validation before any load", func(t *testing.T) {
		// "nope" is not a registered test: the scenario must refuse to
		// start at all rather than run phase 1 and fail mid-scenario
		writeConfig(t, `{"phases": [{"name": "ok", "test": "general", "duration": "10ms"}, {"name": "bad", "test": "nope"}]}`)
		if code := scenarioCmd(nil); code != 2 {
			t.Fatalf("scenarioCmd() exit code = %d, want 2", code)
		}
	})

	t.Run("cycles below 1 is a setup error", func(t *testing.T) {
		writeConfig(t, `{"phases": [{"test": "general", "duration": "10ms"}]}`)
		if code := scenarioCmd([]string{"-cycles", "0"}); code != 2 {
			t.Fatalf("scenarioCmd() exit code = %d, want 2", code)
		}
	})
}

// The menu must not spin forever when stdin is exhausted (piped input,
// closed terminal): empty input at the prompt means quit, not "press
// Enter forever with the default action".
func TestMenuLoopExitsOnEOF(t *testing.T) {
	done := make(chan int, 1)
	go func() { done <- menuLoop(bufio.NewReader(strings.NewReader("")), config.Default()) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("menuLoop(EOF) exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("menuLoop spun forever on exhausted stdin")
	}
}

// -dry-run compiles generators and repeats, prints the plan, and applies
// no load at all: no log file, no journal, exit 0.
func TestScenarioDryRun(t *testing.T) {
	requireFixtureCPUs(t, 4) // the fixture names cores 0..3 across plain/burst/per-process phases
	dir := chdirTemp(t)
	cfg := `{"test":"general","workload_a":"int","duration":"10ms","log_file":"dry.log","phases":[` +
		`{"name":"plain","cores":[0,1]},` +
		`{"burst":{"cores":[2],"load":"float","load_duration":"20ms","idle_duration":"1ms"}},` +
		`{"name":"pp","per_process":true,"settle":"1ms","rotate":true,"cores":[3]}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if code := scenarioCmd([]string{"-dry-run"}); code != 0 {
		t.Fatalf("scenarioCmd(-dry-run) exit code = %d, want 0", code)
	}
	for _, name := range []string{"dry.log", "dry.log.journal.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("dry-run created %s — it must not touch the filesystem beyond config", name)
		}
	}
}

func TestPrintScenarioPlan(t *testing.T) {
	opts := []plan.Options{
		{TestName: "general", A: "int", B: "mem", Cores: []int{0, 1}, Workers: 1, Duration: 10 * time.Second},
		{TestName: "general", Phase: "pp", A: "float", PerProcess: true, Rotate: true,
			Settle: 2 * time.Second, Cores: []int{2}, Workers: 2, Duration: 5 * time.Second},
	}
	var out strings.Builder
	printScenarioPlan(&out, config.Default(), opts, 3)
	got := out.String()
	for _, want := range []string{
		"2 phase(s) x 3 cycle(s)",
		"1) phase 1: test=general a=int b=mem cores=0,1 workers=1 duration=10s mode=in-process",
		"2) pp:",
		"cores=2",
		"mode=per-process+rotate+settle2s",
		"configured load time: 15s per cycle",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan output missing %q:\n%s", want, got)
		}
	}

	// Empty core list reads "all", single cycle drops the multiplier.
	out.Reset()
	printScenarioPlan(&out, config.Default(), []plan.Options{{TestName: "general", A: "int", Duration: time.Second}}, 1)
	if !strings.Contains(out.String(), "cores=all") || strings.Contains(out.String(), " x ") {
		t.Errorf("plan output = %q, want cores=all and no cycle multiplier", out.String())
	}
}

// The dry-run journal estimate must reflect the beat cadence: beats
// dominate long runs, and rotation keeps children alive across the whole
// rotation, not just the phase duration.
func TestJournalEstimate(t *testing.T) {
	cases := []struct {
		name        string
		opts        []plan.Options
		cycles      int
		numCPU      int
		wantRecords int
		wantBeats   int
	}{
		{
			name:   "default 1s cadence",
			opts:   []plan.Options{{Cores: []int{0, 1}, Duration: 10 * time.Second}},
			cycles: 1, numCPU: 32,
			// 2 + 2x2 + 1 tele + 2x10 beats = 27
			wantRecords: 27, wantBeats: 20,
		},
		{
			name:   "5s cadence shrinks beats",
			opts:   []plan.Options{{Cores: []int{0, 1}, Duration: 10 * time.Second, BeatEvery: 5 * time.Second}},
			cycles: 1, numCPU: 32,
			wantRecords: 11, wantBeats: 4, // 2 + 4 + 1 + 2x2
		},
		{
			name:   "all cores use numCPU",
			opts:   []plan.Options{{Duration: 3 * time.Second, PerProcess: true}},
			cycles: 1, numCPU: 4,
			// 2 + 2x4 + 4 tele + 4x3 beats = 26
			wantRecords: 26, wantBeats: 12,
		},
		{
			name:   "rotation spans the whole rotation",
			opts:   []plan.Options{{Cores: []int{0, 1}, Duration: 2 * time.Second, PerProcess: true, Rotate: true}},
			cycles: 1, numCPU: 32,
			// span = 2x2+2 = 6s -> 6 beats/core: 2 + 4 + 2 + 12 = 20
			wantRecords: 20, wantBeats: 12,
		},
		{
			name:   "cycles multiply everything",
			opts:   []plan.Options{{Cores: []int{0}, Duration: 1 * time.Second}},
			cycles: 100, numCPU: 32,
			// one cycle: 2 + 2 + 1 + 1 = 6
			wantRecords: 600, wantBeats: 100,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			records, beats, _ := journalEstimate(tt.opts, tt.cycles, tt.numCPU)
			if records != tt.wantRecords || beats != tt.wantBeats {
				t.Errorf("journalEstimate = (%d, %d), want (%d, %d)",
					records, beats, tt.wantRecords, tt.wantBeats)
			}
		})
	}
}

func TestCompileScenario(t *testing.T) {
	base := config.Default()
	base.Test = "general"
	base.WorkloadA = "int"
	base.Duration = "10ms"

	t.Run("generators and repeats expand before any load", func(t *testing.T) {
		cfg := base
		cfg.Phases = []config.Phase{
			{Name: "b", Burst: &config.Burst{Cores: []int{0, 1}, Repeat: 2, IdleDuration: "1ms"}},
			{Name: "plain", Repeat: 3},
		}
		opts, _, err := compileScenario(cfg)
		if err != nil {
			t.Fatalf("compileScenario: %v", err)
		}
		if len(opts) != 7 { // burst: 2 cores x 2 cycles = 4; plain: 3 repeats
			t.Fatalf("compiled %d phases, want 7 (4 burst + 3 plain)", len(opts))
		}
		for _, opt := range opts {
			if opt.Phase == "" {
				t.Errorf("compiled phase %v lost its label", opt.TestName)
			}
		}
	})

	t.Run("an invalid phase is reported with its index", func(t *testing.T) {
		cfg := base
		cfg.Phases = []config.Phase{{Name: "ok"}, {Name: "bad", Test: "nope"}}
		if _, _, err := compileScenario(cfg); err == nil {
			t.Fatal("compileScenario: expected error for unknown test in phase 2")
		}
	})
}

// The interactive scenario session must run the same pipeline: scripted
// input either cancels without touching the filesystem, or compiles the
// scenario it described (execution of real phases is covered by the
// subprocess and runCycles suites; spawning children from a test binary
// here just adds environment flakiness).
func TestScenarioSession(t *testing.T) {
	dir := chdirTemp(t)
	cfg := config.Default()
	cfg.Test = "general"
	cfg.WorkloadA = "int"
	cfg.Duration = "10ms"
	cfg.LogFile = "sess.log"
	in := bufio.NewReader(strings.NewReader("1\n0\n1\n10ms\n1ms\n1\nn\nn\n"))
	code := scenarioSession(in, newScreenUI(os.Stdout), cfg)
	if code != 0 {
		t.Fatalf("scenarioSession(cancel) exit code = %d, want 0", code)
	}
	for _, name := range []string{"sess.log", "sess.log.journal.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("cancelled session created %s", name)
		}
	}
}
