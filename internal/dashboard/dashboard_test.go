package dashboard

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRender(t *testing.T) {
	snap := Snapshot{
		Label: "general", Phase: "heavy",
		Elapsed: 12 * time.Second, Duration: 30 * time.Second,
		Iters: 12_300_000, Rate: 410_000_000, HasRun: true,
		Busy:    []float64{98, 97},
		MHz:     map[int]float64{0: 4127},
		LoadPct: 62, LoadKnown: true,
		MemTotal: 34 << 30, MemUsed: 12 << 30, MemKnown: true,
		WheaN: 0,
	}
	lines := Render(snap, 4)
	if len(lines) != 3 { // header + 1 grid row + footer
		t.Fatalf("Render produced %d lines: %q", len(lines), lines)
	}
	header := lines[0]
	for _, want := range []string{"general · heavy", "40%", "12.0s/30.0s"} {
		if !strings.Contains(header, want) {
			t.Errorf("header %q missing %q", header, want)
		}
	}
	grid := lines[1]
	if !strings.Contains(grid, "c0   98% 4127t") {
		t.Errorf("grid %q missing measured clock cell", grid)
	}
	if !strings.Contains(grid, "c1   97%") {
		t.Errorf("grid %q missing busy cell for core 1", grid)
	}
	footer := lines[len(lines)-1]
	for _, want := range []string{"load 62%", "temp n/a", "mem ", "WHEA 0"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer %q missing %q", footer, want)
		}
	}
	// The run's fault counter is always stated while a run is live:
	// an explicit 0 ("nothing reported so far") beats the old
	// ambiguous silence that left "not shown" and "none observed"
	// indistinguishable.
	if !strings.Contains(footer, "FAULTS 0") {
		t.Errorf("footer %q must state the fault count even when it is zero", footer)
	}
}

func TestRenderUnknowns(t *testing.T) {
	lines := Render(Snapshot{Label: "run", HasRun: true, Faults: 2}, 4)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "busy: n/a") {
		t.Errorf("panel without busy samples must say n/a, got:\n%s", joined)
	}
	if !strings.Contains(joined, "load n/a") || !strings.Contains(joined, "mem n/a") {
		t.Errorf("panel must degrade unknowns to n/a, got:\n%s", joined)
	}
	if !strings.Contains(joined, "FAULTS 2") {
		t.Errorf("panel must surface faults, got:\n%s", joined)
	}
	// The counter belongs to a run: an idle panel has no live
	// observation and no right to claim any fault count, not even a
	// leftover one from an old snapshot.
	idle := strings.Join(Render(Snapshot{Label: "idle", Faults: 2}, 4), "\n")
	if strings.Contains(idle, "FAULTS") {
		t.Errorf("idle panel must make no fault claim, got:\n%s", idle)
	}
}

// The fault context line rides directly under the header in the plain
// render too: the counter says how many, this line says where.
func TestRenderLastFaultLine(t *testing.T) {
	s := Snapshot{Label: "ram", HasRun: true, Faults: 1,
		LastFault: "FAULT core 5 · ram-moving-v1 · 12:03:11 · 1 mismatch in pass 1"}
	lines := Render(s, 4)
	if len(lines) < 2 || lines[1] != "  "+s.LastFault {
		t.Errorf("fault line must be the second line under the header, got %q", lines)
	}
	footer := lines[len(lines)-1]
	if !strings.Contains(footer, "FAULTS 1") {
		t.Errorf("footer %q must still carry the count alongside the line", footer)
	}
	s.LastFault = ""
	if joined := strings.Join(Render(s, 4), "\n"); strings.Contains(joined, "mismatch in pass 1") {
		t.Errorf("no fault line must appear when LastFault is empty:\n%s", joined)
	}
}

// A redirected stdout must decline the panel entirely: no ANSI ever
// reaches a pipe.
func TestConsoleDeclinesPipes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	c := New(w) // keep one owner; a second os.File finalizer can close a reused handle
	if c.Active() {
		c.Stop()
		t.Fatal("console claims to be active on a pipe")
	}
	c.Push(Snapshot{Label: "x"})
	c.Start()
	c.Stop() // must not block or panic even though the loop never ran
}

// The sampler runs against the real OS: after two frames it must have
// per-core busy numbers on any platform with CoreTimes. The rendered
// frame is logged so a human can eyeball the real panel shape in -v runs.
func TestSamplerAppliesHardware(t *testing.T) {
	s := NewSampler()
	defer s.Close()
	var first, second Snapshot
	s.Apply(&first)
	time.Sleep(50 * time.Millisecond)
	s.Apply(&second)
	if second.Busy == nil {
		t.Skip("platform has no per-core times")
	}
	if len(second.Busy) == 0 {
		t.Fatal("busy slice empty")
	}
	for i, b := range second.Busy {
		if b < 0 || b > 100 {
			t.Errorf("core %d busy %.1f%% out of range", i, b)
		}
	}
	// The SMT mapping must cover every shown CPU with contiguous core
	// indices on any machine that reports a usable topology.
	if len(second.CoreOf) == len(second.Busy) {
		seen := map[int]bool{}
		for i, c := range second.CoreOf {
			if c < 0 || c >= len(second.CoreOf) {
				t.Fatalf("core index %d for CPU %d out of range", c, i)
			}
			seen[c] = true
		}
		for c := 0; c < len(seen); c++ {
			if !seen[c] {
				t.Errorf("core index %d missing from the mapping", c)
			}
		}
	} else {
		t.Logf("topology unmapped on this platform (%d CPUs shown)", len(second.Busy))
	}
	t.Logf("rated base: %.0f MHz", second.BaseMHz)
	second.Label = "smoke"
	t.Logf("\n%s", strings.Join(Render(second, 4), "\n"))
	second.Color = true
	t.Logf("\n%s", strings.Join(RenderFor(second, 90, 40), "\n"))
}
