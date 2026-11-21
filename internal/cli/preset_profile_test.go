package cli

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// captureStderr runs fn with os.Stderr redirected and returns what it
// printed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The preset entry refuses an unavailable card with the card's own
// reason: the menu may offer what the build can honestly run, and an
// unavailable profile states WHY instead of being quietly swapped.
func TestStartPresetProfileRefusesUnavailable(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("mix-simultaneous")
	if !ok {
		t.Fatal("frozen card set lost mix-simultaneous")
	}
	var code int
	out := captureStderr(t, func() {
		code = startPresetProfile(bufio.NewReader(strings.NewReader("")), newScreenUI(os.Stdout), config.Default(), card)
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(out, card.Reason) {
		t.Errorf("refusal did not state the card's reason:\n%s", out)
	}
}

// A session-shape card is not a preset: the entry says so instead of
// guessing which preset resembles the goal.
func TestStartPresetProfileRefusesWrongShape(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("cpu-all")
	if !ok {
		t.Fatal("frozen card set lost cpu-all")
	}
	var code int
	out := captureStderr(t, func() {
		code = startPresetProfile(bufio.NewReader(strings.NewReader("")), newScreenUI(os.Stdout), config.Default(), card)
	})
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(out, "not a preset") {
		t.Errorf("refusal wording:\n%s", out)
	}
}

// captureStdout (in lastresult_test.go) collects what the dialog
// prints: the budget prompt speaks on stdout, not stderr.

// A preset that promises memory verification asks for the budget in
// the dialog when config has none - configureSession's rule, finally
// applied to the menu's profile cards too. The old behavior refused
// with "set memory in config.json ... or pick another profile" and
// the menu then closed the program anyway, contradicting its own
// offer (user: "почему в других ситуациях [auto] работает, а тут нет
// такого?"). Now: q cancels with nothing saved, an invalid answer
// re-prompts with the reason, and an accepted answer reaches the
// confirm screen (proven by cancelling there) instead of dying in a
// preflight refusal. The answer belongs to this run only.
func TestStartPresetProfileAsksMemoryBudget(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("ram-extended")
	if !ok {
		t.Fatal("frozen card set lost ram-extended")
	}
	cfg := config.Default()
	cfg.Memory = ""

	var code int
	out := captureStdout(t, func() {
		code = startPresetProfile(bufio.NewReader(strings.NewReader("q\n")), newScreenUI(os.Stdout), cfg, card)
	})
	if code != 0 {
		t.Fatalf("budget-cancel exit = %d, want 0", code)
	}
	if !strings.Contains(out, "memory budget") || !strings.Contains(out, "Run cancelled") {
		t.Errorf("budget dialog wording:\n%s", out)
	}

	out = captureStdout(t, func() {
		code = startPresetProfile(bufio.NewReader(strings.NewReader("banana\n\nn\n")), newScreenUI(os.Stdout), cfg, card)
	})
	if code != 0 {
		t.Fatalf("confirm-cancel exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Invalid memory budget") {
		t.Errorf("invalid budget must re-prompt with the reason:\n%s", out)
	}
	if !strings.Contains(out, "Run cancelled") {
		t.Errorf("never reached the confirm screen:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}
	if _, err := os.Stat(lastResultPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset armed a receipt")
	}
}

// Cancelling at the confirm screen starts no load and saves nothing:
// the recipe is only displaced by a request the user actually
// launched.
func TestStartPresetProfileCancelSavesNothing(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("cpu-transitions")
	if !ok {
		t.Fatal("frozen card set lost cpu-transitions")
	}
	cfg := config.Default()
	in := bufio.NewReader(strings.NewReader("n\n"))
	code := startPresetProfile(in, newScreenUI(os.Stdout), cfg, card)
	if code != 0 {
		t.Fatalf("cancel exit = %d, want 0", code)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}
	if _, err := os.Stat(lastResultPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset armed a receipt")
	}
}

// R27 (owner complaint 2026-11-12: the mixed/complex preset could not
// be given a time or a cycle count): the preset path now asks both,
// and the answers reach the plan screen - duration fills the
// "empty = config duration" slot for the generated phases, cycles
// repeat the whole list. q at the cycles prompt cancels with nothing
// saved, exactly like every other wizard step.
func TestStartPresetProfileAsksDurationAndCycles(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("ram-extended")
	if !ok {
		t.Fatal("frozen card set lost ram-extended")
	}
	cfg := config.Default()
	cfg.Memory = "64MiB" // never allocated: this test always cancels before load

	out := captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("1m\n2\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Repetitions of the whole phase list") {
		t.Errorf("cycles prompt missing:\n%s", out)
	}
	if !strings.Contains(out, "x 2 cycle(s)") {
		t.Errorf("asked cycles did not reach the plan screen:\n%s", out)
	}
	if !strings.Contains(out, "planned clock windows across 2 cycle(s)") {
		t.Errorf("plan screen never stated the whole multi-cycle clock up front:\n%s", out)
	}
	if !strings.Contains(out, "1m0s") {
		t.Errorf("asked duration did not reach the phase lines:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}

	// q at the new cycles step is a clean cancel: no recipe, no
	// receipt, no load.
	out = captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("1m\nq\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("cycles-cancel exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Run cancelled. No load was started.") {
		t.Errorf("cycles-cancel wording:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("cycles-cancel saved a recipe")
	}
}

// The duration answer honors fixed-time promises: quick-check's 10s
// phases are the preset's stated promise and the knob never overwrites
// them - the override only fills phases that inherit the config
// duration. (Honesty rule: the screen's promise survives the form.)
func TestStartPresetProfileKnobKeepsFixedPromises(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("quick-check")
	if !ok {
		t.Fatal("frozen card set lost quick-check")
	}
	cfg := config.Default()
	out := captureStdout(t, func() {
		code := startPresetProfile(bufio.NewReader(strings.NewReader("5m\n\nn\n")), newScreenUI(os.Stdout), cfg, card)
		if code != 0 {
			t.Fatalf("confirm-cancel exit = %d, want 0", code)
		}
	})
	if strings.Contains(out, "duration=5m0s") {
		t.Errorf("the knob overwrote quick-check's fixed 10s promises:\n%s", out)
	}
	if strings.Count(out, "duration=10s") < 2 {
		t.Errorf("quick-check's promised 10s phases vanished from the plan:\n%s", out)
	}
}

// The shared scenario recipe stamps the request id and the catalog
// card behind the preset - the same identity the receipt binds to.
func TestScenarioRecipeIdentity(t *testing.T) {
	cfg := config.Default()
	lr := scenarioRecipe(cfg, 2, nil, true, "core-cycle")
	if lr.Kind != lastRunScenario || lr.Cycles != 2 || !lr.ContinueOnFault {
		t.Errorf("request parts lost: %+v", lr)
	}
	if lr.RecipeID == "" {
		t.Error("recipe id not stamped at construction")
	}
	if lr.Preset != "core-cycle" || lr.ProfileID != "cpu-core-cycle" || lr.ProfileVersion != "v1" {
		t.Errorf("preset provenance = %q/%q/%q", lr.Preset, lr.ProfileID, lr.ProfileVersion)
	}
	// A preset without a card (or no preset at all) stays unknown.
	if lr2 := scenarioRecipe(cfg, 1, nil, false, ""); lr2.ProfileID != "" || lr2.RecipeID == "" {
		t.Errorf("raw phase list fabricated provenance: %+v", lr2)
	}
}
