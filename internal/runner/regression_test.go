package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func readRun(t *testing.T, logPath string) journal.RunReport {
	t.Helper()
	f, err := os.Open(journal.Path(logPath))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reports := journal.RecoverRuns(journal.Parse(f))
	if len(reports) != 1 {
		t.Fatalf("got %d runs, want 1", len(reports))
	}
	return reports[0]
}

func preserveRunnerState(t *testing.T) {
	t.Helper()
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})
}

func TestExecutionTelemetryUsesTheSameSlotsAsLifecycle(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skip("requires CPUs 2 and 3")
	}
	preserveRunnerState(t)
	workload.Register(&workload.Workload{Name: "review-wait", Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
		rep.Add(1)
		rep.Beat()
		<-ctx.Done()
		return 1, nil
	}})
	t.Cleanup(func() { workload.Delete("review-wait") })
	for _, cores := range [][]int{{2, 3}, {1, 0}} {
		opt := plan.Options{TestName: "general", Cores: cores, A: "review-wait", Workers: 1,
			// 600ms, not 100ms: under a full-suite load a worker can miss a
			// 100ms window, and a mandatory slot that never started exits 1
			// honestly (incomplete mandatory check), breaking this telemetry test.
			Duration: 600 * time.Millisecond, ControlCore: -1, LogFile: filepath.Join(t.TempDir(), "run.log")}
		if res := ExecuteWithResult(opt); res.Exit != 0 {
			t.Fatalf("execute: %+v", res)
		}
		rep := readRun(t, opt.LogFile)
		if len(rep.Cores) != 2 {
			t.Fatalf("telemetry fabricated CPU outcomes: %+v", rep.Cores)
		}
		sum := journal.SummarizeCores([]journal.RunReport{rep})
		if len(sum.Cores) != 2 {
			t.Fatalf("wrong CPU mapping: %+v", sum)
		}
		for i, c := range rep.Cores {
			if c.Core != i || c.Status != journal.OK || c.LoadTime == nil {
				t.Fatalf("slot outcome: %+v", c)
			}
			// These OS sources exist on the supported Windows and Linux platforms.
			if (runtime.GOOS == "windows" || runtime.GOOS == "linux") && len(c.Telemetry) != 1 {
				t.Fatalf("slot %d telemetry: %+v", i, c.Telemetry)
			}
		}
		for _, c := range sum.Cores {
			if c.Runs != 1 || c.Exposure <= 0 || c.Exposure > opt.Duration {
				t.Fatalf("inflated exposure/runs: %+v", c)
			}
		}
	}
}

func TestExecutionSetupFailureIsNotAHardwareFault(t *testing.T) {
	preserveRunnerState(t)
	for _, piped := range []bool{false, true} {
		spec := workload.ExternalSpec{Command: filepath.Join(t.TempDir(), "missing-tool.exe")}
		if piped {
			spec.Beat = "stdout"
		}
		workload.RegisterExternal(map[string]workload.ExternalSpec{"review-missing": spec})
		t.Cleanup(func() { workload.Delete("review-missing") })
		opt := plan.Options{TestName: "general", Cores: []int{0}, A: "review-missing", Workers: 2,
			Duration: 5 * time.Second, StopOnFault: false, ControlCore: -1, LogFile: filepath.Join(t.TempDir(), "run.log")}
		start := time.Now()
		res := ExecuteWithResult(opt)
		if res.Exit != 2 || !res.Aborted || len(res.Faults) != 0 {
			t.Fatalf("setup failure classified as CPU fault: %+v", res)
		}
		if time.Since(start) >= opt.Duration {
			t.Fatal("setup failure did not stop the run")
		}
		rep := readRun(t, opt.LogFile)
		if rep.Exit != 2 || rep.Cores[0].Status != journal.SetupError {
			t.Fatalf("wrong journal outcome: %+v", rep)
		}
		sum := journal.SummarizeCores([]journal.RunReport{rep})
		if sum.Cores[0].Faults != 0 || sum.Cores[0].SetupErrors != 1 || sum.Cores[0].Exposure != 0 {
			t.Fatalf("setup failure credited load/faults: %+v", sum)
		}
	}
}

func TestInterruptedPeerIsNotSuccessfulAndReceivesActualExposure(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("requires two CPUs")
	}
	preserveRunnerState(t)
	workload.Register(&workload.Workload{Name: "review-delayed-fault", Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
		time.Sleep(20 * time.Millisecond)
		return 0, errors.New("calculation mismatch")
	}})
	workload.Register(&workload.Workload{Name: "review-peer", Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
		rep.Add(1)
		rep.Beat()
		<-ctx.Done()
		return 1, nil
	}})
	t.Cleanup(func() { workload.Delete("review-delayed-fault"); workload.Delete("review-peer") })
	opt := plan.Options{TestName: "abab", Cores: []int{0, 1}, A: "review-delayed-fault", B: "review-peer", Workers: 1,
		Duration: 5 * time.Second, StopOnFault: true, ControlCore: -1, LogFile: filepath.Join(t.TempDir(), "run.log")}
	res := ExecuteWithResult(opt)
	if res.Exit != 1 || len(res.Faults) != 1 {
		t.Fatalf("fault result: %+v", res)
	}
	rep := readRun(t, opt.LogFile)
	if rep.Cores[0].Status != journal.Fault || rep.Cores[1].Status != journal.Interrupted {
		t.Fatalf("peer falsely passed: %+v", rep.Cores)
	}
	for _, c := range rep.Cores {
		if c.LoadTime == nil || *c.LoadTime <= 0 || *c.LoadTime >= opt.Duration {
			t.Fatalf("wrong actual duration: %+v", c)
		}
	}
	sum := journal.SummarizeCores([]journal.RunReport{rep})
	if sum.Cores[1].Interrupted != 1 || sum.Cores[1].Exposure >= opt.Duration {
		t.Fatalf("partial exposure lost: %+v", sum)
	}
}

func TestConcurrentWorkersDoNotMultiplyExposure(t *testing.T) {
	c := &coreStatus{}
	base := time.Now()
	// All workers on a slot start together; their overlapping windows
	// must not be added as if the CPU had worked for three times as long.
	c.recordLoad(base, base.Add(10*time.Millisecond))
	c.recordLoad(base.Add(time.Millisecond), base.Add(20*time.Millisecond))
	c.recordLoad(base.Add(2*time.Millisecond), base.Add(15*time.Millisecond))
	if got := c.loadDuration(); got != 20*time.Millisecond {
		t.Fatalf("overlapping worker windows credited %s, want 20ms", got)
	}
}
