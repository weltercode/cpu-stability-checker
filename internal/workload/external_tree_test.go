//go:build windows

package workload

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireTreeTools marks the test's dependency level (audit A07): these
// tests exist to verify behavior against REAL console tools and a real
// grandchild process, so they cannot be made hermetic without losing
// the point. In a restricted environment they skip naming the missing
// capability; on a machine where the tools exist, a broken tree stop
// must still fail the test. The multi-second observation windows also
// put them outside the fast -short gate.
func requireTreeTools(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("test level 2 (Windows integration): real cmd+ping process tree with multi-second observation windows; run without -short")
	}
	for _, tool := range []string{"cmd", "ping"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("test level 2 (Windows integration): %s is not available: %v", tool, err)
		}
	}
}

// treeBatch writes a batch file whose process spawns a grandchild that,
// after roughly delaySeconds, creates marker in the working directory. The
// parent itself runs far longer, so the marker only disappears if stopping
// the parent also killed the whole tree.
func treeBatch(t *testing.T, dir, marker string, delaySeconds int) string {
	t.Helper()
	batch := filepath.Join(dir, "parent.cmd")
	body := "@echo off\r\n" +
		`start /b cmd /c "ping -n ` + strconv.Itoa(delaySeconds+1) + ` 127.0.0.1 >nul & echo alive > ` + marker + `"` + "\r\n" +
		"ping -n 30 127.0.0.1 >nul\r\n"
	if err := os.WriteFile(batch, []byte(body), 0o644); err != nil {
		t.Fatalf("write batch: %v", err)
	}
	return batch
}

func TestExternalTreeKilledWithProcess(t *testing.T) {
	requireTreeTools(t)
	dir := t.TempDir()
	spec := ExternalSpec{
		Command: "cmd",
		Args:    []string{"/c", treeBatch(t, dir, "survived.txt", 2)},
		Dir:     dir,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := spec.run(ctx, &recordingReporter{}); err != nil {
		t.Fatalf("run() error = %v, want nil for our own stop", err)
	}

	// The grandchild would write its marker ~3s after starting; by now the
	// whole tree must already be dead.
	time.Sleep(4500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived.txt")); !os.IsNotExist(err) {
		t.Fatal("descendant survived the stop, want the whole process tree killed")
	}
}

// TestExternalTreeMarkerAppearsWhenNotKilledEarly is the control for the
// test above: it proves the batch really spawns a grandchild that writes
// the marker, so the negative test cannot pass vacuously.
func TestExternalTreeMarkerAppearsWhenNotKilledEarly(t *testing.T) {
	requireTreeTools(t)
	dir := t.TempDir()
	spec := ExternalSpec{
		Command: "cmd",
		Args:    []string{"/c", treeBatch(t, dir, "survived.txt", 1)},
		Dir:     dir,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := spec.run(ctx, &recordingReporter{}); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "survived.txt")); err != nil {
		t.Fatalf("descendant never wrote its marker: %v", err)
	}
}

// TestLevelMarkingIsDistinguishableByCommandLine is the A07 proof that
// the level marking is not decorative: re-running this very binary with
// the documented name pattern and -test.short must SKIP exactly the
// TestExternalTree* integration pair, naming their level, while pulling
// no other test into the run. It re-execs the test binary, so it needs
// no toolchain and costs only the skip handshake.
func TestLevelMarkingIsDistinguishableByCommandLine(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestExternalTree", "-test.short", "-test.v", "-test.timeout=60s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("short-mode child must end clean with skips, got %v:\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"--- SKIP: TestExternalTreeKilledWithProcess",
		"--- SKIP: TestExternalTreeMarkerAppearsWhenNotKilledEarly",
		"test level 2",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("short-mode output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "=== RUN   TestExternalSingleInstance") {
		t.Fatalf("the level name pattern pulled non-integration tests into the run:\n%s", text)
	}
}
