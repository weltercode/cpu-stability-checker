package workload

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// The word function is the reproduction spec: pin exact golden
// values so a future "optimization" that changes the mixing cannot
// silently pass - it would change every past run's expected data.
// If these constants move, the kernel version MUST move with them.
func TestRamAddressWordGoldenVectors(t *testing.T) {
	tests := []struct {
		seed, pass, wordIdx uint64
		want                uint32
	}{
		{0, 1, 0, 0x057291b3},
		{1, 1, 1, 0x1438bcb5},
		{0xDEADBEEFCAFEBABE, 7, 12345, 0xeb7bb54b},
	}
	for _, tt := range tests {
		if got := ramAddressWord(tt.seed, tt.pass, tt.wordIdx); got != tt.want {
			t.Errorf("ramAddressWord(%d,%d,%d) = %08x, want the pinned %08x", tt.seed, tt.pass, tt.wordIdx, got, tt.want)
		}
	}
	// Distinct coordinates must give distinct words (sampled): a
	// collapse would silently shrink what the test can detect.
	seen := map[uint32]uint64{}
	for i := range 500 {
		w := ramAddressWord(9, 3, uint64(i))
		if prev, dup := seen[w]; dup {
			t.Fatalf("words collide: idx %d and %d both %08x", prev, i, w)
		}
		seen[w] = uint64(i)
	}
	for _, c := range []struct{ seed, pass, idx uint64 }{
		{10, 3, 7}, {9, 4, 7}, {9, 3, 8},
	} {
		if ramAddressWord(c.seed, c.pass, c.idx) == ramAddressWord(9, 3, 7) {
			t.Errorf("coordinate %v maps to the same word as (9,3,7): the seed/pass/position must each matter", c)
		}
	}
}

// End-to-end: after a run, every word must equal f(seed, pass,
// GLOBAL word index) for the completed pass - or, where a graceful
// stop interrupted the next fill, f(seed, pass+1, idx) as a
// chunk-aligned prefix. Regions must hold DIFFERENT data at equal
// local offsets (their global offsets differ, per the contract).
func TestRamAddressContentsAndRegionDiversity(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 42}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamAddress(ctx, rep)
	if err != nil {
		t.Fatalf("runRamAddress: %v", err)
	}
	if it == 0 {
		t.Fatal("no passes completed")
	}
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		state := 0
		for i := 0; i+4 <= len(v); i += 4 {
			gi := base + uint64(i)/4
			cur := ramAddressWord(42, it+1, gi)
			prev := ramAddressWord(42, it, gi)
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word %d (global %d) = %08x, want %08x (completed pass) or %08x-prefix layout",
					r.Grant.Region, i/4, gi, got, prev, cur)
			}
		}
	}
	v0 := rep.regions[0].Block.Bytes()
	v1 := rep.regions[1].Block.Bytes()
	if binary.LittleEndian.Uint32(v0) == binary.LittleEndian.Uint32(v1) {
		t.Error("regions hold identical words at equal local offsets: the global position must differentiate them")
	}
}

// The expected word must depend on the seed and the pass: verify a
// region written under seed 42 / pass 1 with seed 43 or pass 2 and
// EVERY word must mismatch. A kernel whose expected value ignored
// the seed would pass its own verify and silently test nothing.
func TestRamAddressWrongSeedAndPassDetected(t *testing.T) {
	e := openTestEngine(t, 128<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 42}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 4 // 128K region = 2 chunks: pass 1 fill=beats 1,2, verify=3,4; cancel lands at the loop top
	it, err := runRamAddress(ctx, rep)
	if err != nil || it != 1 {
		t.Fatalf("setup: iters=%d err=%v", it, err)
	}
	v := rep.regions[0].Block.Bytes()
	badSeed := ramVerifyChunk(v, 43, 1, 0, ramAddressWord, nil)
	if badSeed != len(v)/4 {
		t.Errorf("wrong seed: %d mismatches, want every word (%d)", badSeed, len(v)/4)
	}
	badPass := ramVerifyChunk(v, 42, 2, 0, ramAddressWord, nil)
	if badPass != len(v)/4 {
		t.Errorf("wrong pass: %d mismatches, want every word (%d)", badPass, len(v)/4)
	}
	if ok := ramVerifyChunk(v, 42, 1, 0, ramAddressWord, nil); ok != 0 {
		t.Errorf("the same seed+pass no longer verifies clean: %d", ok)
	}
}

// A block swap moves REAL data between positions: the words are
// valid for their source offsets and wrong for their destination -
// exactly what an address-derived test catches that a constant
// pattern cannot. Verify must flag them at the destination offsets.
func TestRamAddressBlockSwapDetected(t *testing.T) {
	e := openTestEngine(t, 128<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 42}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 4 // pass 1 completes cleanly; the region holds pass 1's data
	it, err := runRamAddress(ctx, rep)
	if err != nil || it != 1 {
		t.Fatalf("setup: iters=%d err=%v", it, err)
	}
	v := rep.regions[0].Block.Bytes()
	// Copy chunk 1 over chunk 2 (both 64 KiB, same region).
	copy(v[64<<10:128<<10], v[0:64<<10])
	var got []ramFindingLite
	report := func(local int, exp, act uint32) {
		got = append(got, ramFindingLite{Offset: uint64(local), Expected: uint64(exp), Actual: uint64(act)})
	}
	bad := ramVerifyChunk(v[64<<10:128<<10], 42, 1, 64<<10, ramAddressWord, report)
	if bad == 0 {
		t.Fatal("block swap went undetected: address-derived data must catch relocated blocks")
	}
	// The swapped chunk is entirely wrong (chunk 1's words at chunk
	// 2's positions): every word of the destination chunk mismatches.
	if bad != (64<<10)/4 {
		t.Errorf("swap mismatches = %d, want the whole destination chunk (%d)", bad, (64<<10)/4)
	}
	if len(got) > 0 && got[0].Offset != 0 {
		t.Errorf("first finding offset = %d, want 0 (chunk-local; the base offset is added by the kernel)", got[0].Offset)
	}
}

// Repartition invariance (the plan's "changing worker count must not
// silently change the chosen seed semantics"): the SAME global
// positions must demand the SAME words under a single 192K region
// and under a 128K+64K split. Both runs stop at the identical beat
// (deterministic), so completed passes match and every global word
// must be byte-identical across the two layouts.
func TestRamAddressRepartitionInvariance(t *testing.T) {
	seed := uint64(0xBEEF)

	runLayout := func(sizes ...uint64) []byte {
		e := openTestEngine(t, sizes...)
		rep := &regionReporter{regions: e.Regions(), seed: seed}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rep.cancel = cancel
		rep.cancelAt = 8 // same deterministic stop point in both layouts (3 chunks per phase each)
		it, err := runRamAddress(ctx, rep)
		if err != nil || it != 1 {
			t.Fatalf("layout %v: iters=%d err=%v", sizes, it, err)
		}
		// Flatten into global-offset order (grants are laid out
		// contiguously by openTestEngine).
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
			t.Fatalf("global word %d differs between layouts: %08x vs %08x - the budget split silently changed the expected data", i/4, a, b)
		}
	}
}
