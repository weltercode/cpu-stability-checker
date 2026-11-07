//go:build linux

package sysinfo

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// linuxCPUSet is the 1024-bit cpu_set_t layout used by sched_setaffinity on
// Linux. We keep this tiny wrapper here instead of adding a dependency solely
// for an optional backend.
type linuxCPUSet [16]uint64

func (s *linuxCPUSet) add(cpu int) error {
	if cpu < 0 || cpu >= len(s)*64 {
		return fmt.Errorf("core index %d out of range [0,%d]", cpu, len(s)*64-1)
	}
	s[cpu/64] |= uint64(1) << uint(cpu%64)
	return nil
}

func linuxSchedSetAffinity(tid int, set *linuxCPUSet) error {
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_SETAFFINITY,
		uintptr(tid),
		unsafe.Sizeof(*set),
		uintptr(unsafe.Pointer(set)),
	)
	runtime.KeepAlive(set)
	if errno != 0 {
		return errno
	}
	return nil
}

// SetAffinity applies the core mask to every existing thread of the process.
// This is best-effort: threads created later inherit the creator's mask.
func SetAffinity(cores []int) error {
	var set linuxCPUSet
	for _, c := range cores {
		if err := set.add(c); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return fmt.Errorf("read /proc/self/task: %w", err)
	}
	var firstErr error
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if e := linuxSchedSetAffinity(tid, &set); e != nil && firstErr == nil {
			firstErr = fmt.Errorf("SchedSetaffinity(tid %d): %w", tid, e)
		}
	}
	return firstErr
}

// SetThreadAffinity pins the calling thread (pid 0 means self) to one core.
// Call it from a goroutine locked to its thread (runtime.LockOSThread).
func SetThreadAffinity(core int) error {
	var set linuxCPUSet
	if err := set.add(core); err != nil {
		return err
	}
	if err := linuxSchedSetAffinity(0, &set); err != nil {
		return fmt.Errorf("SchedSetaffinity(self): %w", err)
	}
	return nil
}

// CurrentThreadAffinity reports the calling thread's cpuset as a core
// bitmap (first 64 cores), and whether it could be read.
func CurrentThreadAffinity() (uint64, bool) {
	var set linuxCPUSet
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_SCHED_GETAFFINITY,
		0,
		unsafe.Sizeof(set),
		uintptr(unsafe.Pointer(&set)),
	)
	runtime.KeepAlive(&set)
	if errno != 0 {
		return 0, false
	}
	return set[0], true
}

// WidenAffinity is a no-op on Linux: per-thread sched_setaffinity is not
// bounded by a process mask, so pinning either works or fails on its own
// (a cgroup cpuset restriction shows up at SetThreadAffinity time).
func WidenAffinity(cores []int) []int { return nil }

// savedProcessAffinity is the full cpu_set_t as the calling thread held
// it at snapshot time. Linux affinity is per-thread, and the
// process-wide effect in this package comes from SetAffinity walking
// every thread: the snapshot therefore captures the mask the process
// ran with BEFORE the change, and the restore walks every thread back
// to it. A thread that held its own different affinity before the
// change cannot be told apart - the restore normalizes all threads to
// the pre-change mask, exactly the contract the change itself used.
type savedProcessAffinity struct {
	set linuxCPUSet
	ok  bool
}

func saveProcessAffinity() savedProcessAffinity {
	var set linuxCPUSet
	_, _, errno := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0,
		uintptr(unsafe.Sizeof(set)), uintptr(unsafe.Pointer(&set)))
	runtime.KeepAlive(&set)
	return savedProcessAffinity{set: set, ok: errno == 0}
}

// restoreProcessAffinity walks the same thread list SetAffinity walked,
// handing every thread its mask back; a cpuset that refuses to come
// back is reported, not swallowed.
func restoreProcessAffinity(s savedProcessAffinity) error {
	if !s.ok {
		return errors.New("process affinity was unreadable before the change: the cpuset stays as the run left it")
	}
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return fmt.Errorf("restoring thread cpusets: read /proc/self/task: %w", err)
	}
	var firstErr error
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if serr := linuxSchedSetAffinity(tid, &s.set); serr != nil && firstErr == nil {
			firstErr = fmt.Errorf("restoring the cpuset of tid %d: %w", tid, serr)
		}
	}
	return firstErr
}

// ProcessAffinity reports the calling thread's cpuset: on Linux affinity
// is per-thread, and the process-wide effect comes from SetAffinity
// walking every thread, which is also how a saved mask is restored.
func ProcessAffinity() (uint64, bool) { return CurrentThreadAffinity() }

// ProcessAffinityOf cannot read a foreign process's affinity here: the
// caller must report the coverage as unconfirmed, never assume it.
func ProcessAffinityOf(pid int) (uint64, error) {
	return 0, fmt.Errorf("reading another process's affinity (pid %d) is not supported on this platform", pid)
}

// SetProcessAffinityOf cannot confine a foreign process here.
func SetProcessAffinityOf(pid int, mask uint64) error {
	return fmt.Errorf("setting another process's affinity (pid %d) is not supported on this platform", pid)
}

// ProcessorGroups cannot report group layout here.
func ProcessorGroups() (int, int, bool) { return 0, 0, false }
