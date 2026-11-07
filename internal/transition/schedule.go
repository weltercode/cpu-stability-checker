package transition

import (
	"time"
)

// The burst schedule (P08): a PREPARED, fully concrete timeline.
// The plan demands the prepared schedule be journaled before the run
// so replay never depends on a future PRNG version: jitter is
// expanded into explicit windows up front, the journal stores the
// windows, and reproduction reads the windows - not the seed.
//
// The runner below walks that timeline against a clock and keeps the
// honesty contract: a late start is recorded as a deviation, the
// following windows shift from the ACTUAL close (the prepared gaps
// are preserved), and nothing ever compresses or busy-loops to
// "catch up" - the plan forbids the catch-up loop, and a schedule
// that quietly rewrites its own history is exactly how missed
// deadlines disappear.

// Window is one commanded burst slot: an idle gap (measured from the
// previous burst's close, or from the session becoming ready for the
// first window) followed by a run time. Gap=0 is legal: back-to-back
// bursts with no idle between them.
type Window struct {
	Gap    time.Duration
	RunFor time.Duration
}

// Schedule is a prepared timeline plus the provenance that produced
// it. The Windows are the reproduction artifact; Base/Jitter/Seed
// only say how they were built (and Seed is nil for a fixed
// schedule - jitter absent is not jitter-with-seed-zero).
type Schedule struct {
	Base    time.Duration
	Jitter  time.Duration
	Seed    *uint64
	Windows []Window
}

// jitterWord is the fixed PRNG spec for schedule jitter: splitmix64
// over (seed, window index). Like the ram word functions, this IS
// the generation spec - golden vectors pin it, and changing it means
// bumping the spec, never quietly re-deriving old runs' windows
// (which is precisely why the concrete windows get journaled).
func jitterWord(seed uint64, idx uint64) uint64 {
	x := seed + 0x9E3779B97F4A7C15*(idx+1)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}

// BuildSchedule expands count windows around a base idle gap with
// seeded jitter in [-jitter, +jitter] (clamped at zero - a negative
// gap is not a thing). Jitter=0 gives the fixed sequence. The result
// is fully concrete: every later consumer (journal, runner, report)
// works from these numbers, never from the seed.
func BuildSchedule(count int, baseGap, runFor, jitter time.Duration, seed uint64) Schedule {
	s := Schedule{Base: baseGap, Jitter: jitter, Windows: make([]Window, 0, count)}
	if jitter > 0 {
		seedCopy := seed
		s.Seed = &seedCopy
	}
	for i := 0; i < count; i++ {
		gap := baseGap
		if jitter > 0 {
			// Map the draw into [-jitter, +jitter) without bias:
			// the high bits of a splitmix64 draw (top bit cleared
			// so the int64 conversion is never negative) scale the
			// signed offset.
			off := int64(jitterWord(seed, uint64(i))>>1)%(2*int64(jitter)) - int64(jitter)
			gap += time.Duration(off)
			if gap < 0 {
				gap = 0
			}
		}
		s.Windows = append(s.Windows, Window{Gap: gap, RunFor: runFor})
	}
	return s
}

// ScheduleRunner walks a prepared schedule against a Clock, telling
// the driver WHEN a TRIGGER is planned and recording what actually
// happened. It is pure bookkeeping: it never sleeps, never resends,
// and never catches up.
type ScheduleRunner struct {
	sched Schedule
	clock Clock
	// due is the planned moment of the next TRIGGER (zero = no more
	// windows); it is re-anchored from the ACTUAL close of each
	// burst, so lateness shifts the rest of the timeline instead of
	// compressing it.
	due  time.Time
	idx  int
	open *Window // window currently running (Begin without End)
}

// NewScheduleRunner starts the timeline at `start` (the moment the
// session became ready): the first window's gap is measured from it.
func NewScheduleRunner(s Schedule, start time.Time, clock Clock) *ScheduleRunner {
	if clock == nil {
		clock = RealClock{}
	}
	r := &ScheduleRunner{sched: s, clock: clock}
	if len(s.Windows) > 0 {
		r.due = start.Add(s.Windows[0].Gap)
	}
	return r
}

// Due is the planned moment of the next TRIGGER; zero time means the
// schedule is exhausted.
func (r *ScheduleRunner) Due() time.Time { return r.due }

// Remaining counts the windows not yet begun.
func (r *ScheduleRunner) Remaining() int { return len(r.sched.Windows) - r.idx }

// Begin records the actual TRIGGER moment for the current window and
// returns the window plus the deviation from the planned moment
// (actual - planned: positive = late, negative = early). An early
// start means the prepared gap was cut short - that is a deviation
// too, and the driver must not hide it either. At the schedule's end
// Begin returns a zero window; the driver must not send.
func (r *ScheduleRunner) Begin(actual time.Time) (Window, time.Duration) {
	if r.idx >= len(r.sched.Windows) {
		return Window{}, 0
	}
	w := r.sched.Windows[r.idx]
	r.open = &w
	return w, actual.Sub(r.due)
}

// End records the actual VERIFY close of the begun window and
// re-anchors the next planned TRIGGER at actual-close + the NEXT
// window's prepared gap. The lateness of this window therefore
// shifts every later window by the same amount - the prepared idle
// gaps stay intact, and there is no catch-up.
func (r *ScheduleRunner) End(actual time.Time) {
	if r.open == nil {
		return
	}
	r.open = nil
	r.idx++
	if r.idx < len(r.sched.Windows) {
		r.due = actual.Add(r.sched.Windows[r.idx].Gap)
	} else {
		r.due = time.Time{}
	}
}
