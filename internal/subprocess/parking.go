package subprocess

// Q04b: the parking observer that makes a parking policy REAL inside
// one driven session. The domain vocabulary and the fold rules live in
// internal/transition (Q04a); this file is only the when/where: when to
// poll (a fixed cadence, never touching the session's timing loop),
// where the measured idle windows come from (the driver's OWN honest
// timestamps - idleFrom to CommandedAt, the same pair the
// IdleBeforeMS journal field is computed from), and how the folded
// evidence maps to its wire form.
//
// The design boundary, on purpose:
//   - sampling runs on its own goroutine and NEVER slows the schedule:
//     a slow syscall must not push a burst late (that would make the
//     observation itself create the deviation it reports on);
//   - windows are MEASURED (parent view, including the IPC round trip -
//     the same honesty label the session_burst intervals carry), never
//     the prepared timeline;
//   - the poller is an injected function, so tests observe with a fake
//     source (L1, every platform) while the real run reads the kernel,
//     and a nil observer means zero behavior change - "off" stays
//     today's run bit-for-bit.

import (
	"fmt"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// ParkingPollInterval is the sampling cadence: parkwatch's default,
// chosen because that spike is what proved this source answers at this
// rate without measurable cost. One query enumerates every CPU Set
// entry, so even 32 cores at 5 Hz is a rounding error against a
// 10 s parkwatch run - and per-core goroutines stay independent,
// which is exactly what keeps every core's evidence tied to its own
// session clock. Exported because the dry-run plan must state the same
// cadence the observer will actually run - plan text and engine may
// never drift apart.
const ParkingPollInterval = 200 * time.Millisecond

// ParkingSourceName names the observation source in every record. It
// is text on purpose: a reader of an old journal must be able to see
// WHICH kernel query the evidence came from, and a future second
// source (clock/throughput, §8.3) will not silently rename this one.
const ParkingSourceName = "cpuset"

// ParkingPoller reads the OS's parking view for ONE target logical
// CPU. An error is a gap in coverage, never a reading of "active".
type ParkingPoller func(target int) (parked bool, err error)

// parkingProbe is the ONE source-query seam: the required-preflight
// probe and every per-core poller go through it, so a platform where
// the query cannot answer speaks with one voice - and tests swap one
// variable to script the whole observation (L1, hermetic) without
// ever depending on the machine's real parking state.
var parkingProbe = sysinfo.CpuSetStates

// ParkingFold buckets one run's per-core parking evidence for the
// verdict gate and the summary line. It reads the folds the observers
// already produced - it adds no observations and re-judges nothing
// about the samples themselves. Only the aggregate decides: under
// required every core must carry observed evidence (transition's
// RequiredGate rule: observed passes and ONLY observed), so a nil
// fold (the session never reached ready-idle: the question was never
// asked) counts as unevidenced exactly like a dead source. Unknown
// statuses from any future build land in the worst-case bucket.
type ParkingFold struct {
	Total       int
	Observed    []int
	Unconfirmed []int
	Unavailable []int
	// NeverAsked: cores whose session produced no observation at all
	// (never reached ready-idle, or the policy was inert). "Never
	// asked" is its own bucket: it must never read as "not parked",
	// but under required it still blocks a full-success claim.
	NeverAsked    []int
	SamplesInIdle int
	ParkedInIdle  int
}

func FoldParkingOutcomes(cores []int, outcomes []SessionOutcome) ParkingFold {
	f := ParkingFold{Total: len(cores)}
	for i, core := range cores {
		if i >= len(outcomes) || outcomes[i].Parking == nil {
			f.NeverAsked = append(f.NeverAsked, core)
			continue
		}
		ev := outcomes[i].Parking
		f.SamplesInIdle += ev.SamplesInIdle
		f.ParkedInIdle += ev.ParkedInIdle
		switch ev.Status {
		case transition.ParkingObserved:
			f.Observed = append(f.Observed, core)
		case transition.ParkingUnconfirmed:
			f.Unconfirmed = append(f.Unconfirmed, core)
		case transition.ParkingUnavailable:
			f.Unavailable = append(f.Unavailable, core)
		default:
			f.Unavailable = append(f.Unavailable, core) // unknown: worst case, never a pass
		}
	}
	return f
}

// Unevidenced reports cores that cannot support a required claim.
func (f ParkingFold) Unevidenced() bool {
	return len(f.Unconfirmed)+len(f.Unavailable)+len(f.NeverAsked) > 0
}

// Message is the one honest run-level line: the counts, which status
// landed on which cores, and the sample totals - the freshness stays
// inspectable exactly as the journal keeps it.
func (f ParkingFold) Message() string {
	msg := fmt.Sprintf("parking evidence (%s, sampled every %s inside measured idle windows): observed %d of %d core(s); idle samples %d, parked %d",
		ParkingSourceName, ParkingPollInterval, len(f.Observed), f.Total, f.SamplesInIdle, f.ParkedInIdle)
	if len(f.Unconfirmed) > 0 {
		msg += fmt.Sprintf("; source alive but never saw parked on cores %v", f.Unconfirmed)
	}
	if len(f.Unavailable) > 0 {
		msg += fmt.Sprintf("; source unavailable on cores %v", f.Unavailable)
	}
	if len(f.NeverAsked) > 0 {
		msg += fmt.Sprintf("; no observation was made on cores %v (the question was never asked)", f.NeverAsked)
	}
	return msg
}

// CpuSetPoller is the real poller: the kernel's CPU Set enumeration
// (sysinfo.GetSystemCpuSetInformation) filtered to the target. A
// target missing from the enumeration is an error: claiming "not
// parked" about a CPU the source refuses to talk about would be
// inference dressed as observation.
func CpuSetPoller(target int) (bool, error) {
	states, err := parkingProbe()
	if err != nil {
		return false, err
	}
	for _, s := range states {
		if s.LogicalCPU == target {
			return s.Parked, nil
		}
	}
	return false, fmt.Errorf("logical cpu %d is not in the cpu set enumeration", target)
}

// ParkingObserver accumulates samples and MEASURED idle windows for
// one session slot and folds them (transition.ObserveParking) at
// Close. Start begins polling; Window is called only by the session
// driver, from its own timestamps; Close stops polling, waits for the
// poll goroutine to finish (so the fold never races a half-appended
// sample), and returns the evidence. All state is mutex-guarded: the
// poller is deliberately concurrent with the driver.
type ParkingObserver struct {
	target   int
	source   string
	interval time.Duration
	poll     ParkingPoller

	done   chan struct{}
	exited chan struct{}

	mu      sync.Mutex
	samples []transition.ParkingSample
	windows []transition.IdleWindow
	closed  bool
	started bool
}

func NewParkingObserver(target int, source string, interval time.Duration, poll ParkingPoller) *ParkingObserver {
	return &ParkingObserver{
		target: target, source: source, interval: interval, poll: poll,
		done: make(chan struct{}), exited: make(chan struct{}),
	}
}

// Start opens the observation window of the WHOLE session: polling
// runs until Close. Samples taken outside a recorded idle window are
// not discarded - the fold classifies them, which is what proves the
// parked readings really landed in idle and not during load.
func (o *ParkingObserver) Start() {
	o.mu.Lock()
	if o.started || o.closed {
		o.mu.Unlock()
		return
	}
	o.started = true
	o.mu.Unlock()
	go func() {
		defer close(o.exited)
		poll := func() {
			parked, err := o.poll(o.target)
			o.mu.Lock()
			o.samples = append(o.samples, transition.ParkingSample{At: time.Now(), Parked: parked, Err: err})
			o.mu.Unlock()
		}
		poll() // the moment the worker reported ready-idle: the first window opens right here
		tick := time.NewTicker(o.interval)
		defer tick.Stop()
		for {
			select {
			case <-o.done:
				return
			case <-tick.C:
				poll()
			}
		}
	}()
}

// Window records one MEASURED idle span (both bounds from the driver:
// the previous burst's close, and the moment the next burst was
// actually commanded). Both bounds must be real: an idle span that
// was never closed by a next command (interrupted or faulted sessions)
// records NO window - a half-known span is not a measurement, and the
// session ends with the honest reason it already carries.
func (o *ParkingObserver) Window(start, end time.Time) {
	if !end.After(start) {
		return
	}
	o.mu.Lock()
	o.windows = append(o.windows, transition.IdleWindow{Start: start, End: end})
	o.mu.Unlock()
}

// Close stops polling, waits for the poll goroutine, and folds. Safe
// to call exactly once; the driver calls it on every path that reached
// Start.
func (o *ParkingObserver) Close() *transition.ParkingEvidence {
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.done)
	}
	started := o.started
	o.mu.Unlock()
	if started {
		<-o.exited // only wait for a goroutine that actually exists
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ev := transition.ObserveParking(o.target, o.source, o.interval, o.windows, o.samples)
	return &ev
}

// toJournalParking maps the folded evidence to its wire form (domain
// -> integers and stamps, the toJournalSchedule direction). Empty
// FirstParked/LastParked mean "never parked in idle" - the wire's
// honest absence marker, matching the domain's zero times.
func toJournalParking(ev *transition.ParkingEvidence) journal.ParkingObservation {
	first, last := "", ""
	if !ev.FirstParked.IsZero() {
		first = ev.FirstParked.UTC().Format(time.RFC3339Nano)
	}
	if !ev.LastParked.IsZero() {
		last = ev.LastParked.UTC().Format(time.RFC3339Nano)
	}
	return journal.ParkingObservation{
		Source: ev.Source, IntervalNS: int64(ev.Interval),
		Status:        string(ev.Status),
		SamplesInIdle: ev.SamplesInIdle, ParkedInIdle: ev.ParkedInIdle,
		IdleWindows: ev.IdleWindows, UnsampledWindows: ev.UnsampledWindows,
		FirstParked: first, LastParked: last,
		Reason: ev.Reason,
	}
}
