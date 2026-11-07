package human

import "fmt"

// TelemetryStats accumulates clock and idle-share observations over a
// window and renders them as the shared "telemetry:" log line. The
// in-process final summary, the in-process watchdog tick and the
// per-process parent tick all speak this exact format; keeping the
// accumulator here means the min/avg/max math and the wording live in
// exactly one place.
//
// The two clock segments measure different quantities and never merge:
// "observed cycle rate" is the OS cycle counter over wall time (on
// Windows a TSC-based rate: it shows busyness and preemption, not the
// boost clock), while "core clock" is a producer-reported conventional
// core clock (HWiNFO/WMI) and names its source.
type TelemetryStats struct {
	clockN   int
	clockMin float64
	clockMax float64
	clockSum float64
	coreN    int
	coreMin  float64
	coreMax  float64
	coreSum  float64
	coreSrc  string
	idleN    int
	idleSum  float64
}

// AddClock records one observed-cycle-rate observation. Zero is a real
// reading — a frozen or vanished process genuinely cycles at 0 Hz — so
// only negative values are dropped as impossible. Callers that want to
// skip zeros (the final per-run summary) guard before calling.
func (s *TelemetryStats) AddClock(mhz float64) {
	if mhz < 0 {
		return
	}
	if s.clockN == 0 || mhz < s.clockMin {
		s.clockMin = mhz
	}
	if mhz > s.clockMax {
		s.clockMax = mhz
	}
	s.clockSum += mhz
	s.clockN++
}

// AddCoreClock records one producer-reported core-clock observation
// along with its source label; the line names the source so the two
// clock numbers can never be confused. A source switch mid-window
// keeps the last label (the journal groups carry the full story).
func (s *TelemetryStats) AddCoreClock(mhz float64, src string) {
	if mhz <= 0 || src == "" {
		return // a producer that did not name this core says nothing
	}
	if s.coreN == 0 || mhz < s.coreMin {
		s.coreMin = mhz
	}
	if mhz > s.coreMax {
		s.coreMax = mhz
	}
	s.coreSum += mhz
	s.coreN++
	s.coreSrc = src
}

// AddIdle records one idle-share observation; nil means the platform
// could not say, and unknowns never pose as zeros.
func (s *TelemetryStats) AddIdle(pct *float64) {
	if pct == nil {
		return
	}
	s.idleSum += *pct
	s.idleN++
}

// Line renders the compact log line, or "" when nothing was observed,
// so callers can skip the record entirely.
func (s *TelemetryStats) Line() string {
	if s.clockN == 0 && s.coreN == 0 && s.idleN == 0 {
		return ""
	}
	var seg []string
	if s.clockN > 0 {
		seg = append(seg, fmt.Sprintf("observed cycle rate min %.0f avg %.0f max %.0f MHz", s.clockMin, s.clockSum/float64(s.clockN), s.clockMax))
	}
	if s.coreN > 0 {
		seg = append(seg, fmt.Sprintf("core clock min %.0f avg %.0f max %.0f MHz (%s)", s.coreMin, s.coreSum/float64(s.coreN), s.coreMax, s.coreSrc))
	}
	if s.idleN > 0 {
		seg = append(seg, fmt.Sprintf("idle avg %.1f%%", s.idleSum/float64(s.idleN)))
	}
	return "telemetry: " + joinSegs(seg)
}

func joinSegs(seg []string) string {
	out := seg[0]
	for _, s := range seg[1:] {
		out += ", " + s
	}
	return out
}
