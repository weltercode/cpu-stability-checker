// Package hwinfo reads the sensor values HWiNFO publishes through its
// shared-memory interface. This is the only practical way to see real
// core clocks, core voltages, CPU temperatures and package power on
// Windows without a kernel driver: those live in MSRs and the SMU
// (ring-0), and HWiNFO already owns a signed driver that reads them.
// When HWiNFO is not running, every call degrades to "unavailable" and
// the panel keeps its honest OS-side numbers.
package hwinfo

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Reading is one sensor value as published by HWiNFO.
type Reading struct {
	Type  uint32
	ID    uint32
	Index uint32
	Name  string
	Unit  string
	Value float64
}

// Sensor reading types (HWiNFO's published enum).
const (
	TypeNone = iota
	TypeTemp
	TypeVolt
	TypeFan
	TypeCurrent
	TypePower
	TypeClock
	TypeUsage
	TypeOther
)

// Core Clock is HWiNFO's conventional ratio * bus clock reading.
// AMD's optional perf suffix ranks favored cores; it is not a CPU ID.
// Effective Clock measures a different quantity and never joins this map.
var coreClockRe = regexp.MustCompile(`(?i)^(?:core|p-core) (#?)(\d+) clock(?: \(perf #\d+\))?$`)

// CoreClocks returns conventional clocks keyed by physical core number.
// The caller maps physical cores to logical CPUs using OS topology.
func CoreClocks(readings []Reading) map[int]float64 {
	out := map[int]float64{}
	for _, r := range readings {
		if r.Type != TypeClock || r.Value <= 0 || math.IsNaN(r.Value) || math.IsInf(r.Value, 0) ||
			(r.Unit != "" && !strings.EqualFold(r.Unit, "MHz")) {
			continue
		}
		if m := coreClockRe.FindStringSubmatch(strings.TrimSpace(r.Name)); m != nil {
			n, err := strconv.Atoi(m[2])
			if m[1] == "#" {
				n-- // older one-based naming
			}
			if err == nil && n >= 0 {
				out[n] = r.Value
			}
		}
	}
	return out
}

// Find returns the first reading whose type matches and whose name
// contains any of the given substrings (case-insensitive), preferring
// earlier substrings. Empty value means nothing matched.
func Find(readings []Reading, typ uint32, subs ...string) (Reading, bool) {
	for _, sub := range subs {
		sub = strings.ToLower(sub)
		for _, r := range readings {
			if r.Type == typ && strings.Contains(strings.ToLower(r.Name), sub) {
				return r, true
			}
		}
	}
	return Reading{}, false
}
