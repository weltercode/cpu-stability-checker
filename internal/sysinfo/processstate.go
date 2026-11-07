package sysinfo

import (
	"runtime"
)

// SavedProcessState is the process-wide scheduler state a run only
// borrows: the process affinity mask and GOMAXPROCS. Both belong to the
// whole process - a menu host or the next run would otherwise inherit
// one run's pinning (audit A01) - so whoever changes them must snapshot
// them BEFORE the first mutation and hand them back on every exit path.
// What a saved affinity is, and how it goes back, is defined per
// platform beside the affinity calls themselves; platforms that never
// mutate process affinity hand back an accurate no-op.
type SavedProcessState struct {
	aff   savedProcessAffinity
	procs int
}

// SaveProcessState snapshots both values. Call it as the first act
// before touching affinity or GOMAXPROCS, and register Restore through
// defer right there: every later return must give the state back,
// setup failures included.
func SaveProcessState() SavedProcessState {
	return SavedProcessState{aff: saveProcessAffinity(), procs: runtime.GOMAXPROCS(0)}
}

// Restore hands the state back: the affinity mask first (so the
// scheduler never faces more Ps than reachable CPUs while the mask is
// still narrowed), then GOMAXPROCS. GOMAXPROCS cannot fail; any error
// names exactly what could NOT be handed back - reporting it beats
// pretending the process is clean again. A caller must record that as
// infrastructure evidence, never as a CPU fault.
func (s SavedProcessState) Restore() error {
	err := restoreProcessAffinity(s.aff)
	runtime.GOMAXPROCS(s.procs)
	return err
}
