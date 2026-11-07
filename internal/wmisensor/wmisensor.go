// Package wmisensor reads the sensor values LibreHardwareMonitor and
// OpenHardwareMonitor publish through their WMI provider. Like
// HWiNFO's shared memory, this is a ring-0-backed feed we cannot read
// ourselves (the monitors own signed drivers for the same MSRs/SMU),
// and unlike HWiNFO the two monitors are free and common. When
// neither is running with its WMI provider enabled, every call
// degrades to "unavailable" and the panel keeps its OS-side numbers.
package wmisensor

import (
	"regexp"
	"strconv"
	"strings"
)

// Reading is one sensor value as published by the WMI provider.
// Type is the published SensorType string: Temperature, Voltage,
// Power, Clock, Load, Current, Fan.
type Reading struct {
	Type  string
	Name  string
	Value float64
}

// coreClockRe matches the per-core clock sensors both monitors name
// "Core #N" (one-based), with Intel's "Core #N Effective Clock" also
// accepted.
var coreClockRe = regexp.MustCompile(`(?i)^core #(\d+)(?: effective clock)?$`)

// CoreClocks picks the per-core clock readings out of a snapshot,
// keyed by zero-based core number. The monitors publish one clock
// sensor per physical core; logical-CPU mapping is left to the
// caller's honesty rules (unknown siblings stay unknown).
func CoreClocks(readings []Reading) map[int]float64 {
	out := map[int]float64{}
	for _, r := range readings {
		if r.Type != "Clock" {
			continue
		}
		m := coreClockRe.FindStringSubmatch(strings.TrimSpace(r.Name))
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= 1 {
			out[n-1] = r.Value
		}
	}
	return out
}

// Find returns the first reading whose type matches and whose name
// contains any of the given substrings (case-insensitive, earlier
// substrings preferred).
func Find(readings []Reading, typ string, subs ...string) (Reading, bool) {
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
