package subprocess

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/procjob"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

func TestWorkerLaunchRefusesUnsafeReentry(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%v", child), func(t *testing.T) {
			prevExe, prevPrefix := selfExecutable, workerArgsPrefix
			t.Cleanup(func() { selfExecutable, workerArgsPrefix = prevExe, prevPrefix })
			workerArgsPrefix = nil
			t.Setenv(workerChildEnv, "")
			want := "Go test binary needs an explicit worker helper"
			if child {
				t.Setenv(workerChildEnv, "1")
				// Even a correctly configured helper may not become a parent.
				workerArgsPrefix = []string{"-test.run=TestWorkerHelperProcess", "--"}
				want = "a worker child cannot launch more CPU workers"
			}
			resolved := 0
			selfExecutable = func() (string, error) {
				resolved++
				// Fail closed even if the regression returns: this test must
				// never reproduce the process avalanche it is checking for.
				return "", fmt.Errorf("unexpected executable lookup")
			}
			opt := plan.Options{TestName: "general", A: "int", Cores: []int{0},
				Workers: 1, Duration: time.Millisecond, ControlCore: -1,
				LogFile: filepath.Join(t.TempDir(), "run.log"), Bursts: 1,
				BurstRun: time.Millisecond}
			res := ExecuteSubprocessWithResult(opt)
			if res.Exit != 2 || !strings.Contains(res.AbortMsg, want) {
				t.Fatalf("one-shot refusal: %+v, want %q", res, want)
			}
			res = ExecuteSessionResult(opt, nil)
			if res.Exit != 2 || !strings.Contains(res.AbortMsg, want) {
				t.Fatalf("transition refusal: %+v, want %q", res, want)
			}
			h, err := StartSession([]string{"worker", "--session"}, "blocked", transition.DefaultTimeouts())
			if h != nil || err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("direct session refusal: handle=%v err=%v, want %q", h, err, want)
			}
			if resolved != 0 {
				t.Fatalf("unsafe launch reached executable resolution %d times", resolved)
			}
			if _, err := os.Stat(journal.Path(opt.LogFile)); !os.IsNotExist(err) {
				t.Fatalf("refused launch touched journal: %v", err)
			}
		})
	}
}

func TestWorkerLaunchAllowsExplicitHelper(t *testing.T) {
	useTestBinaryAsWorker(t)
	t.Setenv(workerChildEnv, "")
	exe, err := workerExecutable()
	if err != nil || exe != os.Args[0] {
		t.Fatalf("configured helper: exe=%q err=%v", exe, err)
	}
}

func TestWorkerStartFailureReleasesJob(t *testing.T) {
	before := len(procjob.LiveJobs())
	cmd := exec.Command(filepath.Join(t.TempDir(), "missing-worker"))
	job, err := startWorker(cmd)
	if err == nil || job != nil || cmd.Process != nil {
		t.Fatalf("missing executable: job=%v process=%v err=%v", job, cmd.Process, err)
	}
	if after := len(procjob.LiveJobs()); after != before {
		t.Fatalf("failed start leaked job: before=%d after=%d", before, after)
	}
}
