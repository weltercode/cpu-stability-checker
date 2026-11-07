package transition

// Arbiter is the parent-side ownership ledger for a P07e handoff
// session. It is the single arbiter: workers never negotiate
// ownership with each other - every lease is granted, tracked and
// retired here - so there is no distributed race to lose. The
// invariant it enforces is the whole point of P07e: a reader is only
// ever granted a block after the writer's WRITTEN ack has been seen
// by this parent. A race inside the tester can never be mistaken for
// a hardware error, because the tester never races.
//
// The hardware half of the guarantee (the writer flushing its write
// buffers before the WRITTEN ack, the reader flushing after the
// LEASE_READ command) is the worker side's obligation (P07e3); the
// arbiter is what makes the ordering observable and enforceable from
// the parent.
//
// Like Machine, it is a pure bookkeeper over explicit events and is
// not safe for concurrent use: the parent's session loop is the
// single owner.

import "fmt"

// BlockState is the ledger state of one block.
type BlockState string

const (
	BlockFree      BlockState = "free"      // nobody holds it; a write lease may claim it
	BlockWriting   BlockState = "writing"   // exactly one writer holds it (unpublished)
	BlockPublished BlockState = "published" // written and WRITTEN-acked: readable, not writable
	BlockReading   BlockState = "reading"   // exactly one reader holds it
	BlockTainted   BlockState = "tainted"   // a lease on it failed or stalled: no further leases, the parent decides
)

// Lease is one ownership grant. Gen is stamped by the arbiter: every
// WRITE lease bumps the block's generation, and the generation rides
// into the command payload so block content can carry its own
// identity claim (the ram-copy lesson: identity in the bytes, not
// beside them).
type Lease struct {
	Block uint64
	Gen   uint64
	Mode  Kind // CmdLeaseWrite or CmdLeaseRead
}

type Arbiter struct {
	blocks []uint64
	known  map[uint64]bool
	state  map[uint64]BlockState
	gen    map[uint64]uint64
	reason map[uint64]string // taint reasons, kept for the journal
	// reserved holds leases validated but not yet stamped onto a
	// command: Reserve validates, Issue commits once the machine
	// has stamped the seq, Cancel rolls back when the send path
	// failed. Without this split a failed Send could leave the
	// ledger claiming a command that was never written.
	reserved map[uint64]*Lease
	out      map[uint64]*Lease // command seq -> issued lease
}

// NewArbiter tracks exactly these block ids. Duplicate ids are
// refused loudly: two ledger entries for one block would let two
// writers hold it at once, the one failure mode this whole layer
// exists to prevent.
func NewArbiter(blocks []uint64) (*Arbiter, error) {
	a := &Arbiter{
		known:    make(map[uint64]bool, len(blocks)),
		state:    make(map[uint64]BlockState, len(blocks)),
		gen:      make(map[uint64]uint64, len(blocks)),
		reason:   make(map[uint64]string),
		reserved: make(map[uint64]*Lease),
		out:      make(map[uint64]*Lease),
	}
	for _, b := range blocks {
		if a.known[b] {
			return nil, fmt.Errorf("transition: arbiter: block %d listed twice", b)
		}
		a.known[b] = true
		a.state[b] = BlockFree
		a.blocks = append(a.blocks, b)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("transition: arbiter: no blocks to arbitrate")
	}
	return a, nil
}

// Blocks lists the tracked ids in grant order.
func (a *Arbiter) Blocks() []uint64 { return append([]uint64(nil), a.blocks...) }

// StateOf reports the ledger state of one block.
func (a *Arbiter) StateOf(block uint64) (BlockState, error) {
	if !a.known[block] {
		return "", fmt.Errorf("transition: arbiter: block %d is not tracked", block)
	}
	return a.state[block], nil
}

// Generation is the block's current generation: 0 until the first
// write lease is ISSUED (a reservation alone does not advance it).
func (a *Arbiter) Generation(block uint64) uint64 { return a.gen[block] }

// TaintReason is why a block went tainted ("" when it did not).
func (a *Arbiter) TaintReason(block uint64) string { return a.reason[block] }

// Outstanding is the number of issued leases still awaiting their ack.
func (a *Arbiter) Outstanding() int { return len(a.out) }

// Reserve validates the next lease against the ledger and holds it
// pending the command stamp. A write lease needs a free block; a read
// lease needs a published one - reading an unpublished block is
// exactly the race P07e forbids, and it is refused here, before any
// worker is ever asked.
func (a *Arbiter) Reserve(mode Kind, block uint64) (Lease, error) {
	if !a.known[block] {
		return Lease{}, fmt.Errorf("transition: arbiter: block %d is not tracked", block)
	}
	if _, busy := a.reserved[block]; busy {
		return Lease{}, fmt.Errorf("transition: arbiter: block %d already reserved", block)
	}
	switch mode {
	case CmdLeaseWrite:
		if a.state[block] != BlockFree {
			return Lease{}, fmt.Errorf("transition: arbiter: write lease on block %d needs state %s, block is %s",
				block, BlockFree, a.state[block])
		}
		l := Lease{Block: block, Gen: a.gen[block] + 1, Mode: mode}
		a.reserved[block] = &l
		return l, nil
	case CmdLeaseRead:
		if a.state[block] != BlockPublished {
			return Lease{}, fmt.Errorf("transition: arbiter: read lease on block %d needs state %s, block is %s "+
				"(a reader may never touch an unpublished block)", block, BlockPublished, a.state[block])
		}
		l := Lease{Block: block, Gen: a.gen[block], Mode: mode}
		a.reserved[block] = &l
		return l, nil
	default:
		return Lease{}, fmt.Errorf("transition: arbiter: %s is not a lease mode", mode)
	}
}

// Cancel releases a reservation whose send path failed: the ledger
// never claims a command that was never stamped.
func (a *Arbiter) Cancel(block uint64) {
	delete(a.reserved, block)
}

// Issue commits a reserved lease under the command seq the machine
// stamped. The generation only advances here: a reservation that
// never became a command changes nothing.
func (a *Arbiter) Issue(seq uint64, l Lease) error {
	r, ok := a.reserved[l.Block]
	if !ok || *r != l {
		return fmt.Errorf("transition: arbiter: issue of block %d gen %d (%s) does not match its reservation",
			l.Block, l.Gen, l.Mode)
	}
	if _, dup := a.out[seq]; dup {
		return fmt.Errorf("transition: arbiter: seq %d already issued", seq)
	}
	delete(a.reserved, l.Block)
	lc := l
	a.out[seq] = &lc
	switch l.Mode {
	case CmdLeaseWrite:
		a.state[l.Block] = BlockWriting
		a.gen[l.Block] = l.Gen
	case CmdLeaseRead:
		a.state[l.Block] = BlockReading
	}
	return nil
}

// Complete applies a WRITTEN or READ_OK ack. WRITTEN publishes the
// block at its generation (readers may now be granted); READ_OK
// retires the read and the block is free again. A mismatched ack kind
// is a wiring bug and is refused, never patched.
func (a *Arbiter) Complete(seq uint64, kind Kind) error {
	l, ok := a.out[seq]
	if !ok {
		return fmt.Errorf("transition: arbiter: Complete(seq=%d, %s): no outstanding lease for that seq", seq, kind)
	}
	want := ackFor(l.Mode)
	if kind != want {
		return fmt.Errorf("transition: arbiter: Complete(seq=%d): lease %s expects %s, got %s", seq, l.Mode, want, kind)
	}
	delete(a.out, seq)
	switch l.Mode {
	case CmdLeaseWrite:
		a.state[l.Block] = BlockPublished
	case CmdLeaseRead:
		a.state[l.Block] = BlockFree
	}
	return nil
}

// TaintSeq marks the block of an issued lease that failed or stalled:
// no further lease may touch it, and the reason rides into the
// journal. The parent decides what the session does next (the
// machine's Stalled path already refuses everything but STOP).
func (a *Arbiter) TaintSeq(seq uint64, why string) error {
	l, ok := a.out[seq]
	if !ok {
		return fmt.Errorf("transition: arbiter: TaintSeq(%d): no outstanding lease for that seq", seq)
	}
	delete(a.out, seq)
	a.state[l.Block] = BlockTainted
	if why != "" {
		a.reason[l.Block] = why
	}
	return nil
}

// TaintReservation taints a block whose reserved-but-unstamped lease
// is being abandoned (the send path died mid-handoff). Same effect as
// TaintSeq: the block is radioactive until the parent decides.
func (a *Arbiter) TaintReservation(block uint64, why string) error {
	if _, ok := a.reserved[block]; !ok {
		return fmt.Errorf("transition: arbiter: TaintReservation(%d): no reservation for that block", block)
	}
	delete(a.reserved, block)
	a.state[block] = BlockTainted
	if why != "" {
		a.reason[block] = why
	}
	return nil
}
