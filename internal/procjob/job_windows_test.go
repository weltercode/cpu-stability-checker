//go:build windows

package procjob

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var procGetProcessTimes = kernel32.NewProc("GetProcessTimes")

const processQueryLimitedInformation = 0x1000

// cpuTime sums kernel+user time of a process, the observable proof that a
// suspended process really stopped executing.
func cpuTime(t *testing.T, pid int) time.Duration {
	t.Helper()
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		t.Fatalf("OpenProcess(pid %d): %v", pid, err)
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	var creation, exit, kernel, user int64
	r, _, err := procGetProcessTimes.Call(h,
		uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		t.Fatalf("GetProcessTimes(pid %d): %v", pid, err)
	}
	return time.Duration(kernel+user) * 100 * time.Nanosecond // FILETIME is 100ns ticks
}

// TestHelperProcess is not a real test: re-executed with CPU_TEST_BUSY it
// burns one core for a minute, giving the suspend/resume checks a child
// whose CPU time is unmistakable (cmd.exe loops are far too slow to be a
// reliable baseline).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CPU_TEST_BUSY") != "1" {
		return
	}
	x := uint64(0)
	for i := 0; i < 60_000_000_000; i++ {
		x += uint64(i)
	}
	os.Exit(int(x & 1))
}

func busyChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "CPU_TEST_BUSY=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start busy helper: %v", err)
	}
	return cmd
}

// waitBusy blocks until the process has accumulated CPU fast enough to
// prove its loop is running, so slow process startup cannot skew the
// baseline measurement that follows.
func waitBusy(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	prev := cpuTime(t, pid)
	for {
		time.Sleep(200 * time.Millisecond)
		now := cpuTime(t, pid)
		if now-prev > 50*time.Millisecond {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("busy helper never got busy (cpu time %v in 10s)", now-prev)
		}
		prev = now
	}
}

func TestSuspendResumeJob(t *testing.T) {
	job, err := New()
	if err != nil {
		t.Skipf("job objects unavailable: %v", err)
	}

	cmd := busyChild(t)
	defer func() {
		job.Close()
		cmd.Wait()
	}()
	if err := job.Attach(cmd.Process); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Wait until the child is demonstrably burning CPU: process startup
	// (AV scans included) must not be mistaken for a dead baseline.
	waitBusy(t, cmd.Process.Pid)

	before := cpuTime(t, cmd.Process.Pid)
	time.Sleep(400 * time.Millisecond)
	if ran := cpuTime(t, cmd.Process.Pid) - before; ran < 100*time.Millisecond {
		t.Fatalf("baseline CPU = %v, want the loop to be visibly busy", ran)
	}

	if err := job.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the freeze settle
	before = cpuTime(t, cmd.Process.Pid)
	time.Sleep(600 * time.Millisecond)
	if ran := cpuTime(t, cmd.Process.Pid) - before; ran > 50*time.Millisecond {
		t.Fatalf("CPU while suspended = %v, want the process frozen", ran)
	}

	if err := job.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	before = cpuTime(t, cmd.Process.Pid)
	time.Sleep(400 * time.Millisecond)
	if ran := cpuTime(t, cmd.Process.Pid) - before; ran < 100*time.Millisecond {
		t.Fatalf("CPU after resume = %v, want the loop running again", ran)
	}
}

func TestCloseKillsJobMember(t *testing.T) {
	job, err := New()
	if err != nil {
		t.Skipf("job objects unavailable: %v", err)
	}

	// The busy helper (this binary, re-executed) replaces cmd+ping as
	// the attachable member (audit A07): kill semantics do not care
	// which process image is in the job, and the busy loop keeps the
	// test free of console tools and the loopback stack.
	cmd := busyChild(t)
	if err := job.Attach(cmd.Process); err != nil {
		cmd.Process.Kill()
		t.Fatalf("attach: %v", err)
	}
	if err := job.Close(); err != nil {
		cmd.Process.Kill()
		t.Fatalf("close: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
		// the busy loop would run for about a minute: returning this fast
		// means the job killed it. (Job termination reports exit code 0,
		// so the Wait error cannot be the signal.)
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process survived job close, want it terminated")
	}
}
