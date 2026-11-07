package dashboard

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBar(t *testing.T) {
	cases := []struct {
		pct   float64
		width int
		want  string
	}{
		{50, 10, "█████░░░░░"},
		{0, 4, "░░░░"},
		{100, 4, "████"},
		{-5, 4, "░░░░"},  // clamped
		{150, 4, "████"}, // clamped
		{10, 3, "░░░"},   // 10% of 3 cells rounds to empty, not a fake block
	}
	for _, tt := range cases {
		if got := bar(tt.pct, tt.width); got != tt.want {
			t.Errorf("bar(%.0f, %d) = %q, want %q", tt.pct, tt.width, got, tt.want)
		}
	}
}

func TestRenderForLayouts(t *testing.T) {
	busy := make([]float64, 32)
	for i := range busy {
		busy[i] = 50
	}
	snap := Snapshot{
		Label: "general", Elapsed: 13 * time.Second, Duration: 30 * time.Second,
		HasRun: true, Busy: busy, MHz: map[int]float64{0: 4127},
		LoadPct: 52, LoadKnown: true,
		MemTotal: 34 << 30, MemUsed: 12 << 30, MemKnown: true,
	}
	cases := []struct {
		name  string
		cols  int
		rows  int
		lines int
		want  string // must appear
		noBar bool   // bars must NOT appear
	}{
		{"tall terminal single column", 90, 40, 34, "c0  |", false},
		{"short terminal two columns", 100, 20, 18, "c0  |", false},
		{"tiny terminal falls back to grid", 60, 10, 10, "c0   50%", true},
		{"unknown size falls back to grid", 0, 0, 10, "c0   50%", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			lines := RenderFor(snap, tt.cols, tt.rows)
			if len(lines) != tt.lines {
				t.Fatalf("got %d lines, want %d:\n%s", len(lines), tt.lines, strings.Join(lines, "\n"))
			}
			joined := strings.Join(lines, "\n")
			if !strings.Contains(joined, tt.want) {
				t.Errorf("panel missing %q:\n%s", tt.want, joined)
			}
			if tt.noBar && strings.Contains(joined, "█") {
				t.Errorf("fallback layout must not draw bars:\n%s", joined)
			}
			if !tt.noBar {
				// hardware on top, test progress at the bottom
				if !strings.Contains(lines[0], "load 52%") {
					t.Errorf("top line must be the hardware summary: %q", lines[0])
				}
				if !strings.Contains(lines[len(lines)-1], "general") {
					t.Errorf("bottom line must be the test progress: %q", lines[len(lines)-1])
				}
			}
		})
	}
}

// Every core must appear exactly once in each bar layout — nothing gets
// dropped just because the terminal is small.
func TestRenderForCoversAllCores(t *testing.T) {
	busy := make([]float64, 30)
	snap := Snapshot{Label: "x", Busy: busy}
	for _, dims := range [][2]int{{90, 40}, {100, 20}} {
		joined := strings.Join(RenderFor(snap, dims[0], dims[1]), "\n")
		for i := 0; i < len(busy); i++ {
			if !strings.Contains(joined, fmt.Sprintf("c%-2d ", i)) {
				t.Fatalf("dims %v: core %d missing from panel:\n%s", dims, i, joined)
			}
		}
		if got := strings.Count(joined, "|"); got != 2*len(busy) {
			t.Fatalf("dims %v: %d bar delimiters, want %d (each core exactly one bar)",
				dims, got, 2*len(busy))
		}
	}
}

func TestPanelUnknowns(t *testing.T) {
	lines := RenderFor(Snapshot{Label: "run", HasRun: true, Faults: 1}, 90, 40)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "load n/a") || !strings.Contains(joined, "mem n/a") {
		t.Errorf("unknowns must degrade to n/a:\n%s", joined)
	}
	if !strings.Contains(joined, "FAULTS 1") {
		t.Errorf("faults must surface:\n%s", joined)
	}
	// Idle has no live observation: no fault segment at all, not even
	// for a stale count (same honesty rule as the plain renderer).
	idle := strings.Join(RenderFor(Snapshot{Label: "idle", Faults: 1}, 90, 40), "\n")
	if strings.Contains(idle, "FAULTS") {
		t.Errorf("idle panel must make no fault claim:\n%s", idle)
	}
}

// The tested-memory segment: while a run owns engine regions the panel
// must state how many bytes are UNDER TEST, next to (never instead
// of) system memory usage - during a RAM profile the system number can
// be dominated by unrelated processes while only a headroom-sized
// slice is actually verified. Without a tested region the segment is
// absent (no claim), and an idle panel must not show a stale value.
func TestPanelTestedMemorySegment(t *testing.T) {
	snap := Snapshot{Label: "ram", HasRun: true,
		MemKnown: true, MemUsed: 82 << 30, MemTotal: 93 << 30,
		TestMem: 8 << 30}
	hw := hwLine(snap)
	if !strings.Contains(hw, "tested 8.0 GiB") {
		t.Errorf("bar header lacks the tested segment:\n%s", hw)
	}
	if !strings.Contains(hw, "mem 82.0 GiB/93.0 GiB") {
		t.Errorf("the system mem segment was replaced; both answers must be stated:\n%s", hw)
	}
	if f := footerLine(snap); !strings.Contains(f, "tested 8.0 GiB") {
		t.Errorf("compact footer lacks the tested segment:\n%s", f)
	}
	run := hwLine(Snapshot{HasRun: true, MemKnown: true})
	if strings.Contains(run, "tested") {
		t.Errorf("a run with no tested region must not claim one:\n%s", run)
	}
	idle := hwLine(Snapshot{MemKnown: true, TestMem: 8 << 30})
	if strings.Contains(idle, "tested") {
		t.Errorf("an idle panel must make no tested-memory claim:\n%s", idle)
	}
}

// With HWiNFO attached the panel shows the real sensor readings and
// says so; without it the clock column stays honestly labelled tsc.
func TestPanelHWinfoFields(t *testing.T) {
	busy := make([]float64, 4)
	snap := Snapshot{
		Label: "x", Busy: busy,
		MHz:        map[int]float64{0: 5600},
		ClockLabel: "MHz",
		TempC:      72, TempKnown: true,
		PowerW: 118, PowerKnown: true,
		VoltV: 1.206, VoltKnown: true,
		HWinfo: true,
	}
	joined := strings.Join(RenderFor(snap, 90, 40), "\n")
	for _, want := range []string{"temp 72°C", "PPT 118W", "Vid 1.206V", "[hwinfo]", "5600 MHz"} {
		if !strings.Contains(joined, want) {
			t.Errorf("panel missing %q:\n%s", want, joined)
		}
	}
	gridSnap := Snapshot{Label: "x", Busy: make([]float64, 32), MHz: map[int]float64{0: 5600}, ClockLabel: "MHz"}
	if joined := strings.Join(RenderFor(gridSnap, 60, 10), "\n"); !strings.Contains(joined, "5600M") {
		t.Errorf("grid fallback must show real MHz with hwinfo:\n%s", joined)
	}
	bare := Snapshot{Label: "x", Busy: busy, MHz: map[int]float64{0: 4127}}
	if joined := strings.Join(RenderFor(bare, 90, 40), "\n"); !strings.Contains(joined, "4127 tsc") {
		t.Errorf("without hwinfo the clock unit must stay tsc:\n%s", joined)
	}
}

// The panel advertises Escape only when a watcher is actually armed.
func TestPanelStopHint(t *testing.T) {
	busy := make([]float64, 4)
	joined := strings.Join(RenderFor(Snapshot{Label: "x", Busy: busy, CanStop: true, HasRun: true}, 90, 40), "\n")
	if !strings.Contains(joined, "[esc] stop") {
		t.Errorf("stop hint missing:\n%s", joined)
	}
	joined = strings.Join(RenderFor(Snapshot{Label: "x", Busy: busy}, 90, 40), "\n")
	if strings.Contains(joined, "[esc]") {
		t.Errorf("hint must not appear without an armed watcher:\n%s", joined)
	}
}

// The footer must say what is running, not just the phase tag: the
// user's "session-c3-r1" told nobody it was a one-core burst.
func TestPanelFooterNamesTheRun(t *testing.T) {
	joined := strings.Join(RenderFor(Snapshot{
		Label: "general", Phase: "session-c3-r1", HasRun: true,
		Run: "spin vs idle ×1 per-core", Busy: make([]float64, 4),
	}, 120, 40), "\n")
	if !strings.Contains(joined, "general · spin vs idle ×1 per-core · session-c3-r1") {
		t.Errorf("footer does not name the run:\n%s", joined)
	}
	// No run (monitoring between runs): no progress line at all -
	// "0% 0.0s/0.0s" of a run that does not exist would be a lie.
	// (0% busy CPU bars legitimately show "0%", so the tell is the
	// elapsed/duration pair, which only the progress line renders.)
	joined = strings.Join(RenderFor(Snapshot{Busy: make([]float64, 4)}, 120, 40), "\n")
	if strings.Contains(joined, "0.0s/0.0s") {
		t.Errorf("progress line survived with no run:\n%s", joined)
	}
	// The tool's own report (which y-cruncher test now) rides along.
	joined = strings.Join(RenderFor(Snapshot{
		Label: "general", Run: "y-cruncher ×16", Status: "BBP", HasRun: true, Busy: make([]float64, 4),
	}, 120, 40), "\n")
	if !strings.Contains(joined, "general · y-cruncher ×16 · BBP") {
		t.Errorf("footer does not show the tool status:\n%s", joined)
	}
}

// A line wider than the terminal would wrap and break the painter's
// cursor-up count, so RenderFor clips every line to the width.
func TestClipVisible(t *testing.T) {
	cases := []struct {
		name string
		s    string
		cols int
		want string
	}{
		{"short line untouched", "abc", 5, "abc"},
		{"plain clip", "abcdef", 3, "abc"},
		{"exact fit", "abc", 3, "abc"},
		{"ansi kept, visible counted", "\x1b[91mabc\x1b[0m", 2, "\x1b[91mab"},
		{"utf8 bars not split", "░█░█", 3, "░█░"},
		{"zero cols means no clip", "abc", 0, "abc"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := clipVisible(tt.s, tt.cols); got != tt.want {
				t.Errorf("clipVisible(%q, %d) = %q, want %q", tt.s, tt.cols, got, tt.want)
			}
		})
	}
	// RenderFor clips: a footer far wider than 20 columns must not
	// survive longer than 20 visible characters.
	lines := RenderFor(Snapshot{Label: "x", Run: strings.Repeat("y", 60), HasRun: true, Busy: make([]float64, 2)}, 20, 40)
	for _, l := range lines {
		if n := len([]rune(stripANSI(l))); n > 20 {
			t.Errorf("line not clipped to 20 columns (%d): %q", n, l)
		}
	}
}

// stripANSI removes SGR sequences for visible-length checks.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
