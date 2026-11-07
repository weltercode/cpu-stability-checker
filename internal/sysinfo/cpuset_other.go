//go:build !windows

package sysinfo

import "fmt"

// CpuSetStates has no backend on this platform yet (Linux exposes parking
// through sysfs cpuidle/cpufreq, a future best-effort mapping). Callers
// must treat the result as unverified, never assume parked.
func CpuSetStates() ([]CpuSetState, error) {
	return nil, fmt.Errorf("cpu set telemetry not implemented on this platform")
}
