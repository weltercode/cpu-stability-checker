package dashboard

import "strings"

// Keep hardware at the top and the report immediately below it throughout
// a run and its cooldown. Leave one terminal row unused: Enter's echo
// must not scroll the viewport before the menu can take over.
func renderConsole(s Snapshot, report, prompt string, cols, rows int) []string {
	height := max(1, rows-1)
	reportRows := min(10, max(2, height/3))
	reportRows = min(reportRows, height-1)
	panelRows := height - reportRows
	lines := livePanel(s, cols, panelRows)
	if len(lines) > panelRows {
		// Extremely small windows cannot fit every CPU even in the grid.
		// Say so explicitly rather than silently losing the last cores.
		lines = []string{hwLine(s), "Window too small for all CPUs; enlarge it."}
		lines = lines[:min(len(lines), panelRows)]
	}
	var messages []string
	for _, line := range strings.Split(strings.ReplaceAll(report, "\r\n", "\n"), "\n") {
		// Blank report lines waste scarce rows without adding information.
		if strings.TrimSpace(line) != "" {
			messages = append(messages, line)
		}
	}
	// The panel budget chooses a stable CPU layout; unused panel rows
	// must not become a large blank gap before the results.
	available := max(0, height-len(lines))
	if available > 1 && (len(messages) > 0 || prompt != "") {
		lines = append(lines, "")
		available--
	}
	if prompt != "" && available > 0 {
		available--
	}
	if len(messages) > available {
		messages = messages[len(messages)-available:]
	}
	lines = append(lines, messages...)
	if prompt != "" && len(lines) < height {
		lines = append(lines, prompt)
	}
	return clipAll(lines, cols)
}

// Use the same hardware-first order in both bar and compact layouts.
// Removing completed progress must not move the CPU rows upward.
func livePanel(s Snapshot, cols, rows int) []string {
	extra := len(temperatureLines(s))
	n := len(s.Busy)
	switch {
	case n > 0 && rows >= n+2+extra && cols >= 44:
		return barsPanel(s, cols, 1)
	case n > 0 && rows >= half(n)+2+extra && cols >= 88:
		return barsPanel(s, cols, 2)
	}
	_, labelWidth := coreLabels(s)
	gridCols := max(1, (cols-2)/(labelWidth+13))
	lines := append([]string{hwLine(s)}, temperatureLines(s)...)
	lines = append(lines, gridLines(s, gridCols)...)
	if progress := progressLine(s); progress != "" {
		lines = append(lines, progress)
	}
	return lines
}
