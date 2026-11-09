package subprocess

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// P08 session tests: real child processes driven through the JSONL
// session protocol. The readiness bar for this step: ONE prepared
// worker (one PID, one allocation) serves many verified bursts, and
// every abnormal end leaves an honest journal story and a reaped
// process.

func sessionArgv(jp, runID string, extra ...string) []string {
	base := []string{"--core", "0", "--journal", jp, "--run-id", runID, "--session"}
	return append(base, extra...)
}

// Register cleanup before any assertion can fail. A failed PREPARE/VERIFY
// used to leave the worker alive until the entire test process exited.
func startTestSession(t *testing.T, argv []string, id string, timeouts transition.Timeouts) (*SessionHandle, error) {
	t.Helper()
	h, err := StartSession(argv, id, timeouts)
	if err == nil {
		t.Cleanup(func() { h.Reap(time.Second) })
	}
	return h, err
}

func TestSessionReapReleasesUnreadAckAndIsRepeatable(t *testing.T) {
	useTestBinaryAsWorker(t)
	argv := sessionArgv(filepath.Join(t.TempDir(), "j.jsonl"), "reap", "--workload", "int")
	h, err := startTestSession(t, argv, "reap", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := h.Command(transition.CmdPrepare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Await(cmd.Seq, transition.AckReady, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// Leave the unsolicited IDLE_ACK unread, just as a failed/cancelled
	// owner can do. Reap must release the decoder and cache its exit code.
	code := h.Reap(time.Second)
	select {
	case <-h.readerDone:
	default:
		t.Fatal("Reap left its acknowledgement reader blocked")
	}
	if h.Cmd.ProcessState == nil || !h.Cmd.ProcessState.Exited() {
		t.Fatal("worker was not reaped")
	}
	if again := h.Reap(time.Millisecond); again != code {
		t.Fatalf("repeated Reap=%d, first=%d", again, code)
	}
}

func TestSessionHungHelper(t *testing.T) {
	if os.Getenv(workerChildEnv) != "1" {
		return
	}
	// Deliberately ignore stdin and STOP; sleep rather than burn a core.
	// The 30s sleep and its NATURAL end (a passing test binary, exit 0)
	// are part of the contract: TestSessionReapKillsUnresponsiveWorker
	// proves the kill landed by observing a non-zero exit. Never let
	// this helper end non-zero, or the proof degrades.
	time.Sleep(30 * time.Second)
}

func TestSessionReapKillsUnresponsiveWorker(t *testing.T) {
	useTestBinaryAsWorker(t)
	workerArgsPrefix = []string{"-test.run=^TestSessionHungHelper$", "--"}
	h, err := startTestSession(t, nil, "hung", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code := h.Reap(20 * time.Millisecond)
	elapsed := time.Since(start)
	// R11 asymmetric bounds: WHAT must be true is proven directly, not
	// proxied by wall time. (1) The forced kill was attempted and the
	// OS ACCEPTED it (KillFailure stays nil). (2) The worker ended
	// FORCED rather than waited out: the hung helper's natural end is
	// a passing test binary - exit 0 at its 30s sleep - so any exit
	// code that is not 0 is deterministic proof the kill won the
	// race. Wall time is measured and logged, not fatal: under
	// commit pressure (2026-11-07: one full-suite run at ~94GB
	// resident showed the reap return only at ~30s) the OS can delay
	// a teardown it already accepted; killing a promise from the
	// test with a 5s proxy bound made an honest machine condition
	// look like a code failure. A hung Reap still dies on the test
	// timeout, and a worker that outlives the kill still fails on
	// exit code 0 below, with a named reason.
	if err := h.KillFailure(); err != nil {
		t.Fatalf("the OS refused the forced kill: %v", err)
	}
	t.Logf("reap latency: %v (kill dispatched at the 20ms grace mark)", elapsed)
	if code == 0 || h.Cmd.ProcessState == nil {
		t.Fatalf("forced termination looked successful: code=%d state=%v (elapsed %v: the helper ran out its 30s sleep, so the kill never landed before the natural end)", code, h.Cmd.ProcessState, elapsed)
	}
	if again := h.Reap(time.Millisecond); again != code {
		t.Fatalf("repeated Reap=%d, first=%d", again, code)
	}
}

func sessionPrepare(t *testing.T, h *SessionHandle) {
	t.Helper()
	cmd, err := h.Command(transition.CmdPrepare)
	if err != nil {
		t.Fatalf("PREPARE send: %v", err)
	}
	if _, err := h.Await(cmd.Seq, transition.AckReady, 30*time.Second); err != nil {
		t.Fatalf("PREPARE await: %v (stderr: %s)", err, h.Stderr.String())
	}
	if err := h.AwaitIdle(10 * time.Second); err != nil {
		t.Fatalf("idle after prepare: %v (stderr: %s)", err, h.Stderr.String())
	}
}

func sessionBurst(t *testing.T, h *SessionHandle, soak time.Duration) transition.Ack {
	t.Helper()
	trig, err := h.Command(transition.CmdTrigger)
	if err != nil {
		t.Fatalf("TRIGGER send: %v", err)
	}
	if _, err := h.Await(trig.Seq, transition.AckStarted, 10*time.Second); err != nil {
		t.Fatalf("TRIGGER await: %v (stderr: %s)", err, h.Stderr.String())
	}
	time.Sleep(soak)
	ver, err := h.Command(transition.CmdVerify)
	if err != nil {
		t.Fatalf("VERIFY send: %v", err)
	}
	ack, err := h.Await(ver.Seq, transition.AckVerified, 30*time.Second)
	if err != nil {
		t.Fatalf("VERIFY await: %v (stderr: %s)", err, h.Stderr.String())
	}
	if err := h.AwaitIdle(10 * time.Second); err != nil {
		t.Fatalf("idle after verify: %v", err)
	}
	return ack
}

// One prepared worker, three verified bursts, one allocation: the
// plan's readiness core. The journal must carry exactly one mem_done
// (the session's allocation, never re-counted per burst) and one
// core_done whose checks are the session total.
func TestSessionRamConstantMultiBurst(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s1",
		"--workload", "ram-constant",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--ram-seed", "7",
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:destination:65536:65536")
	h, err := startTestSession(t, argv, "s1", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	pid := h.Cmd.Process.Pid
	sessionPrepare(t, h)

	var ackChecks []uint64
	for b := 0; b < 3; b++ {
		ack := sessionBurst(t, h, 150*time.Millisecond)
		if ack.Checks == nil || *ack.Checks == 0 {
			t.Fatalf("burst %d reported no verified checks: %+v", b, ack)
		}
		ackChecks = append(ackChecks, *ack.Checks)
		if ack.Bytes == nil || *ack.Bytes == 0 {
			t.Errorf("burst %d reported no verified bytes: %+v", b, ack)
		}
	}
	if err := h.Stop(10 * time.Second); err != nil {
		t.Fatalf("STOP: %v (stderr: %s)", err, h.Stderr.String())
	}
	if code := h.Reap(10 * time.Second); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.Stderr.String())
	}

	recs, iss := journal.ReadAll(journal.FragmentPath(jp, "rid-s1", 0))
	if iss.ReadErr != "" {
		t.Fatalf("read fragment: %s", iss.ReadErr)
	}
	var memDone, coreDone int
	var mem *journal.MemAccounting
	var core *journal.Record
	var bursts []journal.BurstAccounting
	for i := range recs {
		switch recs[i].Type {
		case journal.MemDone:
			memDone++
			mem = recs[i].Mem
		case journal.CoreDone:
			coreDone++
			core = &recs[i]
		case journal.BurstDone:
			bursts = append(bursts, *recs[i].Burst)
		}
	}
	// Every closed burst left its worker-side record: identity,
	// counts, complete flag. Three bursts commanded, three records,
	// all complete, and their check sum equals the session total
	// (the bursts ARE the session's work - nothing ran outside them).
	if len(bursts) != 3 {
		t.Fatalf("burst_done records = %d, want 3: %+v", len(bursts), bursts)
	}
	var burstChecks uint64
	for i, b := range bursts {
		if b.Burst != "s1-b"+strconv.Itoa(i+1) {
			t.Errorf("burst %d id = %q, want s1-b%d (machine-issued identity)", i, b.Burst, i+1)
		}
		if !b.Complete {
			t.Errorf("burst %d reads incomplete after a clean VERIFY: %+v", i, b)
		}
		if b.Checks == 0 || b.Bytes == 0 {
			t.Errorf("burst %d lost its counts: %+v", i, b)
		}
		if b.TriggeredAt == "" || b.StartedAt == "" || b.ClosedAt == "" || b.ExecMS <= 0 {
			t.Errorf("burst %d lost its worker-local timing: %+v", i, b)
		}
		burstChecks += b.Checks
	}
	if core == nil || core.Checks == nil || *core.Checks != burstChecks {
		t.Errorf("session checks %v != burst sum %d", core.Checks, burstChecks)
	}
	if memDone != 1 {
		t.Errorf("mem_done records = %d, want exactly 1 per session (bursts must not re-count the allocation)", memDone)
	}
	if coreDone != 1 {
		t.Errorf("core_done records = %d, want exactly 1 per session", coreDone)
	}
	if mem == nil {
		t.Fatal("mem_done missing")
	}
	if mem.Requested != 131072 || mem.Verified != 131072 {
		t.Errorf("mem accounting: %+v, want 131072 requested/verified", mem)
	}
	if mem.Seed == nil || *mem.Seed != 7 {
		t.Errorf("seed = %v, want 7", mem.Seed)
	}
	if mem.VerifiedUnique == nil || *mem.VerifiedUnique != 131072 {
		t.Errorf("verified_unique = %v, want 131072", mem.VerifiedUnique)
	}
	if mem.VerifiedTotal == nil || *mem.VerifiedTotal < 3*131072 {
		t.Errorf("verified_total = %v, want >= 3 passes x 131072", mem.VerifiedTotal)
	}
	if core == nil || core.Result != journal.OK {
		t.Fatalf("core_done wrong: %+v", core)
	}
	if core.KernelVersion != "ram-constant-v1" {
		t.Errorf("kernel version = %q, want ram-constant-v1", core.KernelVersion)
	}
	if core.Checks == nil || *core.Checks < ackSum(ackChecks) {
		t.Errorf("session checks = %v, want >= the burst sum %d", core.Checks, ackSum(ackChecks))
	}
	if core.Ram == nil || core.Ram.Total != 0 {
		t.Errorf("clean session evidence wrong: %+v", core.Ram)
	}
	// One PID for the whole session: the core_start record stamps it.
	for i := range recs {
		if recs[i].Type == journal.CoreStart && recs[i].PID != pid {
			t.Errorf("core_start pid = %d, want the session pid %d (bursts reused the process)", recs[i].PID, pid)
		}
	}
}

func ackSum(v []uint64) uint64 {
	var s uint64
	for _, x := range v {
		s += x
	}
	return s
}

// A tampered grant list must be refused at PREPARE with the worker's
// own reason in the ERROR ack, exit 2 (resource/setup, never a CPU
// fault), and no mem_done may exist for an allocation that never was.
func TestSessionPrepareRefusedExit2(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s2",
		"--workload", "ram-constant",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		// overlapping grants: ValidateGrants must refuse before any
		// allocation happens
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:source:0:65536")
	h, err := startTestSession(t, argv, "s2", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := h.Command(transition.CmdPrepare)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Await(cmd.Seq, transition.AckReady, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "worker error") {
		t.Fatalf("PREPARE with overlapping grants: err = %v, want a worker error", err)
	}
	if code := h.Reap(10 * time.Second); code != 2 {
		t.Errorf("exit = %d, want 2 (setup/resource, never fault)", code)
	}
	recs, _ := journal.ReadAll(journal.FragmentPath(jp, "rid-s2", 0))
	var memDone int
	var core *journal.Record
	for i := range recs {
		switch recs[i].Type {
		case journal.MemDone:
			memDone++
		case journal.CoreDone:
			core = &recs[i]
		}
	}
	if memDone != 0 {
		t.Errorf("mem_done present for a refused allocation: %+v", recs)
	}
	if core == nil || core.Result != journal.SetupError {
		t.Errorf("core_done = %+v, want setup_error", core)
	}
}

// STOP during a running burst: the burst closes at its honest
// boundary, the engine releases, the session ends clean (exit 0).
// Cancellation must have an exit from every state.
func TestSessionStopMidBurst(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s3",
		"--workload", "ram-constant",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:destination:65536:65536")
	h, err := startTestSession(t, argv, "s3", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	sessionPrepare(t, h)
	trig, err := h.Command(transition.CmdTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Await(trig.Seq, transition.AckStarted, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(15 * time.Second); err != nil {
		t.Fatalf("STOP mid-burst: %v (stderr: %s)", err, h.Stderr.String())
	}
	if code := h.Reap(10 * time.Second); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.Stderr.String())
	}
	recs, _ := journal.ReadAll(journal.FragmentPath(jp, "rid-s3", 0))
	var core *journal.Record
	var bursts []journal.BurstAccounting
	for i := range recs {
		if recs[i].Type == journal.CoreDone {
			core = &recs[i]
		}
		if recs[i].Type == journal.BurstDone && recs[i].Burst != nil {
			bursts = append(bursts, *recs[i].Burst)
		}
	}
	if core == nil || core.Result != journal.OK {
		t.Errorf("core_done = %+v, want ok (a clean STOP is not a fault)", core)
	}
	// The burst STOP interrupted was recorded as incomplete - a
	// clean session end does not retroactively credit a burst that
	// never closed a verified block.
	if len(bursts) != 1 || bursts[0].Complete {
		t.Errorf("bursts at STOP = %+v, want exactly 1 with complete=false", bursts)
	}
}

// A fault with NO verify pending (the "fault" probe workload fails on
// its own) must surface immediately through an unsolicited ERROR ack
// and exit 1 - the session loop does not wait for the next command
// to notice the burst died.
func TestSessionFaultSpontaneous(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s4", "--workload", "fault")
	h, err := startTestSession(t, argv, "s4", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	sessionPrepare(t, h)
	trig, err := h.Command(transition.CmdTrigger)
	if err != nil {
		t.Fatal(err)
	}
	// Either order is honest: STARTED then the ERROR, or the ERROR
	// winning the race. Both end in the fault verdict.
	_, err1 := h.Await(trig.Seq, transition.AckStarted, 10*time.Second)
	sawFault := err1 != nil && strings.Contains(err1.Error(), "injected fault")
	if err1 != nil && !sawFault {
		t.Fatalf("TRIGGER: %v (stderr: %s)", err1, h.Stderr.String())
	}
	if !sawFault {
		if _, err := h.Await(0, transition.AckError, 10*time.Second); err == nil ||
			!strings.Contains(err.Error(), "injected fault") {
			t.Fatalf("waiting for the fault ack: %v", err)
		}
	}
	if code := h.Reap(10 * time.Second); code != 1 {
		t.Errorf("exit = %d, want 1 (a real fault)", code)
	}
	recs, _ := journal.ReadAll(journal.FragmentPath(jp, "rid-s4", 0))
	var core *journal.Record
	for i := range recs {
		if recs[i].Type == journal.CoreDone {
			core = &recs[i]
		}
	}
	if core == nil || core.Result != journal.Fault {
		t.Errorf("core_done = %+v, want fault", core)
	}
}

// A parent that closes the channel without STOP ends the session as
// an INCOMPLETE setup condition (exit 2): the end was never
// confirmed, and nothing here may read as "ok".
func TestSessionEOFIncomplete(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s5",
		"--workload", "ram-constant",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:destination:65536:65536")
	h, err := startTestSession(t, argv, "s5", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	sessionPrepare(t, h)
	// No STOP: reap closes stdin (parent gone).
	if code := h.Reap(10 * time.Second); code != 2 {
		t.Errorf("exit = %d, want 2 (unconfirmed end is never ok)", code)
	}
	recs, _ := journal.ReadAll(journal.FragmentPath(jp, "rid-s5", 0))
	var core *journal.Record
	for i := range recs {
		if recs[i].Type == journal.CoreDone {
			core = &recs[i]
		}
	}
	if core == nil || core.Result != journal.SetupError ||
		!strings.Contains(core.Msg, "without STOP") {
		t.Errorf("core_done = %+v, want setup_error naming the unconfirmed end", core)
	}
}

// IDLE aborts a running burst: the ack answers with the command's
// seq, the machine returns to idle, and the session lives to take a
// clean STOP. The aborted burst's completed checks still ride the
// ack (honest partial), while the parent marks the burst incomplete.
func TestSessionIdleAbortsBurst(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s6",
		"--workload", "ram-constant",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:destination:65536:65536")
	h, err := startTestSession(t, argv, "s6", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	sessionPrepare(t, h)
	trig, err := h.Command(transition.CmdTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Await(trig.Seq, transition.AckStarted, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	idle, err := h.Command(transition.CmdIdle)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := h.Await(idle.Seq, transition.AckIdle, 15*time.Second)
	if err != nil {
		t.Fatalf("IDLE await: %v (stderr: %s)", err, h.Stderr.String())
	}
	if ack.Checks == nil {
		t.Errorf("aborted burst ack carries no check count: %+v", ack)
	}
	if h.Machine.State() != transition.Idle {
		t.Errorf("machine state = %s, want idle", h.Machine.State())
	}
	if err := h.Stop(10 * time.Second); err != nil {
		t.Fatalf("STOP after idle: %v", err)
	}
	if code := h.Reap(10 * time.Second); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.Stderr.String())
	}
	// The aborted burst still left a record - complete=false, its
	// partial counts intact. Silence would erase real observations.
	recs, _ := journal.ReadAll(journal.FragmentPath(jp, "rid-s6", 0))
	var bursts []journal.BurstAccounting
	for i := range recs {
		if recs[i].Type == journal.BurstDone && recs[i].Burst != nil {
			bursts = append(bursts, *recs[i].Burst)
		}
	}
	if len(bursts) != 1 || bursts[0].Complete {
		t.Errorf("aborted burst records = %+v, want exactly 1 with complete=false", bursts)
	}
}

// The session refuses a -duration: a fixed window in a mode whose
// whole point is "no fixed window" is a corrupted contract, and
// corrupted contracts surface as exit 2, not as quiet reinterpretation.
func TestSessionRejectsDuration(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	argv := sessionArgv(jp, "rid-s7", "--workload", "ram-constant", "--duration", "5s")
	h, err := startTestSession(t, argv, "s7", transition.DefaultTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	if code := h.Reap(10 * time.Second); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(h.Stderr.String(), "no -duration") {
		t.Errorf("stderr = %q, want the refusal reason", h.Stderr.String())
	}
}
