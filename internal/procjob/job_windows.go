//go:build windows

package procjob

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject           = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")

	ntdll                = syscall.NewLazyDLL("ntdll.dll")
	procNtSuspendProcess = ntdll.NewProc("NtSuspendProcess")
	procNtResumeProcess  = ntdll.NewProc("NtResumeProcess")
)

const (
	processSetQuota      = 0x0100
	processTerminate     = 0x0001
	processSuspendResume = 0x0800

	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	jobObjectBasicProcessIdList       = 3
)

// The Win32 structs below are mirrored only as far as we use them; Go's
// field alignment reproduces the C layout on 386/amd64.
type jobObjectBasicLimitInformation struct {
	perProcessUserTimeLimit int64
	perJobUserTimeLimit     int64
	limitFlags              uint32
	maximumWorkingSetSize   uintptr
	minimumWorkingSetSize   uintptr
	activeProcessLimit      uint32
	affinity                uintptr
	priorityClass           uint32
	schedulingClass         uint32
}

type ioCounters struct {
	readOperationCount  uint64
	writeOperationCount uint64
	otherOperationCount uint64
	readTransferCount   uint64
	writeTransferCount  uint64
	otherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	basicLimitInformation jobObjectBasicLimitInformation
	ioInfo                ioCounters
	processMemoryLimit    uintptr
	jobMemoryLimit        uintptr
	peakProcessMemoryUsed uintptr
	peakJobMemoryUsed     uintptr
}

// Job is an open Job Object handle. It is not safe for concurrent use, but
// the intended pattern (create, attach, close from one goroutine) is.
type Job struct {
	handle syscall.Handle
}

// live tracks every open job in this process. The per-core worker uses it
// to suspend or resume every external tool it owns without the workload
// package having to expose its process groups.
var (
	liveMu sync.Mutex
	live   = map[*Job]struct{}{}
)

// LiveJobs returns the job handles currently open in this process.
func LiveJobs() []*Job {
	liveMu.Lock()
	defer liveMu.Unlock()
	out := make([]*Job, 0, len(live))
	for j := range live {
		out = append(out, j)
	}
	return out
}

// New creates a job whose members are all terminated when the last handle
// to it closes. That covers both our explicit stop (Close) and the case
// where the owning process itself is killed: its handle closes anyway.
func New() (*Job, error) {
	h, _, callErr := procCreateJobObject.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", callErr)
	}

	var info jobObjectExtendedLimitInfo
	info.basicLimitInformation.limitFlags = jobObjectLimitKillOnJobClose
	r, _, callErr := procSetInformationJobObject.Call(
		h,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, fmt.Errorf("SetInformationJobObject: %w", callErr)
	}
	j := &Job{handle: syscall.Handle(h)}
	liveMu.Lock()
	live[j] = struct{}{}
	liveMu.Unlock()
	return j, nil
}

// Attach puts an already-started process into the job. Ownership starts only
// after this call succeeds: a parent death or a child spawning descendants
// between Start and Attach is still a race. Callers must handle attachment
// failure and terminate/reap the direct child. Windows 8+ supports nested jobs.
func (j *Job) Attach(p *os.Process) error {
	if j == nil || p == nil {
		return nil
	}
	hProc, _, callErr := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(p.Pid))
	if hProc == 0 {
		return fmt.Errorf("OpenProcess(pid %d): %w", p.Pid, callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(hProc))

	r, _, callErr := procAssignProcessToJobObject.Call(uintptr(j.handle), hProc)
	if r == 0 {
		return fmt.Errorf("AssignProcessToJobObject(pid %d): %w", p.Pid, callErr)
	}
	return nil
}

// Close releases the job handle; kill-on-close terminates every member that
// is still alive. Safe on a nil Job and safe to call twice.
func (j *Job) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	liveMu.Lock()
	delete(live, j)
	liveMu.Unlock()
	if err := syscall.CloseHandle(j.handle); err != nil {
		return fmt.Errorf("CloseHandle(job): %w", err)
	}
	j.handle = 0
	return nil
}

// SuspendProcess freezes every thread of the process; ResumeProcess thaws
// it. A frozen process still accepts TerminateProcess, so kill-on-close and
// context cancellation keep working while a process is suspended.
func SuspendProcess(pid int) error {
	h, _, callErr := procOpenProcess.Call(processSuspendResume, 0, uintptr(pid))
	if h == 0 {
		return fmt.Errorf("OpenProcess(pid %d): %w", pid, callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	r1, _, _ := procNtSuspendProcess.Call(h)
	if r1 != 0 {
		return fmt.Errorf("NtSuspendProcess(pid %d): status %#x", pid, r1)
	}
	return nil
}

// ResumeProcess thaws a process suspended by SuspendProcess. Thawing a
// process that was never suspended is a harmless no-op.
func ResumeProcess(pid int) error {
	h, _, callErr := procOpenProcess.Call(processSuspendResume, 0, uintptr(pid))
	if h == 0 {
		return fmt.Errorf("OpenProcess(pid %d): %w", pid, callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	r1, _, _ := procNtResumeProcess.Call(h)
	if r1 != 0 {
		return fmt.Errorf("NtResumeProcess(pid %d): status %#x", pid, r1)
	}
	return nil
}

// processIdListInfo mirrors the Win32 JOBOBJECT_BASIC_PROCESS_ID_LIST; the
// array is a one-element placeholder, the real list is read out of an
// over-sized buffer.
type processIdListInfo struct {
	numberOfAssignedProcesses uint32
	numberOfProcessIdsInList  uint32
	processIdList             [1]uintptr
}

// Suspend freezes every process directly inside the job. Processes in
// nested jobs are not listed here — that is why the per-core worker
// suspends its own nested tool job through the control channel instead of
// relying on the parent to reach through the nesting.
func (j *Job) Suspend() error { return j.eachPID(SuspendProcess) }

// Resume thaws every process directly inside the job.
func (j *Job) Resume() error { return j.eachPID(ResumeProcess) }

func (j *Job) eachPID(fn func(int) error) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	const capacity = 128
	buf := make([]byte, unsafe.Sizeof(processIdListInfo{})+(capacity-1)*unsafe.Sizeof(uintptr(0)))
	var returned uint32
	r, _, callErr := procQueryInformationJobObject.Call(
		uintptr(j.handle),
		jobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r == 0 {
		return fmt.Errorf("QueryInformationJobObject(process list): %w", callErr)
	}
	hdr := (*processIdListInfo)(unsafe.Pointer(&buf[0]))
	pids := unsafe.Slice(&hdr.processIdList[0], int(hdr.numberOfProcessIdsInList))

	var firstErr error
	for _, pid := range pids {
		if err := fn(int(pid)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
