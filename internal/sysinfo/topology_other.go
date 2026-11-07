//go:build !windows

package sysinfo

import "runtime"

// Non-Windows topology is intentionally conservative for now. It exposes all
// logical CPUs but does not claim that each entry is a real physical core.
func discoverRawLogicalCPUs() ([]rawLogicalCPU, string, bool, error) {
	n := runtime.NumCPU()
	out := make([]rawLogicalCPU, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, rawLogicalCPU{
			group:     uint16(i / 64),
			number:    uint8(i % 64),
			coreIndex: uint8(i % 64),
		})
	}
	return out, "runtime.NumCPU", true, nil
}
