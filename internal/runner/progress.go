package runner

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// isTerminal reports whether f is attached to a real console (not a pipe).
func isTerminal(f *os.File) bool {
	if st, err := f.Stat(); err == nil {
		return st.Mode()&os.ModeCharDevice != 0
	}
	return false
}

// startProgress rewrites one compact console line until stop is closed. It
// deliberately avoids ANSI cursor movement: a carriage return works in both
// Windows Terminal and classic consoles without filling the scrollback.
func startProgress(rs *runStatus, test string, showMetrics bool, stop chan struct{}) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	lastWidth := 0
	for {
		select {
		case <-stop:
			fmt.Println()
			return
		case <-tick.C:
			line := compactProgressLine(rs, test, showMetrics)
			width := utf8.RuneCountInString(line)
			padding := ""
			if width < lastWidth {
				padding = strings.Repeat(" ", lastWidth-width)
			}
			fmt.Printf("\r%s%s", line, padding)
			lastWidth = width
		}
	}
}

func compactProgressLine(rs *runStatus, test string, showMetrics bool) string {
	elapsed := time.Since(rs.start)
	pct := 0
	if rs.duration > 0 {
		pct = int(100 * float64(elapsed) / float64(rs.duration))
		if pct > 100 {
			pct = 100
		}
	}
	sec := elapsed.Seconds()
	if sec < 0.05 {
		sec = 0.05
	}
	var total uint64
	for _, c := range rs.cores {
		total += c.itersNow()
	}
	line := fmt.Sprintf("%s %3d%% %s/%s it=%s rate=%s/s",
		test, pct, human.Dur(elapsed), human.Dur(rs.duration), human.Count(total), human.Rate(float64(total)/sec))
	if showMetrics {
		if metrics := compactMetrics(sysinfo.CPUStats()); metrics != "" {
			line += " | " + metrics
		}
	}
	return line
}

func compactMetrics(m sysinfo.Metrics) string {
	var parts []string
	if m.LoadKnown {
		parts = append(parts, fmt.Sprintf("cpu=%.0f%%", m.LoadPct))
	}
	if m.FreqKnown {
		parts = append(parts, fmt.Sprintf("freq=%.0fMHz", m.FreqMaxMHz))
	}
	if m.TempKnown {
		parts = append(parts, fmt.Sprintf("temp=%.0fC", m.TempC))
	}
	return strings.Join(parts, " ")
}

// startDashboard feeds the live hardware panel until stop is closed. The
// panel package owns hardware sampling and repainting; here we only
// translate run state into snapshots and measure the clock our own
// worker threads achieved (the only per-core clock visible unprivileged).
// startDashboard paints the run into the panel. The WHEA counters
// arrive as a getter (the shared Guard owns the observation), so the
// painter never touches the observer's internals directly.
func startDashboard(d *dashboard.Console, rs *runStatus, test string, opt plan.Options, wheaStats func() (int64, string), selected []int, stop chan struct{}) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	// What is running, in a few honest characters (see plan.RunSummary).
	run := plan.RunSummary(opt, len(rs.cores))
	prev := map[int]uint64{} // slot -> last cycle reading
	prevAt := time.Now()
	for {
		select {
		case <-stop:
			// The run is over. Hand the panel back to plain monitoring:
			// a session-owned console keeps painting between runs, and a
			// stale "98% - y-cruncher x16" footer would lie about what
			// is happening now.
			d.ClearRun()
			return
		case <-tick.C:
			now := time.Now()
			sec := now.Sub(rs.start).Seconds()
			if sec < 0.05 {
				sec = 0.05
			}
			el := now.Sub(prevAt).Nanoseconds()
			var total uint64
			mhz := map[int]float64{}
			for i, c := range rs.cores {
				total += c.itersNow()
				tid := c.tid.Load()
				if tid == 0 || i >= len(selected) {
					continue
				}
				if cyc, err := sysinfo.ThreadCycleTime(int(tid)); err == nil {
					if p, ok := prev[i]; ok && el > 0 {
						mhz[selected[i]] = float64(cyc-p) * 1000 / float64(el)
					}
					prev[i] = cyc
				}
			}
			prevAt = now
			// The first slot whose tool self-reported something owns the
			// footer (single-instance external tools: exactly one speaks).
			status := ""
			for _, c := range rs.cores {
				if s := c.statusNow(); s != "" {
					status = s
					break
				}
			}
			wheaN, wheaState := wheaStats()
			// The newest fault on its own red line: core, the workload
			// it ran, the moment, and the kernel's own story (the ram
			// kernels name themselves, their pass, region and first
			// offset inside the fault error - which run/pass answers
			// the footer's attempt line, because this counter counts
			// THIS attempt only).
			lastFault := ""
			if core, msg, at, ok := rs.lastFault(); ok {
				wl := opt.A
				if opt.B != "" {
					wl = opt.A + "/" + opt.B
				}
				lastFault = fmt.Sprintf("FAULT core %d · %s · %s · %s", core, wl, at.Format("15:04:05"), msg)
			}
			d.Push(dashboard.Snapshot{
				Label:     test,
				Phase:     opt.Phase,
				Run:       run,
				Status:    status,
				Elapsed:   now.Sub(rs.start),
				Duration:  rs.duration,
				Iters:     total,
				Rate:      float64(total) / sec,
				HasRun:    true,
				MHz:       mhz,
				WheaN:     int(wheaN),
				WheaState: wheaState,
				Faults:    rs.faultCount(),
				LastFault: lastFault,
			})
		}
	}
}
