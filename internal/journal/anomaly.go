package journal

import "sort"

// Clock-anomaly detection is DIAGNOSTIC ONLY, and deliberately so. The
// effective clock is a derived measurement (cycles per wall time): it
// legitimately sags with power state, workload mix and scheduling. A dip
// says "look at this window", it never says "the CPU is unstable", and it
// never changes exit codes. What it does catch, cheaply and without any
// privileged sensor, is the shape a real throttle event leaves behind.

// ClockDip is one load window whose observed clock fell well below the
// median of its own comparison group.
type ClockDip struct {
	RunID     string
	ClockMHz  float64
	MedianMHz float64
	IdlePct   float64 // -1 when idle was unknown for the window
}

const (
	anomalyIdleGate = 20.0 // windows idler than this are not load: settle, frozen rotation, idle work
	anomalyMinGroup = 4    // fewer windows cannot establish a median worth trusting
	anomalyDipFrac  = 0.6  // below 60% of the group median counts as a dip
)

// DetectClockAnomalies groups every core's telemetry windows by phase —
// the only comparable grouping (same work, same duration) — and flags
// windows under 60% of the group's own median clock. Windows that were
// mostly idle are skipped outright: a settle window or a rotation freeze
// has a low clock by construction, and calling that an anomaly is the
// false positive this detector must never produce. Cores are mapped to
// logical numbers exactly as SummarizeCores does.
func DetectClockAnomalies(reports []RunReport) map[int][]ClockDip {
	type key struct {
		core  int
		phase string
	}
	type sample struct {
		runID string
		clock float64
		idle  float64
	}
	groups := map[key][]sample{}

	for _, rep := range reports {
		label := rep.Phase
		if label == "" {
			label = "test:" + rep.Test
		}
		for _, c := range rep.Cores {
			logical := c.Core
			if !rep.PerProcess && logical >= 0 && logical < len(rep.Selected) {
				logical = rep.Selected[logical]
			}
			for _, t := range c.Telemetry {
				if t.ClockMHz <= 0 {
					continue
				}
				idle := -1.0
				if t.IdlePct != nil {
					idle = *t.IdlePct
					if idle > anomalyIdleGate {
						continue // not a load window
					}
				}
				k := key{logical, label}
				groups[k] = append(groups[k], sample{rep.RunID, t.ClockMHz, idle})
			}
		}
	}

	out := map[int][]ClockDip{}
	for k, gs := range groups {
		if len(gs) < anomalyMinGroup {
			continue
		}
		clocks := make([]float64, len(gs))
		for i, s := range gs {
			clocks[i] = s.clock
		}
		sort.Float64s(clocks)
		median := clocks[len(clocks)/2]
		if median <= 0 {
			continue
		}
		for _, s := range gs {
			if s.clock < median*anomalyDipFrac {
				out[k.core] = append(out[k.core], ClockDip{
					RunID: s.runID, ClockMHz: s.clock, MedianMHz: median, IdlePct: s.idle,
				})
			}
		}
	}
	for core := range out {
		sort.Slice(out[core], func(i, j int) bool { return out[core][i].ClockMHz < out[core][j].ClockMHz })
	}
	return out
}
