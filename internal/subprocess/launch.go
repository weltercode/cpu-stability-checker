package subprocess

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/procjob"
)

const workerChildEnv = "CPU_TEST_WORKER_CHILD"

// workerExecutable is shared by both launch paths. A Go test executable does
// not dispatch our "worker" subcommand: it runs its tests again, potentially
// spawning a new generation on every CPU. Refuse before opening pipes or
// starting any process unless the tests explicitly installed a helper entry.
// Detect the test harness by its registered flag, not the executable's name
// (go test -c -o can rename it).
func workerExecutable() (string, error) {
	if os.Getenv(workerChildEnv) == "1" {
		return "", fmt.Errorf("worker launch refused: a worker child cannot launch more CPU workers")
	}
	if flag.Lookup("test.v") != nil && len(workerArgsPrefix) == 0 {
		return "", fmt.Errorf("worker launch refused: Go test binary needs an explicit worker helper; launching it as 'worker' would recursively run the test suite")
	}
	exe, err := selfExecutable()
	if err != nil {
		return "", fmt.Errorf("cannot find worker executable: %w", err)
	}
	return exe, nil
}

// startWorker gives the parent ownership of the worker's process tree on
// Windows. External workloads have their own nested jobs; those alone cannot
// clean up a CPU worker when its parent test harness is killed.
// The caller must close the returned job after waiting for the child.
func startWorker(cmd *exec.Cmd) (*procjob.Job, error) {
	job, err := procjob.New()
	if err != nil {
		return nil, fmt.Errorf("worker job: %w", err)
	}
	// A descendant retaining stderr must not keep Cmd.Wait blocked forever.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		job.Close()
		return nil, err
	}
	if err := job.Attach(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = job.Close()
		_ = cmd.Wait()
		return nil, fmt.Errorf("attach worker to parent job: %w", err)
	}
	return job, nil
}
