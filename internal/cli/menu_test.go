package cli

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// The crash-loop workflow: a stopped or faulted run (exit 1) must NOT
// kick the user out of the menu (user: "Escape выбрасывает из
// программы"), while setup errors and force quits from runs that
// actually armed an attempt still end the session, and the menu's
// own exit code still honors the contract. A refusal that provably
// never armed an attempt is a dialog event, not a run outcome: it
// stays in the menu (its own wording says "pick another profile").
func TestMenuAfterRunPolicy(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	cases := []struct {
		name      string
		code      int
		wantStop  bool
		wantExit  int
		wantWorst int
	}{
		{"clean run stays", 0, false, 0, 0},
		{"stopped run stays", 1, false, 0, 1},
		{"setup error with no receipt to compare exits", 2, true, 2, 0},
		{"force quit with no receipt to compare exits", 130, true, 130, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			worst := 0
			stop, exit := menuAfterRun(&worst, tt.code, "")
			if stop != tt.wantStop || exit != tt.wantExit || worst != tt.wantWorst {
				t.Errorf("menuAfterRun(%d) = stop %v exit %d worst %d, want %v %d %d",
					tt.code, stop, exit, worst, tt.wantStop, tt.wantExit, tt.wantWorst)
			}
		})
	}

	// With a readable receipt captured before the action, armedness
	// is provable: unchanged id = never armed = stay; new id = a real
	// run ending = the contract stands.
	writeFinalReceipt(t, "att-1", "completed", "", 0)
	armedCases := []struct {
		name     string
		code     int
		before   string
		wantStop bool
		wantExit int
	}{
		{"refusal before any attempt stays in the menu", 2, "att-1", false, 2},
		{"escape before any attempt stays in the menu", 130, "att-1", false, 130},
		{"setup error from an armed run still exits", 2, "att-older", true, 2},
	}
	for _, tt := range armedCases {
		t.Run(tt.name, func(t *testing.T) {
			worst := 0
			stop, exit := menuAfterRun(&worst, tt.code, tt.before)
			if stop != tt.wantStop || exit != tt.wantExit {
				t.Errorf("menuAfterRun(%d, %q) = stop %v exit %d, want %v %d",
					tt.code, tt.before, stop, exit, tt.wantStop, tt.wantExit)
			}
		})
	}
	if runStopNote(1) == "" || runStopNote(0) != "" {
		t.Error("runStopNote must explain stopped runs only")
	}
}

// The stay itself must be stated and readable: a bare return to the
// menu after a refusal looks like the refusal did nothing, and the
// silent program end looked like a crash. The wrapper names the
// reason, waits for Enter and keeps the session alive; armed-run
// endings pass through to menuStopExit unchanged.
func TestMenuRunOutcomeStatesTheStay(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	writeFinalReceipt(t, "att-1", "completed", "", 0)

	worst := 0
	var stop bool
	out := captureStderr(t, func() {
		stop, _ = menuRunOutcome(bufio.NewReader(strings.NewReader("\n")), nil, &worst, 2, "att-1")
	})
	if stop {
		t.Error("a proven unarmed refusal must not end the session")
	}
	if !strings.Contains(out, "Not started (exit 2)") || !strings.Contains(out, "Back to the menu") {
		t.Errorf("stay note missing:\n%s", out)
	}

	stop = false
	var exit int
	out = captureStderr(t, func() {
		worst := 0
		stop, exit = menuRunOutcome(bufio.NewReader(strings.NewReader("\n")), nil, &worst, 2, "att-older")
	})
	if !stop || exit != 2 {
		t.Errorf("armed-run refusal = stop %v exit %d, want true 2", stop, exit)
	}
	if strings.Contains(out, "Back to the menu") {
		t.Errorf("the stay note must not fire for an armed run:\n%s", out)
	}
	if worst != 0 {
		t.Errorf("worst = %d, want untouched 0", worst)
	}
}

// A setup error (bad config for the scenario) still ends the session
// with that code - the stay-in-menu policy must not swallow real
// failures. The scenario run lives under Advanced tools (task menu
// C5, R39 item 8): "8" opens it, "1" runs the config.json scenario.
func TestMenuLoopPropagatesSetupError(t *testing.T) {
	dir := chdirTemp(t)
	os.WriteFile("config.json", []byte(`{"test": "general"}`), 0o644) // no phases: setup error
	_ = dir
	done := make(chan int, 1)
	go func() {
		done <- menuLoop(bufio.NewReader(strings.NewReader("8\n1\n")), config.Default())
	}()
	select {
	case code := <-done:
		if code != 2 {
			t.Fatalf("menuLoop exit code = %d, want 2", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("menuLoop did not propagate the setup error")
	}
}

// R39b: the About screen sits at 7 of Advanced tools and carries the
// fixed honesty sections verbatim. Reading it only prints: it starts
// no run, writes no record, and the vocabulary dictionary names the
// unknown instead of smoothing it over.
func TestAboutScreenFixedSections(t *testing.T) {
	chdirTemp(t)
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("8\n7\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"--- About this tool ---",
		"The result words, always exactly these:",
		"PASS       - every check the run performed matched its exact",
		"UNKNOWN    - the records cannot say: missing, unreadable or",
		"What PASS never means: PASS is bounded",
		"claim that the machine is stable",
		"the tool states them instead of counting them as clean",
		"word, or history_keep) removes session records TOGETHER WITH",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("About screen lost %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{lastrunPath, lastResultPath, findingsPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("reading About created %s", gone)
		}
	}
}

// R40 (owner-named): the card's own screen must carry MORE than the
// short list row in front of it, never less, and it must be the ONLY
// description level - the separate "Detailed description" submenu is
// gone. The owner caught the old shape reading backwards twice (the
// "detailed" screen shorter than the list rows: "right now is shorter
// than menu items"), so the property itself is what this pins: list
// row (title + Short) ⊂ card screen (full summary, facet block,
// planned cost, full contract, landmark).
func TestCardScreenCarriesMoreThanTheList(t *testing.T) {
	chdirTemp(t)
	ui := &screenUI{} // append mode: width falls back to 80
	cards := categoryCards(catalog.MIX, catalog.Normal)
	if len(cards) != 3 {
		t.Fatalf("MIX normal cards = %d, want the three catalog entries", len(cards))
	}

	// Premise: the list row is the SHORT form - it carries neither
	// the full summary nor the cost line (the owner's complaint was
	// exactly this content standing in the list).
	list := strings.Join(cardLines(1, cards[0], 80), "\n")
	if strings.Contains(list, cards[0].Summary) {
		t.Fatalf("premise broken: the list row carries the full summary: %q", list)
	}
	if strings.Contains(list, "planned at your settings") || strings.Contains(list, "your settings") {
		t.Fatalf("premise broken: the list row carries a cost line again: %q", list)
	}

	// The available card: Enter on its screen is consent (true).
	in := bufio.NewReader(strings.NewReader("\n"))
	var out string
	done := make(chan struct{})
	go func() {
		defer close(done)
		out = captureStdout(t, func() {
			if !cardScreen(in, ui, cards[0]) {
				t.Error("Enter on an available card did not continue into the run flow")
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cardScreen did not return")
	}
	if !strings.Contains(out, "--- "+cards[0].Title+" ---") {
		t.Errorf("card screen header missing:\n%s", out)
	}
	// The FULL summary is here (wrapped, never clipped): rejoin.
	lines := strings.Split(out, "\n")
	block := []string{}
	for _, l := range lines {
		if strings.HasPrefix(l, "     ") {
			block = append(block, strings.TrimSpace(l))
		} else if len(block) > 0 {
			break
		}
	}
	if strings.Join(block, " ") != cards[0].Summary {
		t.Errorf("card screen does not carry the full summary:\n got %q\nwant %q", strings.Join(block, " "), cards[0].Summary)
	}
	// The facet block keeps the accepted labels.
	f, ok := catalog.FacetsFor(cards[0].ID)
	if !ok {
		t.Fatal("the available MIX card has no facet record")
	}
	if !strings.Contains(out, "PASS means / limits: "+f.Limits) {
		t.Errorf("the facet block was not shown for the available card:\n%s", out)
	}
	// The long contract and the planned-cost line live here.
	if !strings.Contains(out, "will not promise one") {
		t.Error("the long Details text was not shown for the available card")
	}
	if !strings.Contains(out, "Planned cost: durations and budgets come from your settings") {
		t.Errorf("the cost line is missing from the card screen:\n%s", out)
	}
	// And the machine landmark, in its honest absence.
	if !strings.Contains(out, "last attempt here: no observation on record") {
		t.Errorf("the landmark line is missing from the card screen:\n%s", out)
	}

	// Enter on an UNAVAILABLE card is back, never consent: the
	// recorded Reason stands and nothing continues.
	in = bufio.NewReader(strings.NewReader("\n"))
	out = captureStdout(t, func() {
		if cardScreen(in, ui, cards[1]) {
			t.Error("Enter on an unavailable card continued - consent from a refusal")
		}
	})
	if !strings.Contains(out, "Not available: co-execution is not implemented yet") {
		t.Errorf("the unavailable card did not state its Reason:\n%s", out)
	}
	if !strings.Contains(out, "(unavailable)") && !strings.Contains(out, "not available") {
		t.Error("no text mark on the refusal screen")
	}

	// Any other input is back too.
	in = bufio.NewReader(strings.NewReader("x\n"))
	if cardScreen(in, ui, cards[0]) {
		t.Error("'x' on a card screen was treated as consent")
	}
	// EOF is back, never consent.
	in = bufio.NewReader(strings.NewReader(""))
	if cardScreen(in, ui, cards[0]) {
		t.Error("EOF on a card screen was treated as consent")
	}
}

// R40: no separate "Detailed description" item exists in a category
// menu - the cards and the manual choice are the whole list, and the
// range hint matches that (the old bug class: hints excluding valid
// choices).
func TestCategoryMenuNoSeparateDetailsItem(t *testing.T) {
	chdirTemp(t)
	worst := 0
	out := captureStdout(t, func() {
		if code := categoryMenu(bufio.NewReader(strings.NewReader("99\nq\n")), &screenUI{},
			config.Default(), sysinfo.Topology{}, catalog.MIX, "Mixed CPU + RAM tests", &worst); code != 0 {
			t.Errorf("categoryMenu(back) = %d, want 0", code)
		}
	})
	for _, gone := range []string{"Detailed description", ") Details"} {
		if strings.Contains(out, gone) {
			t.Errorf("category menu still offers %q:\n%s", gone, out)
		}
	}
	cards := categoryCards(catalog.MIX, catalog.Normal)
	want := "Enter a number from 1 to " + strconv.Itoa(len(cards)+1) + " or q to go back."
	if !strings.Contains(out, want) {
		t.Errorf("bad-input hint does not match the real range:\n%s", out)
	}
}

// One flat screen per category (owner, 2026-11-09: "не нужна такая
// дикая вложенность"): no "Extended profiles" sub-level exists, the
// ex-extended cards sit among the others, and the manual registry
// choice is visible at the top level. The wizard's algorithm picker
// hides the synthetic fault prober (the catalog rule "diagnostics
// never in a user list" covers the manual pick too) while `list`
// keeps showing the complete registry.
func TestMenuNoSecondLevel(t *testing.T) {
	chdirTemp(t)
	worst := 0
	out := captureStdoutAndRun(t, func() {
		if code := categoryMenu(bufio.NewReader(strings.NewReader("q\n")), &screenUI{},
			config.Default(), sysinfo.Topology{}, catalog.CPU, "CPU check", &worst); code != 0 {
			t.Errorf("categoryMenu(back) = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"General CPU check · floating point",
		"General CPU check · SHA-256",
		"Manually pick an algorithm or an external engine",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flat CPU menu lost %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Extended profiles", "The same all-core walk"} {
		if strings.Contains(out, bad) {
			t.Errorf("menu still carries %q:\n%s", bad, out)
		}
	}

	out = captureStdoutAndRun(t, func() {
		// "q" at the picker must go back: ok=false, nothing chosen.
		if _, ok := chooseWorkload(bufio.NewReader(strings.NewReader("q\n")), "int"); ok {
			t.Error("chooseWorkload(q) did not back out")
		}
	})
	if strings.Contains(out, "always fails") {
		t.Errorf("the synthetic fault prober is offered in the manual picker:\n%s", out)
	}
	for _, want := range []string{"float", "int ", "sha256", "ram-walking"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker lost %q:\n%s", want, out)
		}
	}
}
