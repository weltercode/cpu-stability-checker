package transition

import (
	"strconv"
	"testing"
	"time"
)

// fakeClock drives deadlines deterministically: no wall time, no
// flakiness, and the ~0.5ms monotonic granularity of this machine
// cannot distort the assertions.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) Add(d time.Duration) { c.t = c.t.Add(d) }

func newTestMachine() (*Machine, *fakeClock) {
	fc := &fakeClock{t: time.Unix(1000, 0)}
	m := NewMachine("s1", fc, Timeouts{Trigger: time.Second, Verify: 2 * time.Second, Stop: time.Second})
	return m, fc
}

func u64(v uint64) *uint64 { return &v }

// ackOf builds a well-formed ack for the machine's pending command.
func ackOf(m *Machine, kind Kind) Ack {
	return Ack{V: ProtocolVersion, Seq: m.pending.Seq, Burst: m.pending.Burst, Kind: kind}
}

// The full commanded chain, three bursts deep: every transition clean,
// sequence numbers strictly increasing, burst ids per trigger, and the
// record log keeps commanded and acked times apart (the report must be
// able to tell the commanded transition from the observation).
func TestHappyChainThreeBursts(t *testing.T) {
	m, fc := newTestMachine()

	cmd, err := m.Send(CmdPrepare)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.V != ProtocolVersion || cmd.Seq != 1 {
		t.Fatalf("prepare command wire: %+v", cmd)
	}
	fc.Add(100 * time.Millisecond)
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Ready {
		t.Fatalf("state = %s, want ready", m.State())
	}
	m.IdleAck()
	if m.State() != Idle {
		t.Fatalf("state = %s, want idle after IDLE_ACK", m.State())
	}

	for b := 1; b <= 3; b++ {
		trig, err := m.Send(CmdTrigger)
		if err != nil {
			t.Fatal(err)
		}
		if trig.Burst != "s1-b"+strconv.Itoa(b) {
			t.Errorf("burst id = %q, want s1-b%d", trig.Burst, b)
		}
		fc.Add(5 * time.Millisecond)
		if err := m.Ack(ackOf(m, AckStarted)); err != nil {
			t.Fatal(err)
		}
		if m.State() != Running {
			t.Fatalf("state = %s, want running", m.State())
		}
		fc.Add(50 * time.Millisecond)
		if _, err := m.Send(CmdVerify); err != nil {
			t.Fatal(err)
		}
		a := ackOf(m, AckVerified)
		a.Checks, a.Bytes = u64(7), u64(65536)
		if err := m.Ack(a); err != nil {
			t.Fatal(err)
		}
		if m.State() != Verifying {
			t.Fatalf("state = %s, want verifying (IDLE_ACK closes the burst)", m.State())
		}
		m.IdleAck()
		if m.State() != Idle {
			t.Fatalf("state = %s, want idle closing the burst", m.State())
		}
	}

	if _, err := m.Send(CmdStop); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckStopped)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Stopped {
		t.Fatalf("state = %s, want stopped", m.State())
	}

	recs := m.Records()
	var deviations int
	for _, r := range recs {
		if r.Deviation != "" {
			deviations++
		}
	}
	if deviations != 0 {
		t.Errorf("clean chain logged %d deviations: %+v", deviations, recs)
	}
	// Commanded never after acked on every completed pair (the fake
	// clock may not move between Send and Ack, so equality is legal;
	// ack-before-command would be causality broken and must fail),
	// and the commanded time is the fake time at Send, not at write.
	for _, r := range recs {
		if r.Cmd != "" && r.Ack != "" && r.AckedAt.Before(r.CommandedAt) {
			t.Errorf("record %+v: ack before command (causality)", r)
		}
	}
}

// The plan demands the trigger be marked BEFORE the send: the command's
// timestamp is taken at Send, so a slow transport write afterwards can
// never inflate the trigger timestamp retroactively.
func TestTriggerMarkedBeforeSend(t *testing.T) {
	m, fc := newTestMachine()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	m.IdleAck()

	atSend := fc.Now()
	cmd, err := m.Send(CmdTrigger)
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.CommandedAt.Equal(atSend) {
		t.Errorf("CommandedAt = %v, want the clock at Send (%v)", cmd.CommandedAt, atSend)
	}
	fc.Add(30 * time.Second) // pretend the transport write and ack were slow
	if !cmd.CommandedAt.Equal(atSend) {
		t.Errorf("CommandedAt drifted to %v after the clock moved; it must stay at Send time", cmd.CommandedAt)
	}
}

// A hang is recorded once as a missed deadline (never retried, never
// hidden), the machine goes Stalled, and a late ack that finally
// arrives keeps BOTH facts: it completed, and it was late.
func TestHangLateAck(t *testing.T) {
	m, fc := newTestMachine()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	fc.Add(DefaultTimeouts().Prepare) // the machine's Prepare timeout is the default
	rec := m.Tick()
	if rec == nil || rec.Deviation != "missed deadline" || rec.Cmd != CmdPrepare {
		t.Fatalf("Tick after deadline = %+v, want one missed-deadline record", rec)
	}
	if m.State() != Stalled {
		t.Fatalf("state = %s, want stalled", m.State())
	}
	if again := m.Tick(); again != nil {
		t.Errorf("second Tick = %+v, want nil: one deadline, one record", again)
	}
	// The ack finally arrives: the transition completes, honestly late.
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Ready {
		t.Fatalf("state = %s, want ready (the worker DID respond)", m.State())
	}
	var late int
	for _, r := range m.Records() {
		if r.Deviation == "late ack after missed deadline" {
			late++
		}
		if r.Deviation == "missed deadline" {
			t.Errorf("the missed deadline was erased: %+v", r)
		}
	}
	if late != 1 {
		t.Errorf("late-ack deviations = %d, want 1", late)
	}
}

// Duplicate, stale and unexpected acks are classified and recorded,
// never silently eaten, and never move the state machine.
func TestAckClassification(t *testing.T) {
	m, fc := newTestMachine()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	ready := ackOf(m, AckReady)
	if err := m.Ack(ready); err != nil {
		t.Fatal(err)
	}
	// Duplicate of a completed ack.
	if err := m.Ack(ready); err != nil {
		t.Fatal(err)
	}
	if m.State() != Ready {
		t.Fatalf("duplicate ack moved the state to %s", m.State())
	}
	// Stale: a seq that never existed.
	if err := m.Ack(Ack{V: ProtocolVersion, Seq: 99, Kind: AckStarted}); err != nil {
		t.Fatal(err)
	}
	// Unexpected kind for the pending command (IDLE_ACK arrives as an
	// unsolicited state ack, so use a wrong command ack here): send
	// TRIGGER, then ack with the wrong kind.
	m.IdleAck()
	if _, err := m.Send(CmdTrigger); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckVerified)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Triggered {
		t.Fatalf("unexpected ack moved the state to %s, want triggered", m.State())
	}
	// Wrong protocol version is refused outright.
	if err := m.Ack(Ack{V: 2, Seq: m.pending.Seq, Kind: AckStarted}); err == nil {
		t.Error("ack with unknown protocol version accepted")
	}
	_ = fc

	want := map[string]int{"duplicate ack": 1, "stale ack": 1, "unexpected ack (want STARTED)": 1, "unknown protocol version": 1}
	got := map[string]int{}
	for _, r := range m.Records() {
		if r.Deviation != "" {
			got[r.Deviation]++
		}
	}
	for w, n := range want {
		if got[w] != n {
			t.Errorf("deviation %q count = %d, want %d (all: %v)", w, got[w], n, got)
		}
	}
}

// Cancellation has an exit from every live state: STOP is legal from
// all of them, supersedes a pending command, and only STOPPED closes
// the session.

func mustSend(t *testing.T, m *Machine, k Kind) {
	t.Helper()
	if _, err := m.Send(k); err != nil {
		t.Fatalf("Send(%s): %v", k, err)
	}
}

func mustAck(t *testing.T, m *Machine, k Kind) {
	t.Helper()
	if err := m.Ack(ackOf(m, k)); err != nil {
		t.Fatalf("Ack(%s): %v", k, err)
	}
}

func setupPreparing(m *Machine) { m.Send(CmdPrepare) }

func setupReady(t *testing.T, m *Machine) {
	t.Helper()
	mustSend(t, m, CmdPrepare)
	mustAck(t, m, AckReady)
}

func setupIdle(t *testing.T, m *Machine) {
	t.Helper()
	setupReady(t, m)
	m.IdleAck()
}

func setupRunning(t *testing.T, m *Machine) {
	t.Helper()
	setupIdle(t, m)
	mustSend(t, m, CmdTrigger)
	mustAck(t, m, AckStarted)
}

func setupVerifying(t *testing.T, m *Machine) {
	t.Helper()
	setupRunning(t, m)
	mustSend(t, m, CmdVerify)
}

func setupStalled(m *Machine, fc *fakeClock) {
	m.Send(CmdPrepare)
	fc.Add(DefaultTimeouts().Prepare)
	m.Tick()
}

func TestStopFromEveryState(t *testing.T) {
	setups := []struct {
		name  string
		setup func(t *testing.T, m *Machine, fc *fakeClock)
		want  State
	}{
		{"new", func(_ *testing.T, _ *Machine, _ *fakeClock) {}, New},
		{"preparing", func(_ *testing.T, m *Machine, _ *fakeClock) { setupPreparing(m) }, Preparing},
		{"ready", func(t *testing.T, m *Machine, _ *fakeClock) { setupReady(t, m) }, Ready},
		{"idle", func(t *testing.T, m *Machine, _ *fakeClock) { setupIdle(t, m) }, Idle},
		{"running", func(t *testing.T, m *Machine, _ *fakeClock) { setupRunning(t, m) }, Running},
		{"verifying", func(t *testing.T, m *Machine, _ *fakeClock) { setupVerifying(t, m) }, Verifying},
		{"stalled", func(_ *testing.T, m *Machine, fc *fakeClock) { setupStalled(m, fc) }, Stalled},
	}
	for _, tt := range setups {
		t.Run(tt.name, func(t *testing.T) {
			m, fc := newTestMachine()
			tt.setup(t, m, fc)
			if m.State() != tt.want {
				t.Fatalf("setup state = %s, want %s", m.State(), tt.want)
			}
			if _, err := m.Send(CmdStop); err != nil {
				t.Fatalf("STOP from %s: %v", tt.name, err)
			}
			if m.State() != Stopping {
				t.Errorf("state = %s, want stopping", m.State())
			}
			if err := m.Ack(ackOf(m, AckStopped)); err != nil {
				t.Fatal(err)
			}
			if m.State() != Stopped {
				t.Errorf("state = %s, want stopped", m.State())
			}
			if _, err := m.Send(CmdStop); err == nil {
				t.Error("STOP after stopped accepted: the session is over")
			}
		})
	}
}

// Commands illegal for the current state are refused: the machine
// never jumps (a TRIGGER before PREPARE would silently "run" a worker
// that was never prepared).
func TestIllegalCommandsRefused(t *testing.T) {
	m, _ := newTestMachine()
	if _, err := m.Send(CmdTrigger); err == nil {
		t.Error("TRIGGER from new accepted")
	}
	if _, err := m.Send(CmdVerify); err == nil {
		t.Error("VERIFY from new accepted")
	}
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(CmdPrepare); err == nil {
		t.Error("second PREPARE while pending accepted")
	}
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	m.IdleAck()
	if _, err := m.Send(CmdVerify); err == nil {
		t.Error("VERIFY from idle accepted: nothing is running to verify")
	}
}

// IDLE_ACK is only meaningful from ready/running/verifying; from
// anywhere else it is recorded as a deviation, not ignored.
func TestIdleAckUnexpected(t *testing.T) {
	m, _ := newTestMachine()
	m.IdleAck() // from New: nothing was ready
	var dev int
	for _, r := range m.Records() {
		if r.Deviation == "unexpected idle ack from state new" {
			dev++
		}
	}
	if dev != 1 {
		t.Errorf("unsolicited IDLE_ACK from new: deviations = %d, want 1 (records: %+v)", dev, m.Records())
	}
	if m.State() != New {
		t.Errorf("state moved to %s from an unsolicited idle ack", m.State())
	}
}

// An ERROR ack (allocation refused, kernel fault mid-burst) is
// recorded with the worker's reason, moves the session to Stalled and
// clears the pending command: the machine never retries and never
// pretends the step happened - the parent decides cleanup. STOP still
// has an exit from Stalled.
func TestErrorAckStalls(t *testing.T) {
	m, _ := newTestMachine()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(Ack{V: ProtocolVersion, Seq: 1, Kind: AckError, Msg: "free commit cannot cover it"}); err != nil {
		t.Fatal(err)
	}
	if m.State() != Stalled {
		t.Fatalf("state = %s, want stalled after worker error", m.State())
	}
	var found bool
	for _, r := range m.Records() {
		if r.Deviation == "worker error: free commit cannot cover it" {
			found = true
		}
	}
	if !found {
		t.Errorf("the worker's refusal reason was not recorded: %+v", m.Records())
	}
	if _, err := m.Send(CmdStop); err != nil {
		t.Errorf("STOP after worker error: %v", err)
	}
	if err := m.Ack(ackOf(m, AckStopped)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Stopped {
		t.Errorf("state = %s, want stopped", m.State())
	}
}
