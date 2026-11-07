//go:build windows

package sysinfo

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procSetProcessAffinityMask = kernel32.NewProc("SetProcessAffinityMask")
	procGetProcessAffinityMask = kernel32.NewProc("GetProcessAffinityMask")
	procSetThreadAffinityMask  = kernel32.NewProc("SetThreadAffinityMask")
	procGetThreadGroupAffinity = kernel32.NewProc("GetThreadGroupAffinity")

	procGetMaximumProcessorGroupCount = kernel32.NewProc("GetMaximumProcessorGroupCount")
	procGetActiveProcessorCount       = kernel32.NewProc("GetActiveProcessorCount")
)

// processSetLimitedInformation is the narrow right SetProcessAffinityMask
// needs on a foreign process handle (procOpenProcess and
// processQueryLimitedInformation live beside the cycle-time procs).
const processSetLimitedInformation = 0x0800

// allProcessorGroups selects every group in GetActiveProcessorCount.
const allProcessorGroups = 0xFFFF

// ProcessorGroups reports how many processor groups the machine can
// have and how many logical CPUs are active across all of them. Our
// mask-based pinning speaks group-0 64-bit masks only: when the machine
// has more than one group (or more than 64 CPUs), cores beyond the mask
// are NOT silently truncated away - callers must say they are outside
// what pinning can express.
//
// (GetProcessGroupAffinity, the per-process group query, was measured
// broken on Windows build 26300 - it returns FALSE with ERROR_BAD_LENGTH
// for every handle and buffer size - so per-process group membership is
// not claimed anywhere: a child with no group-0 mask is reported as
// "possibly another group", never as a confirmed group number.)
func ProcessorGroups() (maxGroups int, activeCPUs int, ok bool) {
	g, _, _ := procGetMaximumProcessorGroupCount.Call()
	n, _, _ := procGetActiveProcessorCount.Call(allProcessorGroups)
	if g == 0 || n == 0 {
		return 0, 0, false
	}
	return int(g), int(n), true
}

// currentThread is the GetCurrentThread() pseudo-handle, (HANDLE)-2.
const currentThread = ^uintptr(1)

// SetAffinity pins the whole process to the given logical cores.
func SetAffinity(cores []int) error {
	var mask uintptr
	for _, c := range cores {
		if c < 0 || c >= 64 {
			return fmt.Errorf("core index %d out of range [0,63]", c)
		}
		mask |= 1 << uint(c)
	}
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	r, _, callErr := procSetProcessAffinityMask.Call(uintptr(h), mask)
	if r == 0 {
		return fmt.Errorf("SetProcessAffinityMask failed: %w", callErr)
	}
	return nil
}

// SetThreadAffinity pins the calling OS thread to one logical core. Call it
// from a goroutine locked to its thread (runtime.LockOSThread); the thread's
// mask must stay a subset of the process mask, so ApplyAffinity has to cover
// the core first.
func SetThreadAffinity(core int) error {
	if core < 0 || core >= 64 {
		return fmt.Errorf("core index %d out of range [0,63]", core)
	}
	r, _, callErr := procSetThreadAffinityMask.Call(currentThread, uintptr(1)<<uint(core))
	if r == 0 {
		return fmt.Errorf("SetThreadAffinityMask failed: %w", callErr)
	}
	return nil
}

// CurrentThreadAffinity reports the calling thread's affinity mask as a
// core bitmap, and whether the platform could report it at all. It reads
// the group affinity because GetThreadAffinityMask is not exported by
// kernel32 on recent Windows builds; a thread pinned into a non-zero
// processor group reports "unknown" (our core numbers are group-0 only).
func CurrentThreadAffinity() (uint64, bool) {
	var ga struct {
		mask        uintptr
		group       uint16
		reserved    [3]uint16
		groupHandle uintptr
	}
	r, _, _ := procGetThreadGroupAffinity.Call(currentThread, uintptr(unsafe.Pointer(&ga)))
	if r == 0 || ga.group != 0 {
		return 0, false
	}
	return uint64(ga.mask), true
}

// WidenAffinity grows the process affinity mask to cover the given cores,
// bounded by the system affinity mask. A process can inherit a narrower
// mask from its parent shell, and SetThreadAffinityMask rejects any core
// outside the process mask — widening first is what makes pinning work in
// that case. Returns the cores that stay unreachable (outside the system
// mask, e.g. a job-object or processor-group restriction).
func WidenAffinity(cores []int) []int {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return cores
	}
	var procMask, sysMask uintptr
	if r, _, _ := procGetProcessAffinityMask.Call(uintptr(h), uintptr(unsafe.Pointer(&procMask)), uintptr(unsafe.Pointer(&sysMask))); r == 0 {
		return cores
	}
	var missing []int
	want := uintptr(0)
	for _, c := range cores {
		if c < 0 || c >= 64 {
			missing = append(missing, c)
			continue
		}
		bit := uintptr(1) << uint(c)
		if procMask&bit != 0 {
			continue
		}
		if sysMask&bit == 0 {
			missing = append(missing, c)
			continue
		}
		want |= bit
	}
	if want == 0 {
		return missing
	}
	if r, _, _ := procSetProcessAffinityMask.Call(uintptr(h), procMask|want); r == 0 {
		return cores // widening refused: everything not already covered stays unreachable
	}
	return missing
}

// savedProcessAffinity is this process's exact group-0 affinity mask as
// GetProcessAffinityMask reported it before any change. A mask read from
// the own process is by construction a legal value to hand back; what
// sits beyond a 64-bit group-0 mask stays outside mask-based pinning
// here (ProcessorGroups says how much of the machine that is).
type savedProcessAffinity struct {
	mask uint64
	ok   bool
}

func saveProcessAffinity() savedProcessAffinity {
	m, ok := ProcessAffinity()
	return savedProcessAffinity{mask: m, ok: ok}
}

// restoreProcessAffinity applies the saved mask - never "all cpus", the
// caller's restriction may itself have been narrower - and verifies it
// by readback: attempts are not trusted in this project (see
// ProcessAffinityOf), so a mask that does not come back is reported,
// not wished away.
func restoreProcessAffinity(s savedProcessAffinity) error {
	if !s.ok {
		return errors.New("process affinity was unreadable before the change: the mask stays as the run left it")
	}
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return fmt.Errorf("restoring the process affinity mask: %w", err)
	}
	if r, _, callErr := procSetProcessAffinityMask.Call(uintptr(h), uintptr(s.mask)); r == 0 {
		return fmt.Errorf("SetProcessAffinityMask(%016x): %w", s.mask, callErr)
	}
	if back, okRead := ProcessAffinity(); okRead && back != s.mask {
		return fmt.Errorf("process affinity reads back as %016x after restoring %016x", back, s.mask)
	}
	return nil
}

// ProcessAffinity reports the process affinity mask as a core bitmap, for
// saving and restoring it around a temporary restriction.
func ProcessAffinity() (uint64, bool) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, false
	}
	var procMask, sysMask uintptr
	if r, _, _ := procGetProcessAffinityMask.Call(uintptr(h), uintptr(unsafe.Pointer(&procMask)), uintptr(unsafe.Pointer(&sysMask))); r == 0 {
		return 0, false
	}
	return uint64(procMask), true
}

// ProcessAffinityOf reads back the affinity a foreign process actually
// has applied - not what we hoped it inherited. It reports the group-0
// mask only: per-process group membership cannot be read reliably
// (GetProcessGroupAffinity is broken on build 26300, see ProcessorGroups),
// so a mask of zero means "no group-0 affinity" and callers must treat
// the process as possibly living in another processor group, never as
// confirmed group-0.
func ProcessAffinityOf(pid int) (mask uint64, err error) {
	h, _, callErr := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return 0, fmt.Errorf("OpenProcess(%d): %w", pid, callErr)
	}
	defer procCloseHandle.Call(h)
	var procMask, sysMask uintptr
	if r, _, ce := procGetProcessAffinityMask.Call(h, uintptr(unsafe.Pointer(&procMask)), uintptr(unsafe.Pointer(&sysMask))); r == 0 {
		return 0, fmt.Errorf("GetProcessAffinityMask(%d): %w", pid, ce)
	}
	return uint64(procMask), nil
}

// SetProcessAffinityOf confines a foreign process to a core mask. It is
// best effort by nature: the tool may change its own affinity again
// afterwards, which is exactly why callers read the mask back instead
// of trusting the attempt.
func SetProcessAffinityOf(pid int, mask uint64) error {
	h, _, callErr := procOpenProcess.Call(processQueryLimitedInformation|processSetLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return fmt.Errorf("OpenProcess(%d): %w", pid, callErr)
	}
	defer procCloseHandle.Call(h)
	if r, _, ce := procSetProcessAffinityMask.Call(h, uintptr(mask)); r == 0 {
		return fmt.Errorf("SetProcessAffinityMask(%d): %w", pid, ce)
	}
	return nil
}
