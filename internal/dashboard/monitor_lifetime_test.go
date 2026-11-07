package dashboard

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

type paintedFrames struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	frame chan struct{}
}

func (w *paintedFrames) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	// In the compact layout the hardware footer completes each frame.
	if bytes.Contains(p, []byte("load ")) {
		select {
		case w.frame <- struct{}{}:
		default:
		}
	}
	return n, err
}
func (w *paintedFrames) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.buf.String() }

func TestPanelKeepsRepaintingAfterRunStateIsCleared(t *testing.T) {
	out := &paintedFrames{frame: make(chan struct{}, 8)}
	c := New(os.Stdout)
	// Exercise the real VT repaint loop against a synchronized buffer;
	// no terminal or stress workload is needed for this regression.
	c.vt = true
	c.file = os.Stdout
	c.out = out
	c.painter = tui.NewPainter(out)
	c.Push(Snapshot{HasRun: true, Label: "finished-regression-run"})
	c.Start()
	defer c.Stop()
	awaitFrame := func() {
		t.Helper()
		select {
		case <-out.frame:
		case <-time.After(5 * time.Second):
			t.Fatal("panel did not repaint hardware")
		}
	}
	awaitFrame()
	if !strings.Contains(out.text(), "finished-regression-run") {
		t.Fatal("initial run frame missing")
	}
	c.ClearRun()
	before := len(out.text())
	awaitFrame()
	tail := out.text()[before:]
	if !strings.Contains(tail, "load ") || strings.Contains(tail, "finished-regression-run") || c.Stopped() {
		t.Fatalf("monitor froze or kept finished run state: %q", tail)
	}
	c.Stop()
}
