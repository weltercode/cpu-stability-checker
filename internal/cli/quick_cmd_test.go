package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// R17 composition pin: the quick-check preset is bounded by its OWN
// durations (never borrowed from config), needs no memory budget (it
// must answer on any machine), and its description names in advance
// what it does NOT cover - the honesty vocabulary's "no silent
// scope" rule lives in the composition itself, not in a footnote.
func TestQuickCheckPresetComposition(t *testing.T) {
	p, ok := lookupPreset("quick-check")
	if !ok {
		t.Fatal("quick-check preset missing")
	}
	if p.NeedsMemory {
		t.Error("quick-check marked NeedsMemory: it must answer on a machine with no config and no budget")
	}
	d := p.Description
	for _, want := range []string{"RAM", "soak", "not a stability verdict"} {
		if !strings.Contains(d, want) {
			t.Errorf("quick-check description must name %q (what it is NOT): %q", want, d)
		}
	}

	ph := p.Phases(config.Default())
	if len(ph) != 3 {
		t.Fatalf("quick-check phases = %d, want exactly 3: %+v", len(ph), ph)
	}
	wantPhases := []struct {
		name, a, dur string
		bursts       int
	}{
		{"quick-int", "int", "10s", 0},
		{"quick-float", "float", "10s", 0},
		{"quick-trans", "int", "", 3},
	}
	for i, w := range wantPhases {
		got := ph[i]
		if got.Name != w.name || got.A != w.a {
			t.Errorf("phase %d = %+v, want %s/%s", i, got, w.name, w.a)
		}
		if got.PerProcess != true {
			t.Errorf("phase %q: quick-check runs per-process pinned children, like every other CPU preset", got.Name)
		}
		if got.Duration != w.dur {
			t.Errorf("phase %q duration = %q, want %q (the bound is the preset's own promise)", got.Name, got.Duration, w.dur)
		}
		if got.Bursts != w.bursts {
			t.Errorf("phase %q bursts = %d, want %d", got.Name, got.Bursts, w.bursts)
		}
	}
	// The whole window must stay small and stated: three phases with
	// no Repeat, no curve, no cycle walk.
	for _, got := range ph {
		if got.Repeat != 0 || got.Cycle != nil || got.Curve != nil {
			t.Errorf("phase %q grew beyond the promised quick window: repeat/cycle/curve are not part of quick-check", got.Name)
		}
	}
}

// The menu half of R17: the SAME preset is reached through a
// ShapePreset catalog card, so the menu entry rides the existing
// card->startProfile->startPresetProfile routing - no second
// executor, and recipe provenance stamps the card id for free.
func TestQuickCheckCardRoutesThroughMenuPresetPath(t *testing.T) {
	card, ok := catalog.Get("quick-check")
	if !ok {
		t.Fatal("quick-check card missing: the menu entry IS the card")
	}
	if card.Shape != catalog.ShapePreset || card.Preset != "quick-check" {
		t.Errorf("quick-check card = shape %q preset %q, want ShapePreset/quick-check so startPresetProfile drives it", card.Shape, card.Preset)
	}
	if card.Category != catalog.CPU {
		t.Errorf("quick-check category = %q, want CPU (it verifies no RAM engine)", card.Category)
	}
	if card.Availability != catalog.Available {
		t.Errorf("quick-check availability = %q (%q): it needs nothing, so it must be available", card.Availability, card.Reason)
	}
	if !strings.Contains(strings.ToLower(card.Summary), "not a stability verdict") {
		t.Errorf("quick-check card summary must state the scope limit up front: %q", card.Summary)
	}
	cards := categoryCards(catalog.CPU, catalog.Normal)
	if len(cards) == 0 || cards[0].ID != "quick-check" {
		t.Errorf("quick-check should lead the CPU category screen (Order 5); first card is %q", cards[0].ID)
	}
	if p := profileForPreset("quick-check"); p.ID != "quick-check" {
		t.Errorf("profileForPreset(quick-check) = %q: a saved quick run must know its own card", p.ID)
	}
}

// Dry-run half: composition is inspectable WITHOUT arming anything -
// no attempt, no lastresult, no journal files. This is what makes
// "bounded and cancellable" verifiable by reading, not trusting.
func TestQuickDryRunArmsNothing(t *testing.T) {
	dir := chdirTemp(t)
	before := lastAttemptID()
	code, out := captureAnswer(t, func() int { return quickCmd([]string{"-dry-run"}) })
	if code != 0 {
		t.Fatalf("quick -dry-run exit = %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"quick-int", "quick-float", "quick-trans", "Dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("quick -dry-run output missing %q:\n%s", want, out)
		}
	}
	if lastAttemptID() != before {
		t.Error("quick -dry-run armed an attempt: a dry run must load nothing")
	}
	if _, err := os.Stat(lastResultPath); !os.IsNotExist(err) {
		t.Error("quick -dry-run wrote a lastresult: nothing may be saved")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "journal.jsonl") {
			t.Errorf("quick -dry-run created journal file %q", e.Name())
		}
	}
}

// R15 applies to this new entry point from its first day: a broken
// config.json is a refusal, never silent defaults under a "quick"
// label.
func TestQuickRefusesBrokenConfig(t *testing.T) {
	dir := chdirTemp(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	errOut := captureStderr(t, func() {
		code = quickCmd([]string{"-yes"})
	})
	if code != 2 {
		t.Fatalf("quick over broken config: exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "configuration error") {
		t.Errorf("refusal must name the config failure:\n%s", errOut)
	}
}

// Without -yes and without a confirm, NOTHING loads: the quick label
// does not make a run less consented than any other load. (stdin at
// NUL reads as EOF, the same answer as "user typed nothing".)
func TestQuickNeedsConfirm(t *testing.T) {
	dir := chdirTemp(t)
	oldIn := os.Stdin
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = devNull
	t.Cleanup(func() {
		os.Stdin = oldIn
		devNull.Close()
	})
	before := lastAttemptID()
	code, out := captureAnswer(t, func() int { return quickCmd(nil) })
	if code != 0 {
		t.Fatalf("quick declined-confirm exit = %d, want 0 (a cancel is not a failure): %s", code, out)
	}
	if !strings.Contains(out, "Run cancelled. No load was started.") {
		t.Errorf("missing cancel note:\n%s", out)
	}
	if lastAttemptID() != before {
		t.Error("a declined confirm armed an attempt")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Error("quick wrote config.json")
	}
}

// A LIVE quickCmd run cannot be pinned from this package: the
// per-process worker re-exec wiring (selfExecutable +
// TestWorkerHelperProcess) lives in the subprocess package, whose
// tests prove the pinned-child tail live; nothing in cli may fake a
// second executor to make a test pass. What IS pinned here is the
// quickCmd glue around that shared tail: composition, refusals,
// consent, dry-run inertness, and the closing block's honest shape.
// The one real live verification of `cpu-stability quick -yes` was
// done with the BUILT binary on the owner machine (2026-11-09) and
// its outcome is recorded in HANDOFF - named, not silently absent.
