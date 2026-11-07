package sysinfo

import "strings"

// TemperatureSensor reads the ACPI thermal zone temperature. Two honest
// caveats travel with every reading: it belongs to a board thermal zone
// (not to a CPU core), and many desktop boards expose no thermal zone at
// all — unavailability is the normal answer on such machines, not a
// defect to hide. The Windows implementation polls the PDH "Thermal Zone
// Information" counter set, which Microsoft documents in degrees Kelvin;
// the provider only samples while a consumer is active, so the sensor
// owns a live PDH query for as long as the caller wants readings. The
// Linux implementation reads a /sys/class/thermal zone (millidegrees
// Celsius), preferring CPU-ish zone types.
type TemperatureSensor struct {
	query   uintptr // platform handle, 0 once closed or never opened
	counter uintptr
	zone    string // Linux: the chosen /sys/class/thermal zone path
}

// NewTemperatureSensor opens the platform temperature source and checks
// that it can produce a sample. The error explains why the source is
// unavailable so callers can report it honestly instead of guessing.
func NewTemperatureSensor() (*TemperatureSensor, error) { return newTemperatureSensor() }

// Read returns the thermal zone temperature in Celsius. ok=false means
// "no current sample" — the caller must treat that as unknown, never as
// "cool enough".
func (s *TemperatureSensor) Read() (float64, bool) { return s.read() }

// Close releases the platform handle.
func (s *TemperatureSensor) Close() { s.close() }

// kelvinToCelsius converts the documented Kelvin reading. Values outside
// a plausible board range (a frozen basement to a red-hot zone) mean the
// provider handed us garbage: unknown, not a number to act on.
func kelvinToCelsius(k float64) (float64, bool) {
	c := k - 273.15
	if c < -50 || c > 500 {
		return 0, false
	}
	return c, true
}

// thermalZoneScore ranks Linux thermal zone types by how closely they
// track the CPU (higher = closer).
func thermalZoneScore(zoneType string) int {
	switch {
	case strings.Contains(zoneType, "pkg") || strings.Contains(zoneType, "x86"):
		return 3
	case strings.Contains(zoneType, "cpu") || strings.Contains(zoneType, "core"):
		return 2
	case strings.Contains(zoneType, "acpi"):
		return 1
	default:
		return 0
	}
}
