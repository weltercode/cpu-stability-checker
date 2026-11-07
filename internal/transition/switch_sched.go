package transition

// Prepared pair-switch schedule (Q05): two prepared participant groups
// (each one an aggressor/observer pair) alternate turns on ONE
// commanded, prepared timeline - every participant is prepared before
// the first switch, so the measured transition contains no spawn and
// no allocation, only the turn itself. The design deliberately borrows
// the two shapes already proven in this package, without borrowing
// their objects: schedule.go (fully concrete prepared windows are the
// reproduction artifact; the seed is provenance, never re-derived) and
// arbiter.go (a pure bookkeeper over explicit events, single owner,
// refuse-don't-patch). A lease answers "who may touch this block"; a
// TURN answers "who may load now". The ledger below makes the
// roadmap's no-overlap rule structural rather than timing-based: the
// turn sequence holds at most one outstanding turn at a time, so an
// overlap cannot be invented by drifting clocks - and clocks are what
// we measure here, they must not arbitrate.

import (
	"fmt"
	"time"
)

// Order words of the pair-switch schedule: the default alternates the
// groups in listed order every round; "seeded" gives each round a
// deterministic permutation drawn from the pinned word spec below.
// The empty word IS "fixed": the default must not fork a second mode
// identity in the plan hash.
const (
	SwitchOrderFixed  = ""
	SwitchOrderSeeded = "seeded"
)

func validSwitchOrder(order string) bool {
	return order == SwitchOrderFixed || order == SwitchOrderSeeded
}

// SwitchTurn is one commanded group turn on the prepared timeline.
// Group indexes the prepared group that loads; Round records which
// cycle this turn belongs to, so a reader can tell "every group ran
// every round" without recounting; the durations are measured exactly
// like the P08 windows (Gap since the PREVIOUS turn's actual close,
// RunFor the load window).
type SwitchTurn struct {
	Group  int
	Round  int
	Gap    time.Duration
	RunFor time.Duration
}

// SwitchSchedule is the prepared turn sequence plus the provenance
// that produced it - the same contract as Schedule: Turns are the
// reproduction artifact; Base/Jitter/Order/Seed only say how they were
// built. Seed is nil when neither jitter nor a seeded order consumed
// it (absent is not "seeded with 0" - 0 is a valid seed).
type SwitchSchedule struct {
	GroupCount int
	Base       time.Duration
	Jitter     time.Duration
	Order      string
	Seed       *uint64
	Turns      []SwitchTurn
}

// switchOrderKey is the fixed indexing of the seeded turn order: the
// i-th swap of round r draws under (seed, r*groupCount+i). That
// namespace is structurally disjoint from the jitter draws
// (seed, global turn index), so one schedule can consume both without
// either shifting the other's numbers. This key IS the generation
// spec - realized orders are pinned by golden vectors, and changing
// it means bumping the spec, never quietly reshuffling what old runs
// meant.
func switchOrderKey(seed uint64, round, groupCount, idx int) uint64 {
	return jitterWord(seed, uint64(round)*uint64(groupCount)+uint64(idx))
}

// switchGroupOrder returns the order groups take in one round: the
// identity permutation for fixed alternation, a deterministic
// Fisher-Yates shuffle for "seeded". The caller has validated order.
func switchGroupOrder(order string, seed uint64, round, groupCount int) []int {
	perm := make([]int, groupCount)
	for i := range perm {
		perm[i] = i
	}
	if order == SwitchOrderSeeded {
		for i := groupCount - 1; i >= 1; i-- {
			j := int(switchOrderKey(seed, round, groupCount, i) % uint64(i+1))
			perm[i], perm[j] = perm[j], perm[i]
		}
	}
	return perm
}

// BuildSwitchSchedule expands rounds cycles over groupCount groups
// into concrete turns. Jitter spreads each turn's Gap by
// [-jitter, +jitter] (clamped at zero - a negative gap is not a
// thing), drawn exactly like the P08 jitter: same pinned word, indexed
// by the GLOBAL turn number. The result is fully concrete: journal,
// driver and report work from these turns, never from the seed.
func BuildSwitchSchedule(groupCount, rounds int, baseGap, runFor, jitter time.Duration, order string, seed uint64) (SwitchSchedule, error) {
	if groupCount < 2 {
		return SwitchSchedule{}, fmt.Errorf("transition: pair-switch needs at least two groups to take turns, got %d", groupCount)
	}
	if rounds < 1 {
		return SwitchSchedule{}, fmt.Errorf("transition: pair-switch needs at least one round, got %d", rounds)
	}
	if runFor <= 0 {
		return SwitchSchedule{}, fmt.Errorf("transition: pair-switch run window must be positive, got %s", runFor)
	}
	if baseGap < 0 || jitter < 0 {
		return SwitchSchedule{}, fmt.Errorf("transition: pair-switch gap (%s) and jitter (%s) must not be negative", baseGap, jitter)
	}
	if !validSwitchOrder(order) {
		return SwitchSchedule{}, fmt.Errorf(`transition: unknown pair-switch order word %q (want the default fixed alternation or %q)`, order, SwitchOrderSeeded)
	}
	s := SwitchSchedule{GroupCount: groupCount, Base: baseGap, Jitter: jitter, Order: order}
	if jitter > 0 || order == SwitchOrderSeeded {
		seedCopy := seed
		s.Seed = &seedCopy
	}
	turn := 0
	for r := 0; r < rounds; r++ {
		for _, g := range switchGroupOrder(order, seed, r, groupCount) {
			gap := baseGap
			if jitter > 0 {
				// Same draw mapping as BuildSchedule (the splitmix64
				// word scaled into the signed range): one PRNG spec,
				// two disjoint index namespaces, no drift between them.
				off := int64(jitterWord(seed, uint64(turn))>>1)%(2*int64(jitter)) - int64(jitter)
				gap += time.Duration(off)
				if gap < 0 {
					gap = 0
				}
			}
			s.Turns = append(s.Turns, SwitchTurn{Group: g, Round: r, Gap: gap, RunFor: runFor})
			turn++
		}
	}
	return s, nil
}

// SwitchRunner walks a prepared turn sequence against a Clock. Same
// honesty contract as ScheduleRunner: it never sleeps, never resends,
// never catches up; a late start is a signed deviation, and the next
// planned moment re-anchors from the ACTUAL close so prepared gaps
// survive lateness. The difference is who says yes: the runner only
// says WHEN the next turn was planned - the TurnLedger below says
// whether the machine may take it at all.
type SwitchRunner struct {
	sched SwitchSchedule
	clock Clock
	due   time.Time
	idx   int
	open  *SwitchTurn
}

// NewSwitchRunner starts the timeline at start (the moment every
// group reported ready-idle): the first turn's gap is measured from
// it.
func NewSwitchRunner(s SwitchSchedule, start time.Time, clock Clock) *SwitchRunner {
	if clock == nil {
		clock = RealClock{}
	}
	r := &SwitchRunner{sched: s, clock: clock}
	if len(s.Turns) > 0 {
		r.due = start.Add(s.Turns[0].Gap)
	}
	return r
}

// Due is the planned moment of the next TRIGGER; zero time means the
// schedule is exhausted.
func (r *SwitchRunner) Due() time.Time { return r.due }

// Remaining counts the turns not yet begun.
func (r *SwitchRunner) Remaining() int { return len(r.sched.Turns) - r.idx }

// Begin records the actual TRIGGER moment for the current turn and
// returns it with the deviation from the planned moment (actual -
// planned: positive late, negative early - a cut-short gap is a
// deviation too). At the end of the schedule Begin returns a zero
// turn; the driver must not send.
func (r *SwitchRunner) Begin(actual time.Time) (SwitchTurn, time.Duration) {
	if r.idx >= len(r.sched.Turns) {
		return SwitchTurn{}, 0
	}
	tt := r.sched.Turns[r.idx]
	r.open = &tt
	return tt, actual.Sub(r.due)
}

// End records the actual close of the begun turn (for pair-switch:
// the turn's last VERIFIED and IDLE_ACK, observed by the parent) and
// re-anchors the next planned moment at actual-close + the NEXT
// turn's prepared gap.
func (r *SwitchRunner) End(actual time.Time) {
	if r.open == nil {
		return
	}
	r.open = nil
	r.idx++
	if r.idx < len(r.sched.Turns) {
		r.due = actual.Add(r.sched.Turns[r.idx].Gap)
	} else {
		r.due = time.Time{}
	}
}

// GroupState is the turn-ledger state of one prepared group.
type GroupState string

const (
	GroupIdle    GroupState = "idle"    // prepared and loaded by nobody: may take a turn
	GroupRunning GroupState = "running" // turn issued, not yet closed: its acks are outstanding
	GroupTainted GroupState = "tainted" // a turn on it was refused/failed/died: no further turns, the parent decides
)

// TurnLedger is the parent-side turn-taking bookkeeper for a
// pair-switch session: the parent is the single arbiter, participants
// never negotiate turns with each other, and every grant is issued,
// retired or tainted HERE - so there is no distributed race to lose.
// It mirrors the Arbiter's Reserve/Issue/Cancel/Complete/Taint split
// for exactly the same reason: a failed send must never leave the
// ledger claiming a command that was never written, and a state
// change happens only when the command was actually stamped.
//
// Like the Arbiter (and Machine), it is pure bookkeeping over explicit
// events and is NOT safe for concurrent use: the parent's session
// loop is the single owner.
type TurnLedger struct {
	sched    SwitchSchedule
	state    []GroupState
	reason   map[int]string // taint reasons, kept for the journal
	next     int            // index of the next scheduled turn that may be taken
	reserved *SwitchTurn    // validated but unstamped (at most one: turns are strictly serialized)
	out      map[uint64]int // command seq -> the group whose turn it closed
}

// NewTurnLedger tracks the groups of one prepared schedule. Like
// NewArbiter it refuses loudly at construction what would otherwise
// become a silent race later.
func NewTurnLedger(s SwitchSchedule) *TurnLedger {
	t := &TurnLedger{
		sched:  s,
		state:  make([]GroupState, s.GroupCount),
		reason: make(map[int]string, s.GroupCount),
		out:    make(map[uint64]int, 2),
	}
	for g := range t.state {
		t.state[g] = GroupIdle
	}
	return t
}

// StateOf reports the ledger state of one group.
func (t *TurnLedger) StateOf(group int) GroupState {
	if group < 0 || group >= len(t.state) {
		return ""
	}
	return t.state[group]
}

// TaintReason is why a group went tainted ("" when it did not).
func (t *TurnLedger) TaintReason(group int) string { return t.reason[group] }

// RemainingTurns counts the scheduled turns not yet issued.
func (t *TurnLedger) RemainingTurns() int { return len(t.sched.Turns) - t.next }

// Outstanding is the number of issued turns still awaiting their
// close. Turn-taking holds at most one by design; the getter exists
// for tests and for the journal, not for tolerance.
func (t *TurnLedger) Outstanding() int { return len(t.out) }

// NextTurn is the scheduled turn a ReserveTurn would validate (zero
// value and false past the end). The driver reads it for planning
// and logging; validation stays in ReserveTurn.
func (t *TurnLedger) NextTurn() (SwitchTurn, bool) {
	if t.next >= len(t.sched.Turns) {
		return SwitchTurn{}, false
	}
	return t.sched.Turns[t.next], true
}

// ReserveTurn validates the next scheduled turn against the ledger
// and holds it pending the command stamp. Two refusals are the point:
// the turn's group must not be tainted, and NO group may be running -
// the no-overlap rule is enforced here, before any worker is asked,
// and it never depends on parent-side sleep timing.
func (t *TurnLedger) ReserveTurn() (SwitchTurn, error) {
	if t.reserved != nil {
		return SwitchTurn{}, fmt.Errorf("transition: turn ledger: turn %d (group %d) is already reserved; finish or cancel it before reserving another", t.next, t.reserved.Group)
	}
	if t.next >= len(t.sched.Turns) {
		return SwitchTurn{}, fmt.Errorf("transition: turn ledger: the turn sequence is exhausted (%d turns)", len(t.sched.Turns))
	}
	tt := t.sched.Turns[t.next]
	if t.state[tt.Group] == GroupTainted {
		return SwitchTurn{}, fmt.Errorf("transition: turn ledger: turn %d refused: group %d is tainted (%s)", t.next, tt.Group, t.reason[tt.Group])
	}
	for g, st := range t.state {
		if st == GroupRunning {
			return SwitchTurn{}, fmt.Errorf("transition: turn ledger: turn %d (group %d) cannot start: group %d still holds an unclosed turn (turn-taking holds at most one outstanding turn; overlap would need a schedule mode this tool does not have)", t.next, tt.Group, g)
		}
	}
	held := tt
	t.reserved = &held
	return tt, nil
}

// CancelTurn releases a reservation whose send path failed: the
// ledger never claims a command that was never stamped. A group whose
// participant died while merely reserved is radioactively tainted
// with TaintReservation - same rule as the block arbiter.
func (t *TurnLedger) CancelTurn() { t.reserved = nil }

// TaintReservation taints the group of a reserved-but-unstamped turn
// that is being abandoned (the send path died mid-switch).
func (t *TurnLedger) TaintReservation(why string) error {
	if t.reserved == nil {
		return fmt.Errorf("transition: turn ledger: TaintReservation: nothing is reserved")
	}
	g := t.reserved.Group
	t.reserved = nil
	t.state[g] = GroupTainted
	if why != "" {
		t.reason[g] = why
	}
	return nil
}

// IssueTurn commits the reserved turn under the command seq the
// machine stamped: the group enters running and the schedule advances
// by exactly one turn. A reservation that never became a command
// changes nothing.
func (t *TurnLedger) IssueTurn(seq uint64) error {
	if t.reserved == nil {
		return fmt.Errorf("transition: turn ledger: IssueTurn(%d): nothing is reserved", seq)
	}
	if _, dup := t.out[seq]; dup {
		return fmt.Errorf("transition: turn ledger: seq %d already issued", seq)
	}
	g := t.reserved.Group
	t.reserved = nil
	t.out[seq] = g
	t.state[g] = GroupRunning
	t.next++
	return nil
}

// CompleteTurn closes the issued turn under seq. The parent calls it
// only after it has SEEN the turn's final VERIFIED ack and its
// IDLE_ACK - that pair is the idle barrier, the moment the group is
// provably back ready-unloaded; a VERIFIED alone is not enough (the
// load still has to stop) and an IDLE_ACK alone is not enough (the
// block may never have been verified). The barrier close is also what
// the next turn's planned moment re-anchors from.
func (t *TurnLedger) CompleteTurn(seq uint64) error {
	g, ok := t.out[seq]
	if !ok {
		return fmt.Errorf("transition: turn ledger: CompleteTurn(%d): no outstanding turn for that seq", seq)
	}
	delete(t.out, seq)
	t.state[g] = GroupIdle
	return nil
}

// TaintTurn marks the group of an issued turn whose turn failed,
// stalled or whose participant died: no further turn may touch it,
// and the reason rides into the journal. The parent decides what the
// session does next (the honest stop is already the driver's path).
func (t *TurnLedger) TaintTurn(seq uint64, why string) error {
	g, ok := t.out[seq]
	if !ok {
		return fmt.Errorf("transition: turn ledger: TaintTurn(%d): no outstanding turn for that seq", seq)
	}
	delete(t.out, seq)
	t.state[g] = GroupTainted
	if why != "" {
		t.reason[g] = why
	}
	return nil
}
