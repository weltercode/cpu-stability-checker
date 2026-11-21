package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// R30 (owner question 2026-11-12: "can we make a test at the
// TestMem5 level?"). The ram-sustained card packages a sequence whose
// STRUCTURE mirrors the publicly disclosed TestMem5 Extreme1 layout
// (data phases, repeated verified-transfer interludes, locality
// contrasts, kernels repeated inside one cycle) using our own
// kernels. These pins hold the promise shape: the sequence is static
// and reproducible, every phase's length stays the operator's answer
// (no fixed short interlude would claim windows our kernels cannot
// earn at GiB scale), and the card text never reads as a TM5 parity
// claim.

func TestRamSustainedPresetStructure(t *testing.T) {
	p, ok := lookupPreset("ram-sustained")
	if !ok {
		t.Fatal("ram-sustained preset missing")
	}
	if !p.NeedsMemory {
		t.Error("ram-sustained must declare NeedsMemory (refusal before load, never dropped phases)")
	}
	if p.SuggestCycles != 3 {
		t.Errorf("SuggestCycles = %d, want 3 (the public Extreme1 config's cycle count, as a default answer only)", p.SuggestCycles)
	}

	cfg := config.Default()
	first := p.Phases(cfg)
	second := p.Phases(cfg)
	if len(first) != 18 {
		t.Fatalf("cycle length = %d, want 18", len(first))
	}
	// Static and byte-reproducible: a detection sequence's value is
	// the adjacency someone chose; planhash and replay lean on the
	// list being identical call after call.
	for i := range first {
		if first[i].Name != second[i].Name || first[i].A != second[i].A ||
			first[i].Duration != second[i].Duration || first[i].PerProcess != second[i].PerProcess {
			t.Fatalf("phase %d differs between calls: the sequence is not static", i)
		}
	}

	seen := map[string]int{}
	copies := 0
	for i, ph := range first {
		if ph.Name == "" || ph.Test != "general" || !ph.PerProcess {
			t.Errorf("phase %d (%q) must be a named, per-process general phase", i, ph.Name)
		}
		if ph.Duration != "" {
			t.Errorf("phase %q carries a fixed duration %q: every window is the asked duration here - a fixed short window at GiB-region scale would close zero verified passes and honestly self-report partial every time (mix-trans lesson)", ph.Name, ph.Duration)
		}
		wl, wok := workload.Get(ph.A)
		if !wok || !wl.RequiresMemory {
			t.Errorf("phase %q names %q: every sustained phase must be a memory-verification kernel", ph.Name, ph.A)
		}
		seen[ph.A]++
		if strings.HasSuffix(ph.Name, "-copy") {
			if ph.A != "ram-copy" {
				t.Errorf("interlude %q carries %q: the named copy interludes must run the verified copy kernel", ph.Name, ph.A)
			}
			copies++
		}
	}
	if copies < 5 {
		t.Errorf("copy interludes = %d, want >=5 (the Extreme1 structure inserts its verified-transfer kernels repeatedly, not round-robin)", copies)
	}
	repeats := 0
	for _, n := range seen {
		if n >= 2 {
			repeats++
		}
	}
	if repeats < 4 {
		t.Errorf("kernels repeated inside the cycle = %d, want >=4 (public Extreme1 repeats tests within one cycle)", repeats)
	}
}

// The card text is the operator's contract: it may say the sequence
// is STRUCTURE-inspired by the public TM5 config, and it must never
// read as a clone, a class-parity claim or a detection promise.
func TestRamSustainedCardHonesty(t *testing.T) {
	card, ok := catalog.Get("ram-sustained")
	if !ok {
		t.Fatal("frozen card set lost ram-sustained")
	}
	if card.Shape != catalog.ShapePreset || card.Preset != "ram-sustained" ||
		card.Availability != catalog.Available || !card.NeedsMemory {
		t.Errorf("card identity broken: %+v", card)
	}
	p, _ := lookupPreset("ram-sustained")
	text := strings.ToLower(card.Summary + " " + card.Details + " " + p.Description)
	for _, must := range []string{"not a tm5 clone", "unmeasured", "inspired"} {
		if !strings.Contains(text, must) {
			t.Errorf("card text lost %q - the structure-inspiration and no-parity caveats are load-bearing:\n%s", must, text)
		}
	}
	for _, never := range []string{"as strong as", "tm5-class", "better than tm5", "catches everything", "same detection"} {
		if strings.Contains(text, never) {
			t.Errorf("card text claims %q - detection parity is an unmeasured, destructive-level question:\n%s", never, text)
		}
	}
}

// SuggestCycles is a DEFAULT, never a removed question: Enter takes 3,
// a typed number overrides it, and the plan screen shows the real
// count. ram-extended (no suggestion) keeps the generic default of 1.
func TestRamSustainedCyclesDefaultIsAnAnswerNotARemovedQuestion(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("ram-sustained")
	if !ok {
		t.Fatal("frozen card set lost ram-sustained")
	}
	cfg := config.Default()
	cfg.Memory = "64MiB" // never allocated: these flows cancel before load

	out := captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("\n\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Repetitions of the whole phase list [3]") {
		t.Errorf("cycles prompt did not show the card's suggested default 3:\n%s", out)
	}
	if !strings.Contains(out, "x 3 cycle(s)") {
		t.Errorf("Enter at the cycles prompt did not take the card's suggested 3:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}

	out = captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("\n7\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "x 7 cycle(s)") {
		t.Errorf("typed cycles did not override the suggestion:\n%s", out)
	}

	// The suggestion belongs to this card only: ram-extended's prompt
	// must still DEFAULT to 1 (the plan screen says nothing extra at
	// 1 cycle, the prompt's bracket is the visible default).
	ext, ok := catalog.Get("ram-extended")
	if !ok {
		t.Fatal("frozen card set lost ram-extended")
	}
	out = captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("\n\nn\n")), newScreenUI(os.Stdout), cfg, ext)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Repetitions of the whole phase list [1]") {
		t.Errorf("ram-extended default cycles drifted from 1:\n%s", out)
	}
}

// The dry-run entry point proves the new preset rides the SAME
// compileScenario pipeline: an ordinary phases-list plan prints, with
// the 18 named phases and no special execution branch.
func TestRamSustainedDryRunRidesTheSamePipeline(t *testing.T) {
	dir := chdirTemp(t)
	body, err := json.Marshal(map[string]string{"duration": "10ms", "memory": "64MiB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := scenarioCmd([]string{"-preset", "ram-sustained", "-dry-run"})
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if code != 0 {
		t.Fatalf("ram-sustained dry-run exit = %d, want 0:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, must := range []string{"sust-01-constant", "sust-16-dwell", "sust-18-moving"} {
		if !strings.Contains(out, must) {
			t.Errorf("dry-run plan missing %q:\n%s", must, out)
		}
	}
}
