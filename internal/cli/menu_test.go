package cli

import (
	"bufio"
	"os"
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
// failures. The scenario run lives under Settings now (task menu
// C5): "6" opens settings, "1" runs the config.json scenario.
func TestMenuLoopPropagatesSetupError(t *testing.T) {
	dir := chdirTemp(t)
	os.WriteFile("config.json", []byte(`{"test": "general"}`), 0o644) // no phases: setup error
	_ = dir
	done := make(chan int, 1)
	go func() {
		done <- menuLoop(bufio.NewReader(strings.NewReader("6\n1\n")), config.Default())
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

// The "Detailed description" submenu must be MORE detailed than the
// clipped list rows in front of it, never less. The owner caught the
// old shape reading backwards: the main screen already carried the
// summary (clipped) while the "detailed" screen showed bare titles
// ("на первом экране я вижу более подробное описание, чем в
// 'детальном'"). So the submenu rows carry the full summary wrapped
// over continuation lines, and a pick adds the long Details text and,
// for an unavailable goal, its recorded Reason right here.
func TestDetailedSubmenuCarriesMoreThanTheList(t *testing.T) {
	chdirTemp(t)
	ui := &screenUI{} // append mode: width falls back to 80
	cards := categoryCards(catalog.MIX, catalog.Normal)
	if len(cards) != 3 {
		t.Fatalf("MIX normal cards = %d, want the three catalog entries", len(cards))
	}

	// The parent list clips the long summary - that is the showcase
	// this submenu must beat.
	clipped := cardLine(1, cards[0].Title, cards[0].Summary, false, 80)
	if !strings.Contains(clipped, "…") {
		t.Fatalf("premise broken: the 80-col list row is not clipped: %q", clipped)
	}

	// "1" opens the available card, Enter returns to the submenu,
	// "2" opens an unavailable card, Enter, then "q" backs out.
	in := bufio.NewReader(strings.NewReader("1\n\n2\n\nq\n"))
	var out string
	done := make(chan struct{})
	go func() {
		defer close(done)
		out = captureStdout(t, func() { detailsSubmenu(in, ui, cards) })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("detailsSubmenu did not return")
	}

	if !strings.Contains(out, "--- Detailed description ---") {
		t.Errorf("submenu header missing:\n%s", out)
	}
	// The list stage of the submenu carries the FULL summary of the
	// available card: rejoin its wrapped continuation lines and
	// compare with the catalog data, word for word. The parent's
	// row above provably clipped that same summary.
	firstPick := strings.Index(out, cards[0].Details)
	if firstPick < 0 {
		t.Fatalf("the available card's long Details text is missing:\n%s", out)
	}
	listStage := out[:firstPick]
	lines := strings.Split(listStage, "\n")
	block := []string{}
	for i, l := range lines {
		if strings.HasPrefix(l, "  1) ") {
			for _, w := range lines[i+1:] {
				if strings.HasPrefix(w, "  2) ") {
					break
				}
				block = append(block, strings.TrimSpace(w))
			}
			break
		}
	}
	if strings.Join(block, " ") != cards[0].Summary {
		t.Errorf("submenu row 1 does not carry the full summary:\n got %q\nwant %q", strings.Join(block, " "), cards[0].Summary)
	}
	if strings.Contains(listStage, "…") {
		t.Errorf("submenu list rows clip a summary the way the parent does:\n%s", listStage)
	}
	if !strings.Contains(out, "Simultaneous CPU + RAM load (unavailable)") {
		t.Error("unavailable mark lost from the submenu rows")
	}
	// The unavailable goal states its recorded reason inside the
	// submenu itself (the red "Not available:" behind pick 2).
	if !strings.Contains(out, "Not available: co-execution is not implemented yet") {
		t.Error("the unavailable card's recorded Reason was not stated in the submenu")
	}
	// The available card's full contract is present too.
	if !strings.Contains(out, "will not promise one") {
		t.Error("the long Details text was not shown for the available card")
	}
}

// The parent menus call this choice "Detailed description", matching
// what it now actually shows; no menu may still offer a bare "Details"
// row that promises less than the screen above it.
func TestCategoryMenuOffersDetailedDescriptionChoice(t *testing.T) {
	chdirTemp(t)
	worst := 0
	out := captureStdout(t, func() {
		if code := categoryMenu(bufio.NewReader(strings.NewReader("q\n")), &screenUI{},
			config.Default(), sysinfo.Topology{}, catalog.MIX, "Mixed CPU + RAM tests", &worst); code != 0 {
			t.Errorf("categoryMenu(back) = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "Detailed description") {
		t.Errorf("menu lost the Detailed description choice:\n%s", out)
	}
	if strings.Contains(out, ") Details") {
		t.Errorf("the old bare Details row is still offered:\n%s", out)
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
		"Detailed description",
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
