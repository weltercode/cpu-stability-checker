package cli

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// screenUI owns the terminal for one interactive menu session. On a
// VT-capable terminal the whole session lives in the alternate buffer:
// menu, wizard and live panel are screens that replace each other, the
// user's scrollback stays untouched, and leaving restores the terminal
// exactly as it was. On any other output (pipe, file, classic console)
// the painter is nil and every caller keeps today's append behavior —
// scripted tests and redirected output are unaffected.
type screenUI struct {
	f     *os.File
	p     *tui.Painter
	panel *dashboard.Console
	color bool
	lastW int // window size the last screen was drawn for
	lastH int

	// The resize watcher redraws the waiting menu screen when the
	// window geometry changes under it, so the prompt and the full
	// action list come back at the new size without waiting for the
	// next keystroke. mu serializes every painter write (main draw vs
	// watcher redraw); watch closes the current watcher; sizeFn is
	// injectable for tests.
	mu         sync.Mutex
	watch      chan struct{}
	lastFrame  []string
	lastPrompt string
	sizeFn     func() (int, int, bool)

	// gate/trace stabilize the menu's size handling exactly like the
	// live panel's: a terminal that briefly reports a collapsed rect
	// must not make the waiting screen clear and redraw as fragments
	// every poll. gmu guards the gate (watcher goroutine vs menu
	// draws); trace is the same optional raw-number record.
	gate  tui.GeometryGate
	gmu   sync.Mutex
	trace *tui.SizeTrace
}

// resizePollEvery bounds how stale the on-screen frame may get after
// a resize: cheap enough to poll a syscall, fast enough to feel
// instant to a user dragging a window edge.
const resizePollEvery = 400 * time.Millisecond

func newScreenUI(f *os.File) *screenUI {
	caps := tui.Probe(f)
	if caps.Terminal && caps.VT {
		p := tui.NewPainter(f)
		p.EnterAlt()
		return &screenUI{f: f, p: p, color: caps.Color, trace: tui.NewSizeTrace()}
	}
	return &screenUI{}
}

// size is the visible window geometry, through the injectable probe.
func (u *screenUI) size() (int, int, bool) {
	if u.sizeFn != nil {
		return u.sizeFn()
	}
	if u.f == nil {
		return 0, 0, false
	}
	return tui.Size(u.f)
}

// gatedSize feeds one probe result through the same stabilization
// gate the live panel uses and returns the geometry the menu should
// draw against (0,0 = nothing adopted yet; width()'s 80 fallback
// stays honest). One collapsed observation among normal ones must not
// reflow the waiting screen.
func (u *screenUI) gatedSize() (cols, rows int) {
	c, r, ok := u.size()
	u.gmu.Lock()
	cols, rows = u.gate.Observe(c, r, ok)
	st := u.gate.State()
	u.gmu.Unlock()
	if u.trace != nil {
		// The probe reports picks, not raw claims: win/buf/max stay
		// zero here, and the trace says so instead of inventing them.
		u.trace.Line("menu", tui.Geometry{PickCols: c, PickRows: r, OK: ok}, st, nil)
	}
	return cols, rows
}

// paint decorates s only when this terminal wants color. The words
// always stay in the string: color never carries meaning on its own,
// so NO_COLOR and plain output lose only the decoration.
func (u *screenUI) paint(code, s string) string {
	if u == nil {
		return s
	}
	return tui.Colorize(u.color, code, s)
}

// width is the visible terminal width for layout decisions (one-line
// card summaries, truncation). 80 is the honest fallback when the
// size is unknown: append mode, or a probe that failed - the plan's
// smallest target size, so laying out for it never overflows a real
// terminal.
func (u *screenUI) width() int {
	if u == nil {
		return 80
	}
	u.gmu.Lock()
	st := u.gate.State()
	u.gmu.Unlock()
	if st.AccCols > 0 {
		return st.AccCols
	}
	if cols, _, ok := u.size(); ok && cols > 0 {
		return cols
	}
	return 80
}

// finish leaves the alternate buffer. Safe to call once via defer.
func (u *screenUI) finish() {
	u.stopMonitor()
	u.stopWatch()
	if u.p != nil {
		u.p.LeaveAlt()
	}
}

// screenPrompt draws one full screen ending in the prompt the user
// answers next. In append mode it is exactly the old sequential print.
// A window resize between screens drops the painter's in-place
// bookkeeping first: the terminal reflowed the old frame, so nothing
// may try to repaint over it by the old row count. Screen then clears
// and redraws the whole menu at the new size - the prompt and the
// action list come back fully visible, never cut at the bottom.
func (u *screenUI) screenPrompt(lines []string, prompt string) {
	u.stopMonitor()
	u.stopWatch()
	if u.p == nil {
		for _, l := range lines {
			fmt.Println(l)
		}
		fmt.Print(prompt)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if cols, rows := u.gatedSize(); cols > 0 && rows > 0 {
		if (u.lastW > 0 && u.lastW != cols) || (u.lastH > 0 && u.lastH != rows) {
			u.p.Reset()
		}
		u.lastW, u.lastH = cols, rows
	}
	u.lastFrame, u.lastPrompt = lines, prompt
	u.p.Screen(append(lines, prompt))
	u.startWatchLocked()
}

// startWatchLocked arms the resize watcher for the frame just drawn
// (call with u.mu held). The watcher only ever redraws the exact
// frame screenPrompt saved - it never invents content and never
// touches the buffered reader, so an answer typed mid-resize is not
// lost.
func (u *screenUI) startWatchLocked() {
	stop := make(chan struct{})
	u.watch = stop
	go func() {
		t := time.NewTicker(resizePollEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				cols, rows := u.gatedSize()
				if cols <= 0 || rows <= 0 {
					continue
				}
				u.mu.Lock()
				if u.watch == nil {
					u.mu.Unlock()
					return
				}
				if cols != u.lastW || rows != u.lastH {
					u.lastW, u.lastH = cols, rows
					u.p.Reset()
					u.p.Screen(append(u.lastFrame, u.lastPrompt))
				}
				u.mu.Unlock()
			}
		}
	}()
}

// stopWatch disarms the watcher before anything else takes the
// screen: the next menu draw, the live dashboard panel, or leaving
// the alternate buffer.
func (u *screenUI) stopWatch() {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.watch != nil {
		close(u.watch)
		u.watch = nil
	}
}

// note writes a transient message (invalid input) below the current
// screen. In screen mode the next redraw replaces it; in append mode it
// behaves like the old inline error print.
func (u *screenUI) note(msg string) {
	if u.p == nil {
		fmt.Println(msg)
		fmt.Println()
	}
}

// hasScreen reports whether screens are live (callers adjust notes that
// must persist into the next frame).
func (u *screenUI) hasScreen() bool { return u.p != nil }

// The menu owns monitoring across the run, its final report and the Enter
// prompt. Executors borrow the panel; finishing a workload never closes it.
func (u *screenUI) monitor() *dashboard.Console {
	if u == nil {
		return nil
	}
	// The panel owns the screen from here on: its own loop re-queries
	// the size every frame, so the menu watcher must be gone before
	// the first panel frame paints.
	u.stopWatch()
	if u.panel == nil {
		u.panel = dashboard.New(os.Stdout)
		if u.panel.Active() {
			u.panel.Start()
		}
	}
	return u.panel
}

func (u *screenUI) stopMonitor() {
	if u == nil || u.panel == nil {
		return
	}
	u.panel.Stop()
	u.panel = nil
}

func (u *screenUI) waitForEnter(in *bufio.Reader) {
	if u.panel == nil {
		waitForEnter(in)
		return
	}
	defer u.stopMonitor()
	u.panel.SetOnEscape(nil)
	u.panel.ClearRun()
	u.panel.SetPrompt("Press Enter to return to the menu...")
	_, _ = in.ReadString('\n')
}
