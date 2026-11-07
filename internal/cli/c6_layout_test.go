package cli

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// lockedWriter collects painter output across goroutines.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// A window resize while the menu waits for an answer must bring the
// prompt and the whole action list back at the new size on its own -
// not only after the next keystroke, and never by cutting the last
// actions off the bottom (menu plan section 7). The redraw reuses the
// exact saved frame, so nothing is invented and buffered input stays
// untouched.
func TestResizeRedrawsWaitingPrompt(t *testing.T) {
	w := &lockedWriter{}
	u := &screenUI{p: tui.NewPainter(w)}
	var mu sync.Mutex
	cols, rows := 80, 24
	u.sizeFn = func() (int, int, bool) {
		mu.Lock()
		defer mu.Unlock()
		return cols, rows, true
	}
	u.screenPrompt([]string{"меню"}, "> ")
	if n := strings.Count(w.String(), "\x1b[2J"); n != 1 {
		t.Fatalf("first draw cleared %d times, want 1", n)
	}

	mu.Lock()
	cols, rows = 100, 30
	mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(w.String(), "\x1b[2J") >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Count(w.String(), "\x1b[2J") < 2 {
		t.Fatal("resize did not redraw the waiting prompt")
	}

	// Stopped means stopped: no draws after stopWatch (the next real
	// screen, or the live panel, owns the output from here).
	u.stopWatch()
	before := len(w.String())
	mu.Lock()
	cols, rows = 120, 40
	mu.Unlock()
	time.Sleep(3 * resizePollEvery)
	if len(w.String()) != before {
		t.Error("watcher kept drawing after stopWatch")
	}
}

// The live run panel re-measures the terminal itself every frame, so
// the menu watcher must step down the moment the panel is taken: two
// repaint owners on one screen is exactly the old "screen breaks at
// run end" class of bug.
func TestMonitorStopsResizeWatcher(t *testing.T) {
	w := &lockedWriter{}
	u := &screenUI{p: tui.NewPainter(w)}
	var mu sync.Mutex
	cols, rows := 80, 24
	u.sizeFn = func() (int, int, bool) {
		mu.Lock()
		defer mu.Unlock()
		return cols, rows, true
	}
	u.screenPrompt([]string{"меню"}, "> ")
	_ = u.monitor() // the panel takes the screen: the watcher must be gone

	mu.Lock()
	cols, rows = 100, 30
	mu.Unlock()
	time.Sleep(3 * resizePollEvery)
	if n := strings.Count(w.String(), "\x1b[2J"); n != 1 {
		t.Errorf("watcher drew %d screens after monitor(), want 1", n)
	}
}

// cardLine keeps one profile to a single visible line (menu plan
// section 7): the summary is clipped to the terminal width with a
// visible "…", the unavailable mark is text, and a too-narrow screen
// still shows the title instead of crashing or wrapping unpredictably.
func TestCardLineFitsWidth(t *testing.T) {
	card, ok := catalog.Get("cpu-per-core")
	if !ok {
		t.Fatal("frozen card set lost cpu-per-core")
	}
	line := cardLine(1, card.Title, card.Summary, false, 80)
	if w := tui.DisplayWidth(line); w > 80 {
		t.Errorf("line is %d cells wide at 80 cols: %q", w, line)
	}
	if !strings.Contains(line, card.Title) || !strings.Contains(line, "…") {
		t.Errorf("line lost the title or the cut mark: %q", line)
	}
	if strings.Contains(line, "(unavailable)") {
		t.Error("available card marked unavailable")
	}

	un := cardLine(5, card.Title, card.Summary, true, 80)
	if !strings.Contains(un, "(unavailable)") {
		t.Errorf("unavailable mark missing: %q", un)
	}
	if w := tui.DisplayWidth(un); w > 80 {
		t.Errorf("marked line is %d cells wide at 80 cols", w)
	}

	narrow := cardLine(1, "A very long profile title here", card.Summary, false, 20)
	if !strings.Contains(narrow, "A very long") {
		t.Errorf("narrow screen lost the title: %q", narrow)
	}
	if w := tui.DisplayWidth(narrow); w > 80 {
		t.Errorf("narrow layout overflowed: %q", narrow)
	}
}

// The category accents are the plan's fixed coding (CPU blue, RAM
// light blue, MIX purple) - pinned as data so a rename cannot
// silently recolor a category.
func TestCategoryAccentCoding(t *testing.T) {
	if got := categoryAccent(catalog.CPU); got != tui.FGBlue {
		t.Errorf("CPU accent = %q, want blue", got)
	}
	if got := categoryAccent(catalog.RAM); got != tui.FGBrightBlue {
		t.Errorf("RAM accent = %q, want light blue", got)
	}
	if got := categoryAccent(catalog.MIX); got != tui.FGPurple {
		t.Errorf("MIX accent = %q, want purple", got)
	}
}

// The main screen marks the Enter default (item 1) only over a real
// recipe, and the color layer is parity-safe: with color on the
// unknown outcome carries the yellow decoration, with color off the
// exact same words carry no ANSI at all.
func TestMenuLinesMarkerAndColorParity(t *testing.T) {
	chdirTemp(t)
	cfg := config.Default()

	// Fresh machine: no recipe, no marker, no ANSI.
	plain := &screenUI{}
	lines := menuLines(plain)
	for _, l := range lines {
		if strings.Contains(l, "\x1b") {
			t.Fatalf("plain screen carries ANSI: %q", l)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "›") {
			t.Fatalf("no recipe but item 1 is marked: %q", l)
		}
	}

	// Saved recipe: item 1 becomes the marked default, and the
	// outcome line (unknown here) is decorated when color is on.
	lr := sessionRecipe(cfg, SessionOptions{Mode: sessionAllCore, Workload: "int",
		Duration: 30 * time.Second, Passes: 1, PhysicalCores: []int{0}})
	if err := saveLastRun(lr); err != nil {
		t.Fatal(err)
	}
	lines = menuLines(plain)
	var marked bool
	for _, l := range lines {
		if strings.HasPrefix(l, "› 1)") {
			marked = true
		}
		if strings.Contains(l, "\x1b") {
			t.Fatalf("color-off screen carries ANSI: %q", l)
		}
	}
	if !marked {
		t.Error("saved recipe did not mark item 1 as the Enter default")
	}

	colored := menuLines(&screenUI{color: true})
	var decorated bool
	for _, l := range colored {
		if strings.Contains(l, tui.FGYellow) {
			decorated = true
		}
	}
	if !decorated {
		t.Error("unknown outcome was not decorated yellow with color on")
	}
}
