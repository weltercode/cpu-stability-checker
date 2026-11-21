package cli

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// Sol review discriminator (R40): the card screen is a FRAME, not a
// direct print appended under the category's. While its answer is
// pending, a window resize must redraw the CARD screen - never the
// category list that stood before it - so Enter can never consent
// against a stale frame. (The deleted detailsSubmenu direct-print
// carried the same risk; on the consent path it became real.)
func TestCardScreenOwnsFrameOnResize(t *testing.T) {
	chdirTemp(t)
	w := &lockedWriter{}
	u := &screenUI{p: tui.NewPainter(w)}
	var mu sync.Mutex
	cols, rows := 80, 24
	u.sizeFn = func() (int, int, bool) {
		mu.Lock()
		defer mu.Unlock()
		return cols, rows, true
	}
	// The category frame first - the frame the resize watcher would
	// redraw if the card screen did not take frame ownership.
	u.screenPrompt([]string{"--- CPU check ---", "  1) General CPU check"}, "Profile [q]: ")

	card, ok := catalog.Get("cpu-per-core")
	if !ok {
		t.Fatal("frozen card set lost cpu-per-core")
	}
	pr, pw := io.Pipe()
	in := bufio.NewReader(pr)
	done := make(chan bool, 1)
	go func() { done <- cardScreen(in, u, card) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), "PASS means / limits") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(w.String(), "PASS means / limits") {
		pw.Close()
		t.Fatal("cardScreen did not draw its own frame in screen mode")
	}
	before := len(w.String())

	mu.Lock()
	cols, rows = 60, 20
	mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.String()) > before {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(w.String()) == before {
		pw.Close()
		t.Fatal("resize with the card answer pending redrew nothing")
	}
	redrawn := w.String()[before:]
	if !strings.Contains(redrawn, "PASS means / limits") {
		t.Errorf("the resize redrew something other than the card frame:\n%s", redrawn)
	}
	if strings.Contains(redrawn, "Profile [q]:") {
		t.Errorf("the stale category frame owns the screen while the card answer is pending:\n%s", redrawn)
	}

	pw.Close() // EOF on input: back, never consent
	if consent := <-done; consent {
		t.Error("EOF on the card screen was treated as consent")
	}
	u.stopWatch()
}

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

// R40 list rows (owner: "short names and one line very short
// description"): title line carries ONLY number, title and the text
// unavailable mark - no cost noise; the second line is the Short
// facet, one row at normal width, wrapping (never clipping) on
// narrow screens. The cost line and every longer explanation belong
// to the card screen alone.
func TestCardLinesShortFormR40(t *testing.T) {
	for _, c := range categoryCards(catalog.RAM, catalog.Normal) {
		lines := cardLines(1, c, 80)
		if len(lines) != 2 {
			t.Errorf("card %s list row is %d lines at 80 cols, want exactly 2: %q", c.ID, len(lines), lines)
			continue
		}
		if !strings.HasPrefix(lines[0], "  1) "+c.Title) {
			t.Errorf("card %s title row wrong: %q", c.ID, lines[0])
		}
		if strings.Contains(lines[0], " — ") || strings.Contains(lines[0], "planned") {
			t.Errorf("card %s title row carries cost noise again: %q", c.ID, lines[0])
		}
		if w := tui.DisplayWidth(lines[0]); w > 80 {
			t.Errorf("card %s title row is %d cells wide at 80 cols", c.ID, w)
		}
		f, ok := catalog.FacetsFor(c.ID)
		if !ok || !strings.Contains(lines[1], f.Short) {
			t.Errorf("card %s short line missing from the row: %q", c.ID, lines[1])
		}
		if w := tui.DisplayWidth(lines[1]); w > 80 {
			t.Errorf("card %s short line is %d cells wide at 80 cols: %q", c.ID, w, lines[1])
		}
	}

	card, ok := catalog.Get("cpu-per-core")
	if !ok {
		t.Fatal("frozen card set lost cpu-per-core")
	}
	lines := cardLines(1, card, 80)
	if strings.Contains(lines[0], "(unavailable)") {
		t.Error("available card marked unavailable")
	}
	unCard, ok := catalog.Get("mix-simultaneous")
	if !ok {
		t.Fatal("frozen card set lost mix-simultaneous")
	}
	un := cardLines(5, unCard, 80)
	if !strings.Contains(un[0], "(unavailable)") {
		t.Errorf("unavailable mark missing: %q", un[0])
	}
	if w := tui.DisplayWidth(un[0]); w > 80 {
		t.Errorf("marked line is %d cells wide at 80 cols", w)
	}
	narrow := cardLines(1, card, 20)
	if !strings.Contains(narrow[0], "Per-core") {
		t.Errorf("narrow screen lost the title: %q", narrow)
	}
	for _, l := range narrow {
		if w := tui.DisplayWidth(l); w > 80 {
			t.Errorf("narrow layout overflowed: %q", l)
		}
	}
}

// The quick check is the one card whose cost is fixed and named, not
// settings-driven: its marker says so, and every other card's cost
// line explains itself (the bare "planned at your settings" the
// owner rejected as unexplained noise must not come back).
func TestCostMarkerHonesty(t *testing.T) {
	q, ok := catalog.Get("quick-check")
	if !ok {
		t.Fatal("frozen card set lost quick-check")
	}
	if m := costMarker(q); !strings.Contains(m, "planned load") {
		t.Errorf("quick-check cost marker lost its bounded wording: %q", m)
	}
	for _, c := range catalog.All() {
		if c.ID == "quick-check" {
			continue
		}
		m := costMarker(c)
		if !strings.Contains(m, "your settings") || !strings.Contains(m, "plan screen") {
			t.Errorf("card %s cost line lost its self-explanation: %q", c.ID, m)
		}
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
