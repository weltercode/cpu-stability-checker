package dashboard

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/weltercode/cpu-stability-checker/internal/human"
)

// The htop-style panel: hardware summary on top, one load bar per logical
// CPU in the middle, the test's own progress at the bottom. RenderFor
// adapts to the terminal — bars when the height allows (one column, then
// two), the compact grid from dashboard.go when it does not. Nothing is
// ever silently dropped: every core appears in whichever layout fits.
//
// The sampler uses MHz for hardware clock sensors or Windows estimates.
// HWiNFO supplies conventional Core Clock, shared by the SMT siblings.
// Legacy snapshots carrying only an OS cycle-counter rate keep the tsc
// label so they cannot be mistaken for core frequency.

// RenderFor draws the best panel for a cols x rows terminal. Unknown or
// zero dimensions fall back to the compact grid. Every line is clipped
// to the terminal width: a wrapped line breaks the painter's cursor-up
// count exactly like a foreign write would.
func RenderFor(s Snapshot, cols, rows int) []string {
	if len(s.Busy) == 0 || cols <= 0 || rows <= 0 {
		return Render(s, 4)
	}
	n := len(s.Busy)
	extra := len(temperatureLines(s)) + faultLineCount(s)
	var lines []string
	switch {
	case rows >= n+2+extra && cols >= 44:
		lines = barsPanel(s, cols, 1)
	case rows >= half(n)+2+extra && cols >= 88:
		lines = barsPanel(s, cols, 2)
	default:
		lines = Render(s, 4)
	}
	return clipAll(lines, cols)
}

func half(n int) int { return (n + 1) / 2 }

// faultLineCount: the context line ("where it broke") takes one row
// of the panel when a run is live and has faulted; the height
// budgets must count it like the temperature lines are counted.
func faultLineCount(s Snapshot) int {
	if s.HasRun && s.LastFault != "" {
		return 1
	}
	return 0
}

// clipAll clips every line to cols visible characters.
func clipAll(lines []string, cols int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = clipVisible(l, cols)
		if strings.Contains(l, "\x1b[") {
			// Clipping may have removed a closing SGR reset.
			out[i] += "\x1b[0m"
		}
	}
	return out
}

// clipVisible truncates a rendered line to cols visible characters,
// keeping ANSI escape sequences (they paint no columns). SGR-only,
// which is all the panel emits. Rune-counted: the bars and separators
// are multi-byte, and cutting one in half would garble the line.
func clipVisible(s string, cols int) string {
	if cols <= 0 {
		return s
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				b.WriteString(s[i : j+1])
				i = j + 1
			} else {
				i = j
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if visible >= cols {
			break
		}
		b.WriteRune(r)
		visible++
		i += size
	}
	return b.String()
}

// coreLabels returns one padded label per logical CPU and their shared
// width. With a valid CoreOf mapping the label names the physical core
// and marks SMT siblings with stars (c0, c0*, c0**...), so a pair reads
// as one core even when the OS numbers them far apart (Intel's N and
// N+16); sibling rank follows the logical index, never adjacency.
// Without the mapping the logical numbers are kept exactly as before.
func coreLabels(s Snapshot) (labels []string, width int) {
	if len(s.CoreOf) != len(s.Busy) {
		labels = make([]string, len(s.Busy))
		for i := range s.Busy {
			labels[i] = fmt.Sprintf("c%-2d", i)
		}
		return labels, 3
	}
	rank := make([]int, len(s.Busy))
	seen := make(map[int]int)
	for i, core := range s.CoreOf {
		rank[i] = seen[core]
		seen[core]++
	}
	labels = make([]string, len(s.Busy))
	for i, core := range s.CoreOf {
		labels[i] = fmt.Sprintf("c%d%s", core, strings.Repeat("*", rank[i]))
		if len(labels[i]) > width {
			width = len(labels[i])
		}
	}
	for i := range labels {
		labels[i] = fmt.Sprintf("%-*s", width, labels[i])
	}
	return labels, width
}

// barsPanel lays the cores out in colCount columns of load bars.
func barsPanel(s Snapshot, cols, colCount int) []string {
	labels, labelW := coreLabels(s)
	grouped := len(s.CoreOf) == len(s.Busy)
	barW := cols/colCount - 26
	if colCount == 1 {
		barW = cols - 26
	}
	barW -= labelW - 3 // labels grew past the classic "c0 " width
	switch {
	case barW > 32:
		barW = 32
	case barW < 4:
		barW = 4
	}

	lines := []string{hwLine(s)}
	lines = append(lines, temperatureLines(s)...)
	// The newest fault gets its own red line right under the header:
	// the header segment says HOW MANY, this line says WHERE - core,
	// workload/kernel and time, with the kernel's own pass/region/
	// offset story inside its message (the ram kernels put that story
	// in their fault error). The run and pass context the user asked
	// for is the footer's "Run x/y | pass x/y" line of the SAME
	// attempt this counter counts.
	if s.LastFault != "" {
		fl := "  " + s.LastFault
		if s.Color {
			fl = "  " + sgr(91, s.LastFault)
		}
		lines = append(lines, fl)
	}
	n := len(s.Busy)
	perCol := (n + colCount - 1) / colCount
	for top := 0; top < perCol; top++ {
		var cells []string
		for k := 0; k < colCount; k++ {
			i := top + k*perCol
			if i >= n {
				continue
			}
			label := labels[i]
			if s.Color && grouped {
				label = sgr(coreHue(s.CoreOf[i]), label)
			}
			loadBar := bar(s.Busy[i], barW)
			if s.Color {
				loadBar = sgr(loadHue(s.Busy[i]), loadBar)
			}
			clock := "   --"
			if mhz, ok := s.MHz[i]; ok {
				clock = fmt.Sprintf("%4.0f", mhz)
				if s.Color && s.ClockLabel == "MHz" && s.BaseMHz > 0 {
					clock = sgr(mhzHue(mhz, s.BaseMHz), clock)
				}
			}
			cells = append(cells, fmt.Sprintf("%s |%s| %3.0f%% %s %s",
				label, loadBar, s.Busy[i], clock, clockUnit(s)))
		}
		lines = append(lines, "  "+strings.Join(cells, "  "))
	}
	if pl := progressLine(s); pl != "" {
		lines = append(lines, pl)
	}
	return lines
}

// bar renders a pct-wide block bar of the given width.
func bar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(pct*float64(width)/100 + 0.5)
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// clockUnit names the clock column honestly: the engine's cycle-counter
// clocks are the invariant TSC; only HWiNFO's readings are real MHz.
func clockUnit(s Snapshot) string {
	if s.ClockLabel == "MHz" {
		return "MHz"
	}
	return "tsc"
}

// hwLine is the top line: what the machine is doing, independent of the
// test itself.
func hwLine(s Snapshot) string {
	var parts []string
	if s.LoadKnown {
		parts = append(parts, fmt.Sprintf("load %d%%", int(s.LoadPct+0.5)))
	} else {
		parts = append(parts, "load n/a")
	}
	if s.MemKnown {
		parts = append(parts, fmt.Sprintf("mem %s/%s", human.Bytes(s.MemUsed), human.Bytes(s.MemTotal)))
	} else {
		parts = append(parts, "mem n/a")
	}
	if s.HasRun && s.TestMem > 0 {
		// The bytes under test this run verifies - the grant envelope
		// the parent computed once. The system "mem" segment beside it
		// says what the OS is doing; ONLY this segment answers "how
		// much memory is actually tested".
		parts = append(parts, "tested "+human.Bytes(s.TestMem))
	}
	if s.Temperature.State != "" {
		// Render the shared sensor and cutoff state on dedicated lines.
	} else if s.TempKnown {
		t := fmt.Sprintf("temp %.0f°C", s.TempC)
		if s.Color {
			t = sgr(tempHue(s.TempC), t)
		}
		parts = append(parts, t)
	} else {
		parts = append(parts, "temp n/a")
	}
	if s.PowerKnown {
		parts = append(parts, fmt.Sprintf("PPT %.0fW", s.PowerW))
	}
	if s.VoltKnown {
		parts = append(parts, fmt.Sprintf("Vid %.3fV", s.VoltV))
	}
	whea := wheaLabel(s)
	switch {
	case s.Color && s.WheaState != "" && s.WheaState != "available":
		whea = sgr(93, whea) // yellow: observation degraded — the count means nothing
	case s.Color && s.WheaN > 0:
		whea = sgr(91, whea) // bright red: the machine reported errors
	case s.Color:
		whea = sgr(32, whea) // green: zero is a fact worth confirming
	}
	parts = append(parts, whea)
	// The fault counter is run-scoped and always stated during a run:
	// green at zero (nothing has faulted SO FAR - a fact about the
	// observation, exactly like the green WHEA zero), bright red
	// above zero. Between runs the segment is absent: an idle panel
	// has no live observation and no right to claim "0 faults".
	if s.HasRun {
		f := fmt.Sprintf("FAULTS %d", s.Faults)
		switch {
		case s.Color && s.Faults > 0:
			f = sgr(91, f) // bright red: a kernel or worker reported a fault
		case s.Color:
			f = sgr(32, f) // green: a zero worth confirming
		}
		parts = append(parts, f)
	}
	if s.HWinfo {
		parts = append(parts, "[hwinfo]") // the data source, bracketed like a tag
	}
	if s.Wmi {
		parts = append(parts, "[wmi]")
	}
	return strings.Join(parts, "  ")
}

// progressLine is the bottom line: the test's own state. With no run
// (a session-owned panel watching hardware between runs) there is no
// progress to show: "0% 0.0s/0.0s" of nothing would be a lie, so the
// line is simply absent.
func progressLine(s Snapshot) string {
	if !s.HasRun {
		return ""
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
		label += " · " + s.Run // what is actually running: workloads ×slots mode
	}
	if s.Status != "" {
		label += " · " + s.Status // the tool's own report: which test now
	}
	if s.Phase != "" {
		label += " · " + s.Phase
	}
	line := fmt.Sprintf("%s  %3d%%  %s/%s", label, pct, human.Dur(s.Elapsed), human.Dur(s.Duration))
	if s.HasRun && s.Iters > 0 {
		// External tools (y-cruncher) never count iterations: zeros
		// there are noise, not information.
		line += fmt.Sprintf("  %s it @ %s/s", human.Count(s.Iters), human.Rate(s.Rate))
	}
	if s.CanStop {
		line += "  [esc] stop"
	}
	return line
}
