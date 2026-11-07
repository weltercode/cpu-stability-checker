package transition

// Q04 parking evidence contract (STABILITY_DEVELOPMENT_PLAN §8): the
// vocabulary that separates what the scheduler COMMANDS (idle windows)
// from what the OS is OBSERVED to do inside them (CPU Set Parked
// samples). Until this contract runs, no product surface may promise
// parking - the catalog guard from Q03a holds that line; this file is
// the other half: the honest data a future verdict can point at.
//
// The honesty rules encoded below (§8.2/§8.3):
//   - an ACK proves the command was executed, never a low-power state;
//   - only MEASURED idle windows count - the prepared timeline with
//     drift is a claim about intent, not about when the CPU was idle;
//   - a parked sample during a busy window is noise, not evidence;
//   - three statuses exist, never more: there is no "almost parked";
//   - nothing here infers a C-state: the Parked flag is what the
//     kernel's parking engine reports, full stop.

import (
	"fmt"
	"strings"
	"time"
)

// ParkingPolicy decides whether a run observes parking at all, and
// what a missing observation does to the verdict. The zero value is
// OFF on purpose: a plan.Options field holding it stays invisible to
// planHash (omitempty), so a saved pre-Q04 recipe replays with a
// bit-identical hash and exactly today's behavior.
type ParkingPolicy string

const (
	ParkingOff      ParkingPolicy = ""         // no observation, no records: today's run, unchanged
	ParkingTry      ParkingPolicy = "try"      // observe and record; a missing observation never changes the verdict
	ParkingRequired ParkingPolicy = "required" // observe; a full success REQUIRES evidence (no success without evidence)
)

// ParseParkingPolicy maps the config/flag word to a policy. "off" and
// "" normalize to the SAME value (ParkingOff) on purpose: writing
// "off" out loud must not silently fork a run's identity versus not
// writing it at all - both mean "do not observe".
func ParseParkingPolicy(word string) (ParkingPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "", "off":
		return ParkingOff, nil
	case "try":
		return ParkingTry, nil
	case "required":
		return ParkingRequired, nil
	}
	return "", fmt.Errorf("parking policy %q is not one of off|try|required", word)
}

// Active reports whether the run must sample the parking source at
// all. Only ParkingTry/ParkingRequired do.
func (p ParkingPolicy) Active() bool { return p != ParkingOff }

// ParkingStatus is the folded outcome of one target CPU's observation
// for one run. Exactly three values; the reducer never invents more.
type ParkingStatus string

const (
	ParkingObserved    ParkingStatus = "observed"    // the source showed this CPU parked inside at least one measured idle window
	ParkingUnconfirmed ParkingStatus = "unconfirmed" // the source stayed alive, but never showed parked during any idle window
	ParkingUnavailable ParkingStatus = "unavailable" // the source failed or was never polled: the question itself could not be asked
)

// ParkingSample is one reading of the OS's view of one logical CPU.
// Err marks a failed reading (the source was asked and failed): it
// stays a visible gap in coverage, never a false claim of "active".
type ParkingSample struct {
	At     time.Time
	Parked bool
	Err    error
}

// IdleWindow is one MEASURED idle span of one target CPU: from the
// measured close of the previous burst to the measured start of the
// next one (the session edges bound the first and the last). Measured,
// never prepared: the runner already journals deviations, and a gap
// the runner shifted is not the gap the CPU actually got (§8.2 keeps
// IdleRequested and IdleObserved apart - windows here are when idle
// REALLY was, as far as this run can measure it).
type IdleWindow struct {
	Start time.Time
	End   time.Time
}

// ParkingEvidence is the folded answer for one target CPU of one run.
// It is a data record, not a verdict: the counts ride into the journal
// so a reader can weigh the freshness themselves (§8.3: source,
// frequency and gaps preserved - never smoothed away).
type ParkingEvidence struct {
	Target           int           // the logical CPU this observation is about
	Source           string        // source name ("windows-cpuset"), never inferred
	Interval         time.Duration // the sampling cadence that produced the samples
	Status           ParkingStatus
	SamplesInIdle    int       // successful samples that landed inside a measured idle window
	ParkedInIdle     int       // of those, how many showed the Parked flag
	IdleWindows      int       // measured idle windows for this core
	UnsampledWindows int       // idle windows that received zero successful samples: the coverage hole stays visible
	FirstParked      time.Time // zero ONLY when no parked-in-idle sample exists (pinned by test)
	LastParked       time.Time
	Reason           string // always non-empty when not observed; may carry honesty notes when observed
}

// ObserveParking folds a raw sample timeline against measured idle
// windows. It is pure by design - no syscall, no clock, no shared
// state - so every honesty rule above is testable with fakes. The
// caller gathers the samples and the measured windows; this function
// only classifies, and classifies without upgrading.
//
// Window classification: [Start, End) - a sample exactly at Start
// belongs to the window, one exactly at End belongs to the next
// window or to nothing. Windows must not overlap (caller contract);
// if they do, the first match wins.
func ObserveParking(target int, source string, interval time.Duration, idle []IdleWindow, samples []ParkingSample) ParkingEvidence {
	ev := ParkingEvidence{Target: target, Source: source, Interval: interval, IdleWindows: len(idle)}
	perWindow := make([]int, len(idle))
	ok, failed := 0, 0
	for _, s := range samples {
		if s.Err != nil {
			failed++
			continue
		}
		ok++
		for i, w := range idle {
			if !s.At.Before(w.Start) && s.At.Before(w.End) {
				ev.SamplesInIdle++
				perWindow[i]++
				if s.Parked {
					ev.ParkedInIdle++
					if ev.FirstParked.IsZero() {
						ev.FirstParked = s.At
					}
					ev.LastParked = s.At
				}
				break
			}
		}
	}
	for _, n := range perWindow {
		if n == 0 {
			ev.UnsampledWindows++ // counted, not named: the counts tell the hole's size, the journal keeps the samples for the shape
		}
	}

	var notes []string
	if failed > 0 {
		notes = append(notes, fmt.Sprintf("%d sampling call(s) failed", failed))
	}
	switch {
	case ok == 0:
		ev.Status = ParkingUnavailable
		if failed > 0 {
			ev.Reason = "every parking sample failed: the observation source never answered"
		} else {
			ev.Reason = "no parking samples were taken: the source was never polled for this run"
		}
	case ev.IdleWindows == 0:
		ev.Status = ParkingUnconfirmed
		ev.Reason = "the measured timeline had no idle window for this core: there was no span in which parking could be observed"
	case ev.ParkedInIdle == 0:
		ev.Status = ParkingUnconfirmed
		ev.Reason = fmt.Sprintf("the source stayed alive (%d idle sample(s)) but this CPU never showed parked during any idle window", ev.SamplesInIdle)
	default:
		ev.Status = ParkingObserved
	}
	if ev.Status != ParkingObserved && ev.UnsampledWindows > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d idle window(s) received no successful sample", ev.UnsampledWindows, ev.IdleWindows))
	} else if ev.Status == ParkingObserved && ev.UnsampledWindows > 0 {
		notes = append(notes, fmt.Sprintf("observed, though %d of %d idle window(s) received no successful sample", ev.UnsampledWindows, ev.IdleWindows))
	}
	if len(notes) > 0 {
		if ev.Reason != "" {
			ev.Reason += "; " + strings.Join(notes, "; ")
		} else {
			ev.Reason = strings.Join(notes, "; ")
		}
	}
	return ev
}

// RequiredGate answers the policy question at verdict time (Q04c uses
// it, §8.2: IdleObserved? is mandatory only for a full success under
// required). Off and try never block - the difference between them is
// display, not verdict. Required passes on observed and ONLY on
// observed: unconfirmed and unavailable are different honest failures,
// and an unknown status (from a future build) is treated as the worst
// case, never as a pass.
func (p ParkingPolicy) RequiredGate(status ParkingStatus) (bool, string) {
	if p != ParkingRequired {
		return true, ""
	}
	switch status {
	case ParkingObserved:
		return true, ""
	case ParkingUnconfirmed:
		return false, "parking evidence required but not observed: the source never showed the parked state during the measured idle windows"
	case ParkingUnavailable:
		return false, "parking evidence required but the observation source was unavailable: the question could not be asked on this machine"
	}
	return false, fmt.Sprintf("parking evidence required but status %q is unknown to this build: treated as unevidenced, never as success", string(status))
}
