//go:build linux

package sysinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// newTemperatureSensor reads a kernel thermal zone under
// newTemperatureSensor reads a kernel thermal zone under
// /sys/class/thermal. Zones report millidegrees Celsius. CPU-ish zone
// types (x86_pkg_temp, cpu, core...) are preferred; a board only ever
// exposes what its firmware published, so unavailability stays the
// honest answer where nothing is exported.
func newTemperatureSensor() (*TemperatureSensor, error) {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	if len(zones) == 0 {
		return nil, fmt.Errorf("no thermal zones exported under /sys/class/thermal")
	}
	type zone struct {
		path  string
		score int // higher = closer to the CPU
	}
	scored := make([]zone, 0, len(zones))
	for _, z := range zones {
		t, err := os.ReadFile(filepath.Join(z, "type"))
		if err != nil {
			continue
		}
		scored = append(scored, zone{z, thermalZoneScore(strings.TrimSpace(string(t)))})
	}
	if len(scored) == 0 {
		return nil, fmt.Errorf("thermal zones exist but none report a type")
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	// Probe once so "unavailable" is decided at open time, like Windows.
	if _, err := readThermalMilliC(scored[0].path); err != nil {
		return nil, fmt.Errorf("thermal zone %s: %w", filepath.Base(scored[0].path), err)
	}
	return &TemperatureSensor{query: 1, zone: scored[0].path}, nil
}

// thermalZoneScore lives in temperature.go (shared, testable everywhere).

func readThermalMilliC(zonePath string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(zonePath, "temp"))
	if err != nil {
		return 0, err
	}
	milli, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse temp: %w", err)
	}
	return milli / 1000.0, nil
}

func (s *TemperatureSensor) read() (float64, bool) {
	if s == nil || s.zone == "" {
		return 0, false
	}
	c, err := readThermalMilliC(s.zone)
	if err != nil {
		return 0, false
	}
	if c < -50 || c > 500 { // implausible: garbage, not a reading
		return 0, false
	}
	return c, true
}

func (s *TemperatureSensor) close() {}
