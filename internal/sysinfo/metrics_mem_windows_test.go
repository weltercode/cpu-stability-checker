//go:build windows

package sysinfo

import (
	"testing"
	"unsafe"
)

// MEMORYSTATUSEX is 64 bytes on every Windows build: two DWORDs plus
// seven QWORDs. uintptr-typed fields would silently shrink the struct
// on 32-bit builds, and GlobalMemoryStatusEx would then write through
// a layout we no longer match - this test pins the ABI.
func TestMemoryStatusExABI(t *testing.T) {
	if unsafe.Sizeof(memoryStatusEx{}) != 64 {
		t.Errorf("memoryStatusEx size = %d, want 64 (MEMORYSTATUSEX)", unsafe.Sizeof(memoryStatusEx{}))
	}
}

// The snapshot on this platform must actually observe both physical
// and commit numbers, and they must be internally consistent.
func TestMemorySnapshotSelf(t *testing.T) {
	s := MemorySnapshotNow()
	if !s.Known || !s.CommitKnown {
		t.Fatalf("windows snapshot should observe physical and commit: %+v", s)
	}
	if s.TotalPhys == 0 {
		t.Error("TotalPhys = 0 on a machine with memory")
	}
	if s.AvailPhys > s.TotalPhys {
		t.Errorf("AvailPhys %d > TotalPhys %d", s.AvailPhys, s.TotalPhys)
	}
	if s.AvailPageFile > s.TotalPageFile {
		t.Errorf("AvailPageFile %d > TotalPageFile %d", s.AvailPageFile, s.TotalPageFile)
	}
}
