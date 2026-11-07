package transition

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The parking reducer is where the Q04 honesty contract becomes
// executable: these tables pin that observation never upgrades, that
// only measured idle windows count, and that coverage holes stay
// visible instead of being smoothed into "looks fine".

func TestParseParkingPolicy(t *testing.T) {
	for _, c := range []struct {
		word string
		want ParkingPolicy
	}{
		{"", ParkingOff},
		{"off", ParkingOff},
		{"OFF", ParkingOff}, // normalization: writing "off" is the SAME policy as not writing it (identity must not fork),
		{" try ", ParkingTry},
		{"Required", ParkingRequired},
	} {
		got, err := ParseParkingPolicy(c.word)
		if err != nil {
			t.Errorf("ParseParkingPolicy(%q): %v", c.word, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseParkingPolicy(%q) = %q, want %q", c.word, got, c.want)
		}
	}
	// Off is the zero value AND the only inactive policy: gates and
	// samplers key on this, pin it.
	if ParkingOff.Active() || !ParkingTry.Active() || !ParkingRequired.Active() {
		t.Error("ParkingPolicy.Active() disagrees with the off/try/required split")
	}
	for _, word := range []string{"on", "yes", "parked", "required!"} {
		if _, err := ParseParkingPolicy(word); err == nil {
			t.Errorf("ParseParkingPolicy(%q) accepted an unknown policy", word)
		}
	}
}

func sampleAt(sec float64, parked bool) ParkingSample {
	return ParkingSample{At: timeZero.Add(time.Duration(sec * float64(time.Second))), Parked: parked}
}

var timeZero = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestObserveParking(t *testing.T) {
	w2 := func(a, b float64) IdleWindow {
		return IdleWindow{Start: timeZero.Add(time.Duration(a * float64(time.Second))), End: timeZero.Add(time.Duration(b * float64(time.Second)))}
	}
	errBoom := errors.New("cpuset query failed")

	cases := []struct {
		name    string
		idle    []IdleWindow
		samples []ParkingSample
		want    ParkingStatus
		check   func(t *testing.T, ev ParkingEvidence)
	}{
		{
			name: "parked inside idle is observed with exact counts and first/last stamps",
			idle: []IdleWindow{w2(1, 2), w2(4, 5)},
			samples: []ParkingSample{
				sampleAt(0.5, true), // OUTSIDE idle (before any window): noise, not evidence
				sampleAt(1.2, false),
				sampleAt(1.6, true), // first parked in idle
				sampleAt(2.5, true), // during the busy stretch between windows: does not count
				sampleAt(4.4, true), // last parked in idle
			},
			want: ParkingObserved,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.SamplesInIdle != 3 || ev.ParkedInIdle != 2 {
					t.Errorf("counts = %d/%d, want 3/2", ev.SamplesInIdle, ev.ParkedInIdle)
				}
				if !ev.FirstParked.Equal(timeZero.Add(1600*time.Millisecond)) || !ev.LastParked.Equal(timeZero.Add(4400*time.Millisecond)) {
					t.Errorf("stamps = %v..%v, want 1.6s..4.4s", ev.FirstParked, ev.LastParked)
				}
				if ev.UnsampledWindows != 0 {
					t.Errorf("unsampled windows = %d, want 0", ev.UnsampledWindows)
				}
			},
		},
		{
			name: "alive source that never shows parked in idle is unconfirmed, never maybe",
			idle: []IdleWindow{w2(1, 2)},
			samples: []ParkingSample{
				sampleAt(1.2, false), sampleAt(1.8, false),
			},
			want: ParkingUnconfirmed,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.Reason == "" || !strings.Contains(ev.Reason, "never showed parked") {
					t.Errorf("unconfirmed reason missing or unhelpful: %q", ev.Reason)
				}
			},
		},
		{
			name: "parked only during busy stretches proves nothing: unconfirmed",
			idle: []IdleWindow{w2(1, 2)},
			samples: []ParkingSample{
				sampleAt(0.5, true), sampleAt(2.5, true), sampleAt(1.4, false),
			},
			want: ParkingUnconfirmed,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.ParkedInIdle != 0 || ev.FirstParked != (time.Time{}) {
					t.Errorf("busy-window parking leaked into evidence: parkedInIdle=%d first=%v", ev.ParkedInIdle, ev.FirstParked)
				}
			},
		},
		{
			name:    "every sample failed: the question could not be asked (unavailable, not unconfirmed)",
			idle:    []IdleWindow{w2(1, 2)},
			samples: []ParkingSample{{At: timeZero.Add(time.Second), Err: errBoom}, {At: timeZero.Add(2 * time.Second), Err: errBoom}},
			want:    ParkingUnavailable,
			check: func(t *testing.T, ev ParkingEvidence) {
				if !strings.Contains(ev.Reason, "never answered") || !strings.Contains(ev.Reason, "2 sampling call(s) failed") {
					t.Errorf("unavailable reason must name the failed calls: %q", ev.Reason)
				}
			},
		},
		{
			name:    "no samples at all: source never polled, also unavailable",
			idle:    []IdleWindow{w2(1, 2)},
			samples: nil,
			want:    ParkingUnavailable,
			check: func(t *testing.T, ev ParkingEvidence) {
				if !strings.Contains(ev.Reason, "never polled") {
					t.Errorf("empty-sample reason: %q", ev.Reason)
				}
			},
		},
		{
			name:    "no measured idle window: parking had nowhere to be observed (back-to-back bursts)",
			idle:    nil,
			samples: []ParkingSample{sampleAt(1, true), sampleAt(2, true)},
			want:    ParkingUnconfirmed,
			check: func(t *testing.T, ev ParkingEvidence) {
				if !strings.Contains(ev.Reason, "no idle window") {
					t.Errorf("no-window reason: %q", ev.Reason)
				}
			},
		},
		{
			name:    "window edges are [Start, End): a sample exactly at Start counts, at End does not",
			idle:    []IdleWindow{w2(1, 2)},
			samples: []ParkingSample{sampleAt(1.0, true), sampleAt(2.0, true)},
			want:    ParkingObserved,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.SamplesInIdle != 1 || ev.ParkedInIdle != 1 {
					t.Errorf("edge classification = %d/%d, want 1/1 (End must belong to the next window or nothing)", ev.SamplesInIdle, ev.ParkedInIdle)
				}
			},
		},
		{
			name:    "observed, but an unsampled window keeps its hole visible in the note",
			idle:    []IdleWindow{w2(1, 2), w2(4, 5)},
			samples: []ParkingSample{sampleAt(1.5, true)}, // window 2 (4..5) never sampled successfully
			want:    ParkingObserved,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.UnsampledWindows != 1 {
					t.Errorf("unsampled windows = %d, want 1", ev.UnsampledWindows)
				}
				if !strings.Contains(ev.Reason, "1 of 2 idle window(s) received no successful sample") {
					t.Errorf("observed status must still state the coverage hole: %q", ev.Reason)
				}
			},
		},
		{
			name: "mixed failure: failed reads are gaps, not false actives",
			idle: []IdleWindow{w2(1, 2)},
			samples: []ParkingSample{
				{At: timeZero.Add(time.Second), Err: errBoom},
				sampleAt(1.4, true),
			},
			want: ParkingObserved,
			check: func(t *testing.T, ev ParkingEvidence) {
				if ev.SamplesInIdle != 1 || ev.ParkedInIdle != 1 || ev.UnsampledWindows != 0 {
					t.Errorf("fold over failed reads wrong: %d/%d uns=%d", ev.SamplesInIdle, ev.ParkedInIdle, ev.UnsampledWindows)
				}
				if !strings.Contains(ev.Reason, "1 sampling call(s) failed") {
					t.Errorf("failed-call note missing: %q", ev.Reason)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ObserveParking(7, "windows-cpuset", 200*time.Millisecond, c.idle, c.samples)
			if ev.Status != c.want {
				t.Fatalf("status = %q, want %q (reason %q)", ev.Status, c.want, ev.Reason)
			}
			if ev.Target != 7 || ev.Source != "windows-cpuset" || ev.Interval != 200*time.Millisecond {
				t.Errorf("identity fields must survive the fold verbatim: %+v", ev)
			}
			// The universal honesty meta-rules, checked for EVERY case:
			if ev.Status == ParkingObserved && ev.ParkedInIdle < 1 {
				t.Errorf("meta: observed without a parked-in-idle sample: %+v", ev)
			}
			if ev.Status != ParkingObserved && !ev.FirstParked.IsZero() {
				t.Errorf("meta: not observed must not carry parked stamps: %+v", ev)
			}
			if ev.Status != ParkingObserved && ev.Reason == "" {
				t.Errorf("meta: not observed must name a reason: %+v", ev)
			}
			if c.check != nil {
				c.check(t, ev)
			}
		})
	}
}

func TestRequiredGateMatrix(t *testing.T) {
	type gate struct {
		pol  ParkingPolicy
		st   ParkingStatus
		want bool
	}
	cases := []gate{
		{ParkingOff, ParkingUnconfirmed, true}, // off: never block, no records were made anyway
		{ParkingTry, ParkingUnconfirmed, true}, // try: display difference, never a verdict difference
		{ParkingTry, ParkingUnavailable, true},
		{ParkingRequired, ParkingObserved, true}, // required + evidence: the only pass
		{ParkingRequired, ParkingUnconfirmed, false},
		{ParkingRequired, ParkingUnavailable, false},
		{ParkingRequired, "future_state", false}, // unknown to this build: worst case, never a pass
	}
	for _, g := range cases {
		ok, reason := g.pol.RequiredGate(g.st)
		if ok != g.want {
			t.Errorf("gate(%q, %q) = %v, want %v (reason %q)", g.pol, g.st, ok, g.want, reason)
		}
		if !ok && reason == "" {
			t.Errorf("gate(%q, %q) refused without a reason", g.pol, g.st)
		}
		if g.pol == ParkingRequired && !ok && !strings.Contains(reason, "required") {
			t.Errorf("refusal must name the policy it enforces: %q", reason)
		}
	}
}
