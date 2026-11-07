package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// Every built-in preset must compile through the SAME pipeline a
// hand-written phases list uses, on the live machine: the preset is a
// phase producer, not an execution path.
func TestPresetsCompile(t *testing.T) {
	cfg := config.Default()
	cfg.Duration = "10ms"
	cfg.Memory = "128MiB" // ram phases compile; the engine itself runs at execute time

	for _, p := range presets {
		t.Run(p.Name, func(t *testing.T) {
			scenario := cfg
			scenario.Phases = p.Phases(cfg)
			if len(scenario.Phases) == 0 {
				t.Fatalf("preset %q produced no phases", p.Name)
			}
			opts, _, err := compileScenario(scenario)
			if err != nil {
				t.Fatalf("compileScenario(preset %q): %v", p.Name, err)
			}
			if len(opts) == 0 {
				t.Fatalf("preset %q compiled to no options", p.Name)
			}
			seen := map[string]bool{}
			for _, opt := range opts {
				if opt.Phase == "" {
					t.Errorf("preset %q produced an unlabelled phase", p.Name)
				}
				if seen[opt.Phase] {
					t.Errorf("preset %q produced duplicate phase label %q", p.Name, opt.Phase)
				}
				seen[opt.Phase] = true
			}
		})
	}

	t.Run("preset shapes carry their purpose", func(t *testing.T) {
		p, _ := lookupPreset("transitions")
		ph := p.Phases(cfg)
		if len(ph) != 1 || ph[0].Bursts != 5 || !ph[0].PerProcess {
			t.Errorf("transitions preset = %+v, want one per-process bursts:5 phase", ph)
		}
		if ph[0].A != "int" {
			t.Errorf("transitions preset load = %q, want the CPU int workload", ph[0].A)
		}
		// An unrelated memory budget must not turn the transitions
		// profile into a RAM test (audit A05): same phases either way.
		bare := cfg
		bare.Memory = ""
		if phBare := p.Phases(bare); phBare[0].A != ph[0].A {
			t.Errorf("transitions algorithm changed with the memory setting: %q vs %q", ph[0].A, phBare[0].A)
		}

		p, _ = lookupPreset("mixed-validation")
		ph = p.Phases(cfg)
		var names []string
		for _, x := range ph {
			names = append(names, x.Name)
		}
		if strings.Join(names, ",") != "mix-cpu,mix-ram,mix-trans,mix-ramp" {
			t.Errorf("mixed preset phases = %v, want cpu+ram+transitions+ramp", names)
		}

		// The producer NEVER drops the RAM phases (audit A05): a
		// budget-less machine is refused at the entry points (see
		// TestPresetBudgetGate), never handed a quietly CPU-shrunken
		// "mixed" plan.
		ram := 0
		for _, x := range p.Phases(bare) {
			if x.A == "ram-constant" {
				ram++
			}
		}
		if ram != 2 {
			t.Errorf("mixed preset without a budget carries %d ram phases, want the promised 2 (refusal belongs to the entry point)", ram)
		}
	})
}

// A05: presets that promise memory verification are refused BEFORE
// any load when no budget exists: the CLI with exit 2 and an
// actionable reason, the wizard by going back to the preset list.
// The alternative was a "mixed" run that quietly deleted its RAM.
func TestPresetBudgetGate(t *testing.T) {
	dir := chdirTemp(t)
	body := `{"duration":"10ms","phases":[{"name":"mine","test":"general","a":"int","duration":"10ms"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	code := scenarioCmd([]string{"-preset", "mixed-validation", "-dry-run"})
	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if code != 2 {
		t.Fatalf("budget-less mixed exit = %d, want 2:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "memory budget") {
		t.Errorf("refusal does not name the missing budget:\n%s", buf.String())
	}

	// The wizard: mixed-validation (sorted list position 3) is sent
	// back to the preset list with the reason; q then cancels.
	// Nothing runs and nothing is saved.
	in := bufio.NewReader(strings.NewReader("5\n3\nq\n"))
	if code := scenarioSession(in, newScreenUI(os.Stdout), config.Default()); code != 0 {
		t.Errorf("wizard refused-preset then q = %d, want 0 (cancel, not a fatal exit)", code)
	}
}

func TestPresetLookup(t *testing.T) {
	if _, ok := lookupPreset("nope"); ok {
		t.Error("lookupPreset(nope) = ok, want not found")
	}
	names := presetNames()
	if len(names) != len(presets) {
		t.Fatalf("presetNames = %v, want one per preset", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("presetNames not sorted: %v", names)
		}
	}
	for _, want := range []string{"cpu-baseline", "ram-baseline", "ram-quick", "ram-standard", "ram-extended", "core-cycle", "transitions", "mixed-validation"} {
		if _, ok := lookupPreset(want); !ok {
			t.Errorf("preset %q missing", want)
		}
	}
}

// The CLI entry: an unknown preset is refused with the list (exit 2,
// before any load); a known preset overrides the config phases and its
// dry-run plan prints.
func TestScenarioPresetFlag(t *testing.T) {
	dir := chdirTemp(t)
	body := `{"duration":"10ms","phases":[{"name":"mine","test":"general","a":"int","duration":"10ms"}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if code := scenarioCmd([]string{"-preset", "nope"}); code != 2 {
		t.Errorf("unknown preset exit = %d, want 2", code)
	}

	// A known preset through dry-run: exit 0 and the preset's phases,
	// not the config's "mine" (the override is visible, not merged).
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	code := scenarioCmd([]string{"-preset", "cpu-baseline", "-dry-run"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if code != 0 {
		t.Fatalf("preset dry-run exit = %d, want 0:\n%s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "cpu-cycle-p") || !strings.Contains(out, "cpu-all") {
		t.Errorf("preset dry-run plan missing preset phases:\n%s", out)
	}
	if strings.Contains(out, "mine") {
		t.Errorf("preset dry-run kept the config's own phases:\n%s", out)
	}
}
