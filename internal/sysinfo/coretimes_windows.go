//go:build windows

package sysinfo

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	ntdll                        = syscall.NewLazyDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")
	procQueryThreadCycleTime     = kernel32.NewProc("QueryThreadCycleTime")
	procQueryProcessCycleTime    = kernel32.NewProc("QueryProcessCycleTime")
)

const systemProcessorPerformanceInformation = 8

// coreTimes queries per-logical-CPU idle/kernel/user accounting. The
// initial zero-length call reports the needed buffer size.
func coreTimes() ([]CoreTime, error) {
	var needed uint32
	procNtQuerySystemInformation.Call(systemProcessorPerformanceInformation, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if needed == 0 {
		return nil, fmt.Errorf("NtQuerySystemInformation: system reports no processor performance data")
	}
	buf := make([]byte, needed)
	var returned uint32
	r, _, callErr := procNtQuerySystemInformation.Call(
		systemProcessorPerformanceInformation,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&returned)))
	if r != 0 { // NTSTATUS: 0 is success
		return nil, fmt.Errorf("NtQuerySystemInformation(processor performance): status 0x%x: %w", r, callErr)
	}
	times := parseProcessorPerformance(buf[:returned])
	if len(times) == 0 {
		return nil, fmt.Errorf("NtQuerySystemInformation: no processor entries")
	}
	return times, nil
}

// threadCycleTimeSelf asks kernel32 for the calling thread's cycle count.
func threadCycleTimeSelf() (uint64, error) {
	var cycles uint64
	r, _, callErr := procQueryThreadCycleTime.Call(currentThread, uintptr(unsafe.Pointer(&cycles)))
	if r == 0 {
		return 0, fmt.Errorf("QueryThreadCycleTime failed: %w", callErr)
	}
	return cycles, nil
}

// currentProcess is the GetCurrentProcess() pseudo-handle, (HANDLE)-1.
const currentProcess = ^uintptr(0)

// processCycleTimeSelf asks kernel32 for the whole process's cycle count.
func processCycleTimeSelf() (uint64, error) {
	var cycles uint64
	r, _, callErr := procQueryProcessCycleTime.Call(currentProcess, uintptr(unsafe.Pointer(&cycles)))
	if r == 0 {
		return 0, fmt.Errorf("QueryProcessCycleTime failed: %w", callErr)
	}
	return cycles, nil
}

var (
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	procOpenThread         = kernel32.NewProc("OpenThread")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
	procGetHandleCount     = kernel32.NewProc("GetProcessHandleCount")
)

// handleCount asks the OS how many handles this process holds right now.
func handleCount() (int, bool) {
	var n uint32
	r, _, _ := procGetHandleCount.Call(currentProcess, uintptr(unsafe.Pointer(&n)))
	if r == 0 { // BOOL: 0 is failure
		return 0, false
	}
	return int(n), true
}

// threadQueryLimitedInformation is the access right QueryThreadCycleTime
// needs; it works on threads of our own process without elevation.
const threadQueryLimitedInformation = 0x0800

// processQueryLimitedInformation is the process-side equivalent, enough
// for QueryProcessCycleTime on our own children.
const processQueryLimitedInformation = 0x1000

func currentThreadID() int {
	tid, _, _ := procGetCurrentThreadID.Call()
	return int(tid)
}

// newProcessCycleCounter opens the child with PROCESS_QUERY_LIMITED_INFORMATION,
// the right QueryProcessCycleTime needs; it works on our own children
// without elevation.
func newProcessCycleCounter(pid int) (*ProcessCycleCounter, error) {
	h, _, callErr := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return nil, fmt.Errorf("OpenProcess(%d) failed: %w", pid, callErr)
	}
	return &ProcessCycleCounter{handle: h}, nil
}

func (c *ProcessCycleCounter) read() (uint64, error) {
	if c == nil || c.handle == 0 {
		return 0, fmt.Errorf("process cycle counter is closed")
	}
	var cycles uint64
	r, _, callErr := procQueryProcessCycleTime.Call(c.handle, uintptr(unsafe.Pointer(&cycles)))
	if r == 0 {
		return 0, fmt.Errorf("QueryProcessCycleTime failed: %w", callErr)
	}
	return cycles, nil
}

func (c *ProcessCycleCounter) close() {
	if c == nil || c.handle == 0 {
		return
	}
	procCloseHandle.Call(c.handle)
	c.handle = 0
}

func threadCycleTime(tid int) (uint64, error) {
	h, _, callErr := procOpenThread.Call(threadQueryLimitedInformation, 0, uintptr(tid))
	if h == 0 {
		return 0, fmt.Errorf("OpenThread(%d) failed: %w", tid, callErr)
	}
	defer procCloseHandle.Call(h)
	var cycles uint64
	r, _, callErr := procQueryThreadCycleTime.Call(h, uintptr(unsafe.Pointer(&cycles)))
	if r == 0 {
		return 0, fmt.Errorf("QueryThreadCycleTime(%d) failed: %w", tid, callErr)
	}
	return cycles, nil
}
