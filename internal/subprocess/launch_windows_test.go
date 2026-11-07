//go:build windows

package subprocess

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// Only this explicitly selected helper creates a child, always exactly one,
// with no load or RAM buffer. The outer test kills this owner after attachment.
func TestWorkerOwnerHelper(t *testing.T) {
	mode := os.Getenv("CPU_TEST_OWNER_HELPER")
	if mode == "" {
		return
	}
	useTestBinaryAsWorker(t)
	jp := filepath.Join(os.Getenv("CPU_TEST_OWNER_DIR"), "child.jsonl")
	var pid int
	if mode == "session" {
		h, err := StartSession(sessionArgv(jp, "owner", "--workload", "int"), "owner", transition.DefaultTimeouts())
		if err != nil {
			t.Fatal(err)
		}
		defer h.Reap(time.Second)
		pid = h.Cmd.Process.Pid
	} else {
		exe, err := workerExecutable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, workerArgs(0, "idle", time.Minute, 0, time.Second, jp, "owner", 1, nil)...)
		cmd.Env = append(os.Environ(), workerChildEnv+"=1")
		job, err := startWorker(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { job.Close(); cmd.Wait() }()
		pid = cmd.Process.Pid
	}
	fmt.Println(pid)
	time.Sleep(30 * time.Second)
}

func TestWorkerDiesWithOwner(t *testing.T) {
	for _, mode := range []string{"one-shot", "session"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			owner := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerOwnerHelper$")
			owner.Env = append(os.Environ(), "CPU_TEST_OWNER_HELPER="+mode, "CPU_TEST_OWNER_DIR="+t.TempDir())
			owner.Stderr = os.Stderr
			out, err := owner.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { owner.Process.Kill(); owner.Wait() })
			scan := bufio.NewScanner(out)
			if !scan.Scan() {
				t.Fatalf("owner did not publish a child PID: %v", scan.Err())
			}
			pid, err := strconv.Atoi(scan.Text())
			if err != nil || pid <= 0 {
				t.Fatalf("invalid child PID: %q", scan.Text())
			}
			// Keep the process handle, not only its PID: no PID-reuse race.
			handle, err := syscall.OpenProcess(0x00100000|0x0001, false, uint32(pid)) // SYNCHRONIZE | TERMINATE
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.TerminateProcess(handle, 2); syscall.CloseHandle(handle) })
			if state, err := syscall.WaitForSingleObject(handle, 0); err != nil || state != syscall.WAIT_TIMEOUT {
				t.Fatalf("child was not alive before owner death: state=%d err=%v", state, err)
			}
			if err := owner.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if state, err := syscall.WaitForSingleObject(handle, 5000); err != nil || state != syscall.WAIT_OBJECT_0 {
				t.Fatalf("child survived owner death: state=%d err=%v", state, err)
			}
		})
	}
}
