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
