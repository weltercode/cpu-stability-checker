package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// longG01Fake registers a workload that holds until its context is
// cancelled: the cancel tests need a run still under load when the
// GUI speaks.
func longG01Fake(t *testing.T, name string) {
	t.Helper()
	workload.Register(&workload.Workload{Name: name,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	t.Cleanup(func() { workload.Delete(name) })
}

// Ghost cancel (audit A01's shuffle repro, same disease family as the
// affinity leak): a managed session whose control pipe EOFs right away
// must cancel its own run (G01c), but the session's close must discard
// the cancels its channel produced. Before ClearPendingCancel, the next
// run in this very test binary died several tests later with "gui
// control pipe closed (EOF)" - in a menu host the same ghost would
// strike the operator's next experiment dead.
func TestManagedSessionCloseClearsItsCancelResidue(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01b-ghost")

	req := sessionRequest()
	req.Session.Workload = "g01b-ghost"
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)

	oldIn := os.Stdin
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = inR
	inW.Close() // the GUI died before saying a word: G01c cancels this session's run
	code := runCmd([]string{"-request", reqPath, "-events", "jsonl"})
	os.Stdin = oldIn
	inR.Close()
	if code == 0 {
		t.Errorf("EOF on the control pipe must cancel the managed session's own run (G01c): exit %d", code)
	}

	// The session is over: the next run in this process belongs to
	// nobody's GUI and must run clean.
	if code := runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
		"-cores", "0", "-control", "-1", "-log", "after.log"}); code != 0 {
		t.Fatalf("a dead session's ghost cancelled the next run: exit %d", code)
	}
}

// managedGUI is a scripted fake GUI: pipes for all three streams,
// the run in a goroutine, and a scanner handing back parsed events
// one line at a time. A non-JSON line fails the test on the spot -
// the acceptance contract says every stdout line is one record.
type managedGUI struct {
	t      *testing.T
	inW    *os.File
	sc     *bufio.Scanner
	code   chan int
	events []eventEnvelope
	done   bool // finish() already collected the verdict: cleanup must not wait again
}

func startManagedGUI(t *testing.T, reqPath, outDir string) *managedGUI {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	gui := &managedGUI{t: t, inW: inW, sc: bufio.NewScanner(outR), code: make(chan int, 1)}
	go func() { // drain stderr so the run never blocks on a full human pipe
		buf := make([]byte, 4096)
		for {
			if _, err := errR.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() {
		code := runCmd([]string{"-request", reqPath, "-events", "jsonl", "-output", outDir})
		// runCmd's managed tail restored os.Stdout to outW (the
		// captured "real" stream): put the test's own handles back.
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		outW.Close() // the stream ends with the run: the scanner sees EOF
		gui.code <- code
	}()
	t.Cleanup(func() {
		inW.Close()
		if !gui.done { // the body bailed early: still do not leak the run
			select {
			case <-gui.code:
			case <-time.After(90 * time.Second):
				t.Error("managed run never finished")
			}
		}
		outW.Close()
		errW.Close()
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	})
	return gui
}

// nextEvent blocks for one event record; a non-JSON line is fatal.
func (g *managedGUI) nextEvent() (eventEnvelope, bool) {
	g.t.Helper()
	for g.sc.Scan() {
		var env eventEnvelope
		if err := json.Unmarshal(g.sc.Bytes(), &env); err != nil {
			g.t.Fatalf("stdout line is not one JSON record: %v\n%s", err, g.sc.Text())
		}
		g.events = append(g.events, env)
		return env, true
	}
	return eventEnvelope{}, false
}

// waitFor blocks for one event of the given type, failing the test
// if the stream ends first.
func (g *managedGUI) waitFor(typ string) eventEnvelope {
	g.t.Helper()
	for {
		env, ok := g.nextEvent()
		if !ok {
			g.t.Fatalf("stream ended before %q (types seen: %s)", typ, g.typesSeen())
		}
		if env.Type == typ {
			return env
		}
	}
}

// waitCompleted drains to the final record and returns it.
func (g *managedGUI) waitCompleted() eventEnvelope {
	g.t.Helper()
	for {
		env, ok := g.nextEvent()
		if !ok {
			g.t.Fatalf("stream ended without completed (types seen: %s)", g.typesSeen())
		}
		if env.Type == "completed" {
			return env
		}
	}
}

func (g *managedGUI) typesSeen() string {
	var ts []string
	for _, e := range g.events {
		ts = append(ts, e.Type)
	}
	return strings.Join(ts, ",")
}

func (g *managedGUI) send(v any) {
	g.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		g.t.Fatal(err)
	}
	fmt.Fprintf(g.inW, "%s\n", b)
}

func (g *managedGUI) sendRaw(line string) {
	g.t.Helper()
	fmt.Fprintln(g.inW, line)
}

// finish closes the control pipe (the GUI disconnects cleanly) and
// waits for the process verdict.
func (g *managedGUI) finish() int {
	g.t.Helper()
	g.inW.Close()
	return g.collect()
}

// collect waits for the verdict when the pipe was already closed
// (the crash simulation): the channel may be consumed only once.
func (g *managedGUI) collect() int {
	g.t.Helper()
	code := <-g.code
	g.done = true
	return code
}

func (g *managedGUI) count(typ string) int {
	n := 0
	for _, e := range g.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func payloadStr(env eventEnvelope, key string) string {
	m, _ := env.Payload.(map[string]any)
	s, _ := m[key].(string)
	return s
}

func payloadBool(env eventEnvelope, key string) bool {
	m, _ := env.Payload.(map[string]any)
	b, _ := m[key].(bool)
	return b
}

// Cancel stops the load, duplicate commands confirm exactly once
// per command id, and the session ends cancelled with a durable
// completed record - all long before the planned duration ran out.
func TestManagedCancelDuplicateAck(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	longG01Fake(t, "g01c-cancel")

	req := sessionRequest()
	req.Session.Workload = "g01c-cancel"
	req.Session.Duration = 30 * time.Second // the cancel must end it, not the clock
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)
	outDir := filepath.Join(dir, "res")

	startedAt := time.Now()
	gui := startManagedGUI(t, reqPath, outDir)
	started := gui.waitFor("started")
	session := started.SessionID
	if session == "" {
		t.Fatal("started event carries no session id")
	}

	cancel := func(id string) {
		gui.send(map[string]any{"type": "cancel", "session_id": session, "command_id": id})
	}
	cancel("cmd-1")
	cancel("cmd-1") // resend: must not produce a second ack
	cancel("cmd-2") // different id: confirmed, marked already-cancelling

	completed := gui.waitCompleted()
	code := gui.finish()

	if elapsed := time.Since(startedAt); elapsed > 20*time.Second {
		t.Errorf("the cancel did not end the 30s run early: %v", elapsed)
	}
	if code == 0 {
		t.Error("a cancelled run exited 0")
	}
	if gui.count("cancelling") != 1 {
		t.Errorf("cancelling events = %d, want exactly 1", gui.count("cancelling"))
	}
	var ack1, ack2 int
	for _, e := range gui.events {
		if e.Type != "cancel_ack" {
			continue
		}
		switch payloadStr(e, "command_id") {
		case "cmd-1":
			ack1++
			if payloadBool(e, "already_cancelling") {
				t.Error("the first cancel ack claims it was already cancelling")
			}
		case "cmd-2":
			ack2++
			if !payloadBool(e, "already_cancelling") {
				t.Error("the duplicate cancel does not admit the stop was already underway")
			}
		}
	}
	if ack1 != 1 {
		t.Errorf("acks for cmd-1 = %d, want exactly 1 (resend must be silent)", ack1)
	}
	if ack2 != 1 {
		t.Errorf("acks for cmd-2 = %d, want exactly 1", ack2)
	}
	if payloadStr(completed, "state") != "cancelled" {
		t.Errorf("completed state = %q, want cancelled", payloadStr(completed, "state"))
	}
	m := onlyManifest(t)
	if m.State != "cancelled" {
		t.Errorf("manifest state = %q, want cancelled", m.State)
	}
}

// EOF on the control pipe in managed mode is a cancel: a GUI that
// vanished (crash, closed pipe) must never leave a machine under
// load with nobody watching.
func TestManagedEOFControlPipeCancels(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	longG01Fake(t, "g01c-eof")

	req := sessionRequest()
	req.Session.Workload = "g01c-eof"
	req.Session.Duration = 30 * time.Second
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)

	gui := startManagedGUI(t, reqPath, filepath.Join(dir, "res"))
	started := gui.waitFor("started")
	if started.SessionID == "" {
		t.Fatal("started event carries no session id")
	}
	gui.inW.Close() // the GUI crashes: the control pipe is gone

	completed := gui.waitCompleted()
	code := gui.collect() // the pipe is already closed (crash): just wait for the verdict
	if code == 0 {
		t.Error("an EOF-cancelled run exited 0")
	}
	if payloadStr(completed, "state") != "cancelled" {
		t.Errorf("completed state = %q, want cancelled", payloadStr(completed, "state"))
	}
	if gui.count("cancelling") != 1 {
		t.Errorf("cancelling events = %d, want exactly 1", gui.count("cancelling"))
	}
}

// Unknown command types, wrong session ids and malformed lines are
// answered with a structured command_error and change nothing: the
// run completes normally.
func TestManagedCommandErrors(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01c-err")

	req := sessionRequest()
	req.Session.Workload = "g01c-err"
	req.Session.Duration = 2 * time.Second
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)

	gui := startManagedGUI(t, reqPath, filepath.Join(dir, "res"))
	started := gui.waitFor("started")
	gui.send(map[string]any{"type": "selftest", "session_id": started.SessionID})
	gui.send(map[string]any{"type": "cancel", "session_id": "not-this-session", "command_id": "x"})
	gui.sendRaw("this is not json")

	completed := gui.waitCompleted()
	code := gui.finish()
	if code != 0 {
		t.Errorf("command errors changed the verdict: exit %d", code)
	}
	if payloadStr(completed, "state") != "completed" {
		t.Errorf("completed state = %q, want completed", payloadStr(completed, "state"))
	}
	if gui.count("command_error") != 3 {
		t.Errorf("command_error events = %d, want 3 (unknown type, wrong session, bad line)", gui.count("command_error"))
	}
	if gui.count("cancel_ack") != 0 || gui.count("cancelling") != 0 {
		t.Error("a refused cancel was treated as real")
	}
}

// The v1 concurrency policy: a managed run refuses to stack onto a
// live one, naming the holder's metadata. Ownership is OS-backed
// (audit B08): the lock file is persistent, the OS lock inside it IS
// the ownership, and a dead holder's leftover file is simply taken
// over - no pid archaeology, no deletion, no stale-file race.
func TestManagedRunLockRefusesConcurrent(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	useScratchRunLock(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01c-lock")

	req := sessionRequest()
	req.Session.Workload = "g01c-lock"
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)

	// A live holder: the OS lock, held by this very test process.
	hold, err := acquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	errR, errW, _ := os.Pipe()
	os.Stderr = errW
	drained := make(chan string, 1)
	go func() { b, _ := io.ReadAll(errR); drained <- string(b) }()
	code := runCmd([]string{"-request", reqPath, "-events", "jsonl", "-output", dir})
	os.Stderr = oldErr
	errW.Close()
	stderr := <-drained
	if code != 2 {
		t.Errorf("concurrent managed run exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "another run session is active") {
		t.Errorf("the refusal did not name the live holder:\n%s", stderr)
	}
	if _, err := loadLastRun(); err == nil {
		t.Error("the lock refusal ran after the slot was already displaced")
	}
	if _, err := os.Stat(runLockPath); err != nil {
		t.Error("the refusal deleted a lock it never held")
	}
	hold()

	// The holder is gone and the file is garbage on purpose: the
	// metadata never decides liveness, the free OS lock does. The
	// next run takes over without touching the file's story.
	if err := os.WriteFile(runLockPath, []byte("total garbage, no pid, no shape"), 0o644); err != nil {
		t.Fatal(err)
	}
	gui := startManagedGUI(t, reqPath, filepath.Join(dir, "res"))
	completed := gui.waitCompleted()
	if code = gui.finish(); code != 0 {
		t.Errorf("the leftover lock file blocked a legitimate run: exit %d", code)
	}
	if payloadStr(completed, "state") != "completed" {
		t.Errorf("completed state = %q after the leftover-file takeover", payloadStr(completed, "state"))
	}
	// The run released its lock: the file stays (persistent), the
	// lock is free again.
	if again, err := acquireRunLock(); err != nil {
		t.Errorf("the run did not release its lock: %v", err)
	} else {
		again()
	}
	if _, err := os.Stat(runLockPath); err != nil {
		t.Errorf("the release deleted the persistent lock file: %v", err)
	}
}
