package workload

import (
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
)

// Beat map for the 128K+64K region pair (3 fill chunks, S class
// flips, 3 verify chunks per pass):
//
//	pass 1 (S=2): fill 1,2,3 | classes 4,5 | verify 6,7,8
//	pass 2 (S=3): fill 9,10,11 | classes 12,13,14 | verify 15,16,17
//	pass 3 (S=4): fill 18,19,20 | classes 21..24 | verify 25,26,27

// The stride schedule is the versioned coverage spec: pinned as
// literals, and the cycle must not drift past the list end.
func TestRamStrideScheduleGolden(t *testing.T) {
	want := []uint64{2, 3, 4, 5, 8, 16, 32}
	if !slices.Equal(ramStrideSchedule, want) {
		t.Fatalf("stride schedule = %v, want the pinned %v", ramStrideSchedule, want)
	}
	// pass p -> schedule[(p-1)%7]: pass 8 wraps back to 2.
	for _, c := range []struct {
		pass uint64
		want uint64
	}{{1, 2}, {7, 32}, {8, 2}, {14, 32}, {15, 2}} {
		if got := ramStrideSchedule[(c.pass-1)%uint64(len(ramStrideSchedule))]; got != c.want {
			t.Errorf("pass %d stride = %d, want %d", c.pass, got, c.want)
		}
	}
}

// End-to-end corruption inside an already-flipped class: pass 2
// (S=3) flips classes at beats 12,13,14; the flip at beat 13 lands
// on word 0 (class 0, flipped at beat 12, now holding ^base). The
// closing class-aware verify expects ^base and must fault with the
// full ram-stride identity.
func TestRamStrideEndToEndFlip(t *testing.T) {
	rep := ramTestReporter(t)
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 13
	rep.flip = func() { v[0] ^= 0x01 } // LSB of global word 0, class 0 of 3

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamStride(ctx, rep)
	if err == nil {
		t.Fatal("flipped word in a flipped stride class went undetected")
	}
	if !strings.Contains(err.Error(), "ram-stride") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-stride-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != 0 {
		t.Errorf("finding offset = %d, want 0", g.Offset)
	}
	want := ^ramAddressWord(7, 2, 0) // class 0 < doneClasses 2 -> inverted expected
	if g.Expected != uint64(want) || g.Actual != uint64(want)^0x01 {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", g.Expected, g.Actual, want, want^0x01)
	}
}

// Graceful stop at a CLASS boundary mid-flip: cancel lands after
// beat 13 (classes 0 and 1 flipped, class 2 never flipped). The
// closing verify must check the exact mixed state - words in
// classes 0,1 read ^base, words in class 2 read base - and the
// partial pass earns nothing. This is the residue-class honesty
// proof: written bytes never go unchecked, unwritten territory is
// expected to be untouched.
func TestRamStrideGracefulMidFlip(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 13

	it, err := runRamStride(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop at a class boundary is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (the interrupted pass must not count)", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1", rep.ramPasses)
	}
	// Pass 2, S=3, doneClasses=2: exact expected state per word.
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		for i := 0; i+4 <= len(v); i += 4 {
			gw := base + uint64(i)/4
			want := ramAddressWord(7, 2, gw)
			if gw%3 < 2 {
				want = ^want
			}
			if got := binary.LittleEndian.Uint32(v[i:]); got != want {
				t.Fatalf("region %d word %d (class %d) = %08x, want %08x",
					r.Grant.Region, gw, gw%3, got, want)
			}
		}
	}
}

// Graceful stop mid-BASE-FILL: cancel lands after beat 10 (pass 2
// refilled all of region 0, region 1 never refilled). The verify
// must check ONLY the filled prefix against the base expectation -
// region 1 legitimately still holds pass 1's fully-inverted end
// state and must NOT be flagged.
func TestRamStrideGracefulMidFill(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 10

	it, err := runRamStride(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop mid-fill is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1", rep.ramPasses)
	}
	v0 := rep.regions[0].Block.Bytes()
	for i := 0; i+4 <= len(v0); i += 4 {
		want := ramAddressWord(7, 2, uint64(i)/4) // filled this pass: base, no class flipped yet
		if got := binary.LittleEndian.Uint32(v0[i:]); got != want {
			t.Fatalf("region 0 word %d = %08x, want pass 2 base %08x", i/4, got, want)
		}
	}
	v1 := rep.regions[1].Block.Bytes()
	for i := 0; i+4 <= len(v1); i += 4 {
		gw := rep.regions[1].Grant.Offset/4 + uint64(i)/4
		want := ^ramAddressWord(7, 1, gw) // pass 1 end state: every class flipped
		if got := binary.LittleEndian.Uint32(v1[i:]); got != want {
			t.Fatalf("region 1 word %d (global %d) = %08x, want pass 1 end state %08x", i/4, gw, got, want)
		}
	}
}

// Tail bytes (a 4097-byte region) must join the class of their
// word consistently across base fill, stride flip and class-aware
// verify: a clean run reports ZERO false mismatches on the tail.
func TestRamStrideTailNoFalseErrors(t *testing.T) {
	e := openTestEngine(t, 4097)
	rep := &regionReporter{regions: e.Regions(), seed: 11}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamStride(ctx, rep)
	if err != nil {
		t.Fatalf("runRamStride over a tail region: %v", err)
	}
	if it == 0 {
		t.Fatal("no stride passes completed in 300ms")
	}
	if rep.ramFound != 0 {
		t.Fatalf("clean tail run reported %d mismatches: the tail class rule is inconsistent", rep.ramFound)
	}
}

// The tail byte IS covered by the stride sweep: flip it after its
// class was inverted and the verify must fault with the tail's own
// offset. Single 4097-byte region: pass 1 (S=2) beats are
// fill=1, class0=2, class1=3, verify=4.
func TestRamStrideTailFlipCaught(t *testing.T) {
	e := openTestEngine(t, 4097)
	rep := &regionReporter{regions: e.Regions(), seed: 7}
	v := e.Regions()[0].Block.Bytes()
	rep.flipAt = 2 // right after class 0's flip wrote the tail byte (word 1024, class 0)
	rep.flip = func() { v[4096] ^= 0x01 }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamStride(ctx, rep)
	if err == nil {
		t.Fatal("flipped tail byte went undetected")
	}
	if it != 0 {
		t.Errorf("completed passes = %d, want 0 (pass 1 aborted)", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-stride-v1" || g.Pass != 1 || g.Phase != "verify" || g.Offset != 4096 {
		t.Errorf("finding identity wrong: %+v", g)
	}
	wantByte := byte(^ramAddressWord(7, 1, 1024)) // lane 0 of the inverted word
	if g.Expected != uint64(wantByte) || g.Actual != uint64(wantByte)^0x01 {
		t.Errorf("finding values = %02x/%02x, want %02x/%02x", g.Expected, g.Actual, wantByte, wantByte^0x01)
	}
}

// Timed run: accounting exact and zero false mismatches across
// several different strides (the schedule rotates 2,3,4,... within
// the window, so multiple stride values get exercised).
func TestRamStridePassesAndCoverage(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamStride(ctx, rep)
	if err != nil {
		t.Fatalf("runRamStride: %v", err)
	}
	if it < 2 {
		t.Fatalf("completed passes = %d, want at least 2 (two different strides)", it)
	}
	const all = uint64(192 << 10)
	if rep.ramPasses != it || rep.adds != it || rep.ramFound != 0 {
		t.Errorf("accounting drift: iters=%d passes=%d adds=%d found=%d", it, rep.ramPasses, rep.adds, rep.ramFound)
	}
	if rep.ramUnique != all || rep.ramTotal != it*all || rep.verified != all {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d", rep.ramUnique, rep.ramTotal, rep.verified)
	}
}

func TestRamStrideUpfrontCancel(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamStride(ctx, rep)
	if err != nil {
		t.Fatalf("upfront cancel: %v", err)
	}
	if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
			it, rep.adds, rep.ramPasses, rep.verified)
	}
}

func TestRamStrideRefusesWithoutEngine(t *testing.T) {
	rep := &regionReporter{}
	_, err := runRamStride(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
		t.Errorf("err = %v, want the no-engine refusal", err)
	}
}

func TestRamStrideRegistryEntry(t *testing.T) {
	w, ok := workloads["ram-stride"]
	if !ok {
		t.Fatal("ram-stride missing from the workload registry")
	}
	if w.KernelVersion != "ram-stride-v1" || !w.RequiresMemory || w.FaultClass != FaultMemory {
		t.Errorf("registry entry wrong: %+v", w)
	}
	if got := KernelVersionOf("ram-stride"); got != "ram-stride-v1" {
		t.Errorf("KernelVersionOf = %q", got)
	}
	if strings.TrimSpace(w.Description) == "" {
		t.Error("registry description empty")
	}
}
