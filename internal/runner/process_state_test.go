package runner

// Q01 (audit A01): process affinity and GOMAXPROCS are process-wide
// state a run only borrows. These tests assert the PRODUCTION handback
// inside ExecuteWithPanel. The safety net in preserveRunnerState only
// keeps the suite survivable after a failed assertion - t.Cleanup runs
// after these checks, never instead of them.

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

func stateProbe(t *testing.T) (mask uint64, known bool, procs int) {
	t.Helper()
	mask, known = sysinfo.ProcessAffinity()
	return mask, known, runtime.GOMAXPROCS(0)
}

func stateRunOptions(t *testing.T, cores []int) plan.Options {
	t.Helper()
	return plan.Options{TestName: "general", Cores: cores, A: "float", B: "mem", Workers: 1,
		Duration: 400 * time.Millisecond, ControlCore: -1, LogFile: filepath.Join(t.TempDir(), "state.log")}
}

func assertHandedBack(t *testing.T, wantMask uint64, wantKnown bool, wantProcs int) {
	t.Helper()
	mask, known, procs := stateProbe(t)
	if known != wantKnown || (known && mask != wantMask) {
		t.Errorf("process affinity mask = %016x (known=%v), want %016x (known=%v): the run kept the process",
			mask, known, wantMask, wantKnown)
	}
	if procs != wantProcs {
		t.Errorf("GOMAXPROCS = %d, want %d: the run kept the scheduler cap", procs, wantProcs)
	}
}

// A single-core run narrows the process mask to cpu 0 and caps the
// scheduler to 1 P: exactly the state the audit saw leak into every
// later test and child process. The guard must undo both on the clean
// return path.
func TestCleanRunHandsBackProcessState(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs a second cpu for the leak this test guards to be possible")
	}
	preserveRunnerState(t) // the safety net runs AFTER these production assertions
	mask0, known0, procs0 := stateProbe(t)
	res := ExecuteWithResult(stateRunOptions(t, []int{0}))
	if res.Exit != 0 {
		t.Fatalf("single-core run: %+v", res)
	}
	assertHandedBack(t, mask0, known0, procs0)
}

// The leak does not stop at clean finishes: ExecuteWithPanel opens the
// log AFTER pinning and GOMAXPROCS, so a setup refusal returned from
// behind those mutations must hand the state back too.
func TestSetupErrorHandsBackProcessState(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("needs a second cpu for the leak this test guards to be possible")
	}
	preserveRunnerState(t)
	mask0, known0, procs0 := stateProbe(t)
	opt := stateRunOptions(t, []int{0})
	// A path into a missing directory: the pinning and the scheduler cap
	// were already applied when logging.New refuses this log.
	opt.LogFile = filepath.Join(t.TempDir(), "no-such-dir", "state.log")
	res := ExecuteWithResult(opt)
	if res.Exit != 2 {
		t.Fatalf("an unopenable log must exit as a setup error, not a fault: %+v", res)
	}
	assertHandedBack(t, mask0, known0, procs0)
}

// The audit's poisoned-sequence symptom ("CPU 1 does not exist; logical
// CPUs 0..0"): after a single-core run the NEXT run or child must find
// cpu 1 in the mask again. Here the next in-process run is the probe.
func TestSequentialRunsNeverInheritTheNarrowMask(t *testing.T) {
	if runtime.NumCPU() < 3 {
		t.Skip("needs cpus 0 and 1 plus scheduler headroom")
	}
	preserveRunnerState(t)
	mask0, known0, procs0 := stateProbe(t)
	if res := ExecuteWithResult(stateRunOptions(t, []int{0})); res.Exit != 0 {
		t.Fatalf("first run: %+v", res)
	}
	if mask1, known1, _ := stateProbe(t); known1 && mask1&(uint64(1)<<1) == 0 {
		t.Fatalf("cpu 1 vanished from the process mask (%016x) after a single-core run", mask1)
	}
	if res := ExecuteWithResult(stateRunOptions(t, []int{0, 1})); res.Exit != 0 {
		t.Fatalf("second run on cpus 0 and 1: %+v", res)
	}
	assertHandedBack(t, mask0, known0, procs0)
}

// A process that was ALREADY narrowed when the run started (a parent
// shell restriction - the case WidenAffinity exists for) must get its
// exact original mask back: the restore gives back the caller's world,
// never a wider "all cpus" convenience and never the run's leftovers.
func TestAlreadyNarrowedMaskComesBackNarrow(t *testing.T) {
	if runtime.NumCPU() < 3 {
		t.Skip("needs cpus 0 and 1 plus scheduler headroom")
	}
	mask, ok := sysinfo.ProcessAffinity()
	if !ok {
		t.Skip("this platform cannot report a process affinity to narrow and verify")
	}
	_ = mask
	preserveRunnerState(t) // lifts the TEST's own narrowing as the final safety net
	if err := sysinfo.SetAffinity([]int{0, 1}); err != nil {
		t.Fatalf("the test could not narrow the process: %v", err)
	}
	mask0, known0, procs0 := stateProbe(t)
	if !known0 || mask0 != 0b11 {
		t.Fatalf("setup: process mask = %016x (known=%v), want the exact two-cpu restriction", mask0, known0)
	}
	// Inside the run ApplyAffinity narrows further (to core 1); the
	// handback must land on the saved restriction, not on either side.
	if res := ExecuteWithResult(stateRunOptions(t, []int{1})); res.Exit != 0 {
		t.Fatalf("run inside the restricted process: %+v", res)
	}
	mask1, known1, procs1 := stateProbe(t)
	if !known1 || mask1 != 0b11 {
		t.Errorf("mask after the run = %016x (known=%v), want the saved 0b11 exactly", mask1, known1)
	}
	if procs1 != procs0 {
		t.Errorf("GOMAXPROCS after the run = %d, want %d", procs1, procs0)
	}
}
