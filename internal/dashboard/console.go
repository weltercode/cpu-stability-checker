package dashboard

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// Console owns the live panel's repaint loop: the engine Push()es run
// state, a goroutine samples hardware once a second and repaints the
// panel in place through the shared tui painter. Without a terminal or
// without VT support it stays silent — callers keep their plain fallback
// line, and redirected output never sees a panel.
//
// A Console can also outlive a single run: the menu's session creates
// one, hands it to every run's engine and stops it only when the menu
// screen takes over — hardware keeps being watched between runs. The
// engine that was handed an external Console must not Stop it (the
// owner does); it only arms and disarms Escape through SetOnEscape.
type Console struct {
	out         io.Writer
	file        *os.File
	painter     *tui.Painter
	sample      *Sampler
	temperature *temperature.Monitor
	vt          bool

	// onEscape, while set, is called if the user presses Escape while a
	// run owns the screen (VT consoles only; pipes never see it). The
	// input watcher exists only while armed: it drains console input,
	// which is safe during a run (the menu's line reader is idle) but
	// must never steal the menu's keystrokes between runs.
	onEscape func()
	watch    *tui.EscapeWatcher

	mu      sync.Mutex
	current Snapshot
	stopped bool

	// pmu arbitrates the screen itself: exactly one of Frame, Clear or a
	// plain-text write owns the terminal at a time, so the painter's
	// cursor bookkeeping never meets a foreign write.
	pmu sync.Mutex
	// The panel and report share one bounded viewport. Stream output must
	// never advance the terminal underneath the live panel.
	messages string
	prompt   string
	lastSnap Snapshot

	// lastCols/lastRows remember the viewport the previous frame was
	// drawn into; a change means the buffer scrolled or reflowed under
	// the painter and in-place repaint can no longer find the old frame.
	lastCols, lastRows int

	// gate turns the per-frame size queries into one stable geometry:
	// a terminal that briefly reports a collapsed rect must not make
	// the panel clear and reflow every second (the full <-> fragments
	// flip the user measured). trace optionally records the raw
	// numbers behind every decision (CPU_STABILITY_SIZEDIAG).
	gate  tui.GeometryGate
	trace *tui.SizeTrace

	stop chan struct{}
	done chan struct{}
}

// New prepares a console panel for f. The panel only paints when f is a
// terminal with virtual-terminal processing enabled (best-effort enabled
// by the probe; classic consoles and pipes decline).
func New(f *os.File) *Console {
	return NewWithTemperature(f, temperature.New())
}

// NewWithTemperature lets an owner supply the monitor used by both the
// panel and the executor. The console closes it when its session ends.
func NewWithTemperature(f *os.File, m *temperature.Monitor) *Console {
	c := &Console{out: f, sample: newSamplerWithTemperature(m), temperature: m, stop: make(chan struct{}), done: make(chan struct{})}
	caps := tui.Probe(f)
	c.vt = caps.Terminal && caps.VT
	c.trace = tui.NewSizeTrace()
	if c.vt {
		c.file = f
		c.painter = tui.NewPainter(f)
	}
	return c
}

func (c *Console) Temperature() *temperature.Monitor { return c.temperature }

// Active reports whether the panel will actually paint; callers use this
// to decide whether to keep their fallback line.
func (c *Console) Active() bool { return c.vt }

// Push hands the latest run state to the painter (latest value wins,
// never blocks the run).
func (c *Console) Push(run Snapshot) {
	c.mu.Lock()
	c.current = run
	c.mu.Unlock()
}

// ClearRun drops the finished run's state. A session-owned console
// keeps painting after the run ends, and its next frames must not
// pretend a completed run is still at 98% with a stale footer.
func (c *Console) ClearRun() {
	c.mu.Lock()
	c.current = Snapshot{}
	c.mu.Unlock()
	c.pmu.Lock()
	c.lastSnap.HasRun = false
	c.pmu.Unlock()
}

// Current returns the last pushed state (for tests and the owner).
func (c *Console) Current() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Print adds text to the report below the hardware panel. On pipes it
// remains an ordinary write; a live terminal never scrolls for a report.
func (c *Console) Print(text string) {
	if !c.vt || c.Stopped() {
		fmt.Fprint(c.out, text)
		return
	}
	c.pmu.Lock()
	defer c.pmu.Unlock()
	c.messages += text
	// Bound long sessions without cutting a partial UTF-8 character.
	lines := strings.Split(c.messages, "\n")
	if len(lines) > 128 {
		c.messages = strings.Join(lines[len(lines)-128:], "\n")
	}
	c.drawLocked(c.lastSnap)
}

// SetPrompt keeps the input hint separate from the report so it cannot
// be pushed out of the viewport by a long summary.
func (c *Console) SetPrompt(prompt string) {
	if !c.vt || c.Stopped() {
		c.Print("\n" + prompt + "\n")
		return
	}
	c.pmu.Lock()
	defer c.pmu.Unlock()
	c.prompt = prompt
	c.drawLocked(c.lastSnap)
}

// Writer routes stream-shaped output (the run summary) through Print,
// so code that only knows io.Writer joins the arbitration without
// learning anything about panels.
func (c *Console) Writer() io.Writer { return consoleWriter{c} }

type consoleWriter struct{ c *Console }

func (w consoleWriter) Write(p []byte) (int, error) {
	w.c.Print(string(p))
	return len(p), nil
}

// Start begins the repaint loop. The first frame lands on a cleared
// screen: the panel owns the screen until Stop. Escape is armed
// separately (SetOnEscape) so a session-owned panel can outlive any
// single run.
func (c *Console) Start() {
	if !c.vt {
		close(c.done)
		return
	}
	c.painter.Screen(nil)
	go c.loop()
}

// SetOnEscape arms (fn != nil) or disarms (fn == nil) the Escape key.
// The console input watcher lives only while armed: draining pending
// input is safe during a run, never between runs, where the menu's
// line reader needs those very keystrokes.
func (c *Console) SetOnEscape(fn func()) {
	c.mu.Lock()
	c.onEscape = fn
	var w *tui.EscapeWatcher
	if fn == nil {
		w, c.watch = c.watch, nil
	} else if c.watch == nil {
		c.watch = tui.WatchEscape(c.escapeFired)
	}
	c.mu.Unlock()
	if w != nil {
		w.Stop() // outside the lock: the watcher may be inside escapeFired
	}
}

// escapeFired is the watcher's callback; it dispatches to whichever run
// is armed right now (the watcher itself fires at most once).
func (c *Console) escapeFired() {
	c.mu.Lock()
	fn := c.onEscape
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (c *Console) loop() {
	defer close(c.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	c.paint() // immediate first frame
	for {
		select {
		case <-c.stop:
			c.paint()
			c.pmu.Lock()
			fmt.Fprintln(c.out) // leave the final frame cleanly
			c.pmu.Unlock()
			return
		case <-tick.C:
			c.paint()
		}
	}
}

func (c *Console) paint() {
	c.mu.Lock()
	snap := c.current
	canStop := c.watch != nil
	c.mu.Unlock()
	c.sample.Apply(&snap)
	snap.CanStop = canStop
	snap.Color = c.vt // the panel only paints on VT consoles; pipes never get here
	// Size is re-queried every frame so a resized terminal reflows the
	// panel (bars <-> grid) without a restart.
	c.pmu.Lock()
	c.lastSnap = snap
	c.drawLocked(snap)
	c.pmu.Unlock()
}

// drawLocked always anchors the complete frame at viewport home. This
// also tolerates the console echoing Enter or reflowing during a resize.
func (c *Console) drawLocked(snap Snapshot) {
	g := tui.SizeGeometry(c.file)
	prevCols, prevRows := c.lastCols, c.lastRows
	// The gate, not the raw query, decides what this frame is painted
	// against: one collapsed observation among normal ones must not
	// clear and reflow the panel every second (the full <-> fragments
	// flip the user measured on a real desktop).
	cols, rows := c.gate.Observe(g.PickCols, g.PickRows, g.OK)
	if c.trace != nil {
		conout := tui.QueryCONOUT() // second handle: does it disagree?
		c.trace.Line("panel", g, c.gate.State(), &conout)
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = prevCols, prevRows
	}
	if cols <= 0 || rows <= 0 {
		cols, rows = 80, 25
	}
	if viewportChanged(prevCols, prevRows, cols, rows) {
		// The adopted geometry changed: the terminal scrolled or
		// reflowed, so the old frame is no longer where the cursor-up
		// count thinks it is (the leftover-frame smear). Relative
		// addressing cannot find it; the honest answer is to clear the
		// viewport and draw the new frame fresh at its top.
		c.painter.Reset()
		fmt.Fprint(c.out, "\x1b[2J\x1b[H")
	}
	c.lastCols, c.lastRows = cols, rows
	c.painter.Reset()
	fmt.Fprint(c.out, "\x1b[H")
	c.painter.Frame(renderConsole(snap, c.messages, c.prompt, cols, rows))
}

// viewportChanged reports whether the terminal geometry changed since
// the previous frame. Unknown dimensions (0) never count: a transient
// size-query failure must not trigger a full redraw every frame.
func viewportChanged(lastCols, lastRows, cols, rows int) bool {
	return lastCols > 0 && lastRows > 0 && cols > 0 && rows > 0 &&
		(cols != lastCols || rows != lastRows)
}

// Stop ends the repaint loop and leaves the final frame on screen. Only
// the Console's owner calls it; engines handed an external Console just
// disarm Escape. Safe to call twice and safe when Start never ran.
func (c *Console) Stop() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	w := c.watch
	c.watch = nil
	c.mu.Unlock()
	if w != nil {
		w.Stop()
	}
	if c.vt {
		close(c.stop)
		<-c.done
	}
	c.sample.Close()
	if c.temperature != nil {
		c.temperature.Close()
	}
}

// Stopped reports whether Stop has run — the ownership contract between
// a session and the engines it hands the panel to.
func (c *Console) Stopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}
