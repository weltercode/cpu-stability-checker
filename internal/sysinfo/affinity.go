// Package sysinfo wraps OS-level CPU facilities: affinity, topology
// discovery and best-effort telemetry.
package sysinfo

// ApplyAffinity restricts the process to the given logical cores when a
// non-empty set is provided. An empty set means "all cores" (no restriction).
func ApplyAffinity(cores []int) error {
	if len(cores) == 0 {
		return nil
	}
	return SetAffinity(cores)
}
