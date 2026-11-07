//go:build windows

package lockfile

import (
	"os"
	"syscall"
	"unsafe"
)

// kernel32 follows the repo precedent (procjob, hwinfo): lazy
// bindings, no x/sys dependency.
var (
	kernel32LF     = syscall.NewLazyDLL("kernel32.dll")
	procLockFile   = kernel32LF.NewProc("LockFileEx")
	procUnlockFile = kernel32LF.NewProc("UnlockFileEx")
)

const (
	lockfileExclusive       = 0x00000002 // LOCKFILE_EXCLUSIVE_LOCK
	lockfileFailImmediately = 0x00000001 // LOCKFILE_FAIL_IMMEDIATELY
)

// lockHandle takes an exclusive, non-blocking lock over the whole
// file (LockFileEx): the lock belongs to the handle, and the kernel
// releases it when the owning process dies, whatever the cause of
// death. Contention returns an error (ERROR_LOCK_VIOLATION or
// ERROR_LOCK_CONFLICT) - a live holder is never evicted, and no pid
// heuristic is consulted.
func lockHandle(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFile.Call(uintptr(f.Fd()),
		uintptr(lockfileExclusive|lockfileFailImmediately), 0,
		0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	return err
}

func unlockHandle(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFile.Call(uintptr(f.Fd()), 0,
		0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	return err
}
