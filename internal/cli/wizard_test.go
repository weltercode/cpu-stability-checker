package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// The widget contract: every step is a full screen; the chosen answer
// stays as a line, and the previous step's option list is gone from the
// next frame — the user never scrolls through a pile of old prompts.
func TestWizardScreenReplacesOptions(t *testing.T) {
	var buf bytes.Buffer
	ui := &screenUI{p: tui.NewPainter(&buf)}
	in := bufio.NewReader(strings.NewReader("2\n\n"))

	w := newWizard(in, ui, "Demo")
	if got := w.choose("Mode", "Choose a mode", []string{"A — first way", "B — second way"}, 0); got != 1 {
		t.Fatalf("first choose = %d, want 1", got)
	}
	if got := w.choose("Exec", "Choose exec", []string{"X only", "Y only"}, 0); got != 0 {
		t.Fatalf("second choose (default) = %d, want 0", got)
	}

	frames := strings.Split(buf.String(), "\x1b[2J")
	if len(frames) != 3 { // leading empty part + 2 frames
		t.Fatalf("expected 2 full-screen frames, got %d:\n%q", len(frames), buf.String())
	}
	first, second := frames[1], frames[2]
	for _, want := range []string{"1) A — first way", "2) B — second way"} {
		if !strings.Contains(first, want) {
			t.Errorf("first frame missing option %q", want)
		}
	}
	if !strings.Contains(second, "Mode:") || !strings.Contains(second, "B") {
		t.Errorf("second frame must keep the recorded answer, got:\n%q", second)
	}
	if strings.Contains(second, "first way") {
		t.Errorf("second frame must not repeat step 1's options:\n%q", second)
	}
	if !strings.Contains(second, "2) Y only") {
		t.Errorf("second frame missing current options:\n%q", second)
	}
}

// Validation errors ride exactly one frame and then vanish.
func TestWizardNoteShownOnce(t *testing.T) {
	var buf bytes.Buffer
	ui := &screenUI{p: tui.NewPainter(&buf)}
	in := bufio.NewReader(strings.NewReader("9\n1\n"))
	w := newWizard(in, ui, "Demo")
	if got := w.choose("Mode", "Choose a mode", []string{"A", "B"}, 0); got != 0 {
		t.Fatalf("choose = %d, want 0", got)
	}
	frames := strings.Split(buf.String(), "\x1b[2J")
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames (initial + error redraw), got %d", len(frames)-1)
	}
	if !strings.Contains(frames[2], "Enter a number from 1 to 2") {
		t.Errorf("error frame missing the validation note:\n%q", frames[2])
	}
	if got := strings.Count(buf.String(), "Enter a number"); got != 1 {
		t.Errorf("note must appear exactly once, got %d times", got)
	}
}

// Without a screen the wizard is the old sequential prompts verbatim:
// no ANSI ever reaches the stream.
func TestWizardAppendModeUnchanged(t *testing.T) {
	var buf bytes.Buffer
	ui := &screenUI{} // no painter: append mode
	// screenPrompt prints to os.Stdout, not a buffer, so assert behavior
	// through the answers instead: the wizard must consume exactly the
	// scripted lines and return the same results as the free functions.
	in := bufio.NewReader(strings.NewReader("2\n\n"))
	w := newWizard(in, ui, "Demo")
	if got := w.choose("Mode", "Choose a mode", []string{"A", "B"}, 0); got != 1 {
		t.Fatalf("append choose = %d, want 1", got)
	}
	if got := w.askPositiveInt("Passes", "Number of complete passes", 3); got != 3 {
		t.Fatalf("append askPositiveInt = %d, want 3 (default)", got)
	}
	if buf.Len() != 0 {
		t.Error("append mode must not write through a painter")
	}
}
