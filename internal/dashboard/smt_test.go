package dashboard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

func topo(cores ...[]sysinfo.LogicalCPU) sysinfo.Topology {
	t := sysinfo.Topology{Source: "test"}
	for i, cpus := range cores {
		t.PhysicalCores = append(t.PhysicalCores, sysinfo.PhysicalCore{Index: i, LogicalCPUs: cpus})
	}
	return t
}

func cpu(group uint16, number uint8) sysinfo.LogicalCPU {
	return sysinfo.LogicalCPU{Group: group, Number: number}
}

func TestBuildCoreOf(t *testing.T) {
	cases := []struct {
		name string
		topo sysinfo.Topology
		n    int
		want []int // nil = must stay unmapped
	}{
		{"amd consecutive pairs", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 1)},
			[]sysinfo.LogicalCPU{cpu(0, 2), cpu(0, 3)},
		), 4, []int{0, 0, 1, 1}},
		{"intel separated siblings", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 2)},
			[]sysinfo.LogicalCPU{cpu(0, 1), cpu(0, 3)},
		), 4, []int{0, 1, 0, 1}},
		{"second group unmapped", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 1)},
			[]sysinfo.LogicalCPU{cpu(1, 0), cpu(1, 1)},
		), 4, nil},
		{"gap unmapped", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 1)},
			[]sysinfo.LogicalCPU{cpu(0, 2)}, // 3 never appears
		), 4, nil},
		{"duplicate unmapped", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 1)},
			[]sysinfo.LogicalCPU{cpu(0, 1)},
		), 4, nil},
		{"out of range unmapped", topo(
			[]sysinfo.LogicalCPU{cpu(0, 0), cpu(0, 9)},
		), 4, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCoreOf(tt.topo, tt.n)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestCoreLabels(t *testing.T) {
	cases := []struct {
		name  string
		snap  Snapshot
		want  []string
		width int
	}{
		{"unmapped keeps logical", Snapshot{Busy: []float64{0, 0, 0}},
			[]string{"c0 ", "c1 ", "c2 "}, 3},
		{"amd pairs", Snapshot{Busy: make([]float64, 4), CoreOf: []int{0, 0, 1, 1}},
			[]string{"c0 ", "c0*", "c1 ", "c1*"}, 3},
		{"separated siblings share the core label", Snapshot{Busy: make([]float64, 4), CoreOf: []int{0, 1, 0, 1}},
			[]string{"c0 ", "c1 ", "c0*", "c1*"}, 3},
		{"three siblings", Snapshot{Busy: make([]float64, 3), CoreOf: []int{0, 0, 0}},
			[]string{"c0  ", "c0* ", "c0**"}, 4},
		{"two digit cores pad", Snapshot{Busy: make([]float64, 32), CoreOf: coreOf32()},
			nil, 4},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, w := coreLabels(tt.snap)
			if w != tt.width {
				t.Fatalf("width %d, want %d", w, tt.width)
			}
			if tt.want == nil {
				for _, l := range got {
					if len(l) != w {
						t.Fatalf("label %q not padded to %d", l, w)
					}
				}
				if got[0] != "c0  " || got[1] != "c0* " {
					t.Fatalf("first pair = %q %q", got[0], got[1])
				}
				if got[31] != "c15*" {
					t.Fatalf("last sibling = %q", got[31])
				}
				return
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("labels %q, want %q", got, tt.want)
				}
			}
		})
	}
}

// coreOf32: 16 cores, consecutive sibling pairs (this machine's shape).
func coreOf32() []int {
	out := make([]int, 32)
	for i := range out {
		out[i] = i / 2
	}
	return out
}

var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Stripping every SGR span must reproduce the plain layout exactly:
// color is decoration, never structure.
func TestColorStripEqualsPlain(t *testing.T) {
	snap := Snapshot{
		Label: "stress", Busy: []float64{100, 100, 42, 7},
		CoreOf: []int{0, 0, 1, 1}, MHz: map[int]float64{0: 5600, 1: 5600, 2: 4300, 3: 1200},
		ClockLabel: "MHz", BaseMHz: 4300, TempC: 81, TempKnown: true, WheaN: 2, HasRun: false,
	}
	plain := strings.Join(RenderFor(snap, 90, 40), "\n")
	snap.Color = true
	colored := strings.Join(RenderFor(snap, 90, 40), "\n")
	if colored == plain {
		t.Fatal("color mode produced no SGR at all")
	}
	if got := sgrRE.ReplaceAllString(colored, ""); got != plain {
		t.Errorf("stripped color != plain:\nplain:\n%s\ncolored:\n%s", plain, colored)
	}
}

func TestSiblingHuesMatch(t *testing.T) {
	snap := Snapshot{Busy: make([]float64, 4), CoreOf: []int{0, 0, 1, 1}, Color: true}
	lines := RenderFor(snap, 90, 40)
	hues := map[string]string{}
	for _, line := range lines {
		m := firstSpanRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if !strings.HasPrefix(m[2], "c") {
			continue // header spans (the green WHEA zero) are not core rows
		}
		hues[m[2]] = m[1]
	}
	if len(hues) != 4 {
		t.Fatalf("expected 4 labeled rows, got %v", hues)
	}
	if hues["c0"] != hues["c0*"] || hues["c1"] != hues["c1*"] {
		t.Errorf("siblings must share a hue: %v", hues)
	}
	if hues["c0"] == hues["c1"] {
		t.Errorf("neighbouring cores must differ: %v", hues)
	}
}

// firstSpanRE captures the SGR code and label of the first colored span;
// the label stops at whitespace or the next escape.
var firstSpanRE = regexp.MustCompile(`\x1b\[(\d+)m([^\s\x1b]+)`)

// WHEA zero is confirmed green and errors bright red; a degraded or
// unavailable observation replaces the count with "?" — a zero we
// never watched is not a fact (and the data source is tagged in
// brackets, per user request: "WHEA 0 зелёный, не ноль красный";
// "hwinfo в квадратные скобки - как источник данных").
func TestHWLineWheaColorAndSourceTag(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Snapshot)
		want  []string
		never []string
	}{
		{"whea zero confirmed green", func(s *Snapshot) { s.Color = true },
			[]string{"\x1b[32mWHEA 0"}, nil},
		{"whea nonzero bright red", func(s *Snapshot) { s.Color = true; s.WheaN = 3 },
			[]string{"\x1b[91mWHEA 3"}, []string{"\x1b[32mWHEA"}},
		{"degraded observation shows ? not a fake green zero", func(s *Snapshot) { s.Color = true; s.WheaN = 0; s.WheaState = "degraded" },
			[]string{"\x1b[93mWHEA ?"}, []string{"WHEA 0", "\x1b[32mWHEA"}},
		{"unavailable observation shows ? even uncolored", func(s *Snapshot) { s.WheaState = "unavailable" },
			[]string{"WHEA ?"}, []string{"WHEA 0"}},
		// P13: armed-but-unproven (no completed query cycle yet) is
		// also not a green zero - coverage starts unknown.
		{"initializing observation shows ? never a green zero", func(s *Snapshot) { s.Color = true; s.WheaN = 0; s.WheaState = "initializing" },
			[]string{"\x1b[93mWHEA ?"}, []string{"WHEA 0", "\x1b[32mWHEA"}},
		{"plain console keeps bare text", func(s *Snapshot) {},
			[]string{"WHEA 0"}, []string{"\x1b["}},
		{"sources bracketed as tags", func(s *Snapshot) { s.HWinfo = true; s.Wmi = true },
			[]string{"[hwinfo]", "[wmi]"}, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := Snapshot{Label: "x", Busy: make([]float64, 2), LoadKnown: true, LoadPct: 10}
			tt.mut(&s)
			joined := strings.Join(RenderFor(s, 90, 40), "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("panel missing %q:\n%s", want, joined)
				}
			}
			for _, bad := range tt.never {
				if strings.Contains(joined, bad) {
					t.Errorf("panel must not contain %q:\n%s", bad, joined)
				}
			}
		})
	}
}

// The fault counter follows the WHEA color precedent (user request:
// show ram errors like WHEA - green zero, bright red above zero), but
// it is run-scoped: during a live run the segment is ALWAYS present
// (green 0 = "nothing has faulted SO FAR", a fact about the
// observation), and an idle panel omits it entirely because it has no
// live observation and no right to claim "0 faults".
func TestHWLineFaultCounter(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Snapshot)
		want  []string
		never []string
	}{
		{"live run zero confirmed green", func(s *Snapshot) { s.Color = true; s.HasRun = true },
			[]string{"\x1b[32mFAULTS 0"}, nil},
		{"live run nonzero bright red", func(s *Snapshot) { s.Color = true; s.HasRun = true; s.Faults = 2 },
			[]string{"\x1b[91mFAULTS 2"}, []string{"\x1b[32mFAULTS"}},
		{"idle panel makes no fault claim", func(s *Snapshot) { s.Color = true },
			nil, []string{"FAULTS"}},
		{"idle panel with stale count stays silent", func(s *Snapshot) { s.Color = true; s.Faults = 3 },
			nil, []string{"FAULTS"}},
		{"plain console keeps bare text", func(s *Snapshot) { s.HasRun = true; s.Faults = 1 },
			[]string{"FAULTS 1"}, []string{"\x1b["}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := Snapshot{Label: "x", Busy: make([]float64, 2), LoadKnown: true, LoadPct: 10}
			tt.mut(&s)
			joined := strings.Join(RenderFor(s, 90, 40), "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Errorf("panel missing %q:\n%s", want, joined)
				}
			}
			for _, bad := range tt.never {
				if strings.Contains(joined, bad) {
					t.Errorf("panel must not contain %q:\n%s", bad, joined)
				}
			}
		})
	}
}

// The "where it broke" line: composed by the executor (core, workload,
// moment, kernel's own pass/region/offset story), painted bright red
// under the hardware header, and counted into the layout's height
// budget like the temperature lines so adding it never overlaps the
// grid.
func TestPanelLastFaultLine(t *testing.T) {
	base := func() Snapshot {
		return Snapshot{Label: "x", Busy: make([]float64, 2), LoadKnown: true, LoadPct: 10, HasRun: true, Faults: 1}
	}
	msg := "FAULT core 5 · ram-moving-v1 · 12:03:11 · child exited with code 1"

	t.Run("colored red under the header", func(t *testing.T) {
		s := base()
		s.Color = true
		s.LastFault = msg
		lines := RenderFor(s, 90, 40)
		idx := -1
		for i, l := range lines {
			if strings.Contains(l, "FAULT core 5") {
				idx = i
			}
		}
		if idx == -1 {
			t.Fatalf("fault line missing:\n%s", strings.Join(lines, "\n"))
		}
		if idx > len(temperatureLines(s))+1 {
			t.Errorf("fault line at %d must sit right under the header block", idx)
		}
		if !strings.Contains(lines[idx], "\x1b[91m") {
			t.Errorf("fault line must be bright red: %q", lines[idx])
		}
	})
	t.Run("plain console paints it uncolored", func(t *testing.T) {
		s := base()
		s.LastFault = msg
		joined := strings.Join(RenderFor(s, 90, 40), "\n")
		if !strings.Contains(joined, msg) || strings.Contains(joined, "\x1b[91mFAULT core 5") {
			t.Errorf("plain panel must show the line without color:\n%s", joined)
		}
	})
	t.Run("height budget counts the fault line", func(t *testing.T) {
		s := base()
		s.Busy = make([]float64, 6)
		// 6 bars in one column need exactly n+2 rows in an 8-row
		// terminal; adding the fault line must switch layout to the
		// two-column budget, not paint a 9th line the terminal has no
		// room for.
		plain := RenderFor(s, 90, 8)
		s.LastFault = msg
		with := RenderFor(s, 90, 8)
		if len(plain) != 8 {
			t.Fatalf("classic single-column budget should fill 8 rows, got %d", len(plain))
		}
		if len(with) > 8 {
			t.Errorf("fault line pushed the panel past the terminal height: %d lines", len(with))
		}
		joined := strings.Join(with, "\n")
		if !strings.Contains(joined, "FAULT core 5") {
			t.Errorf("fault line must survive the layout switch:\n%s", joined)
		}
	})
}

func TestHueThresholds(t *testing.T) {
	tempCases := []struct {
		t    float64
		want int
	}{{0, 32}, {59.9, 32}, {60, 33}, {74.9, 33}, {75, 91}, {95, 91}}
	for _, tt := range tempCases {
		if got := tempHue(tt.t); got != tt.want {
			t.Errorf("tempHue(%v) = %d, want %d", tt.t, got, tt.want)
		}
	}
	mhzCases := []struct {
		mhz, base float64
		want      int
	}{{5600, 4300, 92}, {4730, 4300, 92}, {4729, 4300, 32}, {4300, 4300, 32}, {4299, 4300, 33}, {2150, 4300, 33}, {2149, 4300, 90}, {0, 4300, 90}}
	for _, tt := range mhzCases {
		if got := mhzHue(tt.mhz, tt.base); got != tt.want {
			t.Errorf("mhzHue(%v, %v) = %d, want %d", tt.mhz, tt.base, got, tt.want)
		}
	}
	loadCases := []struct {
		pct  float64
		want int
	}{{0, 32}, {49, 32}, {50, 33}, {79, 33}, {80, 91}, {100, 91}}
	for _, tt := range loadCases {
		if got := loadHue(tt.pct); got != tt.want {
			t.Errorf("loadHue(%v) = %d, want %d", tt.pct, got, tt.want)
		}
	}
}

// Without a rated base there is no honest scale to shade clocks by:
// the clock column must stay uncolored.
func TestNoBaseNoClockColor(t *testing.T) {
	snap := Snapshot{Busy: []float64{100}, MHz: map[int]float64{0: 5600}, ClockLabel: "MHz", Color: true}
	line := strings.Join(RenderFor(snap, 90, 40), "\n")
	if strings.Contains(line, "\x1b[92m") {
		t.Error("clock shaded without a known base")
	}
}
