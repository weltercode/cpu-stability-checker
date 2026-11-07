//go:build windows

package sysinfo

import (
	"fmt"
	"unsafe"
)

var procGetSystemCpuSetInformation = kernel32.NewProc("GetSystemCpuSetInformation")

// systemCpuSetInformation mirrors the Win32 SYSTEM_CPU_SET_INFORMATION
// (winnt.h). Entries are variable-sized by design: iterate using Size.
type systemCpuSetInformation struct {
	size             uint32
	typ              uint32 // 0 = CpuSetInformation
	id               uint32
	group            uint16
	logicalProcessor uint8
	coreIndex        uint8
	lastLevelCache   uint8
	numaNode         uint8
	efficiencyClass  uint8
	allFlags         uint8 // bit0 parked, bit1 allocated, bit2 allocated-to-target, bit3 realtime
	_                uint32
	allocationTag    uint64
}

const cpuSetFlagParked = 0x01

// CpuSetStates queries the kernel's core-parking state for every logical
// processor. Windows 10+ only; where unavailable it returns an error and
// callers must treat parking as unverified, never assume it.
func CpuSetStates() ([]CpuSetState, error) {
	var needed uint32
	// First call with a nil buffer: it fails with ERROR_INSUFFICIENT_BUFFER
	// and reports the required size.
	procGetSystemCpuSetInformation.Call(0, 0, uintptr(unsafe.Pointer(&needed)), 0, 0)
	if needed == 0 {
		return nil, fmt.Errorf("GetSystemCpuSetInformation: system reports no cpu sets")
	}

	buf := make([]byte, needed)
	var returned uint32
	r, _, callErr := procGetSystemCpuSetInformation.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&returned)), 0, 0)
	if r == 0 {
		return nil, fmt.Errorf("GetSystemCpuSetInformation: %w", callErr)
	}

	const entryMin = uint32(unsafe.Sizeof(systemCpuSetInformation{}))
	var out []CpuSetState
	for off := uint32(0); off+entryMin <= returned; {
		entry := (*systemCpuSetInformation)(unsafe.Pointer(&buf[off]))
		if entry.size < entryMin {
			break // malformed size: stop rather than wander the buffer
		}
		if entry.typ == 0 {
			out = append(out, CpuSetState{
				ID:              int(entry.id),
				LogicalCPU:      int(entry.logicalProcessor),
				Core:            int(entry.coreIndex),
				LastLevelCache:  int(entry.lastLevelCache),
				EfficiencyClass: int(entry.efficiencyClass),
				Parked:          entry.allFlags&cpuSetFlagParked != 0,
			})
		}
		off += entry.size
	}
	return out, nil
}
