package plan

import (
	"strings"
	"testing"
)

// R29 (owner complaint 2026-11-12: coverage numbers were invisible in
// every artifact he could actually read). The run-level fold must
// come from what the slots reported and NOTHING else: slots without
// a mem_done fact count as unknown, the sums stay cumulative
// byte-work, and a run where no slot reported keeps the block nil -
// absence means not observed, never a zeroed-out story.
func TestFinalizeRAMFold(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	res := Finalize(FinalInput{Execution: ExecCompleted, Units: []UnitInput{
		{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true,
			Mem: &MemAccounting{Requested: 1 << 20, Allocated: 1<<20 + 4096, Touched: 1 << 20, Verified: 5 << 20, VerifiedUnique: u(1 << 20), VerifiedTotal: u(5 << 20)}},
		{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true,
			Mem: &MemAccounting{Requested: 2 << 20, Allocated: 2 << 20, Touched: 1 << 20, Verified: 3 << 20}}, // no uniqueness proof: unknown, not zero
		{Execution: ExecCompleted, Checks: 0, ChecksKnown: true, VerificationRequired: false}, // no engine at all: unknown slot
	}})
	mem := res.Coverage.Mem
	if mem == nil {
		t.Fatal("two accounted slots and the fold is absent?")
	}
	if mem.EngineSlots != 2 || mem.UnknownSlots != 1 {
		t.Errorf("slots = %d accounted / %d unknown, want 2/1", mem.EngineSlots, mem.UnknownSlots)
	}
	if mem.Requested != 3<<20 || mem.Allocated != 3<<20+4096 || mem.Touched != 2<<20 || mem.Verified != 8<<20 {
		t.Errorf("sums wrong: %+v", mem)
	}
	// The last-pass floor counts only the slot that PROVED its
	// unique volume; the unknown slot never contributes a guess.
	if mem.LastPass != 1<<20 {
		t.Errorf("last-pass = %d, want %d (proven slots only)", mem.LastPass, 1<<20)
	}

	// A run with no accounting anywhere: nil, not an empty struct of
	// zeros. Zero would claim the engine allocated nothing; nil says
	// nobody reported - the honesty contract for every artifact.
	bare := Finalize(FinalInput{Execution: ExecCompleted, Units: []UnitInput{
		{Execution: ExecCompleted, Checks: 5, ChecksKnown: true, VerificationRequired: true},
	}})
	if bare.Coverage.Mem != nil {
		t.Errorf("unaccounted run folded into %+v, want nil (unknown, never zero)", bare.Coverage.Mem)
	}
}

// The block-kernel shapes (R31, Sol review): ram-block4 makes two
// byte-channel combinations common that were incidental before -
// a slot with verified bytes but NO whole-pass declaration (the
// window closed partial sweeps only: Verified known, pass channels
// nil=unknown), and a phase list MIXING such a slot with slots that
// completed full sweeps. The fold must sum what each slot reported,
// take LastPass only from the proven slots, count checks as checks
// (never bytes), and render the unknown uniqueness as absence, not
// as a measured zero.
func TestFinalizeRAMFoldBlockKernelShapes(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	partial := MemAccounting{Requested: 8 << 20, Allocated: 8 << 20, Touched: 8 << 20, Verified: 4 << 20} // blocks verified, no full sweep: pass channels unknown
	full := MemAccounting{Requested: 5 << 20, Allocated: 5 << 20, Touched: 5 << 20, Verified: 5 << 20, VerifiedUnique: u(5 << 20), VerifiedTotal: u(10 << 20)}

	res := Finalize(FinalInput{Execution: ExecCompleted, Units: []UnitInput{
		{Execution: ExecCompleted, Checks: 1, ChecksKnown: true, VerificationRequired: true, Mem: &partial},
		{Execution: ExecCompleted, Checks: 4, ChecksKnown: true, VerificationRequired: true, Mem: &full},
		{Execution: ExecCompleted, Checks: 4, ChecksKnown: true, VerificationRequired: true, Mem: &full},
	}})
	mem := res.Coverage.Mem
	if mem == nil {
		t.Fatal("three accounted slots folded to nil")
	}
	if mem.EngineSlots != 3 || mem.UnknownSlots != 0 {
		t.Errorf("slots = %d/%d, want 3/0", mem.EngineSlots, mem.UnknownSlots)
	}
	if mem.Verified != 14<<20 {
		t.Errorf("verified bytes = %d, want 14MiB (partial slot's blocks count, they were really verified)", mem.Verified)
	}
	// LastPass comes only from the two sweep-proven slots; the
	// partial slot contributes NOTHING (unknown, not zero, not its
	// 4MiB pretending to be a pass volume).
	if mem.LastPass != 10<<20 {
		t.Errorf("last-pass = %d, want 10MiB (proven slots only)", mem.LastPass)
	}
	// The checks channel is verification COUNT, independent of any
	// byte channel: 1 credited block + 4 + 4.
	if res.Coverage.Verified != 9 || !res.Coverage.ChecksKnown {
		t.Errorf("verified checks = %d (known=%v), want 9 (credited blocks are checks, never bytes)", res.Coverage.Verified, res.Coverage.ChecksKnown)
	}

	// Partial-only fold (the T1-slot reality at max budget: no sweep
	// ever completed): the renderer must state verified bytes with NO
	// last-pass clause - unknown uniqueness is absence, never a
	// measured zero.
	partialOnly := Finalize(FinalInput{Execution: ExecCompleted, Units: []UnitInput{
		{Execution: ExecCompleted, Checks: 1, ChecksKnown: true, VerificationRequired: true, Mem: &partial},
	}})
	if p := partialOnly.Coverage.Mem; p == nil || p.Verified != 4<<20 || p.LastPass != 0 {
		t.Fatalf("partial-only fold wrong: %+v", p)
	} else if strings.Contains(p.RAMString(), "last-pass") {
		t.Errorf("unknown uniqueness rendered as a clause anyway: %q", p.RAMString())
	}

	// The mixed render keeps the clause ONLY because two slots
	// proved their pass volume: 10MiB of proven last-pass.
	if !strings.Contains(mem.RAMString(), "last-pass=") {
		t.Errorf("proven uniqueness lost its clause: %q", mem.RAMString())
	}
}

// R34 (Sol review 2026-11-17): the session-shape executors (P08
// transition, P07e handoff, Q05 pair switch) fold the live mem block
// from a scan of their children's own mem_done records WITHOUT
// touching the aggregated verdict units - so they must reach the fold
// through this one rule, never a copy of the arithmetic. The rule
// pinned directly here: unknown-only is nil (not zeros), unreported
// slots count as unknown, the count never renders negative, and
// LastPass stays a floor over proven slots.
func TestFoldRAMSingleRule(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	if got := FoldRAM(nil, 3); got != nil {
		t.Errorf("unknown-only fold = %+v, want nil (a block of zeros would claim the engine touched nothing)", got)
	}
	observed := []*MemAccounting{
		{Requested: 1 << 20, Allocated: 1 << 20, Touched: 1 << 20, Verified: 7 << 20}, // pass channels unknown
		{Requested: 1 << 19, Allocated: 1 << 19, Touched: 1 << 19, Verified: 1 << 19, VerifiedUnique: u(1 << 19), VerifiedTotal: u(1 << 19)},
	}
	got := FoldRAM(observed, 2) // two planned slots never reported (killed worker / budget-skipped core)
	if got == nil {
		t.Fatal("reported slots folded to nil")
	}
	if got.EngineSlots != 2 || got.UnknownSlots != 2 {
		t.Errorf("slots = %d accounted / %d unknown, want 2/2", got.EngineSlots, got.UnknownSlots)
	}
	if got.Requested != 3<<19 || got.Allocated != 3<<19 || got.Touched != 3<<19 || got.Verified != 15<<19 {
		t.Errorf("sums wrong: %+v", got)
	}
	if got.LastPass != 1<<19 {
		t.Errorf("last-pass = %d, want %d (the unproven slot contributes nothing)", got.LastPass, 1<<19)
	}
	// A planned count below the observed records (an identity oddity
	// that should never happen) must never render a negative unknown
	// count - the floor at zero is rendering honesty, not a claim.
	if neg := FoldRAM(observed, -2); neg == nil || neg.UnknownSlots != 0 {
		t.Errorf("negative unreported slots = %+v, want UnknownSlots floored at 0", neg)
	}
}

// The attempt-level fold (receipts): sums across the phase list over
// distinct slot settles, keeps unknown slots honest, and returns nil
// when NO run reported - so a receipt from a CPU-only attempt never
// grows an empty mem block.
func TestCombineRAMAcrossRuns(t *testing.T) {
	withMem := Result{Coverage: Coverage{Mem: &RAMTotals{EngineSlots: 2, UnknownSlots: 1, Requested: 1 << 30, Allocated: 1 << 30, Touched: 1 << 30, Verified: 9 << 30, LastPass: 1 << 30}}}
	plain := Result{}
	comb := CombineRAM([]Result{withMem, plain, withMem})
	if comb == nil {
		t.Fatal("runs with accounting folded into nil")
	}
	if comb.EngineSlots != 4 || comb.UnknownSlots != 2 {
		t.Errorf("slots = %d/%d, want 4 accounted / 2 unknown", comb.EngineSlots, comb.UnknownSlots)
	}
	if comb.Requested != 2<<30 || comb.Verified != 18<<30 {
		t.Errorf("bytes = requested %d verified %d, want 2GiB/18GiB", comb.Requested, comb.Verified)
	}
	if got := CombineRAM([]Result{plain, {}}); got != nil {
		t.Errorf("no run reported, fold returned %+v, want nil (absence, not zeros)", got)
	}
	if got := CombineRAM(nil); got != nil {
		t.Errorf("empty attempt fold = %+v, want nil", got)
	}

	// The omission review's regression (Sol 12.11): a run whose mem
	// block is entirely nil must not drop its slots on the floor. Its
	// PLANNED slots join UnknownSlots - accounting not observed is
	// stated, never zeroed and never invented - while the bytes stay
	// exactly what the accounted run proved.
	acct := Result{Coverage: Coverage{Planned: 2, Mem: &RAMTotals{EngineSlots: 1, UnknownSlots: 1, Requested: 1 << 20, Allocated: 1 << 20, Touched: 1 << 20, Verified: 3 << 20, LastPass: 1 << 20}}}
	silent := Result{Coverage: Coverage{Planned: 3}} // never observed accounting anywhere in this run
	mix := CombineRAM([]Result{silent, acct})
	if mix == nil {
		t.Fatal("an accounted run plus a silent run folded into nil")
	}
	if mix.EngineSlots != 1 || mix.UnknownSlots != 4 {
		t.Errorf("slots = %d accounted / %d unknown, want 1/4 (the silent run's 3 planned slots stay unknown, not dropped)", mix.EngineSlots, mix.UnknownSlots)
	}
	if mix.Requested != 1<<20 || mix.Verified != 3<<20 || mix.LastPass != 1<<20 {
		t.Errorf("bytes drifted for the silent run: %+v (unknowns must carry no invented bytes)", mix)
	}
}

// The summary line carries its own labels: cumulative byte-work, the
// last-pass floor only where proven, unknown slots stated in words.
// It must never phrase the numbers as physical-RAM coverage.
func TestRAMStringHonestLabels(t *testing.T) {
	line := (&RAMTotals{EngineSlots: 16, UnknownSlots: 2, Requested: 48 << 30, Allocated: 48 << 30, Touched: 48 << 30, Verified: 900 << 30, LastPass: 48 << 30}).RAMString()
	for _, must := range []string{"cumulative", "16 slot(s) accounted", "2 slot(s) without accounting (unknown, not zero)", "last-pass="} {
		if !strings.Contains(line, must) {
			t.Errorf("summary line lost %q:\n%s", must, line)
		}
	}
	for _, never := range []string{"unique physical", "100%", "complete coverage"} {
		if strings.Contains(line, never) {
			t.Errorf("summary line claims %q:\n%s", never, line)
		}
	}
	// nil and zero-last-pass shapes: no phantom last-pass clause.
	if (&RAMTotals{}).RAMString() == "" {
		t.Error("an all-zero fold must still render its (zero) accounting when it exists")
	}
	var nilMem *RAMTotals
	if nilMem.RAMString() != "" {
		t.Error("nil must render nothing (absence = not observed)")
	}
	if s := (&RAMTotals{EngineSlots: 1, Verified: 1}).RAMString(); strings.Contains(s, "last-pass") {
		t.Errorf("unproven uniqueness rendered as last-pass: %s", s)
	}
}
