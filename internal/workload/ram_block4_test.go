package workload

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// ram-block4-v1 pins. The kernel's unit is a verified block: fill +
// immediate verify as one atomic step, credit only for units that
// completed whole. Beat map per block: fill-beat, then credit-beat -
// so cancelAt/flipAt numbers are block-phase deterministic, exactly
// like the chunk-beat maps in the other ram suites.

// block4Runner builds one region layout and its reporter.
func block4Runner(t *testing.T, seed uint64, sizes ...uint64) (*regionReporter, []memRegionView) {
	t.Helper()
	e := openTestEngine(t, sizes...)
	rep := &regionReporter{regions: e.Regions(), seed: seed}
	views := make([]memRegionView, 0, len(e.Regions()))
	for _, r := range e.Regions() {
		views = append(views, memRegionView{view: r.Block.Bytes(), goff: r.Grant.Offset})
	}
	return rep, views
}

// memRegionView pairs a region's bytes with its global offset (the
// only thing that differentiates expected words per the pinned spec).
type memRegionView struct {
	view []byte
	goff uint64
}

// Without engine regions the kernel refuses outright: this family
// never tests the Go heap.
func TestRamBlock4RefusesHeap(t *testing.T) {
	rep := &regionReporter{} // no regions
	_, err := runRamBlock4(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "no memory-engine regions") {
		t.Errorf("err = %v, want the engine-only refusal", err)
	}
}

// Regions smaller than a block are one (tail) block each: with 128K
// and 64K regions every sweep is two credited units. A timed run
// must credit units, keep the unique-verified store at exactly the
// region union (never double-counted across sweeps), declare sweep
// passes only in whole-sweep multiples, and report zero findings.
func TestRamBlock4CreditsAndCoverage(t *testing.T) {
	rep, _ := block4Runner(t, 11, 128<<10, 64<<10)
	total := uint64((128 + 64) << 10)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamBlock4(ctx, rep)
	if err != nil {
		t.Fatalf("runRamBlock4: %v", err)
	}
	if it < 2 {
		t.Fatalf("credited blocks = %d, want at least one full sweep (2 units)", it)
	}
	if rep.adds != it {
		t.Errorf("adds = %d, want one per credited block (%d)", rep.adds, it)
	}
	// Unique coverage: the region union, exactly once - never the
	// per-block sum across sweeps (that would count the same
	// positions twice), never 0 (blocks WERE verified).
	if rep.verified != total {
		t.Errorf("MemVerified = %d, want the exact region union %d", rep.verified, total)
	}
	// The journal pass channels stay whole-sweep. Bounds (a timed
	// run may sit anywhere in a sweep; a cancel landing on the last
	// unit's beat leaves an otherwise-complete sweep undeclared -
	// never over-claimed): credited units sit between passes*2 and
	// passes*2+2, and the declared traffic is exactly the declared
	// sweeps' volume. The deterministic subtests pin the exact
	// edges.
	if rep.ramTotal != rep.ramPasses*total {
		t.Errorf("pass traffic = %d, want exactly %d x declared sweeps", rep.ramTotal, total)
	}
	if it < rep.ramPasses*2 || it > rep.ramPasses*2+2 {
		t.Errorf("credited units %d inconsistent with %d declared sweeps (2 units each)", it, rep.ramPasses)
	}
	if rep.ramPasses > 0 && rep.ramUnique != total {
		t.Errorf("last-sweep unique = %d, want %d", rep.ramUnique, total)
	}
	if rep.ramFound != 0 {
		t.Errorf("false findings = %d", rep.ramFound)
	}
}

// The core honesty pins, deterministic: a 5 MiB region splits into a
// full 4 MiB block plus a 1 MiB tail. Cancellation is honored ONLY at
// block boundaries - a unit whose fill-beat is where the cancel
// lands still completes its verify and its credit (bounded overrun,
// written bytes never left unchecked) - and the pass-coverage
// channel stays UNKNOWN (nil-equivalent: zero declarations) until a
// full sweep completes, while MemVerified tells the partial truth.
func TestRamBlock4BoundariesAndPartialTruth(t *testing.T) {
	const region = 5 << 20
	full, tail := uint64(4<<20), uint64(1<<20)

	t.Run("cancel at first credit beat leaves the tail untouched", func(t *testing.T) {
		rep, v := block4Runner(t, 3, region)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rep.cancel, rep.cancelAt = cancel, 2 // cancel fires at block 1's credit-beat
		it, err := runRamBlock4(ctx, rep)
		if err != nil || it != 1 {
			t.Fatalf("iters=%d err=%v, want 1 credited block", it, err)
		}
		if rep.verified != full {
			t.Errorf("MemVerified = %d, want %d (credited blocks only)", rep.verified, full)
		}
		if rep.ramPasses != 0 || rep.ramTotal != 0 {
			t.Errorf("pass channels spoke for an incomplete sweep: passes=%d total=%d", rep.ramPasses, rep.ramTotal)
		}
		// The tail block was never even filled: committed-zero
		// territory is honest unknown ground, never credited.
		if v[0].view[full] != 0 || v[0].view[full+tail-1] != 0 {
			t.Error("the uncredited tail block was written")
		}
		// The credited block holds pass-1's image at its global
		// positions (identity: seed+pass+global word).
		if bad := ramVerifyChunk(v[0].view[:full], 3, 1, 0, ramAddressWord, nil); bad != 0 {
			t.Errorf("credited block does not hold pass-1 words: %d mismatches", bad)
		}
	})

	t.Run("cancel inside a unit never aborts the unit", func(t *testing.T) {
		// Cancel at the TAIL block's fill-beat: the fill already
		// happened, so the verify still runs (written-never-unchecked)
		// and the unit completes whole before the boundary return.
		rep, v := block4Runner(t, 3, region)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rep.cancel, rep.cancelAt = cancel, 3 // tail block's fill-beat
		it, err := runRamBlock4(ctx, rep)
		if err != nil || it != 2 {
			t.Fatalf("iters=%d err=%v, want 2: a started unit always completes", it, err)
		}
		if rep.verified != full+tail {
			t.Errorf("MemVerified = %d, want %d", rep.verified, full+tail)
		}
		// Even though every block of the sweep was credited, the
		// cancel landed before the sweep-end declaration: the pass
		// channel stays unknown, never retroactively inflated.
		if rep.ramPasses != 0 {
			t.Errorf("passes = %d, want 0: cancel landed inside the sweep", rep.ramPasses)
		}
		if bad := ramVerifyChunk(v[0].view[full:full+tail], 3, 1, full, ramAddressWord, nil); bad != 0 {
			t.Errorf("tail block content wrong: %d mismatches", bad)
		}
	})

	t.Run("second sweep re-credits positions but unique coverage never regrows", func(t *testing.T) {
		rep, v := block4Runner(t, 3, region)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Sweep 1 = beats 1-4 (then the sweep-end declaration);
		// cancel at block 1's fill-beat of sweep 2: both sweeps'
		// units complete, nothing else runs.
		rep.cancel, rep.cancelAt = cancel, 5
		it, err := runRamBlock4(ctx, rep)
		if err != nil || it != 3 {
			t.Fatalf("iters=%d err=%v, want 3", it, err)
		}
		if rep.verified != full+tail {
			t.Errorf("MemVerified = %d, want %d exactly: no double-credit of the same positions", rep.verified, full+tail)
		}
		if rep.ramPasses != 1 || rep.ramTotal != full+tail {
			t.Errorf("passes=%d total=%d, want exactly one whole sweep (%d traffic)", rep.ramPasses, rep.ramTotal, full+tail)
		}
		// Positions now hold the SECOND sweep's generation: a stale
		// pass-1 read would fault in any later compare.
		if bad := ramVerifyChunk(v[0].view[:full], 3, 2, 0, ramAddressWord, nil); bad != 0 {
			t.Errorf("sweep-2 refill did not land pass-2 words: %d mismatches", bad)
		}
		if bad := ramVerifyChunk(v[0].view[:full], 3, 1, 0, ramAddressWord, nil); bad == 0 {
			t.Error("pass-1 words still satisfy the pass-2 expectation: generations collapsed")
		}
	})
}

// Fault path with the evidence frozen: the flip lands between a
// block's fill-beat and its verify - the only window this kernel has,
// and its immediate verify catches it in the same unit. Everything
// credited before stays credited, the faulty unit earns nothing, the
// pass channels stay unknown (no sweep completed), and the kernel
// stops BEFORE any later sweep can refill the scene: the flipped
// byte is still there when the run returns.
func TestRamBlock4FlipCaughtInUnit(t *testing.T) {
	rep, v := block4Runner(t, 7, 128<<10, 64<<10)
	// beat 3 = region 1's fill-beat: corrupt its word at logical
	// offset 4 right after the fill, before its verify reads it.
	flipped := ^uint32(0)
	rep.flipAt = 3
	rep.flip = func() {
		binary.LittleEndian.PutUint32(v[1].view[4:8], flipped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamBlock4(ctx, rep)
	if err == nil {
		t.Fatal("flip between fill and verify went undetected")
	}
	if !strings.Contains(err.Error(), "ram-block4") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("credited blocks = %d, want 1 (region 0 clean, region 1's unit faults)", it)
	}
	if rep.verified != 128<<10 {
		t.Errorf("MemVerified = %d, want only the credited region (%d)", rep.verified, 128<<10)
	}
	if rep.ramPasses != 0 {
		t.Errorf("a faulting sweep declared pass coverage: %d", rep.ramPasses)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-block4-v1" || g.Seed != 7 || g.Pass != 1 || g.Phase != "verify-block" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Region != "worker=0 region=1 role=source offset=131072" {
		t.Errorf("finding region identity = %q", g.Region)
	}
	if g.Offset != 4 {
		t.Errorf("finding offset = %d, want 4 (logical within the region)", g.Offset)
	}
	want := ramAddressWord(7, 1, (131072+4)/4)
	if uint32(g.Expected) != want || uint32(g.Actual) != flipped {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", uint32(g.Expected), uint32(g.Actual), want, flipped)
	}
	// Evidence frozen where it was found: no refill overwrote the
	// scene before the run stopped.
	if binary.LittleEndian.Uint32(v[1].view[4:8]) != flipped {
		t.Error("the faulting bytes were overwritten after the mismatch")
	}
}

// The expected-word identity is the shared pinned spec: a run under
// seed 42 must fault EVERY word when verified under seed 43 or pass
// 2, and must verify clean under its own seed+pass. A kernel whose
// image ignored the seed would self-verify and test nothing.
func TestRamBlock4WrongSeedAndPassDetected(t *testing.T) {
	rep, v := block4Runner(t, 42, 128<<10, 64<<10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel, rep.cancelAt = cancel, 4 // sweep 1's last unit credits at beat 4, the boundary return follows (beat 6 would already refill region 0 with pass-2 words)
	it, err := runRamBlock4(ctx, rep)
	if err != nil || it != 2 {
		t.Fatalf("setup: iters=%d err=%v", it, err)
	}
	for i, rv := range v {
		badSeed := ramVerifyChunk(rv.view, 43, 1, rv.goff, ramAddressWord, nil)
		if badSeed != len(rv.view)/4 {
			t.Errorf("region %d under seed 43: %d mismatches, want every word (%d)", i, badSeed, len(rv.view)/4)
		}
		badPass := ramVerifyChunk(rv.view, 42, 2, rv.goff, ramAddressWord, nil)
		if badPass != len(rv.view)/4 {
			t.Errorf("region %d under pass 2: %d mismatches, want every word (%d)", i, badPass, len(rv.view)/4)
		}
		if ok := ramVerifyChunk(rv.view, 42, 1, rv.goff, ramAddressWord, nil); ok != 0 {
			t.Errorf("region %d no longer verifies clean under its own seed+pass: %d", i, ok)
		}
	}
	// Regions must hold DIFFERENT data at equal local offsets (the
	// global position differentiates, same as every ram kernel).
	if binary.LittleEndian.Uint32(v[0].view) == binary.LittleEndian.Uint32(v[1].view) {
		t.Error("regions identical at equal local offsets: position stopped mattering")
	}
}

// The registry contract rides the journal: version, fault class,
// verification class and the checks-count promise (verified blocks
// are this kernel's confirmed checks).
func TestRamBlock4RegistryContract(t *testing.T) {
	w, ok := workloads["ram-block4"]
	if !ok {
		t.Fatal("ram-block4 missing from the registry")
	}
	if !w.RequiresMemory {
		t.Error("ram-block4 must RequireMemory (engine regions or a refusal before load)")
	}
	if w.Verification != VerifyIndependent {
		t.Errorf("verification = %q, want independent reconstruction of every expected word", w.Verification)
	}
	if got := KernelVersionOf("ram-block4"); got != "ram-block4-v1" {
		t.Errorf("kernel version = %q, want ram-block4-v1", got)
	}
	if got := FaultClassOf("ram-block4"); got != FaultMemory {
		t.Errorf("fault class = %q, want %q", got, FaultMemory)
	}
	if !CountsIterationsAsChecks("ram-block4") {
		t.Error("verified blocks must count as confirmed checks")
	}
}
