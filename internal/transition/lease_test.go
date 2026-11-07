package transition

import (
	"testing"
	"time"
)

// newHandoffMachine is a session that opted into leases: PREPAREd,
// READY, idle - the only state leases are legal from.
func newHandoffMachine(t *testing.T) (*Machine, *fakeClock) {
	t.Helper()
	m, fc := newTestMachine()
	m.AllowLeases()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	m.IdleAck()
	if m.State() != Idle {
		t.Fatalf("state = %s, want idle", m.State())
	}
	return m, fc
}

// Leases are refused on ordinary sessions: a lease on a burst session
// is a wiring bug, and the machine refuses it without moving state.
func TestLeaseRefusedWithoutHandoff(t *testing.T) {
	m, _ := newTestMachine()
	if _, err := m.Send(CmdPrepare); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckReady)); err != nil {
		t.Fatal(err)
	}
	m.IdleAck()
	if _, err := m.SendLease(CmdLeaseWrite, 3, 1); err == nil {
		t.Error("lease accepted on a non-handoff session")
	}
	if m.State() != Idle {
		t.Errorf("state = %s after refused lease, want idle", m.State())
	}
}

// The clean lease cycle: LEASE_WRITE -> leasing -> WRITTEN -> idle;
// LEASE_READ -> leasing -> READ_OK -> idle. Payload pointers ride on
// the wire command AND the record, and unset stays nil elsewhere.
func TestLeaseCycle(t *testing.T) {
	m, fc := newHandoffMachine(t)

	cmd, err := m.SendLease(CmdLeaseWrite, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.State() != Leasing {
		t.Fatalf("state = %s, want leasing", m.State())
	}
	if cmd.Block == nil || *cmd.Block != 7 || cmd.Gen == nil || *cmd.Gen != 1 {
		t.Errorf("lease wire payload = %v/%v, want 7/1", cmd.Block, cmd.Gen)
	}
	fc.Add(20 * time.Millisecond)
	if err := m.Ack(ackOf(m, AckWritten)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Idle {
		t.Fatalf("state = %s after WRITTEN, want idle", m.State())
	}

	cmd, err = m.SendLease(CmdLeaseRead, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	fc.Add(15 * time.Millisecond)
	a := ackOf(m, AckReadOK)
	a.Checks, a.Bytes = u64(16384), u64(65536)
	if err := m.Ack(a); err != nil {
		t.Fatal(err)
	}
	if m.State() != Idle {
		t.Fatalf("state = %s after READ_OK, want idle", m.State())
	}

	recs := m.Records()
	var leaseRecs []Record
	for _, r := range recs {
		if r.Cmd == CmdLeaseWrite || r.Cmd == CmdLeaseRead {
			leaseRecs = append(leaseRecs, r)
		}
	}
	if len(leaseRecs) != 2 {
		t.Fatalf("lease records = %d, want 2", len(leaseRecs))
	}
	for i, want := range []struct {
		block, gen uint64
		ack        Kind
	}{{7, 1, AckWritten}, {7, 1, AckReadOK}} {
		r := leaseRecs[i]
		if r.Block != want.block || r.Gen != want.gen || r.Ack != want.ack || r.Deviation != "" {
			t.Errorf("lease record %d = %+v, want block=%d gen=%d ack=%s clean", i, r, want.block, want.gen, want.ack)
		}
	}
	// Non-lease records never claim a payload: zero stays zero and
	// the kind says the fields do not mean anything there.
	for _, r := range recs {
		if r.Cmd == CmdPrepare && (r.Block != 0 || r.Gen != 0) {
			t.Errorf("prepare record carries payload %d/%d", r.Block, r.Gen)
		}
	}
}

// Wrong ack for a lease is an "unexpected ack" deviation and the
// state does not move: a READ_OK answering a LEASE_WRITE is a wiring
// bug, not a transition.
func TestLeaseWrongAckKindDeviation(t *testing.T) {
	m, _ := newHandoffMachine(t)
	if _, err := m.SendLease(CmdLeaseWrite, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(ackOf(m, AckReadOK)); err != nil {
		t.Fatal(err)
	}
	recs := m.Records()
	last := recs[len(recs)-1]
	if last.Deviation == "" {
		t.Error("wrong ack kind recorded clean")
	}
	if m.State() != Leasing {
		t.Errorf("state = %s after wrong ack, want leasing", m.State())
	}
	// The right ack afterwards still completes the step.
	if err := m.Ack(ackOf(m, AckWritten)); err != nil {
		t.Fatal(err)
	}
	if m.State() != Idle {
		t.Errorf("state = %s, want idle", m.State())
	}
}

// A missed lease deadline goes Stalled exactly like every other
// transition: one deviation record, no retry, only STOP from there.
func TestLeaseMissedDeadlineStalls(t *testing.T) {
	m, fc := newHandoffMachine(t)
	m.timeouts.Lease = time.Second
	if _, err := m.SendLease(CmdLeaseRead, 1, 1); err != nil {
		t.Fatal(err)
	}
	fc.Add(2 * time.Second)
	rec := m.Tick()
	if rec == nil || rec.Deviation != "missed deadline" {
		t.Fatalf("tick record = %+v, want one missed-deadline record", rec)
	}
	if m.State() != Stalled {
		t.Fatalf("state = %s, want stalled", m.State())
	}
	if _, err := m.SendLease(CmdLeaseWrite, 2, 1); err == nil {
		t.Error("lease accepted from stalled")
	}
	if _, err := m.Send(CmdStop); err != nil {
		t.Errorf("stop from stalled: %v", err)
	}
}

// STOP supersedes an outstanding lease (the cancellation path), and
// duplicate/stale acks around leases are classified, not eaten.
func TestLeaseStopAndAckClassification(t *testing.T) {
	m, _ := newHandoffMachine(t)
	cmd, err := m.SendLease(CmdLeaseWrite, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Re-ack the pending one twice: first applies, second is a
	// duplicate deviation. The acks are built from the stamped seq
	// because after the first ack there is no pending command left
	// to echo.
	dup := Ack{V: ProtocolVersion, Seq: cmd.Seq, Kind: AckWritten}
	if err := m.Ack(dup); err != nil {
		t.Fatal(err)
	}
	if err := m.Ack(dup); err != nil {
		t.Fatal(err)
	}
	recs := m.Records()
	last := recs[len(recs)-1]
	if last.Deviation != "duplicate ack" {
		t.Errorf("second WRITTEN deviation = %q, want duplicate ack", last.Deviation)
	}
	// Stale ack for an old seq: classified.
	if err := m.Ack(Ack{V: ProtocolVersion, Seq: cmd.Seq - 99, Kind: AckReadOK}); err != nil {
		t.Fatal(err)
	}
	recs = m.Records()
	if recs[len(recs)-1].Deviation != "stale ack" {
		t.Errorf("stale classification = %q", recs[len(recs)-1].Deviation)
	}
	// STOP while a lease is pending supersedes it.
	if _, err := m.SendLease(CmdLeaseRead, 4, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(CmdStop); err != nil {
		t.Errorf("stop while leasing: %v", err)
	}
	if m.State() != Stopping {
		t.Errorf("state = %s, want stopping", m.State())
	}
}

// SendLease refuses non-lease kinds: the payload stamping path is
// only for leases, never a general Send with hidden fields.
func TestSendLeaseRejectsNonLeaseKind(t *testing.T) {
	m, _ := newHandoffMachine(t)
	if _, err := m.SendLease(CmdTrigger, 1, 1); err == nil {
		t.Error("SendLease accepted TRIGGER")
	}
}
