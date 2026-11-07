package transition

import (
	"testing"
	"time"
)

// The jitter PRNG spec is the generation contract: golden values are
// pinned as literals. (Replay itself never needs this function -
// the concrete windows get journaled - but generation must stay
// reproducible for the same seed, and a silent mixing change would
// make "same seed, same schedule" a lie.)
func TestJitterWordGoldenVectors(t *testing.T) {
	tests := []struct {
		seed, idx uint64
		want      uint64
	}{
		{0, 0, 0xe220a8397b1dcdaf},
		{0, 1, 0x6e789e6aa1b965f4},
		{42, 3, 0x581ce1ff0e4ae394},
		{0xDEADBEEFCAFEBABE, 7, 0x796b308a7fe49981},
	}
	for _, tt := range tests {
		if got := jitterWord(tt.seed, tt.idx); got != tt.want {
			t.Errorf("jitterWord(%d,%d) = %016x, want the pinned %016x", tt.seed, tt.idx, got, tt.want)
		}
	}
}

// A fixed schedule (jitter 0) carries no seed claim: "no jitter" is
// not "jitter with seed 0" - the journal must be able to tell them
// apart.
func TestBuildScheduleFixed(t *testing.T) {
	s := BuildSchedule(4, 100*time.Millisecond, 50*time.Millisecond, 0, 7)
	if s.Seed != nil {
		t.Errorf("fixed schedule claims seed %d: jitter-absent must not read as seeded", *s.Seed)
	}
	if len(s.Windows) != 4 {
		t.Fatalf("windows = %d, want 4", len(s.Windows))
	}
	for i, w := range s.Windows {
		if w.Gap != 100*time.Millisecond || w.RunFor != 50*time.Millisecond {
			t.Errorf("window %d = %+v, want the fixed 100ms/50ms", i, w)
		}
	}
}

// Seeded jitter: same seed -> identical concrete windows (now and
// in any future build - the spec is pinned); different seed ->
// different windows; every gap inside [max(0, base-j), base+j].
func TestBuildScheduleJitter(t *testing.T) {
	const base, jit = 100 * time.Millisecond, 30 * time.Millisecond
	a := BuildSchedule(20, base, 10*time.Millisecond, jit, 1234)
	b := BuildSchedule(20, base, 10*time.Millisecond, jit, 1234)
	if a.Seed == nil || *a.Seed != 1234 {
		t.Fatalf("seeded schedule lost its seed: %+v", a.Seed)
	}
	differs := 0
	for i := range a.Windows {
		if a.Windows[i] != b.Windows[i] {
			t.Errorf("window %d differs for the same seed: %v vs %v", i, a.Windows[i], b.Windows[i])
		}
		if a.Windows[i].Gap < base-jit || a.Windows[i].Gap > base+jit {
			t.Fatalf("window %d gap %v outside [%v, %v]", i, a.Windows[i].Gap, base-jit, base+jit)
		}
		if a.Windows[i].Gap != base {
			differs++
		}
	}
	if differs == 0 {
		t.Errorf("20 jittered windows all landed exactly on base: the jitter is not doing anything")
	}
	c := BuildSchedule(20, base, 10*time.Millisecond, jit, 1235)
	identical := true
	for i := range a.Windows {
		if a.Windows[i].Gap != c.Windows[i].Gap {
			identical = false
			break
		}
	}
	if identical {
		t.Error("seed 1235 produced the same schedule as 1234")
	}
}

// A base smaller than the jitter must clamp at zero, never invent a
// negative gap (a negative idle is not a thing, and silently
// reinterpreting it would rewrite the commanded timeline).
func TestBuildScheduleClampsAtZero(t *testing.T) {
	s := BuildSchedule(30, 5*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond, 9)
	for i, w := range s.Windows {
		if w.Gap < 0 {
			t.Fatalf("window %d has a negative gap %v", i, w.Gap)
		}
	}
}

// The runner on time: each window's planned moment is exactly the
// previous actual close plus the prepared gap.
func TestScheduleRunnerOnTime(t *testing.T) {
	fc := &fakeClock{t: time.Unix(2000, 0)}
	s := BuildSchedule(3, 100*time.Millisecond, 20*time.Millisecond, 0, 0)
	r := NewScheduleRunner(s, fc.Now(), fc)
	for i := 0; i < 3; i++ {
		due := r.Due()
		if due.IsZero() {
			t.Fatalf("window %d: no planned moment", i)
		}
		fc.t = due // the driver fires exactly on plan
		w, dev := r.Begin(fc.Now())
		if dev != 0 {
			t.Errorf("window %d deviation = %v, want 0", i, dev)
		}
		if w.Gap != 100*time.Millisecond || w.RunFor != 20*time.Millisecond {
			t.Errorf("window %d = %+v, want the prepared 100ms/20ms", i, w)
		}
		fc.Add(w.RunFor)
		r.End(fc.Now())
	}
	if !r.Due().IsZero() || r.Remaining() != 0 {
		t.Errorf("exhausted schedule: due=%v remaining=%d, want zero/0", r.Due(), r.Remaining())
		if w, _ := r.Begin(fc.Now()); w.Gap != 0 {
			t.Errorf("Begin past the end returned window %+v", w)
		}
	}
}

// Drift, the plan's core demand: a late start is recorded (the
// deviation rides Begin's return), and the NEXT window shifts from
// the actual close - the prepared gap is preserved, nothing
// compresses, nothing busy-loops to catch up.
func TestScheduleRunnerDriftShiftsNoCatchUp(t *testing.T) {
	fc := &fakeClock{t: time.Unix(2000, 0)}
	s := BuildSchedule(2, 100*time.Millisecond, 20*time.Millisecond, 0, 0)
	start := fc.Now()
	r := NewScheduleRunner(s, start, fc)

	fc.Add(160 * time.Millisecond) // the driver woke 60ms late
	w, dev := r.Begin(fc.Now())
	if dev != 60*time.Millisecond {
		t.Fatalf("deviation = %v, want the honest +60ms", dev)
	}
	fc.Add(w.RunFor)
	closed := fc.Now()
	r.End(closed)
	// Next planned moment: actual close + the prepared 100ms gap -
	// NOT the old absolute timeline (that would compress the idle
	// gap to "catch up", which the plan forbids).
	want := closed.Add(100 * time.Millisecond)
	if !r.Due().Equal(want) {
		t.Errorf("next due = %v, want %v (shift from actual close, no compression)", r.Due(), want)
	}
}

// An early start is a deviation too (the prepared gap was cut
// short): the runner reports the negative number instead of
// pretending it was on time.
func TestScheduleRunnerEarlyIsHonest(t *testing.T) {
	fc := &fakeClock{t: time.Unix(2000, 0)}
	s := BuildSchedule(1, 100*time.Millisecond, 20*time.Millisecond, 0, 0)
	r := NewScheduleRunner(s, fc.Now(), fc)
	fc.Add(40 * time.Millisecond) // 60ms EARLY
	_, dev := r.Begin(fc.Now())
	if dev != -60*time.Millisecond {
		t.Errorf("deviation = %v, want -60ms (cutting the prepared gap is a deviation, not a shortcut)", dev)
	}
}

// Zero-length intervals are legal: back-to-back bursts with no idle
// between them. The runner must not stall or invent a gap.
func TestScheduleRunnerZeroGap(t *testing.T) {
	fc := &fakeClock{t: time.Unix(2000, 0)}
	s := Schedule{Windows: []Window{{Gap: 0, RunFor: 5 * time.Millisecond}, {Gap: 0, RunFor: 5 * time.Millisecond}}}
	r := NewScheduleRunner(s, fc.Now(), fc)
	if !r.Due().Equal(fc.Now()) {
		t.Fatalf("first due = %v, want the start moment (zero gap)", r.Due())
	}
	_, dev := r.Begin(fc.Now())
	if dev != 0 {
		t.Errorf("deviation = %v, want 0", dev)
	}
	fc.Add(5 * time.Millisecond)
	r.End(fc.Now())
	if !r.Due().Equal(fc.Now()) {
		t.Errorf("second due = %v, want the close moment (zero gap)", r.Due())
	}
}
