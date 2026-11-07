// Package dashboard renders the live hardware panel shown during runs:
// per-core busy share and measured clock, system load, memory, the board
// temperature when one is exposed, and the run's WHEA tally. It is
// display-only by construction — it never touches run logic, the journal
// or exit codes — and the renderer is a pure function so the panel's
// shape is testable without a console.
package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
)

// Snapshot is one panel frame: run state pushed by the engine plus
// hardware readings sampled by the Sampler. Every unknown piece carries
// its Known flag so the panel can say n/a instead of inventing a zero.
type Snapshot struct {
	Label    string // test name
	Phase    string // scenario phase label (empty for single runs)
	Run      string // what is running: workloads, slot count, mode (set by the runner)
	Status   string // the tool's own "what I am doing now" report (external workloads)
	Elapsed  time.Duration
	Duration time.Duration
	Iters    uint64
	Rate     float64 // iterations per second
	HasRun   bool    // false until the engine has pushed real run state

	Busy       []float64       // per logical CPU busy %, nil = not sampled yet
	MHz        map[int]float64 // measured clock of cores under test
	ClockLabel string          // unit for the clock column: "tsc" or "MHz"
	CoreOf     []int           // logical index -> physical core index; nil = unknown
	BaseMHz    float64         // firmware-rated base clock (color scale), 0 = unknown
	LoadPct    float64
	LoadKnown  bool
	MemTotal   uint64
	MemUsed    uint64
	MemKnown   bool
	// TestMem: the bytes this run actually VERIFIES inside memory-engine
	// regions (sum of the parent-computed data grants). 0 = this run
	// owns no tested region (CPU-only run, external tool, idle panel)
	// and the segment is absent. This is deliberately NOT system
	// memory usage: during a RAM profile most system usage may belong
	// to other processes while only a headroom-sized slice is under
	// test - the two numbers answer different questions and the panel
	// says which is which.
	TestMem     uint64
	TempC       float64
	TempKnown   bool
	Temperature temperature.Status
	PowerW      float64 // CPU package power when a sensor exposes one
	PowerKnown  bool
	VoltV       float64 // core voltage when a sensor exposes one
	VoltKnown   bool
	HWinfo      bool // readings enriched from HWiNFO shared memory
	Wmi         bool // readings enriched from LHM/OHM WMI sensors
	Color       bool // VT console: decorate with SGR (set by the console only)
	CanStop     bool // Escape stops this run (the panel says so)
	WheaN       int
	WheaState   string // WHEA observation state: available/degraded/unavailable ("" = older caller, treated as available)
	Faults      int
	// LastFault names the newest core fault on one line: which core,
	// which workload/kernel, when, and what the kernel itself reported
	// (the ram kernels put pass/region/offset into their fault error,
	// so "where it broke" travels with the message). "" = no fault
	// observed so far. The panel paints the counter AND this line:
	// the counter says how many, the line says where - which run and
	// pass the footer already states.
	LastFault string
}

// Render draws the panel as plain text lines. The grid walks the logical
// CPUs in order, gridCols per row; measured clocks only exist for cores
// the run actually drives, everything else honestly shows "--".
func Render(s Snapshot, gridCols int) []string {
	if gridCols < 1 {
		gridCols = 4
	}
	var lines []string
	if hl := headerLine(s); hl != "" {
		lines = append(lines, hl)
		if s.LastFault != "" {
			lines = append(lines, "  "+s.LastFault)
		}
	}
	lines = append(lines, temperatureLines(s)...)
	lines = append(lines, gridLines(s, gridCols)...)
	lines = append(lines, footerLine(s))
	return lines
}

func headerLine(s Snapshot) string {
	if !s.HasRun {
		return "" // no run: no progress to report (see progressLine)
	}
	pct := 0
	if s.Duration > 0 {
		pct = int(100 * float64(s.Elapsed) / float64(s.Duration))
		if pct > 100 {
			pct = 100
		}
	}
	label := s.Label
	if s.Run != "" {
		label += " · " + s.Run
	}
	if s.Status != "" {
		label += " · " + s.Status
	}
	if s.Phase != "" {
		label += " · " + s.Phase
	}
	line := fmt.Sprintf("%s  %3d%%  %s/%s", label, pct, human.Dur(s.Elapsed), human.Dur(s.Duration))
	if s.HasRun && s.Iters > 0 {
		line += fmt.Sprintf("  %s it @ %s/s", human.Count(s.Iters), human.Rate(s.Rate))
	}
	return line
}

func gridLines(s Snapshot, cols int) []string {
	if len(s.Busy) == 0 {
		return []string{"  busy: n/a"}
	}
	labels, _ := coreLabels(s)
	grouped := len(s.CoreOf) == len(s.Busy)
	cells := make([]string, len(s.Busy))
	for i, busy := range s.Busy {
		suffix := "t"
		if s.ClockLabel == "MHz" {
			suffix = "M"
		}
		label := labels[i]
		if s.Color && grouped {
			label = sgr(coreHue(s.CoreOf[i]), label)
		}
		clock := "   --"
		if mhz, ok := s.MHz[i]; ok {
			clock = fmt.Sprintf("%4.0f%s", mhz, suffix)
			if s.Color && s.ClockLabel == "MHz" && s.BaseMHz > 0 {
				clock = sgr(mhzHue(mhz, s.BaseMHz), clock)
			}
		}
		cells[i] = fmt.Sprintf("%s %3.0f%% %s", label, busy, clock)
	}
	var lines []string
	for start := 0; start < len(cells); start += cols {
		end := start + cols
		if end > len(cells) {
			end = len(cells)
		}
		lines = append(lines, "  "+strings.Join(cells[start:end], "  "))
	}
	return lines
}

// wheaLabel renders the WHEA counter together with the observation
// state: "WHEA 0" is only a fact when observation was available; when
// collection was degraded or unavailable the count means nothing, so
// the panel shows "?" instead of a number that would imply coverage
// it never had.
func wheaLabel(s Snapshot) string {
	if s.WheaState != "" && s.WheaState != "available" {
		return "WHEA ?"
	}
	return fmt.Sprintf("WHEA %d", s.WheaN)
}

func footerLine(s Snapshot) string {
	var parts []string
	if s.LoadKnown {
		parts = append(parts, fmt.Sprintf("load %d%%", int(s.LoadPct+0.5)))
	} else {
		parts = append(parts, "load n/a")
	}
	if s.Temperature.State != "" {
		// The shared sensor/control gets its own lines above the CPU grid.
	} else if s.TempKnown {
		parts = append(parts, fmt.Sprintf("temp %.0f°C", s.TempC))
	} else {
		parts = append(parts, "temp n/a")
	}
	if s.MemKnown {
		parts = append(parts, fmt.Sprintf("mem %s/%s", human.Bytes(s.MemUsed), human.Bytes(s.MemTotal)))
	} else {
		parts = append(parts, "mem n/a")
	}
	if s.HasRun && s.TestMem > 0 {
		// The compact grid carries the same tested-memory segment the
		// bar panel's header does - the answer must not depend on
		// which layout fit the terminal.
		parts = append(parts, "tested "+human.Bytes(s.TestMem))
	}
	parts = append(parts, wheaLabel(s))
	// The fault counter belongs to a run: between runs there is no
	// live observation to claim "0 faults", so the segment is absent
	// when idle - while a run is live it is ALWAYS present, green at
	// zero (nothing has faulted so far) and bright red above zero.
	// Silence used to hide the zero and make "not shown" ambiguous
	// against "none observed"; an explicit green 0 says which it is.
	if s.HasRun {
		parts = append(parts, fmt.Sprintf("FAULTS %d", s.Faults))
	}
	return strings.Join(parts, "  ")
}

func temperatureLines(s Snapshot) []string {
	t := s.Temperature
	if t.State == "" {
		return nil
	}
	value := "temp n/a"
	if t.Known {
		value = fmt.Sprintf("temp %.1f°C", t.Reading.Celsius)
	}
	control := "control " + t.State
	if t.LimitC > 0 {
		control += fmt.Sprintf(" (%.1f°C)", t.LimitC)
	}
	sensor := "sensor unavailable"
	if t.Reading.Source == "" && t.Reason != "" {
		sensor += ": " + t.Reason
	}
	if t.Reading.Source != "" {
		sensor = "sensor " + t.Reading.Source + ": " + t.Reading.Sensor
		if !t.Known {
			sensor += " (last)"
		}
	}
	return []string{value + " | " + control, sensor}
}
