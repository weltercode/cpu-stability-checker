package workload

import (
	"context"
	"encoding/binary"
	"math/bits"
	"strings"
	"testing"
	"time"
)

// The pass schedule is the reproduction contract: golden values are
// pinned as literals, and the structural checks below pin the whole
// 64-pass cycle at once (every bit visited exactly once in each
// state, ones-half and zeros-half exact complements, period 64). If
// the schedule moves without a kernel-version bump, every past
// journal's expected data silently changes - this test refuses that.
func TestRamWalkingWordGoldenVectors(t *testing.T) {
	tests := []struct {
		pass uint64
		want uint32
	}{
		{1, 0x00000001},
		{2, 0x00000002},
		{32, 0x80000000},
		{33, 0xFFFFFFFE},
		{64, 0x7FFFFFFF},
		{65, 0x00000001},   // the cycle repeats: pass 65 == pass 1
		{100, 0xFFFFFFF7},  // (100-1)%64 = 35 -> walking zero, bit 3 low
		{1000, 0xFFFFFF7F}, // (1000-1)%64 = 39 -> walking zero, bit 7 low
	}
	for _, tt := range tests {
		if got := ramWalkingWord(0, tt.pass, 0); got != tt.want {
			t.Errorf("ramWalkingWord(0,%d,0) = %08x, want the pinned %08x", tt.pass, got, tt.want)
		}
	}

	// Structural pin over the whole cycle: walking-ones passes have
	// exactly one set bit at the scheduled position; walking-zeros
	// passes have exactly one clear bit; the halves are complements.
	for c := uint64(0); c < ramWalkingCycle; c++ {
		w := ramWalkingWord(0, c+1, 0)
		if c < 32 {
			if bits.OnesCount32(w) != 1 || w != uint32(1)<<c {
				t.Errorf("pass %d: %08x is not walking-one at bit %d", c+1, w, c)
			}
			if z := ramWalkingWord(0, c+33, 0); z != ^w {
				t.Errorf("pass %d (%08x) and pass %d (%08x) are not complements", c+1, w, c+33, z)
			}
		} else {
			if bits.OnesCount32(w) != 31 || w != ^(uint32(1)<<(c-32)) {
				t.Errorf("pass %d: %08x is not walking-zero at bit %d", c+1, w, c-32)
			}
		}
	}

	// Period 64 beyond the first cycle: the schedule must not drift.
	for _, p := range []uint64{1, 17, 33, 64, 129, 1000} {
		if ramWalkingWord(0, p, 0) != ramWalkingWord(0, p+64, 0) {
			t.Errorf("pass %d and pass %d+64 differ: the cycle drifted", p, p)
		}
	}

	// Seed and position independence is part of the spec (documented
	// on ramWalkingWord): every word must see every bit state.
	for _, pass := range []uint64{1, 32, 33, 64} {
		w := ramWalkingWord(0, pass, 0)
		if got := ramWalkingWord(0xDEADBEEFCAFEBABE, pass, 9876543); got != w {
			t.Errorf("pass %d: seed/position changed the pattern (%08x vs %08x)", pass, got, w)
		}
	}
}

// The specs must stay distinct: identical outputs at the same
// coordinates would make ram-walking a no-op rename of another
// kernel, not a different test.
func TestRamWalkingDistinctFromOtherSpecs(t *testing.T) {
	for _, c := range []struct{ seed, pass, idx uint64 }{
		{0, 1, 0}, {0xDEADBEEFCAFEBABE, 7, 12345}, {42, 3, 999},
	} {
		w := ramWalkingWord(c.seed, c.pass, c.idx)
		if w == ramConstantWord(c.seed, c.pass, c.idx) {
			t.Errorf("ram-walking and ram-constant agree at %+v: the specs are not distinct", c)
		}
		if w == ramAddressWord(c.seed, c.pass, c.idx) {
			t.Errorf("ram-walking and ram-address agree at %+v: the specs are not distinct", c)
		}
		if w == ramRandomWord(c.seed, c.pass, c.idx) {
			t.Errorf("ram-walking and ram-random agree at %+v: the specs are not distinct", c)
		}
	}
}

// Contents and coverage after a timed run: every word holds the
// completed pass's pattern (or the honest chunk-aligned prefix of
// the interrupted next fill), coverage is accounted exactly, and a
// clean run reports ZERO false mismatches.
func TestRamWalkingPassesAndCoverage(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamWalking(ctx, rep)
	if err != nil {
		t.Fatalf("runRamWalking: %v", err)
	}
	if it == 0 {
		t.Fatal("no walking passes completed in 300ms")
	}
	if rep.ramPasses != it || rep.adds != it {
		t.Errorf("accounting drift: iters=%d ramPasses=%d adds=%d", it, rep.ramPasses, rep.adds)
	}
	const all = uint64(192 << 10)
	if rep.ramUnique != all || rep.ramTotal != it*all || rep.verified != all {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d (it=%d)", rep.ramUnique, rep.ramTotal, rep.verified, it)
	}
	if rep.ramFound != 0 {
		t.Errorf("clean run reported %d mismatches", rep.ramFound)
	}
	// The interrupted NEXT fill (pass it+1) leaves its pattern as a
	// chunk-aligned prefix over the last completed pass's pattern
	// (pass it): both states are honest, anything else is not.
	cur := ramWalkingWord(7, it+1, 0)
	prev := ramWalkingWord(7, it, 0)
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		state := 0 // 0: inside the new-pattern prefix; 1: old-pattern territory
		for i := 0; i+4 <= len(v); i += 4 {
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word %d = %08x: not a prefix of %08x over %08x",
					r.Grant.Region, i/4, got, cur, prev)
			}
		}
	}
}

// End-to-end corruption: flip the LSB of a word pass 2 has already
// written (beat 8 flips during pass 2's fill, landing in already-
// filled territory). Pass 2's pattern is walking-one bit 1
// (0x00000002), so the flipped word reads 0x00000003: the verify
// pass must catch it and fault with the full ram-walking identity.
// The walking-zero half matters here too: a stuck-at-1 line would
// hide in the ones-half and only the zeros-half would expose it -
// this kernel runs both, and the flip proves the verify reads real
// memory either way.
func TestRamWalkingEndToEndFlip(t *testing.T) {
	rep := ramTestReporter(t)
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 8
	rep.flip = func() { v[4] ^= 0x01 } // LSB of the word at logical offset 4

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamWalking(ctx, rep)
	if err == nil {
		t.Fatal("flipped word in engine memory went undetected")
	}
	if !strings.Contains(err.Error(), "ram-walking") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (pass 1 clean, pass 2 aborted)", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-walking-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != 4 {
		t.Errorf("finding offset = %d, want 4 (logical, within the region)", g.Offset)
	}
	if g.Expected != 0x00000002 || g.Actual != 0x00000003 {
		t.Errorf("finding values = %08x/%08x, want 00000002/00000003", g.Expected, g.Actual)
	}
}

// A forced bit high inside the walking-zero half: pass 34's pattern
// is ^(1<<1) = 0xFFFFFFFD, and OR-ing 0x02 into the byte at offset 4
// pins that line high - a stuck-at-1 the ones-half could never see:
// at pass 2 (walking-one bit 1) the word legitimately reads
// 0x00000002, so a line stuck high matches until the zeros-half
// demands the 0. This is the semantic reason the cycle must include
// both halves.
func TestRamWalkingZerosHalfCatchesStuckAt1(t *testing.T) {
	rep := ramTestReporter(t)
	v := rep.regions[0].Block.Bytes()
	// 6 beats per pass (3 fill + 3 verify): passes 1..33 burn 198
	// beats; beat 200 is pass 34's fill of region 0 chunk 2, and the
	// flip lands in chunk 1, already filled with pass 34's pattern.
	const targetBeat = 33*6 + 2
	rep.flipAt = targetBeat
	rep.flip = func() { v[4] |= 0x02 } // force bit 1 high: stuck-at-1 at that line

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	it, err := runRamWalking(ctx, rep)
	if err == nil {
		t.Fatal("stuck-at-1 went undetected through 33 passes of walking ones")
	}
	if it != 33 {
		t.Errorf("completed passes = %d, want 33 (passes 1..33 clean, pass 34 aborted)", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-walking-v1" || g.Pass != 34 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Expected != 0xFFFFFFFD || g.Actual != 0xFFFFFFFF {
		t.Errorf("finding values = %08x/%08x, want FFFFFFFD/FFFFFFFF", g.Expected, g.Actual)
	}
	// The ones-half alone could never have caught this: at pass 2
	// (walking-one bit 1) the word at offset 4 legitimately reads
	// 0x00000002 - a stuck-at-1 line is invisible until the zeros
	// half demands the 0.
	if want := ramWalkingWord(7, 2, 0); want != 0x00000002 {
		t.Fatalf("spec drift: pass 2 pattern = %08x", want)
	}
}

// Graceful stop mid-fill: cancel lands after the current chunk
// (beat 8 = pass 2 fill, region 0 chunk 2). The kernel stops
// filling, verifies the entire filled prefix, and the partial pass
// is NOT counted and NOT credited. Region 0 holds pass 2's pattern
// everywhere; region 1 was never refilled and still holds pass 1's.
func TestRamWalkingGracefulStopMidFill(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamWalking(ctx, rep)
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
	for i := 0; i+4 <= len(v1); i += 4 {
		if got := binary.LittleEndian.Uint32(v1[i:]); got != 0x00000002 {
			t.Fatalf("region 1 word %d = %08x, want 00000002 (pass 2 filled it)", i/4, got)
		}
	}
	for i := 0; i+4 <= len(v2); i += 4 {
		if got := binary.LittleEndian.Uint32(v2[i:]); got != 0x00000001 {
			t.Fatalf("region 2 word %d = %08x, want 00000001 (pass 2 never touched it)", i/4, got)
		}
	}
}

// A fault outranks a cancellation: corrupt and cancel on the same
// beat, and the verdict is the fault.
func TestRamWalkingFaultBeatsCancel(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 8
	rep.flip = func() { v[4] ^= 0x01 }
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamWalking(ctx, rep)
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
// coverage - and no error.
func TestRamWalkingUpfrontCancel(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamWalking(ctx, rep)
	if err != nil {
		t.Fatalf("upfront cancel: %v", err)
	}
	if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
			it, rep.adds, rep.ramPasses, rep.verified)
	}
}

// Without engine regions the kernel refuses outright.
func TestRamWalkingRefusesWithoutEngine(t *testing.T) {
	rep := &regionReporter{}
	_, err := runRamWalking(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
		t.Errorf("err = %v, want the no-engine refusal", err)
	}
}

// The registry must carry the new kernel with its identity and fault
// class: a phase naming ram-walking resolves to THIS versioned spec,
// and its faults attribute to memory, not to the CPU core that
// happened to observe the mismatch.
func TestRamWalkingRegistryEntry(t *testing.T) {
	w, ok := workloads["ram-walking"]
	if !ok {
		t.Fatal("ram-walking missing from the workload registry")
	}
	if w.KernelVersion != "ram-walking-v1" || !w.RequiresMemory || w.FaultClass != FaultMemory {
		t.Errorf("registry entry wrong: %+v", w)
	}
	if got := KernelVersionOf("ram-walking"); got != "ram-walking-v1" {
		t.Errorf("KernelVersionOf = %q", got)
	}
	if got := FaultClassOf("ram-walking"); got != FaultMemory {
		t.Errorf("FaultClassOf = %q, want %s", got, FaultMemory)
	}
	if strings.TrimSpace(w.Description) == "" {
		t.Error("registry description empty: the menu and dry-run show it")
	}
}
