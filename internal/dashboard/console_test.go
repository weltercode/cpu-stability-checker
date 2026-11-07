package dashboard

import (
	"os"
	"testing"
)

// The session owns one Console across runs: engines must not Stop it,
// and Stop must be safe to call exactly once by the owner - even when
// the panel never started (pipes, classic consoles) or when a caller
// loses track and calls it twice.
func TestConsoleStopOwnership(t *testing.T) {
	c := New(os.Stdout) // test stdout is a pipe: the panel declines to paint
	if c.Active() {
		t.Fatal("pipe stdout must not activate the panel")
	}
	if c.Stopped() {
		t.Error("fresh Console reports Stopped")
	}
	c.Stop()
	if !c.Stopped() {
		t.Error("Stop() did not report Stopped")
	}
	c.Stop() // second Stop must not panic (close of closed channel)
}

// A finished run must not haunt the monitoring frames: ClearRun drops
// the run state so the session-owned panel's next frames show plain
// hardware, not a completed run frozen at its last progress.
func TestConsoleClearRun(t *testing.T) {
	c := New(os.Stdout)
	defer c.Stop()
	c.Push(Snapshot{Label: "general", Run: "y-cruncher ×16", HasRun: true})
	if c.Current().Run == "" {
		t.Fatal("Push did not store the run state")
	}
	c.ClearRun()
	if got := c.Current(); got.Run != "" || got.HasRun || got.Label != "" {
		t.Errorf("ClearRun left run state: %+v", got)
	}
}

// The panel hard-redraws (clear viewport + draw at top) exactly when
// the viewport geometry changed: a resize scrolls/reflows the buffer,
// so the painter's cursor-up count can no longer find the old frame.
// Unknown dimensions (a transient size-query failure) must not trigger
// a full clear every second.
func TestViewportChanged(t *testing.T) {
	cases := []struct {
		name               string
		lastC, lastR, c, r int
		want               bool
	}{
		{"same size", 90, 40, 90, 40, false},
		{"width changed", 90, 40, 120, 40, true},
		{"height changed", 90, 40, 90, 25, true},
		{"both changed", 90, 40, 120, 30, true},
		{"previous unknown", 0, 0, 90, 40, false},
		{"current unknown (query failed)", 90, 40, 0, 0, false},
		{"half unknown", 90, 40, 90, 0, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := viewportChanged(tt.lastC, tt.lastR, tt.c, tt.r); got != tt.want {
				t.Errorf("viewportChanged(%d,%d,%d,%d) = %v, want %v", tt.lastC, tt.lastR, tt.c, tt.r, got, tt.want)
			}
		})
	}
}

// On a non-VT stream (a pipe, a file) Print is the plain write: the
// append-mode fallback stays byte-identical to fmt.Print.
func TestConsolePrintFallback(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "panel")
	if err != nil {
		t.Fatal(err)
	}
	c := New(f)
	defer c.Stop()
	if c.Active() {
		t.Fatal("temp file must not activate the panel")
	}
	c.Print("hello\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("fallback wrote %q, want %q", got, "hello\n")
	}
}

// Escape is armed per run: the watcher (and the "ESC stops the run"
// hint) exists only while a run is armed, so the menu's line reader
// never loses keystrokes between runs.
func TestConsoleEscapeArming(t *testing.T) {
	c := New(os.Stdout)
	defer c.Stop()

	fired := 0
	c.SetOnEscape(func() { fired++ })
	c.escapeFired()
	if fired != 1 {
		t.Errorf("armed Escape fired %d times, want 1", fired)
	}

	c.SetOnEscape(nil) // run over: the menu may read input again
	c.escapeFired()    // a stale watcher callback must be inert
	if fired != 1 {
		t.Errorf("disarmed Escape fired (total %d), want no further calls", fired)
	}

	// Re-arming for the next run works on the same Console.
	c.SetOnEscape(func() { fired += 10 })
	c.escapeFired()
	if fired != 11 {
		t.Errorf("re-armed Escape fired %d total, want 11", fired)
	}
}
