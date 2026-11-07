// Package tui holds the screen primitives shared by the interactive menu,
// the configure wizard and the live run panel: terminal capability probes,
// terminal size, and a painter that redraws whole frames in place (with an
// optional alternate buffer, so the UI lives on one screen instead of
// growing an endless scrollback). Input deliberately stays line-oriented
// (bufio.Reader): scripted and piped input keeps working untouched, and a
// non-VT output simply never gets a Painter.
package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Capabilities describes what this output stream can do. Both screen
// flags must be true before any screen drawing is attempted; Color
// additionally honors NO_COLOR and TERM=dumb (menu plan section 7).
type Capabilities struct {
	Terminal bool // attached to a real console, not a pipe or file
	VT       bool // ANSI processing is on (enabled here, or native)
	Color    bool // VT plus the user's color wishes
}

// Probe checks f for screen drawing and turns on VT processing when that
// is possible. Probing never fails; unsupported outputs report false and
// callers keep their plain append-mode behavior.
func Probe(f *os.File) Capabilities {
	c := Capabilities{Terminal: isTerminal(f)}
	if c.Terminal {
		c.VT = enableVT(f)
	}
	c.Color = c.VT && colorAllowed()
	return c
}

// isTerminal reports whether f is attached to a real console (not a pipe).
func isTerminal(f *os.File) bool {
	if st, err := f.Stat(); err == nil {
		return st.Mode()&os.ModeCharDevice != 0
	}
	return false
}

// Painter redraws whole text frames on a VT-capable stream. Two modes:
// Frame repaints a block in place (the live panel), Screen clears the
// whole screen first (dialogs and one-shot screens). The painter never
// reads input and never touches anything but its own output.
type Painter struct {
	out       io.Writer
	lastLines int // block height of the most recent in-place Frame
}

// NewPainter draws to w. Callers must have probed VT support first; the
// Painter itself stays testable against a plain buffer.
func NewPainter(w io.Writer) *Painter { return &Painter{out: w} }

// EnterAlt switches to the terminal's alternate buffer and clears it: the
// UI's screens never pollute the user's scrollback, and leaving restores
// exactly what was there before.
func (p *Painter) EnterAlt() {
	fmt.Fprint(p.out, "\x1b[?1049h")
	p.lastLines = 0
}

// LeaveAlt returns to the main buffer.
func (p *Painter) LeaveAlt() {
	fmt.Fprint(p.out, "\x1b[?1049l")
	p.lastLines = 0
}

// Reset forgets the block height of the most recent Frame without
// touching the screen. Used when the terminal geometry changed under
// the painter: a resize scrolls or reflows the buffer, so the old
// frame is no longer reliably lastLines-1 rows above the cursor and
// the cursor-up bookkeeping must be dropped, not followed.
func (p *Painter) Reset() { p.lastLines = 0 }

// Frame repaints a block in place: cursor back over the previous frame,
// then each line followed by a clear-to-end-of-line. The block must not
// end with a newline: a trailing \r\n scrolls the terminal whenever the
// frame is as tall as the window, which desynchronises the next
// frame's cursor-up count and smears old and new frames together.
func (p *Painter) Frame(lines []string) {
	if p.lastLines > 0 {
		fmt.Fprintf(p.out, "\x1b[%dA\r", p.lastLines-1)
	}
	for i, l := range lines {
		fmt.Fprintf(p.out, "%s\x1b[0J", l)
		if i != len(lines)-1 {
			fmt.Fprint(p.out, "\r\n")
		}
	}
	p.lastLines = len(lines)
}

// Clear erases the block left by the most recent Frame (if any) and
// leaves the cursor at its top line, so the next Frame or a plain-text
// write starts exactly there. Callers use it when text must be printed
// between frames: without it the next Frame's cursor-up count would
// treat the printed text as part of the old block and smear the two
// together (the user's "screen breaks at run end").
func (p *Painter) Clear() {
	if p.lastLines > 0 {
		fmt.Fprintf(p.out, "\x1b[%dA\r\x1b[0J", p.lastLines-1)
		p.lastLines = 0
	}
}

// Screen clears the entire screen and draws one fresh frame. Dialogs use
// this so each step replaces the last: answers stay as lines the caller
// chose to keep, option lists come and go.
func (p *Painter) Screen(lines []string) {
	fmt.Fprint(p.out, "\x1b[2J\x1b[H")
	p.lastLines = 0
	for _, l := range lines {
		fmt.Fprintln(p.out, l)
	}
}

// Join is a tiny helper for screen builders.
func Join(lines ...string) []string { return lines }

// Rule draws a horizontal separator clipped to width.
func Rule(width int, ch string) string {
	if width < 1 {
		width = 1
	}
	return strings.Repeat(ch, width)
}
