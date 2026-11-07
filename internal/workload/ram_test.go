package workload

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// Beat map for the 128K+64K region pair (chunks of 64K: 3 per fill,
// 3 per verify, 6 beats per pass):
//
//	pass 1 fill: beats 1,2,3 (r1c1, r1c2, r2c1)
//	pass 1 verify: beats 4,5,6
//	pass 2 fill: beats 7,8,9; pass 2 verify: beats 10,11,12

func ramTestReporter(t *testing.T) *regionReporter {
	t.Helper()
	e := openTestEngine(t, 128<<10, 64<<10)
	return &regionReporter{regions: e.Regions(), seed: 7}
}

func TestRamConstantPassesAndCoverage(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamConstant(ctx, rep)
	if err != nil {
		t.Fatalf("runRamConstant: %v", err)
	}
	if it == 0 {
		t.Fatal("no pattern passes completed in 300ms")
	}
	if rep.ramPasses != it || rep.adds != it {
		t.Errorf("accounting drift: iters=%d ramPasses=%d adds=%d", it, rep.ramPasses, rep.adds)
	}
	const all = uint64(192 << 10)
	if rep.ramUnique != all {
		t.Errorf("unique coverage = %d, want %d (every byte of both regions per pass)", rep.ramUnique, all)
	}
	if rep.ramTotal != it*all {
		t.Errorf("total coverage = %d, want %d (unique x passes)", rep.ramTotal, it*all)
	}
	if rep.verified != all {
		t.Errorf("MemVerified = %d, want %d", rep.verified, all)
	}
	if rep.ramFound != 0 {
		t.Errorf("clean run reported %d mismatches", rep.ramFound)
	}
	// What the regions hold depends on exactly where the last pass
	// was interrupted: a run that stopped at a pass boundary holds
	// the last completed pattern everywhere; a graceful stop mid-fill
	// holds a PREFIX of the interrupted pass's pattern over the rest.
	// Both are honest; anything else (garbage words, a suffix of the
	// new pattern, the new pattern after the old) is not.
	cur := ramPatterns[it%uint64(len(ramPatterns))]
	prev := ramPatterns[(it-1)%uint64(len(ramPatterns))]
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		state := 0 // 0: still inside the new-pattern prefix; 1: old pattern territory
		for i := 0; i+4 <= len(v); i += 4 {
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word %d = %08x: not a prefix of %08x over %08x (interrupted-fill layout)",
					r.Grant.Region, i/4, got, cur, prev)
			}
		}
	}
}

// The flip test is the semantic pin for the chunk primitives: if the
// compiler ever forwarded stored values through the verification
// read (the DSE hazard measured in this project), flipped bytes in
// real memory would go UNDETECTED and this test fails. It also pins
// that verify does not stop at the first bad word: every mismatch
// inside the chunk is counted and reported with its exact offset,
// including the first word, the middle, the last word and the tail
// byte.
func TestRamConstantChunkFlips(t *testing.T) {
	e := openTestEngine(t, 4097) // exercises the word-tail path too
	v := e.Regions()[0].Block.Bytes()
	for p, pat := range ramPatterns {
		pass := uint64(p + 1)
		ramFillChunk(v, 0, pass, 0, ramConstantWord)
		var got []ramFindingLite
		report := func(local int, exp, act uint32) {
			got = append(got, ramFindingLite{Offset: uint64(local), Expected: uint64(exp), Actual: uint64(act)})
		}
		if bad := ramVerifyChunk(v, 0, pass, 0, ramConstantWord, report); bad != 0 || len(got) != 0 {
			t.Fatalf("pattern %08x: clean chunk reported %d mismatches", pat, bad)
		}
		// Flip: first word LSB, middle word, last word LSB, tail byte.
		v[0] ^= 0x01
		binary.LittleEndian.PutUint32(v[2048:], binary.LittleEndian.Uint32(v[2048:])^0xDEAD0000)
		v[4092] ^= 0x80
		v[4096] ^= 0x10
		got = nil
		bad := ramVerifyChunk(v, 0, pass, 0, ramConstantWord, report)
		if bad != 4 {
			t.Fatalf("pattern %08x: %d mismatches counted, want all 4 (verify must not stop at the first)", pat, bad)
		}
		wantOff := []uint64{0, 2048, 4092, 4096}
		for i, o := range wantOff {
			if i >= len(got) {
				t.Fatalf("pattern %08x: finding %d (offset %d) not reported", pat, i, o)
			}
			if got[i].Offset != o {
				t.Errorf("pattern %08x: finding %d offset = %d, want %d", pat, i, got[i].Offset, o)
			}
			if got[i].Expected != uint64(pat) && o != 4096 {
				t.Errorf("pattern %08x: finding %d expected %08x, want pattern %08x", pat, i, got[i].Expected, pat)
			}
			if got[i].Actual == got[i].Expected {
				t.Errorf("pattern %08x: finding %d expected==actual: the XOR would be zero", pat, i)
			}
		}
		v[0] ^= 0x01
		binary.LittleEndian.PutUint32(v[2048:], binary.LittleEndian.Uint32(v[2048:])^0xDEAD0000)
		v[4092] ^= 0x80
		v[4096] ^= 0x10
	}
}

// End-to-end corruption: flip a word that pass 2 has already written
// (beat 8 = pass 2 fill of region 1 chunk 2; flip lands in chunk 1,
// already filled). The verify pass must catch it, report the finding
// with full identity (algorithm, seed, pass, region, offset, phase),
// and fault - and the mismatch must NOT be overwritten by any later
// pattern (the pass aborts).
func TestRamConstantEndToEndFlip(t *testing.T) {
	rep := ramTestReporter(t)
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 8
	rep.flip = func() { v[4] ^= 0x01 } // LSB of the word at logical offset 4

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamConstant(ctx, rep)
	if err == nil {
		t.Fatal("flipped word in engine memory went undetected")
	}
	if !strings.Contains(err.Error(), "ram-constant") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (pass 1 clean, pass 2 aborted)", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-constant-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != 4 {
		t.Errorf("finding offset = %d, want 4 (logical, within the region)", g.Offset)
	}
	if g.Expected != 0xFFFFFFFF || g.Actual != 0xFFFFFFFE {
		t.Errorf("finding values = %08x/%08x, want FFFFFFFF/FFFFFFFE", g.Expected, g.Actual)
	}
	if g.Region != "worker=0 region=0 role=source offset=0" {
		t.Errorf("finding region identity = %q", g.Region)
	}
}

// Graceful stop mid-fill: cancel lands after the current chunk is
// written (beat 8 = pass 2 fill, region 1 chunk 2). The kernel must
// stop filling, VERIFY the entire filled prefix (no written byte
// goes unchecked), and stop with the partial pass NOT counted and
// coverage NOT credited.
func TestRamConstantGracefulStopMidFill(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamConstant(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (the interrupted pass must not count)", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1 (partial pass earns no coverage)", rep.ramPasses)
	}
	v1 := rep.regions[0].Block.Bytes()
	v2 := rep.regions[1].Block.Bytes()
	// Region 1 was fully refilled with pass 2's pattern (FF); region 2
	// was never refilled this pass and still holds pass 1's pattern
	// (00) - the fill stopped exactly where cancel landed.
	for i := 0; i+4 <= len(v1); i += 4 {
		if got := binary.LittleEndian.Uint32(v1[i:]); got != 0xFFFFFFFF {
			t.Fatalf("region 1 word %d = %08x, want FFFFFFFF (pass 2 filled it)", i/4, got)
		}
	}
	for i := 0; i+4 <= len(v2); i += 4 {
		if got := binary.LittleEndian.Uint32(v2[i:]); got != 0x00000000 {
			t.Fatalf("region 2 word %d = %08x, want 00000000 (pass 2 never touched it)", i/4, got)
		}
	}
}

// A fault outranks a cancellation: when the same beat both corrupts
// memory and cancels the run, the verdict is the fault - the verify
// pass still runs to completion over the filled prefix, finds the
// broken word, and returns the error.
func TestRamConstantFaultBeatsCancel(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 8
	rep.flip = func() { v[4] ^= 0x01 }
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamConstant(ctx, rep)
	if err == nil {
		t.Fatal("cancel masked a real mismatch: the fault must win")
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramFound == 0 {
		t.Error("the mismatch that decided the verdict left no finding")
	}
}

// Cancel before anything ran: zero iterations, zero checks, zero
// coverage - and no error. Nothing may look validated.
func TestRamConstantUpfrontCancel(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamConstant(ctx, rep)
	if err != nil {
		t.Fatalf("upfront cancel: %v", err)
	}
	if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
			it, rep.adds, rep.ramPasses, rep.verified)
	}
}

// Without engine regions the kernel refuses outright: silently
// testing the Go heap instead of RAM would be a fabricated result.
func TestRamConstantRefusesWithoutEngine(t *testing.T) {
	rep := &regionReporter{} // no regions
	_, err := runRamConstant(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
		t.Errorf("err = %v, want the no-engine refusal", err)
	}
}
