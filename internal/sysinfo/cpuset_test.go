//go:build windows

package sysinfo

import (
	"runtime"
	"testing"
)

func TestCpuSetStates(t *testing.T) {
	states, err := CpuSetStates()
	if err != nil {
		t.Fatalf("CpuSetStates() error = %v, want cpu-set telemetry on Windows 10+", err)
	}
	if len(states) < runtime.NumCPU() {
		t.Fatalf("CpuSetStates() returned %d entries, want at least NumCPU=%d", len(states), runtime.NumCPU())
	}

	seen := map[int]bool{}
	for _, s := range states {
		if seen[s.LogicalCPU] {
			t.Errorf("logical cpu %d reported twice", s.LogicalCPU)
		}
		seen[s.LogicalCPU] = true
		if s.ID <= 0 {
			t.Errorf("cpu set for logical cpu %d has id %d, want a positive id", s.LogicalCPU, s.ID)
		}
	}
	for c := 0; c < runtime.NumCPU(); c++ {
		if !seen[c] {
			t.Errorf("logical cpu %d missing from cpu-set telemetry", c)
		}
	}

	// ParkedCount must agree with the entries it is handed.
	want := 0
	for _, s := range states {
		if s.Parked {
			want++
		}
	}
	if got := ParkedCount(states); got != want {
		t.Errorf("ParkedCount() = %d, want %d", got, want)
	}
}
