package runner

import (
	"context"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// pinHelper must confine one helper goroutine's thread to the control
// core (and do nothing at all when there is no control core).
func TestPinHelperConfinesThread(t *testing.T) {
	if _, ok := sysinfo.CurrentThreadAffinity(); !ok {
		t.Skip("platform cannot report thread affinity")
	}
	if runtime.NumCPU() < 4 {
		t.Skipf("test pins core 3; machine has %d CPUs", runtime.NumCPU())
	}

	type probe struct {
		mask  uint64
		ok    int64
		total int64
	}
	run := func(core int) probe {
		ch := make(chan probe, 1)
		go func() {
			var ok, total int64
			pinHelper(core, "test", &ok, &total)
			mask, _ := sysinfo.CurrentThreadAffinity()
			ch <- probe{mask: mask, ok: ok, total: total}
		}()
		return <-ch
	}

	if got := run(3); got.mask != 1<<3 || got.ok != 1 || got.total != 1 {
		t.Errorf("pinHelper(3): mask=%x ok=%d total=%d, want pinned to core 3 and counted 1/1",
			got.mask, got.ok, got.total)
	}
	if got := run(-1); got.ok != 0 || got.total != 0 {
		t.Errorf("pinHelper(-1): ok=%d total=%d, want no pinning and no counting", got.ok, got.total)
	}
}

// In-process workers must run pinned to their slot's logical CPU, so a
// fault attributed to slot i really happened on that core — the same
// guarantee the per-process mode gives. A pinned thread reports a
// single-core mask; an unpinned one inherits the whole process mask.
func TestExecutePinsWorkersToTheirCPUs(t *testing.T) {
	if _, ok := sysinfo.CurrentThreadAffinity(); !ok {
		t.Skip("platform cannot report thread affinity")
	}
	if runtime.NumCPU() < 4 {
		t.Skipf("test pins cores 2,3; machine has %d CPUs", runtime.NumCPU())
	}

	var mu sync.Mutex
	seen := map[uint64]int{}
	workload.Register(&workload.Workload{
		Name:        "test-pin-probe",
		Description: "records the affinity mask of the thread running it (test double)",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			if mask, ok := sysinfo.CurrentThreadAffinity(); ok {
				mu.Lock()
				seen[mask]++
				mu.Unlock()
			}
			var iters uint64
			for ctx.Err() == nil {
				iters++
			}
			return iters, nil
		},
	})
	t.Cleanup(func() { workload.Delete("test-pin-probe") })

	opt := plan.Options{
		TestName: "general",
		A:        "test-pin-probe",
		B:        "test-pin-probe",
		Cores:    []int{2, 3},
		Workers:  2,
		// Generous on purpose: the full suite runs packages in
		// parallel, and on a saturated machine a 300ms window can
		// end before every worker thread has recorded its mask -
		// the probe records affinity once at entry, so each worker
		// needs to actually start. The assertion is about WHERE
		// workers ran, not about how fast the machine is.
		Duration:    1 * time.Second,
		StopOnFault: true,
		LogFile:     filepath.Join(t.TempDir(), "r.log"),
	}

	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("ExecuteWithResult() exit = %d, want 0", res.Exit)
	}
	want := map[uint64]int{1 << 2: 2, 1 << 3: 2} // two workers per slot, each on its core
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("worker thread masks = %v, want %v (every worker pinned to its slot's CPU)", seen, want)
	}
}
