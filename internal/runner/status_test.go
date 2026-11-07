package runner

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestNewRunStatus(t *testing.T) {
	rs := newRunStatus(3, 5*time.Second)

	if len(rs.cores) != 3 {
		t.Fatalf("newRunStatus(3) cores = %d, want 3", len(rs.cores))
	}
	for i, c := range rs.cores {
		if c.core != i {
			t.Fatalf("newRunStatus(3) core[%d].core = %d, want %d", i, c.core, i)
		}
		if c.itersNow() != 0 {
			t.Fatalf("newRunStatus(3) core[%d] starts with %d iterations, want 0", i, c.itersNow())
		}
	}
	if rs.duration != 5*time.Second {
		t.Fatalf("newRunStatus(3) duration = %v, want 5s", rs.duration)
	}
	if aborted, msg := rs.abortInfo(); aborted || msg != "" {
		t.Fatalf("new run abortInfo() = (%v, %q), want (false, \"\")", aborted, msg)
	}
}

func TestRecordFault(t *testing.T) {
	t.Run("first fault wins and aborts the run", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		rs.recordFault(1, errors.New("mismatch at iter 42"))

		aborted, msg := rs.abortInfo()
		if !aborted {
			t.Fatal("recordFault() did not abort the run")
		}
		want := "fault on core 1: mismatch at iter 42"
		if msg != want {
			t.Fatalf("recordFault() abort message = %q, want %q", msg, want)
		}
		got, ok := rs.firstFault(1)
		if !ok || got != "mismatch at iter 42" {
			t.Fatalf("firstFault(1) = (%q, %v), want the recorded message", got, ok)
		}
	})

	t.Run("only the first fault per core is kept", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		rs.recordFault(0, errors.New("first"))
		rs.recordFault(0, errors.New("second"))

		got, ok := rs.firstFault(0)
		if !ok || got != "first" {
			t.Fatalf("firstFault(0) = (%q, %v), want (\"first\", true)", got, ok)
		}
	})

	t.Run("first abort message wins over later faults", func(t *testing.T) {
		rs := newRunStatus(3, time.Second)

		rs.recordFault(0, errors.New("boom on 0"))
		rs.recordFault(1, errors.New("boom on 1"))

		_, msg := rs.abortInfo()
		if msg != "fault on core 0: boom on 0" {
			t.Fatalf("abort message = %q, want the first fault message", msg)
		}
		// every fault is still recorded per core
		for _, core := range []int{0, 1} {
			if _, ok := rs.firstFault(core); !ok {
				t.Fatalf("firstFault(%d) missing, want the recorded fault", core)
			}
		}
	})

	t.Run("unknown core has no fault", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		if msg, ok := rs.firstFault(7); ok || msg != "" {
			t.Fatalf("firstFault(7) = (%q, %v), want (\"\", false)", msg, ok)
		}
	})
}

func TestRequestAbort(t *testing.T) {
	t.Run("sets abort once and never overwrites", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		rs.requestAbort(plan.ExecCancelled, "user interrupt")
		rs.requestAbort(plan.ExecCancelled, "another interrupt")

		aborted, msg := rs.abortInfo()
		if !aborted {
			t.Fatal("requestAbort() did not abort the run")
		}
		if msg != "user interrupt" {
			t.Fatalf("abort message = %q, want the first message", msg)
		}
	})

	t.Run("a fault keeps its message when abort comes later", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		rs.recordFault(1, errors.New("mismatch"))
		rs.requestAbort(plan.ExecCancelled, "user interrupt")

		_, msg := rs.abortInfo()
		if want := "fault on core 1: mismatch"; msg != want {
			t.Fatalf("abort message = %q, want %q", msg, want)
		}
	})

	t.Run("a fault does not overwrite a user abort", func(t *testing.T) {
		rs := newRunStatus(2, time.Second)

		rs.requestAbort(plan.ExecCancelled, "user interrupt")
		rs.recordFault(1, errors.New("mismatch"))

		_, msg := rs.abortInfo()
		if msg != "user interrupt" {
			t.Fatalf("abort message = %q, want the user abort message", msg)
		}
		// the fault itself is still recorded
		if _, ok := rs.firstFault(1); !ok {
			t.Fatal("firstFault(1) missing, want the recorded fault")
		}
	})
}

func TestCoreStatusCounters(t *testing.T) {
	c := &coreStatus{core: 0}

	c.Add(5)
	c.Add(7)
	if got := c.itersNow(); got != 12 {
		t.Fatalf("itersNow() = %d, want 12", got)
	}

	if c.lastBeatNano() != 0 {
		t.Fatalf("lastBeatNano() before Beat() = %d, want 0", c.lastBeatNano())
	}
	c.Beat()
	if got := c.lastBeatNano(); got <= 0 {
		t.Fatalf("lastBeatNano() after Beat() = %d, want a positive timestamp", got)
	}
}

func TestRunStatusConcurrentAccess(t *testing.T) {
	const (
		cores  = 4
		gorout = 8
		adds   = 100
	)
	rs := newRunStatus(cores, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < gorout; i++ {
		wg.Add(1)
		go func(core int) {
			defer wg.Done()
			for n := 0; n < adds; n++ {
				rs.cores[core].Add(1)
				rs.cores[core].Beat()
				rs.recordFault(core, fmt.Errorf("mismatch %d", n))
			}
		}(i % cores)
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs.requestAbort(plan.ExecCancelled, "user interrupt")
		}()
	}
	wg.Wait()

	for i, c := range rs.cores {
		want := uint64(gorout / cores * adds)
		if got := c.itersNow(); got != want {
			t.Fatalf("core %d iterations = %d, want %d", i, got, want)
		}
	}

	aborted, msg := rs.abortInfo()
	if !aborted {
		t.Fatal("run was not aborted, want aborted")
	}
	if msg == "" {
		t.Fatal("abort message is empty, want exactly one recorded cause")
	}
	if !strings.HasPrefix(msg, "fault on core ") && msg != "user interrupt" {
		t.Fatalf("abort message = %q, want a fault message or the user abort", msg)
	}
	for core := 0; core < cores; core++ {
		if _, ok := rs.firstFault(core); !ok {
			t.Fatalf("firstFault(%d) missing, want one fault per core", core)
		}
	}
}
