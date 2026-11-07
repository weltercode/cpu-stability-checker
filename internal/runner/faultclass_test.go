package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// A06 in the threaded engine: a memory-class fault decides the exit
// code and lands as a memory-region finding with the CPU recorded as
// observed_on - it must never enter the faulted-CPU map that the
// scenario blacklist acts on.
func TestMemoryClassFaultNeverBlacklists(t *testing.T) {
	preserveRunnerState(t)
	workload.Register(&workload.Workload{Name: "r01-mem-fault", FaultClass: workload.FaultMemory,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(3)
			return 3, errors.New("engine word differs")
		}})
	t.Cleanup(func() { workload.Delete("r01-mem-fault") })

	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "r01-mem-fault",
		Workers: 1, Duration: 2 * time.Second, StopOnFault: true, ControlCore: -1,
		LogFile: t.TempDir() + "/r01mem.log"}
	res := ExecuteWithResult(opt)
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1: a memory fault still makes the run not-clean", res.Exit)
	}
	if len(res.Faults) != 0 {
		t.Errorf("Faults = %v, want no CPU blacklisted for memory evidence", res.Faults)
	}
	var mem *plan.Finding
	for i, f := range res.Findings {
		if f.Kind == plan.FindDataMismatch {
			mem = &res.Findings[i]
		}
	}
	if mem == nil {
		t.Fatalf("no data-mismatch finding: %+v", res.Findings)
	}
	if mem.Attribution.Kind != plan.AttrMemoryRegion {
		t.Errorf("attribution = %q, want memory_region", mem.Attribution.Kind)
	}
	if mem.Attribution.Index != nil {
		t.Errorf("index = %v, want nil: the CPU is not the accused component", *mem.Attribution.Index)
	}
	if mem.Attribution.ObservedOn == nil || *mem.Attribution.ObservedOn != 0 {
		t.Errorf("observed_on = %v, want CPU 0 recorded separately", mem.Attribution.ObservedOn)
	}
}

// The synthetic prober keeps its documented soak contract (it still
// blacklists, proving the per-CPU pipeline end to end) but its kind
// says synthetic_probe: no report can render it as a real mismatch.
func TestSyntheticProberKind(t *testing.T) {
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "fault",
		Workers: 1, Duration: 2 * time.Second, StopOnFault: true, ControlCore: -1,
		LogFile: t.TempDir() + "/r01probe.log"}
	res := ExecuteWithResult(opt)
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1", res.Exit)
	}
	if res.Faults[0] == "" {
		t.Errorf("Faults = %v, want CPU 0 blacklisted (the prober's pipeline-proof contract)", res.Faults)
	}
	var probe bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindSyntheticProbe {
			probe = true
		}
		if f.Kind == plan.FindDataMismatch {
			t.Errorf("the prober was recorded as a real data mismatch: %+v", f)
		}
	}
	if !probe {
		t.Errorf("no synthetic_probe finding: %+v", res.Findings)
	}
}
