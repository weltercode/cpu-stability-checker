package tui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A redirected stdout must never be probed as a screen: no VT, no painter.
func TestProbeDeclinesPipes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if c := Probe(w); c.Terminal || c.VT {
		t.Fatalf("Probe on a pipe = %+v, want all false", c)
	}
}

func TestPainterFrameRepaintsInPlace(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf)

	p.Frame([]string{"a", "b"})
	first := buf.String()
	if !strings.Contains(first, "a\x1b[0J") || strings.Contains(first, "\x1b[2J") {
		t.Fatalf("first frame wrong: %q", first)
	}
	if strings.HasSuffix(first, "\r\n") {
		t.Fatalf("frame must not end with a newline (it would scroll a full-height terminal): %q", first)
	}

	buf.Reset()
	p.Frame([]string{"c"})
	second := buf.String()
	if !strings.Contains(second, "\x1b[1A") {
		t.Fatalf("second frame must move up over the previous 2 lines (to their first line): %q", second)
	}
	if !strings.Contains(second, "c\x1b[0J") {
		t.Fatalf("second frame content wrong: %q", second)
	}
}

// Clear erases the current block so a text write can take its place:
// the session prints run headers and summaries between panel frames,
// and without this the next Frame's cursor-up count smears everything
// together (the user's "screen breaks at run end").
func TestPainterClear(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf)

	// Nothing painted yet: Clear must emit nothing (no zero-distance
	// cursor-up, no stray clears).
	p.Clear()
	if buf.Len() != 0 {
		t.Fatalf("Clear with no frame emitted %q", buf.String())
	}

	p.Frame([]string{"a", "b", "c"})
	buf.Reset()
	p.Clear()
	got := buf.String()
	if !strings.Contains(got, "\x1b[2A") || !strings.Contains(got, "\x1b[0J") {
		t.Fatalf("Clear must move up to the block top and clear to end of screen: %q", got)
	}

	// After Clear the next Frame draws fresh (no cursor-up): the text
	// that took the block's place stays where it was written.
	buf.Reset()
	p.Frame([]string{"d"})
	if strings.Contains(buf.String(), "\x1b[") && strings.Contains(buf.String(), "A") {
		t.Fatalf("frame after Clear must not move up: %q", buf.String())
	}
}

// Reset drops the cursor bookkeeping without emitting anything: after
// a terminal resize the old frame is no longer where the count points
// (the buffer scrolled/reflowed), so the next Frame must draw fresh
// wherever the caller just moved the cursor (typically viewport home).
func TestPainterReset(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf)

	p.Frame([]string{"a", "b", "c"})
	buf.Reset()
	p.Reset()
	if buf.Len() != 0 {
		t.Fatalf("Reset must not touch the stream: %q", buf.String())
	}

	buf.Reset()
	p.Frame([]string{"d"})
	if strings.Contains(buf.String(), "A") {
		t.Fatalf("frame after Reset must not move up (the count is untrustworthy): %q", buf.String())
	}
	if !strings.Contains(buf.String(), "d\x1b[0J") {
		t.Fatalf("frame after Reset must still paint: %q", buf.String())
	}
}

func TestPainterScreenClearsFirst(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf)
	p.Screen([]string{"x", "y"})
	if got := buf.String(); !strings.HasPrefix(got, "\x1b[2J\x1b[H") {
		t.Fatalf("Screen must clear then home: %q", got)
	}
	if !strings.Contains(buf.String(), "x\ny\n") {
		t.Fatalf("Screen content wrong: %q", buf.String())
	}
}

func TestPainterAltBuffer(t *testing.T) {
	var buf bytes.Buffer
	p := NewPainter(&buf)
	p.EnterAlt()
	if !strings.Contains(buf.String(), "\x1b[?1049h") {
		t.Fatalf("EnterAlt missing: %q", buf.String())
	}
	buf.Reset()
	p.LeaveAlt()
	if !strings.Contains(buf.String(), "\x1b[?1049l") {
		t.Fatalf("LeaveAlt missing: %q", buf.String())
	}
}

func TestRule(t *testing.T) {
	if got := Rule(3, "-"); got != "---" {
		t.Errorf("Rule(3,-) = %q", got)
	}
	if got := Rule(0, "-"); got != "-" {
		t.Errorf("Rule(0,-) = %q, want at least one char", got)
	}
}

// Size must not panic on any stream; on a real console it reports
// plausible dimensions.
func TestSizeNeverPanics(t *testing.T) {
	if cols, rows, ok := Size(os.Stdout); ok && (cols < 2 || rows < 2) {
		t.Errorf("implausible size %dx%d", cols, rows)
	}
}
