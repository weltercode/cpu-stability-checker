package workload

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// The PRNG spec is the reproduction contract: golden values are
// pinned as literals (computed once, then frozen). If the mixing
// moves without a kernel-version bump, every past journal's
// expected data silently changes - this test is what refuses that.
func TestRamRandomWordGoldenVectors(t *testing.T) {
	tests := []struct {
		seed, pass, wordIdx uint64
		want                uint32
	}{
		{0, 1, 0, 0x696908be},
		{1, 1, 1, 0xbd8949c6},
		{0xDEADBEEFCAFEBABE, 7, 12345, 0xa885e180},
	}
	for _, tt := range tests {
		if got := ramRandomWord(tt.seed, tt.pass, tt.wordIdx); got != tt.want {
			t.Errorf("ramRandomWord(%d,%d,%d) = %08x, want the pinned %08x", tt.seed, tt.pass, tt.wordIdx, got, tt.want)
		}
	}
	// The two word functions are different specs: identical outputs
	// at the same coordinates would mean one kernel is a no-op
	// rename of the other, not a different test.
	for _, c := range []struct{ seed, pass, idx uint64 }{
		{0, 1, 0}, {0xDEADBEEFCAFEBABE, 7, 12345}, {42, 3, 999},
	} {
		if ramRandomWord(c.seed, c.pass, c.idx) == ramAddressWord(c.seed, c.pass, c.idx) {
			t.Errorf("ram-random and ram-address agree at (%d,%d,%d): the specs are not distinct", c.seed, c.pass, c.idx)
		}
	}
	// Coordinate sensitivity + no sampled collisions: a collapse
	// would silently shrink what the verify pass can detect.
	seen := map[uint32]uint64{}
	for i := range 500 {
		w := ramRandomWord(9, 3, uint64(i))
		if prev, dup := seen[w]; dup {
			t.Fatalf("stream collides: idx %d and %d both %08x", prev, i, w)
		}
		seen[w] = uint64(i)
	}
	for _, c := range []struct{ seed, pass, idx uint64 }{
		{10, 3, 7}, {9, 4, 7}, {9, 3, 8},
	} {
		if ramRandomWord(c.seed, c.pass, c.idx) == ramRandomWord(9, 3, 7) {
			t.Errorf("coordinate %v maps to the same word as (9,3,7): seed/pass/position must each matter", c)
		}
	}
}

// Contents after a run: every word equals f(seed, pass, GLOBAL word
// index) for the completed pass - or the honest chunk-aligned
// prefix of the interrupted next fill. A clean run must report
// ZERO false mismatches (the plan's reference-buffer check).
func TestRamRandomContentsNoFalseErrors(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 11}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamRandom(ctx, rep)
	if err != nil {
		t.Fatalf("runRamRandom: %v", err)
	}
	if it == 0 {
		t.Fatal("no passes completed")
	}
	if rep.ramFound != 0 {
		t.Fatalf("clean engine memory reported %d false mismatches", rep.ramFound)
	}
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		state := 0
		for i := 0; i+4 <= len(v); i += 4 {
			gi := base + uint64(i)/4
			cur := ramRandomWord(11, it+1, gi)
			prev := ramRandomWord(11, it, gi)
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word (global %d) = %08x, want %08x (completed pass) or %08x-prefix layout",
					r.Grant.Region, gi, got, prev, cur)
			}
		}
	}
}

// Independent reconstruction (the plan's core requirement): the
// expected word comes from the spec alone. Verify the same region
// with the wrong seed and EVERY word mismatches - the kernel's own
// verify cannot be quietly agreeing with what memory happens to
// hold.
func TestRamRandomWrongSeedDetected(t *testing.T) {
	e := openTestEngine(t, 128<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 11}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 4 // 2-chunk region: pass 1 completes, stop at the loop top
	it, err := runRamRandom(ctx, rep)
	if err != nil || it != 1 {
		t.Fatalf("setup: iters=%d err=%v", it, err)
	}
	v := rep.regions[0].Block.Bytes()
	if bad := ramVerifyChunk(v, 12, 1, 0, ramRandomWord, nil); bad != len(v)/4 {
		t.Errorf("wrong seed: %d mismatches, want every word (%d)", bad, len(v)/4)
	}
	if bad := ramVerifyChunk(v, 11, 2, 0, ramRandomWord, nil); bad != len(v)/4 {
		t.Errorf("wrong pass: %d mismatches, want every word (%d)", bad, len(v)/4)
	}
	if bad := ramVerifyChunk(v, 11, 1, 0, ramRandomWord, nil); bad != 0 {
		t.Errorf("same seed+pass no longer verifies clean: %d", bad)
	}
}

// Repartition invariance again (the seed semantics must not wobble
// with the worker count): identical global positions demand
// identical words under a single 192K region and a 128K+64K split,
// both stopped at the same deterministic beat.
func TestRamRandomRepartitionInvariance(t *testing.T) {
	seed := uint64(0x5EED)
	runLayout := func(sizes ...uint64) []byte {
		e := openTestEngine(t, sizes...)
		rep := &regionReporter{regions: e.Regions(), seed: seed}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rep.cancel = cancel
		rep.cancelAt = 8
		it, err := runRamRandom(ctx, rep)
		if err != nil || it != 1 {
			t.Fatalf("layout %v: iters=%d err=%v", sizes, it, err)
		}
		var flat []byte
		for _, r := range rep.regions {
			flat = append(flat, r.Block.Bytes()...)
		}
		return flat
	}
	single := runLayout(192 << 10)
	split := runLayout(128<<10, 64<<10)
	if len(single) != len(split) {
		t.Fatalf("coverage differs: %d vs %d bytes", len(single), len(split))
	}
	for i := 0; i+4 <= len(single); i += 4 {
		a := binary.LittleEndian.Uint32(single[i:])
		b := binary.LittleEndian.Uint32(split[i:])
		if a != b {
			t.Fatalf("global word %d differs between layouts: %08x vs %08x", i/4, a, b)
		}
	}
}

// End-to-end injected corruption: flip one written word mid-pass;
// the verify sweep must fault with the ram-random-v1 identity on
// the finding (the plan: every verifiable part must catch injected
// damage, and the report must be reproducible).
func TestRamRandomEndToEndFlip(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 11}
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 8 // pass 2 fill, region 1 chunk 2: flip a word already written this pass
	rep.flip = func() { v[4] ^= 0x01 }
	it, err := runRamRandom(context.Background(), rep)
	if err == nil {
		t.Fatal("flipped word went undetected")
	}
	if it != 1 {
		t.Errorf("passes = %d, want 1 (pass 2 aborted)", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-random-v1" || g.Seed != 11 || g.Pass != 2 || g.Phase != "verify" || g.Offset != 4 {
		t.Errorf("finding identity wrong: %+v", g)
	}
	want := ramRandomWord(11, 2, 1)
	if g.Expected != uint64(want) || g.Actual != uint64(want^0x01) {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", g.Expected, g.Actual, want, want^0x01)
	}
}
