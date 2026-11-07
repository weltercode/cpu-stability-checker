// Package clocksrc reads the real per-core clock from the best
// machine-wide producer available: HWiNFO's shared memory first (its
// readings carry the producer's own poll timestamp), then the
// LibreHardwareMonitor/OpenHardwareMonitor WMI feed. These are the only
// user-mode windows onto the SMU/MSR clock counters without our own
// signed driver, so when neither producer speaks the caller keeps the
// core clock unknown — the observed OS cycle-rate counter is a different
// quantity (TSC-based) and is never substituted here.
//
// Sampling belongs to the parent process, not to a pinned worker: even
// a cheap shared-memory copy runs syscalls on the core whose clock the
// record is about, and a WMI query is far heavier. One reader per run
// keeps the measured cores clean.
package clocksrc

import (
	"runtime"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

// Source labels recorded in the journal. They say who produced the
// numbers; consumers must never read one source's numbers as the other's.
const (
	SrcHWinfo = "hwinfo"
	SrcWMI    = "wmi"
)

// AgeBasis values say what MaxAgeMS measures. "producer": the producer
// stamped the sample itself (HWiNFO poll_time). "acquired": we only know
// when we asked (WMI publishes no producer timestamp), so the age never
// proves the value was fresh at the producer.
const (
	BasisProducer = "producer"
	BasisAcquired = "acquired"
)

const (
	// maxProducerAge rejects HWiNFO samples whose poll timestamp is
	// older than this: a paused or dying HWiNFO keeps its last table
	// mapped, and re-reading it must not launder an old sample as fresh.
	maxProducerAge = 3 * time.Second
	// maxFutureSkew tolerates small clock differences between our clock
	// and the producer's; anything beyond that is a broken stamp.
	maxFutureSkew = 2 * time.Second
	// openRetryEvery backs off reopen attempts (producers may start
	// after the run does), same stance as the panel sampler.
	openRetryEvery = 10
)

// hwFeed/wmiFeed are the seams that let tests drive fake producers
// without shared memory or a WMI provider.
type hwFeed interface {
	Snapshot() ([]hwinfo.Reading, time.Time)
	Close()
}

type wmiFeed interface {
	Readings() []wmisensor.Reading
	Close()
}

// Reader holds at most one open producer per feed, reopening with
// backoff while a producer is missing.
type Reader struct {
	openHW   func() (hwFeed, error)
	openWMI  func() (wmiFeed, error)
	hw       hwFeed
	wmi      wmiFeed
	hwTries  int
	wmiTries int
	lastPoll time.Time // last accepted HWiNFO producer stamp (duplicate suppression)
}

// Open returns a Reader bound to the real producers.
func Open() *Reader {
	return &Reader{
		openHW:  func() (hwFeed, error) { s, err := hwinfo.Open(); return s, err },
		openWMI: func() (wmiFeed, error) { s, err := wmisensor.Open(); return s, err },
	}
}

// Sample is one tick's real-clock readings for the whole machine.
type Sample struct {
	Clocks map[int]float64 // keyed by physical core index (producer numbering)
	Src    string
	Basis  string
	At     time.Time // producer stamp (hwinfo) or our query time (wmi)
}

// Sample returns the freshest source's readings for this tick. HWiNFO
// wins while its sample is new and not a duplicate of the last accepted
// one; a stale, future-stamped or unchanged HWiNFO table falls through
// to WMI for that tick (never refreshing freshness by rereading).
func (r *Reader) Sample(now time.Time) (Sample, bool) {
	if s, ok := r.sampleHW(now); ok {
		return s, true
	}
	return r.sampleWMI(now)
}

func (r *Reader) sampleHW(now time.Time) (Sample, bool) {
	if r.hw == nil {
		r.hwTries++
		if r.hwTries%openRetryEvery != 1 {
			return Sample{}, false
		}
		src, err := r.openHW()
		if err != nil || src == nil {
			return Sample{}, false
		}
		r.hw = src
	}
	readings, poll := r.hw.Snapshot()
	if readings == nil || poll.IsZero() {
		return Sample{}, false // no table, or no producer timestamp to trust
	}
	if poll.After(now.Add(maxFutureSkew)) {
		return Sample{}, false // future stamp: broken producer, not fresh data
	}
	if now.Sub(poll) > maxProducerAge {
		return Sample{}, false // stale table still mapped after a pause or exit
	}
	if !r.lastPoll.IsZero() && poll.Equal(r.lastPoll) {
		return Sample{}, false // same sample as last tick: rereading proves nothing
	}
	clocks := hwinfo.CoreClocks(readings)
	if len(clocks) == 0 {
		return Sample{}, false
	}
	r.lastPoll = poll
	return Sample{Clocks: clocks, Src: SrcHWinfo, Basis: BasisProducer, At: poll}, true
}

func (r *Reader) sampleWMI(now time.Time) (Sample, bool) {
	if r.wmi == nil {
		r.wmiTries++
		if r.wmiTries%openRetryEvery != 1 {
			return Sample{}, false
		}
		src, err := r.openWMI()
		if err != nil || src == nil {
			return Sample{}, false
		}
		r.wmi = src
	}
	clocks := wmisensor.CoreClocks(r.wmi.Readings())
	if len(clocks) == 0 {
		return Sample{}, false
	}
	return Sample{Clocks: clocks, Src: SrcWMI, Basis: BasisAcquired, At: now}, true
}

// Close releases both producers.
func (r *Reader) Close() {
	if r.hw != nil {
		r.hw.Close()
		r.hw = nil
	}
	if r.wmi != nil {
		r.wmi.Close()
		r.wmi = nil
	}
}

// Agg accumulates one core's accepted samples over a window into
// contiguous per-source groups. A tick where the source did not name
// this core (or named it non-positive) is a gap: it simply contributes
// no sample, and FirstMS/LastMS keep the covered span visible instead of
// pretending the whole window was measured.
type Agg struct {
	start time.Time
	cur   *groupAcc
	out   []journal.CoreClockGroup
}

type groupAcc struct {
	src, basis string
	min, max   float64
	sum        float64
	n          int
	firstMS    int64
	lastMS     int64
	maxAgeMS   int64
}

// NewAgg starts accumulation anchored at windowStart (FirstMS/LastMS
// are relative to it).
func NewAgg(windowStart time.Time) *Agg { return &Agg{start: windowStart} }

// Add folds one accepted sample's reading for physical core phys into
// the accumulation. now is our observation time (the age basis is the
// source's own claim: producer stamp for hwinfo, query time for wmi).
func (a *Agg) Add(s Sample, phys int, now time.Time) {
	mhz, ok := s.Clocks[phys]
	if !ok || mhz <= 0 {
		return
	}
	age := now.Sub(s.At)
	if age < 0 {
		age = 0 // producer clock skew: the age claim stays honest at zero
	}
	off := now.Sub(a.start).Milliseconds()
	if a.cur == nil || a.cur.src != s.Src {
		a.close()
		a.cur = &groupAcc{src: s.Src, basis: s.Basis, min: mhz, max: mhz, firstMS: off, lastMS: off}
	}
	g := a.cur
	if mhz < g.min {
		g.min = mhz
	}
	if mhz > g.max {
		g.max = mhz
	}
	g.sum += mhz
	g.n++
	g.lastMS = off
	if ms := age.Milliseconds(); ms > g.maxAgeMS {
		g.maxAgeMS = ms
	}
}

func (a *Agg) close() {
	if a.cur == nil {
		return
	}
	g := a.cur
	a.out = append(a.out, journal.CoreClockGroup{
		Source:   g.src,
		MinMHz:   g.min,
		AvgMHz:   g.sum / float64(g.n),
		MaxMHz:   g.max,
		N:        g.n,
		FirstMS:  g.firstMS,
		LastMS:   g.lastMS,
		AgeBasis: g.basis,
		MaxAgeMS: g.maxAgeMS,
	})
	a.cur = nil
}

// Groups closes the open group and returns the accumulated coverage,
// nil when nothing was ever measured (unknown stays absent, never zero).
func (a *Agg) Groups() []journal.CoreClockGroup {
	a.close()
	return a.out
}

// LookupCoreOf maps logical CPU numbers (the order CoreTimes indexes)
// to physical core indices for producer-clock attribution. It returns
// nil when the topology does not cover every CPU we count: per-core
// clock attribution then stays unknown rather than guessed onto
// siblings the OS never promised.
func LookupCoreOf() []int {
	topo, err := sysinfo.DiscoverTopology()
	if err != nil {
		return nil
	}
	n := runtime.NumCPU()
	if ct, cerr := sysinfo.CoreTimes(); cerr == nil && len(ct) > n {
		n = len(ct)
	}
	return topo.CoreOf(n)
}
