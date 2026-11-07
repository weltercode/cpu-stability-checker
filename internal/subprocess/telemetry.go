package subprocess

import (
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/clocksrc"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// lookupCoreOf maps logical CPU numbers (the order CoreTimes indexes)
// to physical core indices for producer-clock attribution; the strict
// coverage rules live in clocksrc.LookupCoreOf.
func lookupCoreOf() []int { return clocksrc.LookupCoreOf() }

// Per-process mode has no in-process workers to watch, but the parent
// already owns everything live telemetry needs: a handle on each child
// (opened while the parent still owns the process, so a recycled PID can
// never masquerade as a child) and the machine-wide CoreTimes. A child
// lives entirely on its pinned core, so its cycle counter over wall time
// is that core's observed clock, and CoreTimes deltas give the idle
// share. Children and the journal stay untouched.

// kidSnap is the sampler's per-child view for one tick: the cycle
// counter bound to the child's identity, plus the rotation state so an
// intentional freeze never depresses the clock aggregate.
type kidSnap struct {
	cycles func() (uint64, error)
	frozen bool
	epoch  int // rotation epoch at snapshot time
}

// procTel carries the parent's per-second telemetry window: cycle
// baselines per core with the rotation epoch they belong to, and the
// last CoreTimes snapshot. The first sample only establishes baselines;
// unknown pieces shrink the line, never fake a zero.
type procTel struct {
	cyc   map[int]uint64
	epoch map[int]int
	times []sysinfo.CoreTime
	at    time.Time
	// last holds the per-core measured clock (MHz) of the most recent
	// sample — the live panel reads it; the aggregate line stays ours.
	last map[int]float64
}

// sample returns one compact telemetry line for the window since the
// last call, or "" when nothing is known yet. A freeze boundary inside
// the window (epoch changed) or a currently frozen child only rebaselines
// the counter: cycles across a suspension say nothing about clock. A
// child whose counter vanished (process gone, handle refused) loses its
// baseline and simply shrinks the line until it is readable again. live
// carries the parent sampler's most recent producer clock tick (liveOK
// false when that tick produced nothing): the core-clock segment names
// its source and never merges with the observed cycle rate.
func (t *procTel) sample(kids map[int]kidSnap, selected []int, live clocksrc.Sample, liveOK bool, coreOf []int, now time.Time) string {
	first := t.at.IsZero()
	el := now.Sub(t.at).Nanoseconds()
	if t.cyc == nil {
		t.cyc = map[int]uint64{}
		t.epoch = map[int]int{}
	}
	t.last = map[int]float64{}

	var tel human.TelemetryStats
	for core, k := range kids {
		cyc, err := k.cycles()
		if err != nil {
			delete(t.cyc, core)
			continue
		}
		prev, had := t.cyc[core]
		if !first && had && t.epoch[core] == k.epoch && !k.frozen && el > 0 {
			mhz := float64(cyc-prev) * 1000 / float64(el)
			tel.AddClock(mhz)
			t.last[core] = mhz
		}
		t.cyc[core] = cyc
		t.epoch[core] = k.epoch
	}

	// Producer-reported real clocks for this tick: only children that
	// are alive and unfrozen get a clock attributed to their core.
	if liveOK && coreOf != nil {
		for core, k := range kids {
			if k.frozen {
				continue
			}
			if core >= 0 && core < len(coreOf) {
				if mhz, ok := live.Clocks[coreOf[core]]; ok {
					tel.AddCoreClock(mhz, live.Src)
				}
			}
		}
	}

	if ts, err := sysinfo.CoreTimes(); err == nil {
		if t.times != nil && !first && len(ts) == len(t.times) {
			for _, cpu := range selected {
				if cpu >= 0 && cpu < len(ts) {
					if p, ok := t.times[cpu].IdlePct(ts[cpu]); ok {
						tel.AddIdle(&p)
					}
				}
			}
		}
		t.times = ts
	}
	t.at = now

	return tel.Line()
}
