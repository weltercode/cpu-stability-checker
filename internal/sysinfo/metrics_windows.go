//go:build windows

package sysinfo

import (
	"sync"
	"unsafe"
)

var (
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx mirrors the Windows MEMORYSTATUSEX structure. The
// DWORDLONG fields are uint64, not uintptr: the Windows ABI demands
// 64-bit fields there, and uintptr would silently break the layout on
// 32-bit builds. The size test in metrics_test.go pins the layout.
type memoryStatusEx struct {
	Length        uint32
	MemoryLoad    uint32
	TotalPhys     uint64
	AvailPhys     uint64
	TotalPageFile uint64
	AvailPageFile uint64
	TotalVirtual  uint64
	AvailVirtual  uint64
	AvailExtended uint64
}

func memoryStatsImpl() (uint64, uint64, bool) {
	m, ok := queryMemoryStatus()
	if !ok {
		return 0, 0, false
	}
	return m.TotalPhys, m.TotalPhys - m.AvailPhys, true
}

// queryMemoryStatus is the single GlobalMemoryStatusEx call site; the
// snapshot fields (physical AND commit) all come from one consistent
// query so the budget math never mixes observations from different
// instants.
func queryMemoryStatus() (memoryStatusEx, bool) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return memoryStatusEx{}, false
	}
	return m, true
}

func memorySnapshotImpl() MemorySnapshot {
	m, ok := queryMemoryStatus()
	if !ok {
		return MemorySnapshot{}
	}
	return MemorySnapshot{
		Known:         true,
		CommitKnown:   true,
		TotalPhys:     m.TotalPhys,
		AvailPhys:     m.AvailPhys,
		TotalPageFile: m.TotalPageFile,
		AvailPageFile: m.AvailPageFile,
	}
}

// --- CPU frequency / load ---------------------------------------------------

// coreFrequencies: Windows has no unprivileged per-logical-CPU current
// frequency. NtQuerySystemInformation class 1 (SYSTEM_PROCESSOR_INFORMATION)
// returns a single 12-byte record of rated clocks on this OS — verified on
// Win11 26H2 — and the per-core cycle-time source that would yield a
// measured clock (CPU Set CycleTime) is refused there (see coretimes). The
// honest answer is "unknown"; the engines' own cycle counters measure the
// clock of cores actually under test, and the dashboard shows those.
func coreFrequencies() ([]float64, bool) { return nil, false }

// filetime is a 64-bit FILETIME split into two 32-bit halves.
type filetime struct{ Low, High uint32 }

func (t filetime) u64() uint64 { return uint64(t.High)<<32 | uint64(t.Low) }

var (
	loadMu    sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	havePrev  bool
)

// cpuLoadPct returns system-wide CPU utilization over the interval since the
// previous call (GetSystemTimes reports since-boot counters). The very first
// call has no baseline and returns metricsUnknown.
//
// Note: Windows' kernel time already includes idle time, so busy = (kernel +
// user) - idle.
func cpuLoadPct() float64 {
	var idle, kernel, user filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return metricsUnknown
	}
	curIdle := idle.u64()
	curTotal := kernel.u64() + user.u64()

	loadMu.Lock()
	defer loadMu.Unlock()
	if !havePrev {
		prevIdle, prevTotal, havePrev = curIdle, curTotal, true
		return metricsUnknown
	}
	dIdle := curIdle - prevIdle
	dTotal := curTotal - prevTotal
	prevIdle, prevTotal = curIdle, curTotal
	if dTotal == 0 {
		return metricsUnknown
	}
	pct := 100.0 * float64(dTotal-dIdle) / float64(dTotal)
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// cpuStatsImpl: Windows exposes system load via GetSystemTimes; current
// frequency has no unprivileged source (see coreFrequencies) and
// temperature needs the PDH thermal zone, so both degrade to unknown.
func cpuStatsImpl() Metrics {
	var m Metrics
	if p := cpuLoadPct(); p >= 0 {
		m.LoadPct = p
		m.LoadKnown = true
	}
	return m
}
