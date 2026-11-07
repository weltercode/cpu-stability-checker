package transition

import (
	"strings"
	"testing"
)

func newTestArbiter(t *testing.T, blocks ...uint64) *Arbiter {
	t.Helper()
	a, err := NewArbiter(blocks)
	if err != nil {
		t.Fatalf("NewArbiter: %v", err)
	}
	return a
}

// writeCycle drives one block from free to published and asserts the
// ledger states at every step; readCycle takes it back to free.
func writeCycle(t *testing.T, a *Arbiter, block, seq, wantGen uint64) {
	t.Helper()
	lw, err := a.Reserve(CmdLeaseWrite, block)
	if err != nil {
		t.Fatalf("reserve write(%d): %v", block, err)
	}
	if lw.Gen != wantGen {
		t.Errorf("write gen = %d, want %d", lw.Gen, wantGen)
	}
	if err := a.Issue(seq, lw); err != nil {
		t.Fatalf("issue write: %v", err)
	}
	if st, _ := a.StateOf(block); st != BlockWriting {
		t.Fatalf("state = %s, want writing", st)
	}
	if err := a.Complete(seq, AckWritten); err != nil {
		t.Fatalf("complete write: %v", err)
	}
	if st, _ := a.StateOf(block); st != BlockPublished {
		t.Fatalf("state = %s, want published", st)
	}
}

func readCycle(t *testing.T, a *Arbiter, block, seq, wantGen uint64) {
	t.Helper()
	lr, err := a.Reserve(CmdLeaseRead, block)
	if err != nil {
		t.Fatalf("reserve read(%d): %v", block, err)
	}
	if lr.Gen != wantGen {
		t.Errorf("read gen = %d, want %d (reads never advance the generation)", lr.Gen, wantGen)
	}
	if err := a.Issue(seq, lr); err != nil {
		t.Fatalf("issue read: %v", err)
	}
	if st, _ := a.StateOf(block); st != BlockReading {
		t.Fatalf("state = %s, want reading", st)
	}
	if err := a.Complete(seq, AckReadOK); err != nil {
		t.Fatalf("complete read: %v", err)
	}
	if st, _ := a.StateOf(block); st != BlockFree {
		t.Fatalf("state = %s, want free", st)
	}
}

// The core P07e invariant: a reader may never be granted an
// unpublished block - the race the milestone exists to eliminate is
// refused at the ledger, before any worker is asked.
func TestArbiterRefusesReadBeforeWrite(t *testing.T) {
	a := newTestArbiter(t, 0, 1)
	_, err := a.Reserve(CmdLeaseRead, 0)
	if err == nil {
		t.Fatal("read lease on a free block was accepted")
	}
	if !strings.Contains(err.Error(), "unpublished") {
		t.Errorf("refusal = %v, want it to name the unpublished rule", err)
	}
}

// One writer at a time: double reserve, write-over-published and
// write-over-reading are all refused. Re-writing after the read
// completes is legal and advances the generation.
func TestArbiterSingleWriterInvariant(t *testing.T) {
	a := newTestArbiter(t, 5)
	l1, err := a.Reserve(CmdLeaseWrite, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve(CmdLeaseWrite, 5); err == nil {
		t.Error("double reserve on one block accepted")
	}
	if err := a.Issue(11, l1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve(CmdLeaseWrite, 5); err == nil {
		t.Error("second write lease while writing accepted")
	}
	if err := a.Complete(11, AckWritten); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve(CmdLeaseWrite, 5); err == nil {
		t.Error("write lease over a published block accepted (must complete the read cycle first)")
	}
	readCycle(t, a, 5, 21, 1) // the published block goes through a read first
	// Now free again: the next write cycle carries generation 2.
	writeCycle(t, a, 5, 31, 2)
	readCycle(t, a, 5, 32, 2)
}

// A reservation that never became a command changes nothing: the
// generation stays, Cancel frees the block, and Issue of a mismatched
// lease is refused.
func TestArbiterReservationRollback(t *testing.T) {
	a := newTestArbiter(t, 9)
	l, err := a.Reserve(CmdLeaseWrite, 9)
	if err != nil {
		t.Fatal(err)
	}
	if a.Generation(9) != 0 {
		t.Errorf("generation advanced on a mere reservation: %d", a.Generation(9))
	}
	// Issue with a tampered lease (wrong gen) is refused.
	bad := l
	bad.Gen = 99
	if err := a.Issue(1, bad); err == nil {
		t.Error("issue of a mismatched lease accepted")
	}
	if st, _ := a.StateOf(9); st != BlockFree {
		t.Errorf("state = %s after refused issue, want free", st)
	}
	a.Cancel(9)
	l2, err := a.Reserve(CmdLeaseWrite, 9)
	if err != nil {
		t.Fatalf("reserve after cancel: %v", err)
	}
	// Issue then Cancel must not resurrect the block: issued leases
	// are retired only by Complete or Taint.
	if err := a.Issue(2, l2); err != nil {
		t.Fatal(err)
	}
	a.Cancel(9) // no-op: the reservation was consumed by Issue
	if st, _ := a.StateOf(9); st != BlockWriting {
		t.Errorf("state = %s after cancel of an issued lease, want writing", st)
	}
}

// Complete is strict: unknown seq, and an ack kind that does not
// match the lease mode, are wiring bugs refused loudly.
func TestArbiterCompleteStrictness(t *testing.T) {
	a := newTestArbiter(t, 3)
	if err := a.Complete(42, AckWritten); err == nil {
		t.Error("Complete of an unknown seq accepted")
	}
	l, _ := a.Reserve(CmdLeaseWrite, 3)
	if err := a.Issue(7, l); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(7, AckReadOK); err == nil {
		t.Error("READ_OK completing a write lease accepted")
	}
	if err := a.Complete(7, AckWritten); err != nil {
		t.Errorf("correct Complete: %v", err)
	}
	if err := a.Complete(7, AckWritten); err == nil {
		t.Error("double Complete accepted")
	}
}

// A failed or stalled lease makes its block radioactive: no further
// lease touches it, other blocks keep working, and the reason rides
// for the journal.
func TestArbiterTaint(t *testing.T) {
	a := newTestArbiter(t, 1, 2)
	l, _ := a.Reserve(CmdLeaseWrite, 1)
	if err := a.Issue(5, l); err != nil {
		t.Fatal(err)
	}
	if err := a.TaintSeq(5, "worker stalled mid-write"); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.StateOf(1); st != BlockTainted {
		t.Errorf("state = %s, want tainted", st)
	}
	if a.TaintReason(1) != "worker stalled mid-write" {
		t.Errorf("taint reason = %q", a.TaintReason(1))
	}
	if _, err := a.Reserve(CmdLeaseWrite, 1); err == nil {
		t.Error("lease on a tainted block accepted")
	}
	if _, err := a.Reserve(CmdLeaseRead, 1); err == nil {
		t.Error("read lease on a tainted block accepted")
	}
	// The other block is untouched: the ledger quarantines, it does
	// not punish the whole phase.
	lh, err := a.Reserve(CmdLeaseWrite, 2)
	if err != nil {
		t.Fatalf("healthy block refused: %v", err)
	}
	// TaintReservation covers the send-path-died case: publish the
	// block first, then reserve a read that will never be issued.
	if err := a.Issue(6, lh); err != nil {
		t.Fatal(err)
	}
	if err := a.Complete(6, AckWritten); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve(CmdLeaseRead, 2); err != nil {
		t.Fatalf("reserve read on published block: %v", err)
	}
	if err := a.TaintReservation(2, "transport died"); err != nil {
		t.Fatal(err)
	}
	if st, _ := a.StateOf(2); st != BlockTainted {
		t.Errorf("state = %s, want tainted", st)
	}
	if err := a.TaintReservation(2, ""); err == nil {
		t.Error("taint of a block without a reservation accepted")
	}
}

// Ledger identity rules: duplicate block ids are refused at
// construction (two entries for one block would allow two writers),
// unknown blocks are refused everywhere, and an empty ledger is a
// wiring bug, not a vacuous pass.
func TestArbiterIdentity(t *testing.T) {
	if _, err := NewArbiter([]uint64{1, 2, 1}); err == nil {
		t.Error("duplicate block ids accepted")
	}
	if _, err := NewArbiter(nil); err == nil {
		t.Error("empty arbiter accepted")
	}
	a := newTestArbiter(t, 10, 11)
	if _, err := a.Reserve(CmdLeaseWrite, 77); err == nil {
		t.Error("lease on an untracked block accepted")
	}
	if _, err := a.StateOf(77); err == nil {
		t.Error("state of an untracked block reported")
	}
	if got := a.Blocks(); len(got) != 2 || got[0] != 10 || got[1] != 11 {
		t.Errorf("Blocks = %v, want grant order [10 11]", got)
	}
	// Non-lease modes are refused, not coerced.
	if _, err := a.Reserve(CmdTrigger, 10); err == nil {
		t.Error("Reserve accepted a non-lease mode")
	}
	// Outstanding tracks issued leases.
	l, _ := a.Reserve(CmdLeaseWrite, 10)
	if a.Outstanding() != 0 {
		t.Error("reservation counted as outstanding")
	}
	if err := a.Issue(1, l); err != nil {
		t.Fatal(err)
	}
	if a.Outstanding() != 1 {
		t.Errorf("outstanding = %d, want 1", a.Outstanding())
	}
	if err := a.Issue(1, l); err == nil {
		t.Error("double issue under one seq accepted")
	}
	if err := a.Complete(1, AckWritten); err != nil {
		t.Fatal(err)
	}
	if a.Outstanding() != 0 {
		t.Errorf("outstanding = %d after complete, want 0", a.Outstanding())
	}
}
