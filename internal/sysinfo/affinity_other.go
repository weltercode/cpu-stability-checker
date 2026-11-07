//go:build !windows && !linux

package sysinfo

import "fmt"

// SetAffinity is a no-op on platforms without a supported affinity API; the
// test will still run but is not pinned to specific cores.
func SetAffinity(cores []int) error { return nil }

// SetThreadAffinity is a no-op here, like SetAffinity.
func SetThreadAffinity(core int) error { return nil }

// CurrentThreadAffinity cannot report anything on these platforms.
func CurrentThreadAffinity() (uint64, bool) { return 0, false }

// WidenAffinity has nothing to widen here.
func WidenAffinity(cores []int) []int { return nil }

// savedProcessAffinity is empty here: no facility of this package ever
// changes process affinity on these platforms, so there is nothing to
// hand back. The nil restore error is the accurate no-op - not a claim
// that a save/restore cycle happened, because nothing was ever taken.
type savedProcessAffinity struct{}

func saveProcessAffinity() savedProcessAffinity { return savedProcessAffinity{} }

func restoreProcessAffinity(savedProcessAffinity) error { return nil }

// ProcessAffinity cannot report anything on these platforms.
func ProcessAffinity() (uint64, bool) { return 0, false }

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
