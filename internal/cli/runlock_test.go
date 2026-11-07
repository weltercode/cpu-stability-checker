package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/lockfile"
)

// B08 (audit): run ownership is the OS lock, not a pid heuristic in
// a file. The kernel decides liveness through the held handle; the
// file is persistent and its contents are diagnostics only.

// useScratchRunLock points the run lock at a private path for one
// test and restores the package default afterwards.
func useScratchRunLock(t *testing.T) {
	t.Helper()
	prev := runLockPath
	runLockPath = filepath.Join(t.TempDir(), "run.lock")
	t.Cleanup(func() { runLockPath = prev })
}

// A live holder in ANOTHER process is refused by name; when that
// process dies (killed mid-hold here), the kernel releases the lock
// and the next acquire takes over - no stale-file heuristics, no
// deletion, no race window between "check stale" and "delete".
func TestRunLockCrossProcessHolderAndCrash(t *testing.T) {
	useScratchRunLock(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldRunLock", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "CPU_STABILITY_TEST_HOLD_LOCK="+runLockPath)
	outR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	first, rerr := bufio.NewReader(outR).ReadString('\n')
	if rerr != nil || strings.TrimSpace(first) != "locked" {
		t.Fatalf("helper never reported the lock held: %q %v", first, rerr)
	}

	if _, err := acquireRunLock(); err == nil || !strings.Contains(err.Error(), "another run session is active") {
		t.Errorf("live cross-process holder not refused: %v", err)
	}

	// Crash the holder: the kernel gives up its lock with the
	// process. The file stays; the lock is free.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	release, err := acquireRunLock()
	if err != nil {
		t.Fatalf("crashed holder still holds the lock: %v", err)
	}
	if _, err := os.Stat(runLockPath); err != nil {
		t.Errorf("the lock file vanished: it is persistent, only the lock is released: %v", err)
	}
	release()
}

// TestHelperHoldRunLock is the child half of the cross-process
// tests: it takes the lock named by the env var, reports "locked"
// on stdout, and holds until killed.
func TestHelperHoldRunLock(t *testing.T) {
	path := os.Getenv("CPU_STABILITY_TEST_HOLD_LOCK")
	if path == "" {
		t.Skip("lock helper: runs only as a child process")
	}
	l, err := lockfile.Acquire(path, fmt.Sprintf("pid=%d\n", os.Getpid()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper acquire: %v\n", err)
		os.Exit(3)
	}
	_ = l // held: the handle keeps the OS lock; it goes only with this process
	fmt.Println("locked")
	select {} // hold until the parent kills us
}

// Metadata is diagnostic: garbage inside the lock file never
// invalidates a free lock, and never proves or disproves liveness.
func TestRunLockGarbageMetadataStillAcquirable(t *testing.T) {
	useScratchRunLock(t)
	if err := os.WriteFile(runLockPath, []byte("total garbage, no pid, no shape"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireRunLock()
	if err != nil {
		t.Errorf("garbage metadata blocked a free lock: %v", err)
		return
	}
	release()
}

// The shared lock is re-entrant per operation: inner joins never
// self-refuse, and the OS lock goes out only with the outermost
// leave - never with an inner one, never with a double release.
func TestRunLockSharedIsReentrant(t *testing.T) {
	useScratchRunLock(t)
	outer, err := acquireRunLockShared()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := acquireRunLockShared()
	if err != nil {
		t.Errorf("nested join self-refused: %v", err)
		return
	}
	inner()
	if _, err := acquireRunLock(); err == nil {
		t.Error("the OS lock went out with the INNER join, not the outermost")
	}
	outer()
	release, err := acquireRunLock()
	if err != nil {
		t.Errorf("OS lock still held after the outermost left: %v", err)
		return
	}
	release()
	outer() // double release past zero: silent no-op, no panic
}

// The default lock area is absolute (per-user temp), so a second
// run from any other directory contends with the first (audit B08:
// a cwd lock guarded nobody who chose another cwd). If a real run
// session is live on this machine right now, the honest response is
// to skip, not to fight it.
func TestRunLockDefaultAreaIsCwdIndependent(t *testing.T) {
	release, err := acquireRunLock()
	if err != nil {
		t.Skipf("a live run session holds the default lock right now: %v", err)
	}
	defer release()
	if !filepath.IsAbs(runLockPath) {
		t.Errorf("the default lock area is cwd-relative (%q): a second directory escapes it", runLockPath)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldwd)
	for _, dir := range []string{t.TempDir(), t.TempDir()} {
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := acquireRunLock(); err == nil || !strings.Contains(err.Error(), "another run session is active") {
			t.Errorf("second acquire from %s did not contend: %v", dir, err)
		}
	}
}
