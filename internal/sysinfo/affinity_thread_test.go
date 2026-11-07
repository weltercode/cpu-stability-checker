package sysinfo

import (
	"runtime"
	"testing"
)

// The runner pins every worker thread to one core; that only means anything
// if the per-thread affinity APIs actually work. The test goroutine locks
// its thread and never unlocks: a locked thread is destroyed when the
// goroutine returns, so the single-core mask cannot leak into later tests.
func TestSetThreadAffinityRoundTrip(t *testing.T) {
	if _, ok := CurrentThreadAffinity(); !ok {
		t.Skip("platform cannot report thread affinity")
	}
	runtime.LockOSThread()

	before, ok := CurrentThreadAffinity()
	if !ok || before == 0 {
		t.Fatalf("CurrentThreadAffinity() = %x, %v, want a usable mask", before, ok)
	}
	if err := SetThreadAffinity(0); err != nil {
		t.Fatalf("SetThreadAffinity(0) error = %v", err)
	}
	if after, ok := CurrentThreadAffinity(); !ok || after != 1<<0 {
		t.Fatalf("CurrentThreadAffinity() = %x, %v, want exactly core 0", after, ok)
	}
	if err := SetThreadAffinity(-1); err == nil {
		t.Error("SetThreadAffinity(-1) error = nil, want an out-of-range error")
	}
}
