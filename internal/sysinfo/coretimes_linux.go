//go:build linux

package sysinfo

import (
	"fmt"
	"os"
)

// coreTimes reads per-logical-CPU time accounting from /proc/stat.
// The same idle-inclusive-of-kernel contract as Windows is produced by
// parseProcStatCPUs, so IdlePct behaves identically on both platforms.
func coreTimes() ([]CoreTime, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, fmt.Errorf("read /proc/stat: %w", err)
	}
	times := parseProcStatCPUs(string(data))
	if len(times) == 0 {
		return nil, fmt.Errorf("/proc/stat has no per-cpu lines")
	}
	return times, nil
}

// threadCycleTimeSelf and processCycleTimeSelf stay unsupported on Linux:
// /proc reports CPU time, not cycles, and cycle counts need the taskstats
// netlink or perf_event_open — not worth the machinery until someone
// actually runs this tool there.

func threadCycleTimeSelf() (uint64, error) {
	return 0, fmt.Errorf("thread cycle time is not implemented on this platform")
}

func processCycleTimeSelf() (uint64, error) {
	return 0, fmt.Errorf("process cycle time is not implemented on this platform")
}

func currentThreadID() int { return 0 }

func threadCycleTime(tid int) (uint64, error) {
	return 0, fmt.Errorf("thread cycle time is not implemented on this platform")
}

func newProcessCycleCounter(pid int) (*ProcessCycleCounter, error) {
	return nil, fmt.Errorf("process cycle time is not implemented on this platform")
}

func (c *ProcessCycleCounter) read() (uint64, error) {
	return 0, fmt.Errorf("process cycle time is not implemented on this platform")
}

func (c *ProcessCycleCounter) close() {}

func handleCount() (int, bool) { return 0, false }
