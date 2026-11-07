//go:build windows

package sysinfo

import (
	"testing"
	"unsafe"
)

// TestWindowsClockIsHonestUnknown pins the verified platform reality behind
// coreFrequencies: NtQuerySystemInformation class 1 hands back a single
// small record of rated clocks (12 bytes on Win11 26H2), not a per-processor
// array with a current-MHz field — so the only honest Windows answer for a
// per-core current clock is "unknown". If some future Windows starts
// reporting more, CoreFrequencies must stay false until we verify a real
// source; this test is the tripwire that says "re-check the platform".
func TestWindowsClockIsHonestUnknown(t *testing.T) {
	if freqs, ok := CoreFrequencies(); ok {
		t.Fatalf("CoreFrequencies reported %d values — the honest-unknown assumption broke; verify the platform source before trusting it", len(freqs))
	}
	buf := make([]byte, 1024*32)
	var retLen uint32
	r, _, _ := procNtQuerySystemInformation.Call(
		1,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&retLen)))
	t.Logf("class 1: status=%#x retLen=%d (single rated-clock record, not per-core current MHz)", r, retLen)
}
