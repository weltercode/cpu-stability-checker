package ryzensmucli

// B01/B02 (audit): these tests drive the REAL production execRunner
// against the test binary itself acting as a scripted tool - a fake
// Runner proves nothing about Start/Wait/pipe wiring. The helper is
// re-invoked only under the exact -test.run anchor plus the
// CO_HELPER_MODE marker env, never as a recursive full-binary run.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// TestHelperToolProcess is the scripted "tool": it acts ONLY when
// CO_HELPER_MODE names a mode (set by the parent test via t.Setenv,
// inherited by the child). Under a normal test run it skips, so the
// binary never re-runs the suite through this file.
func TestHelperToolProcess(t *testing.T) {
	mode := os.Getenv("CO_HELPER_MODE")
	if mode == "" {
		t.Skip("CO_HELPER_MODE not set: helper process anchor, not a real test")
	}
	switch mode {
	case "ok":
		fmt.Print("Current PBO offsets:\n0,0\n")
		os.Exit(0)
	case "exit7":
		fmt.Print("no PBO offset register on this SKU\n")
		os.Exit(7)
	case "exact": // exactly CO_HELPER_BYTES bytes, then exit 0
		n := envInt(t, "CO_HELPER_BYTES")
		os.Stdout.Write(bytesOf(n)) // os.Stdout writes are unbuffered syscalls
		os.Exit(0)
	case "over": // CO_HELPER_BYTES+1 bytes: one past the limit
		n := envInt(t, "CO_HELPER_BYTES") + 1
		os.Stdout.Write(bytesOf(n))
		os.Exit(0)
	case "infinite": // never exits, never stops printing
		for {
			os.Stdout.Write(bytesOf(4096))
		}
	case "silent": // never writes, never exits (sleep, not select{}:
		time.Sleep(time.Minute) // the deadlock detector would fake an "instant child"
		os.Exit(5)
	case "stderr":
		fmt.Fprintln(os.Stderr, "diagnostic noise on stderr")
		os.Exit(3)
	case "grandchild":
		// Spawn a grandchild that inherits OUR stdout (the parent's
		// pipe write end) and then sits silent; the helper itself
		// exits 0. The parent must still finish bounded and must
		// leave no process behind (job object kill-on-close).
		gc := exec.Command(os.Args[0], "-test.run=^TestHelperToolProcess$")
		gc.Env = append(os.Environ(), "CO_HELPER_MODE=holdpipe")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "grandchild start failed")
			os.Exit(9)
		}
		fmt.Printf("pid %d\n", gc.Process.Pid)
		os.Exit(0)
	case "holdpipe":
		time.Sleep(time.Minute) // holds the inherited pipe, silent
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown CO_HELPER_MODE %q\n", mode)
		os.Exit(8)
	}
}

func envInt(t *testing.T, key string) int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv(key), "%d", &n); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return n
}

// bytesOf builds n bytes of plausible offset text ("0,0,0..."), so
// size tests exercise realistic stream chunks, not one giant write.
func bytesOf(n int) []byte {
	b := make([]byte, 0, n)
	for len(b) < n {
		if len(b)+4 <= n {
			b = append(b, []byte("0,0,")...)
		} else {
			b = append(b, byte('0'))
		}
	}
	return b[:n]
}

// helperExe is the test binary itself: the production Runner execs
// it with the exact helper anchor. Args carry no marker - the marker
// travels in the inherited CO_HELPER_MODE env.
func helperExe() string { return os.Args[0] }

func helperArgs() []string { return []string{"-test.run=^TestHelperToolProcess$"} }

// B01 core regression: the old Start+Output path returned
// out="" exit=-1 err="exec: already started" for EVERY real call.
// The production runner must deliver stdout and the exit code.
func TestExecRunnerRealProcessStdoutAndExit(t *testing.T) {
	t.Setenv("CO_HELPER_MODE", "ok")
	r := execRunner{}
	out, code, err := r.Run(context.Background(), helperExe(), helperArgs()...)
	if err != nil {
		t.Fatalf("clean run errored: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "Current PBO offsets:\n0,0\n" {
		t.Errorf("stdout = %q, want the scripted answer", out)
	}

	t.Setenv("CO_HELPER_MODE", "exit7")
	out, code, err = r.Run(context.Background(), helperExe(), helperArgs()...)
	if err != nil {
		t.Fatalf("nonzero exit must not be a runner error: %v", err)
	}
	if code != 7 {
		t.Errorf("exit = %d, want 7 (the tool's own code)", code)
	}
	if !strings.Contains(out, "no PBO offset register") {
		t.Errorf("stdout = %q, want the tool's message", out)
	}
}

// Start failure: nothing was started, so no Wait is attempted and
// the error is the plain exec failure (exit -1).
func TestExecRunnerStartFailure(t *testing.T) {
	r := execRunner{}
	out, code, err := r.Run(context.Background(),
		helperExe()+"-definitely-not-here", helperArgs()...)
	if err == nil {
		t.Fatal("start of a missing exe must fail")
	}
	if code != -1 || out != "" {
		t.Errorf("out=%q exit=%d, want \"\" and -1", out, code)
	}
}

// Attach failure (B01: the old code killed WITHOUT waiting): the
// process must still go through its exactly-one Wait and the failure
// must surface, bounded.
type failingAttachJob struct{ closed int }

func (f *failingAttachJob) Attach(p *os.Process) error {
	return errors.New("attach refused by test seam")
}
func (f *failingAttachJob) Close() error { f.closed++; return nil }

func TestExecRunnerAttachFailureReaps(t *testing.T) {
	t.Setenv("CO_HELPER_MODE", "infinite") // would run forever if not killed
	prev := newJobObject
	newJobObject = func() (jobObject, error) { return &failingAttachJob{}, nil }
	t.Cleanup(func() { newJobObject = prev })

	start := time.Now()
	out, _, err := (execRunner{MaxOutput: 1 << 20}).Run(context.Background(), helperExe(), helperArgs()...)
	if err == nil || !strings.Contains(err.Error(), "job attach") {
		t.Fatalf("err = %v, want the attach failure surfaced", err)
	}
	if out != "" {
		t.Errorf("stdout = %q on an attach failure", out)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("attach failure took %v: the kill+Wait reap is not bounded", d)
	}
}

// B02: the cap is enforced DURING the read - exactly limit passes,
// limit+1 is refused as a hard error, never a truncation.
func TestExecRunnerOutputCapBoundary(t *testing.T) {
	const limit = 64
	t.Setenv("CO_HELPER_MODE", "exact")
	t.Setenv("CO_HELPER_BYTES", fmt.Sprint(limit))
	out, code, err := (execRunner{MaxOutput: limit}).Run(context.Background(), helperExe(), helperArgs()...)
	if err != nil || code != 0 || len(out) != limit {
		t.Fatalf("exactly-limit answer: out=%d exit=%d err=%v, want %d bytes accepted", len(out), code, err, limit)
	}

	t.Setenv("CO_HELPER_MODE", "over")
	t.Setenv("CO_HELPER_BYTES", fmt.Sprint(limit))
	_, _, err = (execRunner{MaxOutput: limit}).Run(context.Background(), helperExe(), helperArgs()...)
	var ce *co.Error
	if !errors.As(err, &ce) || ce.Kind != co.KindMalformedOutput {
		t.Fatalf("limit+1 answer: err = %v, want malformed_output", err)
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("the refusal does not name the cap: %v", err)
	}
}

// B02: an infinite writer is finished off and reaped, not buffered:
// the run must end bounded with the overflow error.
func TestExecRunnerInfiniteOutputKilled(t *testing.T) {
	t.Setenv("CO_HELPER_MODE", "infinite")
	start := time.Now()
	_, _, err := (execRunner{MaxOutput: 4096, MaxDuration: 20 * time.Second}).
		Run(context.Background(), helperExe(), helperArgs()...)
	var ce *co.Error
	if !errors.As(err, &ce) || ce.Kind != co.KindMalformedOutput {
		t.Fatalf("err = %v, want malformed_output for the infinite writer", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("infinite writer survived %v: overflow did not kill+reap", d)
	}
}

// B02: cancel and timeout are DIFFERENT causes, and a context
// without a deadline still gets an explicit command budget.
func TestExecRunnerCancelVsTimeout(t *testing.T) {
	t.Setenv("CO_HELPER_MODE", "silent")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := (execRunner{MaxDuration: time.Minute}).Run(ctx, helperExe(), helperArgs()...)
	var ce *co.Error
	if !errors.As(err, &ce) || ce.Kind != co.KindTimeout {
		t.Fatalf("deadline: err = %v, want timeout", err)
	}

	cctx, ccancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		ccancel()
	}()
	_, _, err = (execRunner{MaxDuration: time.Minute}).Run(cctx, helperExe(), helperArgs()...)
	if !errors.As(err, &ce) || ce.Kind != co.KindCancelled {
		t.Fatalf("cancel: err = %v, want cancelled (B02 conflated them before)", err)
	}

	// No deadline from the caller at all: the runner's own budget
	// still ends the silent child, as a timeout.
	start := time.Now()
	_, _, err = (execRunner{MaxDuration: 400 * time.Millisecond}).Run(
		context.Background(), helperExe(), helperArgs()...)
	if !errors.As(err, &ce) || ce.Kind != co.KindTimeout {
		t.Fatalf("no-deadline caller: err = %v, want the runner's own budget to time out", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("own budget took %v: not bounded", d)
	}

	// Cancel BEFORE the run even starts: cancelled, never timeout.
	pctx, pcancel := context.WithCancel(context.Background())
	pcancel()
	_, _, err = (execRunner{MaxDuration: time.Minute}).Run(pctx, helperExe(), helperArgs()...)
	if !errors.As(err, &ce) || ce.Kind != co.KindCancelled {
		t.Fatalf("pre-cancelled: err = %v, want cancelled", err)
	}
}

// B02: a grandchild holding the inherited pipe must not wedge the
// readers, and the kill-on-close job must leave it dead.
func TestExecRunnerGrandchildPipeHeldAndReaped(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("job-object kill-on-close and the tasklist proof are Windows-specific")
	}
	if os.Getenv("CO_HELPER_MODE") != "" {
		t.Fatal("helper mode must not run suite tests")
	}
	t.Setenv("CO_HELPER_MODE", "grandchild")
	start := time.Now()
	out, code, err := (execRunner{MaxDuration: 20 * time.Second}).Run(
		context.Background(), helperExe(), helperArgs()...)
	if err != nil {
		t.Fatalf("err = %v (stdout=%q)", err, out)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	var pid int
	if _, perr := fmt.Sscanf(out, "pid %d\n", &pid); perr != nil {
		t.Fatalf("stdout = %q, want the grandchild pid line", out)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("held pipe survived %v: readers were not unblocked", d)
	}
	// The job object's kill-on-close must have taken the grandchild:
	// poll briefly, then it must be gone.
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still alive after Run returned", pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// processAlive asks the OS (tasklist) whether a pid still exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("%d", pid))
}
