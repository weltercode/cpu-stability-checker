//go:build !windows && !linux

package sysinfo

import "fmt"

// coreTimes has no portable implementation; callers degrade silently.
func coreTimes() ([]CoreTime, error) {
	return nil, fmt.Errorf("per-core time accounting is not implemented on this platform")
}

// threadCycleTimeSelf is Windows-only for now.
func threadCycleTimeSelf() (uint64, error) {
	return 0, fmt.Errorf("thread cycle time is not implemented on this platform")
}

// processCycleTimeSelf is Windows-only for now.
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
