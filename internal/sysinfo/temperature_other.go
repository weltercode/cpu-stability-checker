//go:build !windows && !linux

package sysinfo

import "fmt"

// newTemperatureSensor has no implementation on this platform; on Linux a
// /sys/class/thermal zone is read instead.
func newTemperatureSensor() (*TemperatureSensor, error) {
	return nil, fmt.Errorf("no temperature source implemented on this platform")
}

func (s *TemperatureSensor) read() (float64, bool) { return 0, false }

func (s *TemperatureSensor) close() {}
