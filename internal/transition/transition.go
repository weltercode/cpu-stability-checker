// Package transition is the parent-side state machine for P08 idle<->load
// sessions: one prepared worker process serves many verified bursts instead
// of being re-spawned (and re-allocating) per phase.
//
// The machine is a pure reducer over (state, event): commands it emits are
// timestamped BEFORE the caller writes them to the transport (the plan's
// "mark the trigger before sending"), acknowledgements are classified
// honestly (late, duplicate, stale, unexpected, missed deadline) and every
// deviation is recorded, never dropped. It never resends and never "catches
// up": a missed deadline is an observation, and the scheduling policy above
// it shifts the next window instead of busy-looping.
//
// The wire types (Command/Ack) are the versioned JSONL protocol for the
// session channel (v1): the parent writes Commands to the worker's stdin,
// the worker writes Acks to its stdout. The legacy rotate-mode S/R lines
// stay compatible; sessions use this richer channel instead.
package transition

import (
	"fmt"
	"time"
)

// ProtocolVersion stamps every wire line: a reader that does not
// understand v1 must refuse, never guess.
const ProtocolVersion = 1

// Kind is a command or an acknowledgement kind.
type Kind string

const (
	// Commands (parent -> worker).
	CmdPrepare Kind = "PREPARE" // allocate engine regions, first-touch, warm the kernel: once per session
	CmdTrigger Kind = "TRIGGER" // start a verified burst on this worker
	CmdVerify  Kind = "VERIFY"  // close the current verifiable block and report its counts
	CmdIdle    Kind = "IDLE"    // stop loading and return to blocking idle (the running burst stays incomplete)
	CmdStop    Kind = "STOP"    // end the session: release everything and exit

	// Handoff commands (P07e): block-ownership leases, legal only on
	// sessions whose machine opted into leases (AllowLeases). The
	// parent is the single arbiter - workers never negotiate
	// ownership with each other, so there is no distributed race to
	// lose. These kinds are additive to v1: parent and worker are
	// the same executable, so version skew inside one run is
	// impossible, and a worker that does not implement them answers
	// ERROR, which the machine records as a deviation.
	CmdLeaseWrite Kind = "LEASE_WRITE" // grant write ownership of one block (payload: block+gen)
	CmdLeaseRead  Kind = "LEASE_READ"  // grant read ownership of a published block (payload: block+gen)

	// Acknowledgements (worker -> parent), echoing the command's Seq.
	// STARTED is stamped INSIDE the worker at burst start; VERIFIED
	// after a block whose data was actually checked.
	AckReady    Kind = "READY"    // PREPARE done: regions allocated, touched, kernel ready
	AckIdle     Kind = "IDLE_ACK" // now blocking idle (no busy spin)
	AckStarted  Kind = "STARTED"  // burst running inside the worker
	AckVerified Kind = "VERIFIED" // one verifiable block completed and checked
	AckStopped  Kind = "STOPPED"  // session over, resources released
	// WRITTEN and READ_OK close the handoff leases. WRITTEN means
	// the worker published the block (its stores were flushed with
	// the documented Windows mechanism BEFORE this ack - the worker
	// side's obligation, P07e3); READ_OK means the published block
	// was verified clean. The ack itself returns the worker to
	// blocking idle: no unsolicited IDLE_ACK follows a lease ack.
	AckWritten Kind = "WRITTEN" // lease write done: block published at its generation
	AckReadOK  Kind = "READ_OK" // lease read done: published block verified clean
	// AckError reports a failed command or a fault (msg carries the
	// reason): an allocation refusal is a setup failure (exit 2), a
	// kernel mismatch is a fault (exit 1) - the machine records the
	// deviation either way and the parent decides cleanup. It is
	// never a silent retry.
	AckError Kind = "ERROR"
)

// State is the parent's view of one worker session. It is what the
// parent SAW (commands sent, acks received): it never claims a hardware
// state the OS never confirmed (a blocked worker is not proof of a
// C-state, and no parked flag means no park observation).
type State string

const (
	New       State = "new"       // spawned, nothing prepared
	Preparing State = "preparing" // PREPARE sent, awaiting READY
	Ready     State = "ready"     // READY acked; the worker enters idle on its own
	Idle      State = "idle"      // IDLE_ACK received: blocking, no load
	Triggered State = "triggered" // TRIGGER sent, awaiting STARTED
	Running   State = "running"   // STARTED acked: load active inside the worker
	Verifying State = "verifying" // VERIFY sent, awaiting VERIFIED
	Leasing   State = "leasing"   // a handoff lease sent, awaiting WRITTEN/READ_OK (P07e)
	Stopping  State = "stopping"  // STOP sent, awaiting STOPPED
	Stopped   State = "stopped"   // STOPPED acked: the session is over
	Stalled   State = "stalled"   // a deadline passed without the ack: honest, not failed
)

// Command is a session command on the wire. CommandedAt is stamped by
// the machine when Send is called - before the caller writes the line -
// so the trigger timestamp never includes the transport write.
// Block/Gen ride only on handoff leases (nil elsewhere: unset stays
// null on the wire, never a fabricated zero).
type Command struct {
	V           int       `json:"v"`
	Seq         uint64    `json:"seq"`
	Burst       string    `json:"burst,omitempty"`
	Kind        Kind      `json:"cmd"`
	Block       *uint64   `json:"block,omitempty"`
	Gen         *uint64   `json:"gen,omitempty"`
	CommandedAt time.Time `json:"cmd_at"`
}

// Ack is a worker acknowledgement on the wire. Seq echoes the command
// it answers; Checks/Bytes ride only on VERIFIED and nil means the
// worker did not report them (unknown, never zero).
type Ack struct {
	V      int     `json:"v"`
	Seq    uint64  `json:"seq"`
	Burst  string  `json:"burst,omitempty"`
	Kind   Kind    `json:"ack"`
	Checks *uint64 `json:"checks,omitempty"`
	Bytes  *uint64 `json:"bytes,omitempty"`
	Msg    string  `json:"msg,omitempty"` // ERROR acks only: the refusal reason, verbatim
}

// Record is one observed step of the session: a command and (when it
// arrived) its ack, with the deviation classification. Records are the
// journal material: commanded vs acked times are kept apart because
// the plan demands the commanded transition and the observation be
// distinguishable in the report. Block/Gen are meaningful only when
// Cmd is a lease kind (the kind says whether they mean anything);
// they ride so the handoff journal can name block, generation and
// seq together.
type Record struct {
	Burst       string
	Seq         uint64
	Cmd         Kind
	Ack         Kind
	State       State // machine state after the event
	Block       uint64
	Gen         uint64
	CommandedAt time.Time
	AckedAt     time.Time
	Deviation   string // "" = clean transition
}

// Clock lets tests drive deadlines deterministically.
type Clock interface {
	Now() time.Time
}

// RealClock is the production clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// Timeouts bounds each transition's wait for its ack. Zero means the
// default for that command. Prepare is generous on purpose: allocation
// plus first touch of gigabytes legitimately takes minutes, and a
// timeout there must never fire mid-preparation.
type Timeouts struct {
	Prepare, Trigger, Verify, Idle, Stop time.Duration
	Lease                                time.Duration // handoff lease: one block operation, generous like Verify
}

// DefaultTimeouts are starting points, not guarantees: the plan says
// windows from tens of milliseconds to seconds first, limits measured
// later.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Prepare: 10 * time.Minute,
		Trigger: 10 * time.Second,
		Verify:  2 * time.Minute,
		Idle:    10 * time.Second,
		Stop:    30 * time.Second,
		Lease:   2 * time.Minute,
	}
}

// ForKind is the armed deadline for a command kind (zero fields fall
// back to the documented defaults).
func (t Timeouts) ForKind(k Kind) time.Duration {
	var d time.Duration
	switch k {
	case CmdPrepare:
		d = t.Prepare
	case CmdTrigger:
		d = t.Trigger
	case CmdVerify:
		d = t.Verify
	case CmdIdle:
		d = t.Idle
	case CmdStop:
		d = t.Stop
	case CmdLeaseWrite, CmdLeaseRead:
		d = t.Lease
	}
	if d <= 0 {
		d = DefaultTimeouts().forKind(k)
	}
	return d
}

func (t Timeouts) forKind(k Kind) time.Duration { return t.ForKind(k) }

// ackFor maps a command to the ack that completes it.
func ackFor(k Kind) Kind {
	switch k {
	case CmdPrepare:
		return AckReady
	case CmdTrigger:
		return AckStarted
	case CmdVerify:
		return AckVerified
	case CmdIdle:
		return AckIdle
	case CmdStop:
		return AckStopped
	case CmdLeaseWrite:
		return AckWritten
	case CmdLeaseRead:
		return AckReadOK
	}
	return ""
}

// allowed maps each state to the commands legal from it. STOP is legal
// from every live state: cancellation must always have an exit. The
// table is deliberately strict: TRIGGER only from idle (a worker that
// never reached idle was never prepared to run), VERIFY only while a
// burst is active (verifying nothing would fabricate coverage), and
// from Stalled only STOP (the parent decides what a stalled session
// does; the machine never invents recovery).
//
// Handoff leases are legal only from Idle AND only on a session that
// opted into them (AllowLeases): a lease on an ordinary burst session
// is a wiring bug and is refused, never silently accepted. Leasing
// accepts only STOP: the worker is mid-operation and cannot take
// another command until its ack lands.
func allowed(s State, handoff bool) []Kind {
	switch s {
	case New:
		return []Kind{CmdPrepare, CmdStop}
	case Preparing, Ready:
		return []Kind{CmdStop}
	case Idle:
		if handoff {
			return []Kind{CmdTrigger, CmdLeaseWrite, CmdLeaseRead, CmdStop}
		}
		return []Kind{CmdTrigger, CmdStop}
	case Triggered:
		return []Kind{CmdVerify, CmdIdle, CmdStop}
	case Running:
		return []Kind{CmdVerify, CmdIdle, CmdStop}
	case Verifying:
		return []Kind{CmdIdle, CmdStop}
	case Leasing:
		return []Kind{CmdStop}
	case Stalled:
		return []Kind{CmdStop}
	case Stopping:
		return []Kind{CmdStop}
	case Stopped:
		return nil
	}
	return nil
}

// sentState is the state a command moves the machine into while its
// ack is outstanding: the parent's view of "I asked, no confirmation
// yet". IDLE keeps Running: there is no honest intermediate between
// "load active" and "IDLE_ACK received".
func sentState(k Kind) (State, bool) {
	switch k {
	case CmdPrepare:
		return Preparing, true
	case CmdTrigger:
		return Triggered, true
	case CmdVerify:
		return Verifying, true
	case CmdStop:
		return Stopping, true
	case CmdLeaseWrite, CmdLeaseRead:
		return Leasing, true
	}
	return "", false
}

// Machine is one worker session's state machine. It is not safe for
// concurrent use: the parent's session loop is the single owner.
type Machine struct {
	clock    Clock
	timeouts Timeouts
	session  string
	seq      uint64
	burst    uint64
	state    State
	pending  *Command
	// handoff opts this session into the P07e lease commands. It is
	// set once by the coordinator that owns the block ledger; without
	// it the lease kinds are illegal and refused.
	handoff bool
	// lastDone remembers each completed command's ack kind by seq, so
	// duplicates and stale re-sends are classified, not silently eaten.
	lastDone map[uint64]Kind
	records  []Record
}

// NewMachine starts a session in state New. The session string labels
// records (it rides into the journal in C3).
func NewMachine(session string, clock Clock, timeouts Timeouts) *Machine {
	if clock == nil {
		clock = RealClock{}
	}
	return &Machine{
		clock: clock, timeouts: timeouts, session: session,
		state: New, lastDone: map[uint64]Kind{},
	}
}

func (m *Machine) State() State    { return m.state }
func (m *Machine) Session() string { return m.session }
func (m *Machine) Records() []Record {
	return append([]Record(nil), m.records...)
}

// AllowLeases opts this session into the handoff lease commands
// (P07e). Call it once, before any lease; it never removes the
// option, and ordinary sessions never call it.
func (m *Machine) AllowLeases() { m.handoff = true }

// Send validates a command against the state table, stamps it with the
// clock BEFORE returning (the caller writes the line afterwards), and
// arms the transition deadline. A command illegal in the current state
// is an error: the machine never jumps states quietly.
func (m *Machine) Send(kind Kind) (Command, error) {
	ok := false
	for _, k := range allowed(m.state, m.handoff) {
		if k == kind {
			ok = true
			break
		}
	}
	if !ok {
		return Command{}, fmt.Errorf("transition: %s: command %s not legal from state %s", m.session, kind, m.state)
	}
	if m.pending != nil && kind != CmdStop {
		// One outstanding command at a time: the worker's ack ordering
		// is sequential, and a second send would make "which ack
		// answers which command" ambiguous. STOP while pending is the
		// cancellation path and is allowed (it supersedes).
		return Command{}, fmt.Errorf("transition: %s: %s still awaiting %s", m.session, m.pending.Kind, ackFor(m.pending.Kind))
	}
	m.seq++
	cmd := Command{
		V: ProtocolVersion, Seq: m.seq, Kind: kind,
		CommandedAt: m.clock.Now(), // marked BEFORE the transport write
	}
	if kind == CmdTrigger {
		m.burst++
		cmd.Burst = fmt.Sprintf("%s-b%d", m.session, m.burst)
	}
	m.pending = &cmd
	if s, ok := sentState(kind); ok {
		m.state = s
	}
	m.records = append(m.records, Record{
		Burst: cmd.Burst, Seq: cmd.Seq, Cmd: kind,
		State: m.state, CommandedAt: cmd.CommandedAt,
	})
	return cmd, nil
}

// SendLease stamps a handoff lease command with its block and
// generation payload (the arbiter's grant, see arbiter.go). The
// payload pointers make "unset stays null" impossible here by
// construction: a lease command without a block is not a thing this
// machine can produce.
func (m *Machine) SendLease(kind Kind, block, gen uint64) (Command, error) {
	if kind != CmdLeaseWrite && kind != CmdLeaseRead {
		return Command{}, fmt.Errorf("transition: %s: %s is not a lease kind", m.session, kind)
	}
	cmd, err := m.Send(kind)
	if err != nil {
		return Command{}, err
	}
	b, g := block, gen
	cmd.Block, cmd.Gen = &b, &g
	m.pending.Block, m.pending.Gen = cmd.Block, cmd.Gen // wire and ledger see one identity
	// The record entry was appended by Send with zero payload; fill
	// the same entry (it is the pending one) so journal and wire
	// agree on one identity.
	rec := &m.records[m.pendingIdx()]
	rec.Block, rec.Gen = block, gen
	return cmd, nil
}

// pendingIdx is the record index of the pending command's entry, so the
// ack can complete that record instead of appending a new one.
func (m *Machine) pendingIdx() int { return len(m.records) - 1 }

// WorkerError is a worker refusal or fault surfaced through an ERROR
// ack: the reason is the worker's own message, verbatim. Callers
// classify with errors.As - never by string matching, which breaks
// the moment a message changes.
type WorkerError struct{ Msg string }

func (e *WorkerError) Error() string { return "worker error: " + e.Msg }

// Ack classifies and applies a worker acknowledgement:
//   - unknown seq: "stale ack" deviation, state unchanged
//   - seq already completed: "duplicate ack" deviation, state unchanged
//   - right seq, wrong kind: "unexpected ack" deviation, state unchanged
//   - right seq and kind: applied; if a deadline was already missed for
//     it, the deviation "late ack after missed deadline" is recorded
//     and the transition still completes (the worker DID respond).
//   - AckError (any seq, including 0 for unsolicited faults): recorded
//     as a deviation and the session goes Stalled: the machine never
//     retries and never pretends the step happened; the parent above
//     decides (stop, reap, report).
func (m *Machine) Ack(a Ack) error {
	now := m.clock.Now()
	if a.V != ProtocolVersion {
		m.deviation(0, a.Kind, "unknown protocol version", now)
		return fmt.Errorf("transition: %s: ack with protocol version %d, want %d", m.session, a.V, ProtocolVersion)
	}
	if a.Kind == AckError {
		what := "worker error"
		if a.Msg != "" {
			what += ": " + a.Msg
		}
		m.deviation(a.Seq, a.Kind, what, now)
		m.pending = nil // the command was answered - with a refusal, but answered
		m.state = Stalled
		return nil
	}
	doneKind, done := m.lastDone[a.Seq]
	if done {
		what := "duplicate ack"
		if doneKind != a.Kind {
			what = "stale ack (completed as " + string(doneKind) + ")"
		}
		m.deviation(a.Seq, a.Kind, what, now)
		return nil
	}
	if m.pending == nil || a.Seq != m.pending.Seq {
		m.deviation(a.Seq, a.Kind, "stale ack", now)
		return nil
	}
	want := ackFor(m.pending.Kind)
	if a.Kind != want {
		m.deviation(a.Seq, a.Kind, "unexpected ack (want "+string(want)+")", now)
		return nil
	}
	idx := m.pendingIdx()
	rec := &m.records[idx]
	rec.Ack = a.Kind
	rec.AckedAt = now
	rec.State = m.nextState(m.pending.Kind, a.Kind)
	if rec.Deviation == "missed deadline" {
		// Tick already wrote the gap: the ack DID arrive, just after
		// the deadline - keep the story precise instead of erasing
		// either half.
		rec.Deviation = "late ack after missed deadline"
	}
	m.lastDone[a.Seq] = a.Kind
	m.state = rec.State
	m.pending = nil
	return nil
}

func (m *Machine) nextState(cmd, ack Kind) State {
	switch cmd {
	case CmdPrepare:
		return Ready // the worker sends IDLE_ACK next, on its own
	case CmdTrigger:
		return Running
	case CmdVerify:
		return Verifying // VERIFIED closes the block; IDLE_ACK closes the burst
	case CmdIdle:
		return Idle
	case CmdStop:
		return Stopped
	case CmdLeaseWrite, CmdLeaseRead:
		// The lease ack itself returns the worker to blocking idle:
		// its lease handler finishes and the read loop waits again.
		// No unsolicited IDLE_ACK follows - if a worker sends one,
		// IdleAck records the deviation, as it must.
		return Idle
	}
	_ = ack
	return m.state
}

// Tick checks the armed deadline against the clock. When it passes with
// no ack, ONE "missed deadline" record is written and the machine moves
// to Stalled: honest about the gap, not failed, not retried. The parent
// above it decides (cancel, stop, or keep waiting for a late ack).
func (m *Machine) Tick() *Record {
	if m.pending == nil {
		return nil
	}
	rec := &m.records[m.pendingIdx()]
	if rec.Deviation != "" {
		return nil // already recorded once: no deadline spam
	}
	deadline := rec.CommandedAt.Add(m.timeouts.forKind(m.pending.Kind))
	if m.clock.Now().Before(deadline) {
		return nil
	}
	rec.Deviation = "missed deadline"
	rec.State = Stalled
	m.state = Stalled
	out := *rec
	return &out
}

// IdleAck applies the unsolicited IDLE_ACK that follows READY and the
// IDLE_ACK that closes a verified burst (no IDLE command was sent).
// These carry seq=0: they acknowledge a state, not a command.
func (m *Machine) IdleAck() {
	now := m.clock.Now()
	switch m.state {
	case Ready, Verifying, Running:
		m.records = append(m.records, Record{
			Ack: AckIdle, State: Idle, AckedAt: now,
		})
		m.state = Idle
	default:
		m.deviation(0, AckIdle, "unexpected idle ack from state "+string(m.state), now)
	}
}

func (m *Machine) deviation(seq uint64, kind Kind, what string, now time.Time) {
	m.records = append(m.records, Record{
		Seq: seq, Ack: kind, State: m.state, AckedAt: now, Deviation: what,
	})
}
