//go:build windows

package memtest

import (
	"fmt"
	"unsafe"
)

var (
	procVirtualLock                = modKernel32Alloc.NewProc("VirtualLock")
	procGetProcessWorkingSetSizeEx = modKernel32Alloc.NewProc("GetProcessWorkingSetSizeEx")
)

// lockRegion pins the committed region against paging. Success is
// bounded by the process working-set quota (reported alongside): it
// is not a residency guarantee beyond that, and nothing here
// elevates privileges or retries with tricks.
func lockRegion(addr uintptr, size uint64) error {
	if size > uint64(^uintptr(0)) {
		return fmt.Errorf("memtest: lock size %d exceeds the address space of this process", size)
	}
	r, _, errno := procVirtualLock.Call(addr, uintptr(size))
	if r == 0 {
		return errno
	}
	return nil
}

// workingSetLimits reports the process working-set min/max the lock
// quota is judged against. GetCurrentProcess's pseudo-handle (-1) is
// the first argument: measured without it the call simply fails and
// the limits came back unknown. The hard-WS flags are deliberately
// ignored: the raw limits are what makes a lock failure readable.
func workingSetLimits() (min, max uint64, ok bool) {
	const currentProcess = ^uintptr(0) // GetCurrentProcess() pseudo-handle
	var lo, hi uintptr
	var flags uint32
	r, _, _ := procGetProcessWorkingSetSizeEx.Call(
		currentProcess,
		uintptr(unsafe.Pointer(&lo)), uintptr(unsafe.Pointer(&hi)), uintptr(unsafe.Pointer(&flags)))
	if r == 0 {
		return 0, 0, false
	}
	return uint64(lo), uint64(hi), true
}
