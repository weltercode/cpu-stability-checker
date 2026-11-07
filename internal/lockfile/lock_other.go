//go:build !windows

package lockfile

import (
	"os"
	"syscall"
)

// lockHandle takes an exclusive, non-blocking flock: the kernel
// releases it when the owning process dies, whatever the cause of
// death. EWOULDBLOCK means a live holder - never evicted.
func lockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
