package tui

import (
	"fmt"
	"os"
	"time"
)

// Geometry is one raw observation of the terminal's own numbers. The
// window rect is what the console CLAIMS is visible; buffer and max
// window ride along because a claim can be wrong (a console created
// without a desktop reported a 2x2 window over a 120x30 buffer, and
// the panel rendered the fragment "lo") and the honest answer to
// "which number lies" needs all of them on the record.
type Geometry struct {
	WinCols, WinRows   int
	BufCols, BufRows   int
	MaxCols, MaxRows   int
	PickCols, PickRows int // what pickSize/Size would hand a painter
	OK                 bool
}

// GeometryGate turns a stream of size observations into ONE stable
// geometry to paint against. A terminal (or the pty under it) can
// report a collapsed rect for a moment and a normal one the next: the
// live panel then cleared and reflowed every second and the user
// watched the frame flip between full and unreadable fragments
// (measured: full <-> ~12x8 on a real desktop). The gate adopts a new
// geometry only after it has been observed at least twice AND has
// been the pending candidate for geometryConfirm or longer. A failed
// query never confirms and never clears the pending candidate - a
// transient failure must not discard evidence of a real resize, and
// it must not count as agreement either. Two candidates alternating
// never confirm each other: the last adopted geometry simply keeps
// painting. The first valid observation is adopted at once - there is
// nothing to protect yet. A genuinely persistent small geometry IS
// adopted after the wait: refusing it forever would silently mask the
// user's real window resize, and the panel's own "window too small"
// line keeps telling the truth about it.
type GeometryGate struct {
	Now func() time.Time // injectable clock (tests); nil = time.Now

	accCols, accRows int
	pCols, pRows     int
	pCount           int
	pFirst           time.Time
}

// geometryConfirm is the wall-clock persistence a new geometry must
// show before adoption. It is time-based, not sample-based, so one
// policy serves both the panel's 1s repaint and the menu's 500ms
// resize poll.
const geometryConfirm = time.Second

// Observe feeds one query result to the gate and returns the geometry
// to paint against (0,0 = nothing adopted yet; callers keep their
// documented fallback chain).
func (g *GeometryGate) Observe(cols, rows int, ok bool) (int, int) {
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	if !ok || cols <= 0 || rows <= 0 {
		return g.accCols, g.accRows
	}
	if g.accCols <= 0 || g.accRows <= 0 {
		g.accCols, g.accRows = cols, rows
		g.pCols, g.pRows, g.pCount = 0, 0, 0
		return cols, rows
	}
	switch {
	case cols == g.accCols && rows == g.accRows:
		g.pCols, g.pRows, g.pCount = 0, 0, 0
	case cols == g.pCols && rows == g.pRows:
		g.pCount++
		if g.pCount >= 2 && !now.Before(g.pFirst.Add(geometryConfirm)) {
			g.accCols, g.accRows = cols, rows
			g.pCols, g.pRows, g.pCount = 0, 0, 0
		}
	default:
		g.pCols, g.pRows = cols, rows
		g.pCount = 1
		g.pFirst = now
	}
	return g.accCols, g.accRows
}

// GateState is the gate's current decision record, for tracing.
type GateState struct {
	AccCols, AccRows   int
	PendCols, PendRows int
	PendCount          int
}

func (g *GeometryGate) State() GateState {
	return GateState{g.accCols, g.accRows, g.pCols, g.pRows, g.pCount}
}

// SizeTrace appends raw geometry observations to the file named by
// CPU_STABILITY_SIZEDIAG. Unset (the normal case) means nil and every
// Line call is a no-op: the panel never fails over diagnostics and
// never writes a file nobody asked for.
type SizeTrace struct{ path string }

func NewSizeTrace() *SizeTrace {
	if p := os.Getenv("CPU_STABILITY_SIZEDIAG"); p != "" {
		return &SizeTrace{path: p}
	}
	return nil
}

// Line records one observation: the raw numbers, what the gate
// accepted, what it is waiting on, and - when a second handle was
// queried - what a freshly opened CONOUT$ says. The question this
// answers is "do the handles disagree, or does the host really report
// this geometry?".
func (t *SizeTrace) Line(phase string, g Geometry, st GateState, conout *Geometry) {
	if t == nil {
		return
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // honest: no trace when the file cannot be opened; the run continues
	}
	defer f.Close()
	extra := ""
	if conout != nil {
		extra = fmt.Sprintf(" conout_win=%dx%d conout_ok=%v", conout.WinCols, conout.WinRows, conout.OK)
	}
	fmt.Fprintf(f, "%s %s ok=%v win=%dx%d buf=%dx%d max=%dx%d pick=%dx%d acc=%dx%d pend=%dx%d#%d%s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), phase,
		g.OK, g.WinCols, g.WinRows, g.BufCols, g.BufRows, g.MaxCols, g.MaxRows,
		g.PickCols, g.PickRows,
		st.AccCols, st.AccRows, st.PendCols, st.PendRows, st.PendCount, extra)
}
