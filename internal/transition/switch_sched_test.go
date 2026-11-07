package transition

import (
	"strings"
	"testing"
	"time"
)

// The pair-switch order spec is a generation contract exactly like
// the jitter word: golden vectors pin the REALIZED orders, so a spec
// change cannot quietly reinterpret what old saved schedules meant.
// The pinned sequences below were generated once by this spec and are
// now the truth - same workflow that pinned jitterWord.
func TestSwitchSeededOrderGolden(t *testing.T) {
	// (order, seed, groupCount, rounds) -> realized group sequence,
	// in turn order across rounds.
	cases := []struct {
		name       string
		order      string
		groups     int
		rounds     int
		seed       uint64
		wantGroups []int
	}{
		{"fixed is identity every round", SwitchOrderFixed, 2, 3, 0, []int{0, 1, 0, 1, 0, 1}},
		{"seed 7 two groups four rounds", SwitchOrderSeeded, 2, 4, 7, []int{1, 0, 0, 1, 0, 1, 1, 0}},
		{"seed 11 three groups three rounds", SwitchOrderSeeded, 3, 3, 11, []int{2, 1, 0, 2, 0, 1, 1, 0, 2}},
	}
	for _, tc := range cases {
		s, err := BuildSwitchSchedule(tc.groups, tc.rounds, time.Second, 100*time.Millisecond, 0, tc.order, tc.seed)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var got []int
		for _, tt := range s.Turns {
			got = append(got, tt.Group)
		}
		if !intSliceEqual(got, tc.wantGroups) {
			t.Errorf("%s = %v, want the pinned %v (order spec changed? that is a spec bump, not a quiet edit)", tc.name, got, tc.wantGroups)
		}
		if tc.order == SwitchOrderFixed {
			if s.Seed != nil {
				t.Errorf("%s: fixed without jitter must carry no Seed, got %d", tc.name, *s.Seed)
			}
			continue
		}
		if s.Seed == nil || *s.Seed != tc.seed {
			t.Errorf("%s: seeded order must record the seed, got %v", tc.name, s.Seed)
		}
		// Every round must be a permutation: each group loads exactly
		// once per round - no group skipped, no group double-loaded.
		for r := 0; r < tc.rounds; r++ {
			seen := map[int]bool{}
			for _, g := range got[r*tc.groups : (r+1)*tc.groups] {
				seen[g] = true
			}
			if len(seen) != tc.groups {
				t.Errorf("%s round %d = %v, want a permutation of %d groups", tc.name, r, got[r*tc.groups:(r+1)*tc.groups], tc.groups)
			}
		}
		// Same seed replays to the identical realized order.
		again, err := BuildSwitchSchedule(tc.groups, tc.rounds, time.Second, 100*time.Millisecond, 0, SwitchOrderSeeded, tc.seed)
		if err != nil {
			t.Fatal(err)
		}
		var got2 []int
		for _, tt := range again.Turns {
			got2 = append(got2, tt.Group)
		}
		if !intSliceEqual(got, got2) {
			t.Errorf("%s: seeded replay differs: %v vs %v", tc.name, got, got2)
		}
	}
}

func TestSwitchJitterGolden(t *testing.T) {
	s, err := BuildSwitchSchedule(2, 3, 10*time.Second, 100*time.Millisecond, 5*time.Second, SwitchOrderFixed, 42)
	if err != nil {
		t.Fatal(err)
	}
	wantGaps := []time.Duration{
		11377637706, 11063446145, 9231381929, 5129127882, 8070981625, 7831494531, // ns: pinned by the splitmix64 draw spec
	}
	for i, tt := range s.Turns {
		if tt.RunFor != 100*time.Millisecond {
			t.Errorf("turn %d: run window = %s, want 100ms unchanged", i, tt.RunFor)
		}
		if tt.Gap != wantGaps[i] {
			t.Errorf("turn %d: gap = %v, want the pinned %v (jitter spec changed? that is a spec bump)", i, tt.Gap, wantGaps[i])
		}
		if tt.Gap < 5*time.Second || tt.Gap > 15*time.Second {
			t.Errorf("turn %d: gap %s outside base±jitter (10s±5s)", i, tt.Gap)
		}
	}
	if s.Seed == nil || *s.Seed != 42 {
		t.Errorf("Seed = %v, want 42 (jitter consumed the seed)", s.Seed)
	}
}

// The two PRNG consumers (order swaps, gap jitter) must live in
// disjoint index namespaces: turning on the seeded order must not
// shift a single jitter gap, or one knob would silently fork the
// other's schedule identity.
func TestSwitchOrderDoesNotShiftJitter(t *testing.T) {
	fixed, err := BuildSwitchSchedule(3, 4, 10*time.Second, 100*time.Millisecond, 3*time.Second, SwitchOrderFixed, 99)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := BuildSwitchSchedule(3, 4, 10*time.Second, 100*time.Millisecond, 3*time.Second, SwitchOrderSeeded, 99)
	if err != nil {
		t.Fatal(err)
	}
	var fixedOrder, seededOrder []int
	var fixedGaps, seededGaps []time.Duration
	for i := range fixed.Turns {
		fixedOrder = append(fixedOrder, fixed.Turns[i].Group)
		seededOrder = append(seededOrder, seeded.Turns[i].Group)
		fixedGaps = append(fixedGaps, fixed.Turns[i].Gap)
		seededGaps = append(seededGaps, seeded.Turns[i].Gap)
	}
	if len(fixedGaps) != len(seededGaps) {
		t.Fatalf("gap count differs: %d vs %d", len(fixedGaps), len(seededGaps))
	}
	for i := range fixedGaps {
		if fixedGaps[i] != seededGaps[i] {
			t.Errorf("seeded order shifted the jitter gap of turn %d: %v vs %v", i, fixedGaps[i], seededGaps[i])
		}
	}
	if intSliceEqual(fixedOrder, seededOrder) {
		t.Errorf("seeded order produced the fixed alternation %v (spec or seed wiring broken)", fixedOrder)
	}
}

func TestBuildSwitchScheduleRefusals(t *testing.T) {
	for _, c := range []struct {
		name       string
		groups     int
		rounds     int
		gap, run   time.Duration
		jitter     time.Duration
		order      string
		wantSubstr string
	}{
		{"one group", 1, 2, time.Second, 100 * time.Millisecond, 0, SwitchOrderFixed, "at least two groups"},
		{"zero rounds", 2, 0, time.Second, 100 * time.Millisecond, 0, SwitchOrderFixed, "at least one round"},
		{"zero run", 2, 2, time.Second, 0, 0, SwitchOrderFixed, "run window must be positive"},
		{"negative gap", 2, 2, -time.Millisecond, 100 * time.Millisecond, 0, SwitchOrderFixed, "must not be negative"},
		{"negative jitter", 2, 2, time.Second, 100 * time.Millisecond, -time.Second, SwitchOrderFixed, "must not be negative"},
		{"unknown order", 2, 2, time.Second, 100 * time.Millisecond, 0, "random", `unknown pair-switch order word "random"`},
	} {
		if _, err := BuildSwitchSchedule(c.groups, c.rounds, c.gap, c.run, c.jitter, c.order, 0); err == nil {
			t.Errorf("%s: want refusal, got nil", c.name)
		} else if !strings.Contains(err.Error(), c.wantSubstr) {
			t.Errorf("%s: err = %v, want substring %q", c.name, err, c.wantSubstr)
		}
	}
}

func TestSwitchRunnerHonesty(t *testing.T) {
	s, err := BuildSwitchSchedule(2, 2, time.Second, 100*time.Millisecond, 0, SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(3000, 0)
	r := NewSwitchRunner(s, t0, nil)
	if got, want := r.Due(), t0.Add(time.Second); got != want {
		t.Errorf("first Due = %v, want %v", got, want)
	}
	if r.Remaining() != 4 {
		t.Fatalf("Remaining = %d, want 4", r.Remaining())
	}
	// 5ms late start on turn 0: signed deviation, never smoothed.
	tt, dev := r.Begin(t0.Add(time.Second + 5*time.Millisecond))
	if tt.Group != 0 || tt.Round != 0 {
		t.Errorf("turn 0 = %+v, want group 0 round 0", tt)
	}
	if dev != 5*time.Millisecond {
		t.Errorf("deviation = %s, want +5ms", dev)
	}
	// The window closes LATE (run itself overran by 300ms): the next
	// planned moment must re-anchor from the actual close, so the
	// prepared 1s gap survives intact instead of compressing.
	close := t0.Add(time.Second + 5*time.Millisecond + 400*time.Millisecond)
	r.End(close)
	if got, want := r.Due(), close.Add(time.Second); got != want {
		t.Errorf("after a late close Due = %v, want %v (no catch-up, no compression)", got, want)
	}
	if tt, _ := r.Begin(close.Add(900 * time.Millisecond)); tt.Group != 1 || tt.Round != 0 {
		t.Errorf("turn 1 = %+v, want group 1 round 0", tt)
	}
	// Walk the rest of the schedule and demand it ends: turns 2 and 3
	// close back-to-back (zero-time Ends are fine for the walk - the
	// due times re-anchor from them, and the walk only checks the
	// bookkeeping closes the sequence).
	r.End(t0)
	r.Begin(t0)
	r.End(t0)
	r.Begin(t0)
	r.End(t0)
	if r.Remaining() != 0 || !r.Due().IsZero() {
		t.Errorf("after the last End Remaining = %d Due = %v, want 0 and zero time", r.Remaining(), r.Due())
	}
	if tt, _ := r.Begin(t0); tt != (SwitchTurn{}) {
		t.Errorf("Begin past the end = %+v, want the zero turn (the driver must not send)", tt)
	}
}

func TestTurnLedgerCleanCycle(t *testing.T) {
	s, err := BuildSwitchSchedule(2, 2, time.Second, 100*time.Millisecond, 0, SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	l := NewTurnLedger(s)
	seq := uint64(0)
	for i, tt := range s.Turns {
		if _, ok := l.NextTurn(); !ok {
			t.Fatalf("turn %d: NextTurn says the sequence is over", i)
		}
		got, err := l.ReserveTurn()
		if err != nil {
			t.Fatalf("turn %d: ReserveTurn: %v", i, err)
		}
		if got != tt {
			t.Fatalf("turn %d: reserved %+v, want %+v", i, got, tt)
		}
		seq++
		if err := l.IssueTurn(seq); err != nil {
			t.Fatalf("turn %d: IssueTurn: %v", i, err)
		}
		if l.Outstanding() != 1 {
			t.Errorf("turn %d: Outstanding = %d, want 1", i, l.Outstanding())
		}
		if st := l.StateOf(tt.Group); st != GroupRunning {
			t.Errorf("turn %d: group %d state = %s, want running", i, tt.Group, st)
		}
		if err := l.CompleteTurn(seq); err != nil {
			t.Fatalf("turn %d: CompleteTurn: %v", i, err)
		}
		if st := l.StateOf(tt.Group); st != GroupIdle {
			t.Errorf("turn %d: group %d state = %s, want idle after the barrier", i, tt.Group, st)
		}
	}
	if l.RemainingTurns() != 0 {
		t.Fatalf("RemainingTurns = %d, want 0", l.RemainingTurns())
	}
	if _, err := l.ReserveTurn(); err == nil {
		t.Error("reserve past the end: want refusal, got nil")
	}
}

func TestTurnLedgerRefusals(t *testing.T) {
	s, err := BuildSwitchSchedule(2, 2, time.Second, 100*time.Millisecond, 0, SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Overlap is refused structurally, before any worker is asked.
	l := NewTurnLedger(s)
	tt, err := l.ReserveTurn()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.ReserveTurn(); err == nil || !strings.Contains(err.Error(), "already reserved") {
		t.Errorf("double reserve = %v, want an already-reserved refusal", err)
	}
	if err := l.IssueTurn(1); err != nil {
		t.Fatalf("IssueTurn after re-reserve: %v", err)
	}
	if _, err := l.ReserveTurn(); err == nil || !strings.Contains(err.Error(), "group 0 still holds") || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("overlap reserve = %v, want a refusal naming the running group (turn %v next)", err, tt)
	}
	if err := l.CompleteTurn(2); err == nil || !strings.Contains(err.Error(), "no outstanding turn") {
		t.Errorf("complete of an unissued seq = %v, want refusal", err)
	}
	if err := l.CompleteTurn(1); err != nil {
		t.Fatalf("CompleteTurn(1): %v", err)
	}
	if st := l.StateOf(0); st != GroupIdle {
		t.Errorf("after complete group 0 = %s, want idle", st)
	}
	// Issue without a reservation, and a taint that blocks every
	// later turn touching the group.
	l2 := NewTurnLedger(s)
	if err := l2.IssueTurn(7); err == nil || !strings.Contains(err.Error(), "nothing is reserved") {
		t.Errorf("issue without reservation = %v, want refusal", err)
	}
	if _, err := l2.ReserveTurn(); err != nil {
		t.Fatal(err)
	}
	if err := l2.IssueTurn(9); err != nil {
		t.Fatal(err)
	}
	if err := l2.TaintTurn(9, "worker vanished"); err != nil {
		t.Fatal(err)
	}
	if st := l2.StateOf(0); st != GroupTainted {
		t.Errorf("after taint group 0 = %s, want tainted", st)
	}
	if l2.Outstanding() != 0 {
		t.Errorf("Outstanding after taint = %d, want 0", l2.Outstanding())
	}
	// The next turn belongs to group 1 (alternating), so it may still
	// be reserved...
	if tt, err := l2.ReserveTurn(); err != nil || tt.Group != 1 {
		t.Fatalf("after tainting group 0 the next (group 1) turn must still be reservable: %v %+v", err, tt)
	}
	l2.CancelTurn()
	// ...but once the sequence wraps to group 0 again, the taint is a
	// named refusal carrying the reason.
	if _, err := l2.ReserveTurn(); err != nil {
		t.Fatalf("reserving the group-1 turn: %v", err)
	}
	if err := l2.IssueTurn(11); err != nil {
		t.Fatal(err)
	}
	if err := l2.CompleteTurn(11); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.ReserveTurn(); err == nil || !strings.Contains(err.Error(), "group 0 is tainted (worker vanished)") {
		t.Errorf("turn touching a tainted group = %v, want a refusal carrying the taint reason", err)
	}
}

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
