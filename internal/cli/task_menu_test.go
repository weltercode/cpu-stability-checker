package cli

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// captureStdoutAndRun redirects os.Stdout around fn and returns what
// was printed. The pipe must be drained WHILE fn runs: a menu flow
// that prints more than the pipe buffer would otherwise deadlock in
// Write before it can read the next scripted answer.
func captureStdoutAndRun(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	read := make(chan string, 1)
	go func() {
		data, err := io.ReadAll(r)
		if err != nil {
			t.Errorf("read captured stdout: %v", err)
		}
		read <- string(data)
	}()
	fn()
	w.Close()
	os.Stdout = old
	return <-read
}

// R39 (owner-named menu separation): the main screen is split by
// effort with labeled sections - fast at the top, the full suites,
// evidence and tools, Advanced tools; the quick check sits at 2 with
// its honesty guard right under its name.
func TestMainMenuSectionsR39(t *testing.T) {
	chdirTemp(t)
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"Fast — a bounded pass for first words (minutes):",
		"2) Quick check (bounded) — ~21 s planned load + setup",
		"a clean word here is NOT a stability verdict",
		"Full test suites — longer runs, deeper evidence:",
		"3) CPU check",
		"4) RAM check",
		"5) Mixed CPU + RAM tests",
		"Evidence and tools — what was recorded, and the detailed controls:",
		"6) Session history and reports",
		"7) View findings - none unread",
		"8) Advanced tools (scenario/plan runs, diagnostics, About)",
		"0) Quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("main menu lost %q:\n%s", want, out)
		}
	}
}

// The promoted item 2 is the SAME card route the category screen
// uses: it opens the quick-check plan (its phase names appear), and
// "n" at the confirm arms nothing and saves nothing.
func TestQuickCheckAtTwoUsesOneExecutor(t *testing.T) {
	chdirTemp(t)
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("2\n\n\nn\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	if !strings.Contains(strings.ToLower(out), "quick check") {
		t.Errorf("item 2 did not open the quick-check plan:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled quick-check at the main menu saved a recipe")
	}
}

// Enter on a fresh machine must not start anything (menu plan section
// 1: the shortcut only exists over a real recipe); the screen says
// there is no previous test and the menu waits.
func TestEnterWithoutRecipeStartsNothing(t *testing.T) {
	chdirTemp(t)
	done := make(chan int, 1)
	go func() { done <- menuLoop(bufio.NewReader(strings.NewReader("\n\n0\n")), config.Default()) }()
	code := <-done
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("Enter without a recipe started or saved a run")
	}
}

// ram-extended became a real preset profile in P07f (it used to sit
// in this category as an honest "not yet"): selecting it now shows
// the phase plan, and cancelling at the confirm must save nothing.
// The category-level unavailable-refusal coverage now lives with the
// MIX cards (TestMixMenuHonestAvailability).
func TestRamExtendedPresetSelectableCancelSavesNothing(t *testing.T) {
	chdirTemp(t)
	cfg := config.Default()
	cfg.Memory = "128MiB" // the preset's promise, satisfiable
	card, ok := catalog.Get("ram-extended")
	if !ok {
		t.Fatal("frozen card set lost ram-extended")
	}
	if card.Availability != catalog.Available || card.Preset != "ram-extended" {
		t.Fatalf("ram-extended = %s preset=%q, want an available preset card after P07f", card.Availability, card.Preset)
	}
	// "4" = RAM category (R39 layout), "5" = ram-extended (fifth by
	// Order), Enter on its card screen (R40) continues into the run
	// flow, the R27 run-shape steps take their defaults (Enter,
	// Enter), "n" cancels at the confirm, q back, 0 quit.
	out := captureStdoutAndRun(t, func() {
		code := menuLoop(bufio.NewReader(strings.NewReader("4\n5\n\n\n\nn\nq\n0\n")), cfg)
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "ram-extended") {
		t.Errorf("selection did not show the ram-extended plan:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("cancelling the ram-extended preset at the confirm saved a recipe")
	}
}

// The MIX category shows the two honest "not yet" goals and refuses
// them by Reason, while the sequential comprehensive preset stays
// selectable: cancelling it at the confirm saves nothing.
func TestMixMenuHonestAvailability(t *testing.T) {
	chdirTemp(t)
	cfg := config.Default()
	cfg.Memory = "128MiB" // the comprehensive preset's promise, satisfiable
	sim, _ := catalog.Get("mix-simultaneous")
	ramTr, _ := catalog.Get("mix-transitions-on-ram")
	out := captureStdoutAndRun(t, func() {
		// "5" = MIX (R39 layout); "2" simultaneous -> card screen
		// states the Reason, Enter clears the wait (never consent);
		// "3" transitions-on-ram -> same; "1" comprehensive -> Enter
		// on its card screen (R40) continues -> R27 steps take
		// defaults (Enter, Enter), "n" cancels at confirm; q back;
		// 0 quit.
		code := menuLoop(bufio.NewReader(strings.NewReader("5\n2\n\n3\n\n1\n\n\n\nn\nq\n0\n")), cfg)
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, sim.Reason) || !strings.Contains(out, ramTr.Reason) {
		t.Errorf("unavailable MIX goals did not state their reasons:\n%s", out)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled preset saved a recipe")
	}
}

// A session-shape card cancels cleanly: the collector's answers die
// at the confirm screen without saving a recipe or arming a receipt.
func TestSessionCardCancelSavesNothing(t *testing.T) {
	chdirTemp(t)
	card, ok := catalog.Get("cpu-all")
	if !ok {
		t.Fatal("frozen card set lost cpu-all")
	}
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}},
	}}
	in := bufio.NewReader(strings.NewReader("0\n1ms\n1\nn\n")) // cores, duration, passes, no confirm
	code := startSessionProfile(in, newScreenUI(os.Stdout), config.Default(), topology, card)
	if code != 0 {
		t.Fatalf("cancel exit = %d, want 0", code)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Error("a cancelled card launch saved a recipe")
	}
	if _, err := os.Stat(lastResultPath); !os.IsNotExist(err) {
		t.Error("a cancelled card launch armed a receipt")
	}
}

// A confirmed card launch runs through the one session pipeline,
// stamps the card as fact (not reverse-inferred) and leaves a bound
// final receipt - the same promises the wizard has, verified for the
// card path. 1ms on one core: real pipeline, no real stress.
func TestSessionCardRunStampsCardAndReceipt(t *testing.T) {
	chdirTemp(t)
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
	card, ok := catalog.Get("cpu-per-core")
	if !ok {
		t.Fatal("frozen card set lost cpu-per-core")
	}
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}},
	}}
	in := bufio.NewReader(strings.NewReader("0\n100ms\n1\ny\n"))
	code := startSessionProfile(in, newScreenUI(os.Stdout), config.Default(), topology, card)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0", code)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.ProfileID != "cpu-per-core" || lr.ProfileVersion != "v1" {
		t.Errorf("card provenance = %q %q, want cpu-per-core v1", lr.ProfileID, lr.ProfileVersion)
	}
	if lr.Session == nil || lr.Session.Mode != sessionPerCore {
		t.Errorf("card shape did not fix the mode: %+v", lr.Session)
	}
	r, err := loadLastResult()
	if err != nil {
		t.Fatalf("no readable receipt: %v", err)
	}
	if r.State != receiptFinal || r.RecipeID != lr.RecipeID {
		t.Errorf("receipt = %s bound to %q, want final bound to %q", r.State, r.RecipeID, lr.RecipeID)
	}
}

// The menu's outcome line never upgrades: each receipt state maps to
// its honest verdict plus the matching semantic color (menu plan
// section 7: faults red, unknown/partial yellow, verified neutral),
// and a foreign or missing receipt says unknown.
func TestOutcomeSummaryNeverUpgrades(t *testing.T) {
	lr := &lastRun{RecipeID: "r-1"}
	useScratchReceipt(t)
	if got, code := outcomeSummary(lr); !strings.Contains(got, "UNKNOWN") || code != tui.FGYellow {
		t.Errorf("missing receipt: %q code %q", got, code)
	}
	tr := newAttemptReceipt("other", 1, nil, nil)
	tr.finish(0, []plan.Result{{Execution: plan.ExecCompleted, Validation: plan.ValVerified}})
	if got, code := outcomeSummary(lr); !strings.Contains(got, "UNKNOWN") || code != tui.FGYellow {
		t.Errorf("foreign receipt: %q code %q", got, code)
	}
	tr = newAttemptReceipt("r-1", 2, nil, nil)
	if got, code := outcomeSummary(lr); !strings.Contains(got, "not finished") || !strings.Contains(got, "INCOMPLETE") || code != tui.FGYellow {
		t.Errorf("pending receipt: %q code %q", got, code)
	}
	tr.finish(1, []plan.Result{{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial}})
	// A runtime stop without confirmed fault evidence is INCOMPLETE -
	// the red FAIL word belongs only to a confirmed fault (P13).
	if got, code := outcomeSummary(lr); !strings.Contains(got, "INCOMPLETE") || code != tui.FGYellow {
		t.Errorf("runtime-stopped attempt: %q code %q", got, code)
	}

	tr = newAttemptReceipt("r-1", 1, nil, nil)
	tr.finish(0, []plan.Result{{Execution: plan.ExecCompleted, Validation: plan.ValUnknown}})
	if got, code := outcomeSummary(lr); !strings.Contains(got, "UNVERIFIED") || code != tui.FGYellow {
		t.Errorf("unverified completion: %q code %q", got, code)
	}
	tr = newAttemptReceipt("r-1", 1, nil, nil)
	tr.finish(0, []plan.Result{{Execution: plan.ExecCompleted, Validation: plan.ValVerified}})
	if got, code := outcomeSummary(lr); !strings.Contains(got, "PASS") || code != "" {
		t.Errorf("verified completion: %q code %q", got, code)
	}
	// A confirmed fault reads FAIL red even through the receipt path.
	tr = newAttemptReceipt("r-1", 1, nil, nil)
	tr.finish(1, []plan.Result{{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial,
		Findings: []plan.Finding{{Kind: plan.FindDataMismatch, Source: "workload", Message: "mismatch on core 3",
			Attribution: plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(3), Confidence: plan.ConfConfirmed}}}}})
	if got, code := outcomeSummary(lr); !strings.Contains(got, "FAIL") || code != tui.FGBrightRed {
		t.Errorf("faulted attempt: %q code %q", got, code)
	}
}

// The parameters line names the recipe by its card title when the
// recipe knows one, and by the raw shape when it does not - never by
// an invented profile.
func TestRecipeSummaryLineNamesWhatIsKnown(t *testing.T) {
	cfg := config.Default()
	s := SessionOptions{Mode: sessionPerCore, Workload: "int", Duration: 30 * time.Second,
		Passes: 2, PhysicalCores: []int{0, 1}}
	// The card title comes from the catalog's own card for the ID
	// (Get beats the fixture): both card text and the summary's own
	// format words are English now (Q03a made the cards English, this
	// commit the screens).
	lr := sessionRecipeForProfile(cfg, s, catalog.Profile{ID: "cpu-per-core", Version: "v1"})
	if got := recipeSummaryLine(&lr); !strings.Contains(got, "Per-core check") || !strings.Contains(got, "per core") {
		t.Errorf("card-known recipe line: %q", got)
	}
	// A workload no card compiles from (the manual wizard can reach
	// it) stays raw: the line names the workload, never a profile.
	bare := sessionRecipe(cfg, SessionOptions{Mode: sessionAllCore, Workload: "no-such-workload",
		Duration: time.Second, Passes: 1, PhysicalCores: []int{0}})
	if got := recipeSummaryLine(&bare); !strings.Contains(got, "no-such-workload") {
		t.Errorf("raw recipe line: %q", got)
	}
	sc := lastRun{Kind: lastRunScenario, Phases: []config.Phase{{Name: "a"}, {Name: "b"}}, Cycles: 3}
	if got := recipeSummaryLine(&sc); !strings.Contains(got, "Scenario") || !strings.Contains(got, "2 phase(s)") {
		t.Errorf("scenario recipe line: %q", got)
	}
}
