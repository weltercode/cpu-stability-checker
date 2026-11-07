package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// G01 acceptance (menu plan section 11): the fake GUI launches the
// real executable without a TTY, reads both streams, validates
// every stdout line as JSON, and obtains the result file. Skipped
// under -short: these tests build the exe and run real (tiny: one
// core, a fraction of a second) load.

var (
	e2eExeOnce sync.Once
	e2eExePath string
	e2eExeErr  error
)

// e2eExe builds the real executable once per test binary and hands
// its path to every e2e test.
func e2eExe(t *testing.T) string {
	t.Helper()
	e2eExeOnce.Do(func() {
		root, err := os.Getwd()
		if err != nil {
			e2eExeErr = err
			return
		}
		for {
			if _, serr := os.Stat(filepath.Join(root, "go.mod")); serr == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				e2eExeErr = fmt.Errorf("no go.mod above the test package")
				return
			}
			root = parent
		}
		dir, derr := os.MkdirTemp("", "cpu-stability-e2e-")
		if derr != nil {
			e2eExeErr = derr
			return
		}
		name := "cpu-stability-e2e"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		exe := filepath.Join(dir, name)
		build := exec.Command("go", "build", "-o", exe, ".")
		build.Dir = root
		if b, berr := build.CombinedOutput(); berr != nil {
			e2eExeErr = fmt.Errorf("go build the exe: %v\n%s", berr, b)
			return
		}
		e2eExePath = exe
	})
	if e2eExeErr != nil {
		t.Fatal(e2eExeErr)
	}
	return e2eExePath
}

// e2eGUI drives the exe exactly like a GUI process would: pipes for
// all three streams (no TTY anywhere), a scanner that fails the
// test on any non-JSON stdout line, and a stderr collector.
type e2eGUI struct {
	t      *testing.T
	cmd    *exec.Cmd
	inW    io.WriteCloser
	sc     *bufio.Scanner
	code   chan int
	mu     sync.Mutex
	stderr []string
	ended  bool
}

func startE2E(t *testing.T, exe, cwd, reqPath, outDir string) *e2eGUI {
	t.Helper()
	cmd := exec.Command(exe, "run", "-request", reqPath, "-events", "jsonl", "-output", outDir)
	cmd.Dir = cwd
	inW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := &e2eGUI{t: t, cmd: cmd, inW: inW, sc: bufio.NewScanner(outR), code: make(chan int, 1)}
	go func() {
		es := bufio.NewScanner(errR)
		for es.Scan() {
			g.mu.Lock()
			g.stderr = append(g.stderr, es.Text())
			g.mu.Unlock()
		}
	}()
	go func() {
		werr := cmd.Wait()
		code := 0
		if werr != nil {
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				code = ee.ExitCode()
			} else {
				code = -1 // Wait itself failed: honestly not a process verdict
			}
		}
		g.code <- code
	}()
	t.Cleanup(func() {
		inW.Close()
		if !g.ended {
			select {
			case <-g.code:
			case <-time.After(60 * time.Second):
				// the harness bailed: end the child so nothing keeps
				// the machine (or the next test's lock) busy
				_ = cmd.Process.Kill()
				t.Error("e2e child never finished")
			}
		}
	})
	return g
}

func (g *e2eGUI) nextEvent() (eventEnvelope, bool) {
	g.t.Helper()
	for g.sc.Scan() {
		var env eventEnvelope
		if err := json.Unmarshal(g.sc.Bytes(), &env); err != nil {
			g.t.Fatalf("exe stdout line is not one JSON record: %v\n%s", err, g.sc.Text())
		}
		return env, true
	}
	return eventEnvelope{}, false
}

func (g *e2eGUI) waitFor(typ string) eventEnvelope {
	g.t.Helper()
	for {
		env, ok := g.nextEvent()
		if !ok {
			g.t.Fatalf("exe stream ended before %q", typ)
		}
		if env.Type == typ {
			return env
		}
	}
}

func (g *e2eGUI) send(v any) {
	g.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		g.t.Fatal(err)
	}
	fmt.Fprintf(g.inW, "%s\n", b)
}

// finish ends the control pipe and waits for the process verdict.
func (g *e2eGUI) finish() int {
	g.t.Helper()
	g.inW.Close()
	code := <-g.code
	g.ended = true
	return code
}

func (g *e2eGUI) errText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.stderr, "\n")
}

// e2eRequest copies a request fixture into the run's temp dir so
// the exe reads an absolute path that belongs to this test.
func e2eRequest(t *testing.T, cwd, fixture string, edit func([]byte) []byte) string {
	t.Helper()
	b := fixtureBytes(t, fixture)
	if edit != nil {
		b = edit(b)
	}
	p := filepath.Join(cwd, "request.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The acceptance line end to end: the exe streams pure JSON, the
// human voice stays on stderr, and the result file is on disk with
// the same session truth the stream promised.
func TestE2EHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real load")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	outDir := filepath.Join(cwd, "results")
	reqPath := e2eRequest(t, cwd, "request_session.json", nil)

	g := startE2E(t, exe, cwd, reqPath, outDir)
	seen := map[string]bool{}
	var completed eventEnvelope
	for {
		env, ok := g.nextEvent()
		if !ok {
			break
		}
		seen[env.Type] = true
		if env.Type == "completed" {
			completed = env
			break
		}
	}
	code := g.finish()
	if code != 0 {
		t.Fatalf("happy path exit = %d\nstderr:\n%s", code, g.errText())
	}
	for _, want := range []string{"resolved", "started", "phase_started", "phase_finished", "completed"} {
		if !seen[want] {
			t.Errorf("stream lacks %q (types: %v)", want, seen)
		}
	}
	var payload resultPayload
	b, _ := json.Marshal(completed.Payload)
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "completed" || payload.Exit == nil || *payload.Exit != 0 {
		t.Errorf("completed payload: %+v", payload)
	}
	if len(payload.Runs) != 1 || !payload.Runs[0].Exists {
		t.Errorf("output paths wrong: %+v", payload.Runs)
	}
	rb, err := os.ReadFile(payload.ResultFile)
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var file resultPayload
	if err := json.Unmarshal(rb, &file); err != nil {
		t.Fatalf("result file is not the promised schema: %v", err)
	}
	if file.SessionID != payload.SessionID || file.State != payload.State {
		t.Errorf("result file and completed record disagree: %s vs %s", file.SessionID, payload.SessionID)
	}
	if !strings.Contains(g.errText(), "Starting session") {
		t.Errorf("human voice did not stay on stderr:\n%s", g.errText())
	}
	// The session really recorded itself in history, in the run's
	// own directory: no shared cwd was needed. (The exe wrote it
	// relative to its own working directory - the test process
	// reads it by path, not by its own cwd.)
	if _, merr := os.Stat(filepath.Join(cwd, "history", payload.SessionID+".json")); merr != nil {
		t.Errorf("no session manifest in the run's own directory: %v", merr)
	}
}

// Cancel over the real control pipe: duplicate commands confirm
// once, the load ends early, and the durable record says cancelled.
func TestE2ECancelAndDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real load")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	reqPath := e2eRequest(t, cwd, "request_session.json", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `"duration_ns": 100000000`, `"duration_ns": 30000000000`, 1))
	})

	started := time.Now()
	g := startE2E(t, exe, cwd, reqPath, filepath.Join(cwd, "res"))
	st := g.waitFor("started")
	g.send(map[string]any{"type": "cancel", "session_id": st.SessionID, "command_id": "c1"})
	g.send(map[string]any{"type": "cancel", "session_id": st.SessionID, "command_id": "c1"})
	g.send(map[string]any{"type": "cancel", "session_id": st.SessionID, "command_id": "c2"})

	acks, cancels := 0, 0
	var completed eventEnvelope
stream:
	for {
		env, ok := g.nextEvent()
		if !ok {
			break
		}
		switch env.Type {
		case "cancel_ack":
			acks++
		case "cancelling":
			cancels++
		case "completed":
			completed = env
			break stream
		}
	}
	code := g.finish()
	if elapsed := time.Since(started); elapsed > 25*time.Second {
		t.Errorf("cancel did not end the 30s run early: %v", elapsed)
	}
	if code == 0 {
		t.Error("cancelled run exited 0")
	}
	if acks != 2 {
		t.Errorf("acks = %d, want 2 (one per command id, resend silent)", acks)
	}
	if cancels != 1 {
		t.Errorf("cancelling = %d, want 1", cancels)
	}
	b, _ := json.Marshal(completed.Payload)
	var payload resultPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", payload.State)
	}
}

// A GUI that crashes (control pipe closed) must never leave the
// machine under load: EOF on stdin cancels the managed run.
func TestE2EGUICrashEOF(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real load")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	reqPath := e2eRequest(t, cwd, "request_session.json", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `"duration_ns": 100000000`, `"duration_ns": 30000000000`, 1))
	})
	g := startE2E(t, exe, cwd, reqPath, filepath.Join(cwd, "res"))
	g.waitFor("started")
	g.inW.Close() // the GUI vanished

	var completed eventEnvelope
	for {
		env, ok := g.nextEvent()
		if !ok {
			break
		}
		if env.Type == "completed" {
			completed = env
			break
		}
	}
	code := <-g.code
	g.ended = true
	if code == 0 {
		t.Error("EOF-cancelled run exited 0")
	}
	b, _ := json.Marshal(completed.Payload)
	var payload resultPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.State != "cancelled" {
		t.Errorf("state = %q, want cancelled", payload.State)
	}
}

// An invalid request is refused before anything loads: exit 2, not
// one event on stdout, and the reason on stderr.
func TestE2EInvalidRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	reqPath := e2eRequest(t, cwd, "request_session.json", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `"kind": "session"`, `"kind": "session", "time_travel": true`, 1))
	})
	g := startE2E(t, exe, cwd, reqPath, filepath.Join(cwd, "res"))
	lines := 0
	for g.sc.Scan() {
		lines++
	}
	code := g.finish()
	if code != 2 {
		t.Errorf("invalid request exit = %d, want 2", code)
	}
	if lines != 0 {
		t.Errorf("a refused request streamed %d lines", lines)
	}
	if !strings.Contains(g.errText(), "not readable") {
		t.Errorf("the refusal did not name the unknown field:\n%s", g.errText())
	}
}

// A GUI that never reads stdout while the run is alive must not
// hang the process: the run finishes, the stream ends delivered or
// broken, and the durable result is there to find afterwards. The
// read end stays open (the GUI is "frozen", not gone): an unread
// pipe, not a null device.
func TestE2ESlowReader(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real load")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	outDir := filepath.Join(cwd, "res")
	reqPath := e2eRequest(t, cwd, "request_session.json", nil)

	cmd := exec.Command(exe, "run", "-request", reqPath, "-events", "jsonl", "-output", outDir)
	cmd.Dir = cwd
	inW, _ := cmd.StdinPipe()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = outW // a real pipe the parent never reads
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	inW.Close() // no listener on stdin either: the GUI "froze" at once
	outW.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// any verdict proves the point: an unread stdout pipe never
		// hung the run (EOF on stdin cancels it early as well)
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		outR.Close()
		t.Fatal("the run hung on an unread stdout pipe")
	}
	outR.Close()
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "result-") {
		t.Errorf("no durable result after the slow-reader run: %v", entries)
	}
}

// The v1 concurrency policy at process level: a second managed run
// in the same area is refused while the first lives, naming the
// holder - never stacked load by accident.
func TestE2EConcurrentRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real load")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	reqPath := e2eRequest(t, cwd, "request_session.json", func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `"duration_ns": 100000000`, `"duration_ns": 30000000000`, 1))
	})
	g := startE2E(t, exe, cwd, reqPath, filepath.Join(cwd, "res"))
	g.waitFor("started")

	second := exec.Command(exe, "run", "-request", reqPath, "-events", "jsonl", "-output", filepath.Join(cwd, "res2"))
	second.Dir = cwd
	var sOut, sErr strings.Builder
	second.Stdout = &sOut
	second.Stderr = &sErr
	serr := second.Run()
	if serr == nil {
		t.Errorf("concurrent run was accepted:\n%s", sErr.String())
	} else if sOut.Len() != 0 {
		t.Errorf("the refusal streamed %d bytes of events", sOut.Len())
	}
	if !strings.Contains(sErr.String(), "another run session is active") {
		t.Errorf("the refusal did not name the live holder:\n%s", sErr.String())
	}

	g.send(map[string]any{"type": "cancel", "command_id": "end"})
	for {
		env, ok := g.nextEvent()
		if !ok || env.Type == "completed" {
			break
		}
	}
	g.finish()
}
