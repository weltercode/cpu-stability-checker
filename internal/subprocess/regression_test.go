package subprocess

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func TestSubprocessMissingToolIsSetupError(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	cfg := config.Default()
	cfg.External = map[string]workload.ExternalSpec{"review-missing": {Command: filepath.Join(dir, "missing-tool.exe")}}
	if err := cfg.Save(config.DefaultPath); err != nil {
		t.Fatal(err)
	}
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "review-missing", Workers: 2,
		Duration: 5 * time.Second, StopOnFault: false, ControlCore: -1, LogFile: filepath.Join(dir, "run.log")}
	start := time.Now()
	res := ExecuteSubprocessWithResult(opt)
	if res.Exit != 2 || len(res.Faults) != 0 {
		t.Fatalf("missing tool blacklisted CPU: %+v", res)
	}
	if time.Since(start) >= opt.Duration {
		t.Fatal("setup failure waited out full duration")
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.SetupError || rep.Exit != 2 {
		t.Fatalf("setup result lost: %+v", rep)
	}
}

func TestParentCancellationPreservesCompletedAndFaultedCores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	f, err := journal.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := journal.NewWriter(f, "current")
	defer jw.Close()
	jw.RunStart("general", "", []int{0, 1, 2}, 1, "1h", true)
	for core := range 3 {
		jw.CoreStart(core, "int")
	}
	jw.CoreDoneElapsed(0, journal.OK, "", time.Second)
	jw.CoreDoneElapsed(1, journal.Fault, "mismatch", time.Millisecond)
	// Same recovery the parent engine performs after its own cancel:
	// scan this run's endings, mark unfinished cores interrupted.
	outs, _ := scanCoreOutcomes(path, "current")
	for core, o := range outs {
		if o.status == journal.Crashed {
			jw.CoreDone(core, journal.Interrupted, "worker stopped by parent")
		}
	}
	jw.RunEnd(1)
	rep := readRecoveredRun(t, path)
	if rep.Cores[0].Status != journal.OK || rep.Cores[1].Status != journal.Fault || rep.Cores[2].Status != journal.Interrupted {
		t.Fatalf("cancellation overwrote actual outcomes: %+v", rep.Cores)
	}
	sum := journal.SummarizeCores([]journal.RunReport{rep})
	if sum.Cores[2].Exposure != 0 || sum.Cores[2].Interrupted != 1 {
		t.Fatalf("unmeasured killed child received planned exposure: %+v", sum.Cores[2])
	}
	// The journal remains writable until the parent's ordinary run_end/close.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
