package workload

import (
	"context"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
)

// traversalSeq mirrors what the engine builds for the reporter's
// regions, so tests can reason about the exact visit order.
func traversalSeq(rep *regionReporter) []ramChunkRef {
	regions, _ := ramRegions(rep.regions)
	return ramSeqChunks(regions)
}

// wordAt reads the little-endian word at a byte offset in a region.
func wordAt(v []byte, byteOff int) uint32 { return binary.LittleEndian.Uint32(v[byteOff:]) }

// ---- ram-reverse-v1 ----

// The direction schedule is part of the versioned spec: odd passes
// fill ascending (the shared order), even passes fill descending,
// and the schedule must not drift with seed (seed is unused by
// design - the alternation is deterministic).
func TestRamReverseFillOrderSchedule(t *testing.T) {
	seq := []ramChunkRef{{0, 0, 64}, {0, 64, 128}, {1, 0, 64}}
	if got := ramReverseFillOrder(7, 1, seq); !slices.Equal(got, seq) {
		t.Errorf("pass 1 fill order = %v, want ascending %v", got, seq)
	}
	wantDesc := []ramChunkRef{{1, 0, 64}, {0, 64, 128}, {0, 0, 64}}
	if got := ramReverseFillOrder(7, 2, seq); !slices.Equal(got, wantDesc) {
		t.Errorf("pass 2 fill order = %v, want descending %v", got, wantDesc)
	}
	if got := ramReverseFillOrder(99, 3, seq); !slices.Equal(got, seq) {
		t.Errorf("pass 3 fill order = %v, want ascending (seed must not change the schedule)", got)
	}
}

// End-to-end corruption in a descending fill: pass 2 fills
// [r1c1, r0c2, r0c1] (beats 7,8,9); the flip at beat 9 lands in
// r0c1 right after it was refilled with pass 2's address pattern.
// The verify sweep runs opposite (ascending) and must catch it with
// the full ram-reverse identity.
func TestRamReverseEndToEndFlip(t *testing.T) {
	rep := ramTestReporter(t)
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 9
	rep.flip = func() { v[4] ^= 0x01 } // LSB of the word at logical offset 4

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamReverse(ctx, rep)
	if err == nil {
		t.Fatal("flipped word in a descending fill went undetected")
	}
	if !strings.Contains(err.Error(), "ram-reverse") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-reverse-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != 4 {
		t.Errorf("finding offset = %d, want 4", g.Offset)
	}
	want := ramAddressWord(7, 2, 1) // global word 1 (byte offset 4, region 0 at offset 0)
	if g.Expected != uint64(want) || g.Actual != uint64(want)^0x01 {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", g.Expected, g.Actual, want, want^0x01)
	}
}

// Graceful stop mid-descending-fill: cancel lands after beat 8
// (pass 2 filled r1c1 and r0c2; r0c1 was NEVER refilled). Verify
// must check exactly the completed chunks - r0c2 and r1c1 - and
// must NOT flag r0c1 (it legitimately still holds pass 1's
// pattern). The partial pass is not counted and not credited.
func TestRamReverseGracefulStopMidFill(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamReverse(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (the interrupted pass must not count)", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1", rep.ramPasses)
	}
	v0 := rep.regions[0].Block.Bytes()
	v1 := rep.regions[1].Block.Bytes()
	// r0c1 (bytes 0..65535): still pass 1. r0c2 (65536..131071): pass 2.
	for i := 0; i+4 <= 65536; i += 4 {
		want := ramAddressWord(7, 1, uint64(i)/4)
		if got := wordAt(v0, i); got != want {
			t.Fatalf("r0c1 word %d = %08x, want pass 1 pattern %08x (never refilled)", i/4, got, want)
		}
	}
	for i := 65536; i+4 <= len(v0); i += 4 {
		want := ramAddressWord(7, 2, uint64(i)/4)
		if got := wordAt(v0, i); got != want {
			t.Fatalf("r0c2 word %d = %08x, want pass 2 pattern %08x", i/4, got, want)
		}
	}
	for i := 0; i+4 <= len(v1); i += 4 {
		want := ramAddressWord(7, 2, rep.regions[1].Grant.Offset/4+uint64(i)/4)
		if got := wordAt(v1, i); got != want {
			t.Fatalf("r1 word %d = %08x, want pass 2 pattern %08x", i/4, got, want)
		}
	}
}

// Contents after a timed run: the completed passes left their
// address pattern everywhere, and the interrupted next pass left
// either an ascending prefix (odd pass) or a descending suffix
// (even pass) of its pattern over the last completed one. Both are
// honest; anything else is not.
func TestRamReversePassesAndCoverage(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamReverse(ctx, rep)
	if err != nil {
		t.Fatalf("runRamReverse: %v", err)
	}
	if it == 0 {
		t.Fatal("no reverse passes completed in 300ms")
	}
	if rep.ramPasses != it || rep.adds != it || rep.ramFound != 0 {
		t.Errorf("accounting drift: iters=%d passes=%d adds=%d found=%d", it, rep.ramPasses, rep.adds, rep.ramFound)
	}
	const all = uint64(192 << 10)
	if rep.ramUnique != all || rep.ramTotal != it*all || rep.verified != all {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d", rep.ramUnique, rep.ramTotal, rep.verified)
	}
	nextAscending := (it+1)%2 == 1
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		state := 0
		check := func(i int) {
			cur := ramAddressWord(7, it+1, base+uint64(i)/4)
			prev := ramAddressWord(7, it, base+uint64(i)/4)
			got := wordAt(v, i)
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word %d = %08x: not %08x-over-%08x in the expected %s layout",
					r.Grant.Region, i/4, got, cur, prev,
					map[bool]string{true: "ascending-prefix", false: "descending-suffix"}[nextAscending])
			}
		}
		if nextAscending {
			for i := 0; i+4 <= len(v); i += 4 {
				check(i)
			}
		} else {
			for i := (len(v)/4 - 1) * 4; i >= 0; i -= 4 {
				check(i)
			}
		}
	}
}

// ---- ram-perm-v1 ----

// The permutation spec is the reproduction contract: golden orders
// pinned as literals (computed once, then frozen), bijection over
// every chunk count (including counts that are not powers of two -
// the rejection path), exact replay from (seed, pass), and
// sensitivity to both coordinates. If the mixing moves without a
// kernel-version bump, past journals' reproduction orders silently
// change - this test refuses that.
func TestRamPermOrderGoldenAndBijection(t *testing.T) {
	mkSeq := func(n int) []ramChunkRef {
		out := make([]ramChunkRef, n)
		for i := range out {
			out[i] = ramChunkRef{i, 0, 1}
		}
		return out
	}
	idxOf := func(seq []ramChunkRef) []int {
		out := make([]int, len(seq))
		for i, c := range seq {
			out[i] = c.region
		}
		return out
	}

	// Golden orders (frozen literals).
	golden := []struct {
		seed, pass uint64
		n          int
		want       []int
	}{
		{7, 1, 3, []int{1, 2, 0}},
		{7, 2, 3, []int{0, 1, 2}},
		{7, 1, 5, []int{1, 2, 0, 3, 4}},
		{7, 2, 5, []int{0, 3, 4, 1, 2}},
		{7, 1, 8, []int{1, 2, 7, 0, 5, 6, 3, 4}},
	}
	for _, g := range golden {
		got := idxOf(ramPermOrder(g.seed, g.pass, mkSeq(g.n)))
		if !slices.Equal(got, g.want) {
			t.Errorf("ramPermOrder(%d,%d,%d) = %v, want the pinned %v", g.seed, g.pass, g.n, got, g.want)
		}
	}

	// Bijection for every count up to 40 and a few large ones:
	// every chunk exactly once, no matter the power-of-two padding.
	for _, n := range append([]int{1, 2, 3, 5, 7, 8, 9, 40, 64, 100, 1000, 4097}, func() []int {
		var r []int
		for i := 1; i <= 40; i++ {
			r = append(r, i)
		}
		return r
	}()...) {
		seq := mkSeq(n)
		perm := ramPermOrder(3, 5, seq)
		if len(perm) != n {
			t.Fatalf("n=%d: permutation length %d", n, len(perm))
		}
		got := idxOf(perm)
		slices.Sort(got)
		if !slices.Equal(got, func() []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		}()) {
			t.Errorf("n=%d: not a bijection (sorted visit = %v)", n, got)
		}
	}

	// Exact replay and coordinate sensitivity.
	seq := mkSeq(64)
	if !slices.Equal(ramPermOrder(7, 1, seq), ramPermOrder(7, 1, seq)) {
		t.Error("same (seed, pass) produced different orders: replay is broken")
	}
	if slices.Equal(ramPermOrder(7, 1, seq), ramPermOrder(8, 1, seq)) {
		t.Error("seed 7 and 8 produced the same order: seed does not matter")
	}
	if slices.Equal(ramPermOrder(7, 1, seq), ramPermOrder(7, 2, seq)) {
		t.Error("pass 1 and 2 produced the same order: the pass does not matter")
	}
}

// End-to-end corruption in a permuted fill: the test recomputes the
// exact pass-2 visit order, flips a word inside the FIRST chunk
// that pass 2 filled (beat 8 = after the second fill beat, so the
// first chunk is long written), and the verify sweep must fault
// with the ram-perm identity and the permuted chunk's own offset.
func TestRamPermEndToEndFlip(t *testing.T) {
	rep := ramTestReporter(t)
	seq := traversalSeq(rep)
	visit2 := ramPermOrder(7, 2, seq)
	victim := visit2[0]
	view := rep.regions[victim.region].Block.Bytes()

	rep.flipAt = 8
	rep.flip = func() { view[victim.start+4] ^= 0x01 }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamPerm(ctx, rep)
	if err == nil {
		t.Fatal("flipped word in a permuted fill went undetected")
	}
	if !strings.Contains(err.Error(), "ram-perm") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-perm-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != uint64(victim.start+4) {
		t.Errorf("finding offset = %d, want %d (inside the permuted victim chunk)", g.Offset, victim.start+4)
	}
	globalWord := (rep.regions[victim.region].Grant.Offset + uint64(victim.start) + 4) / 4
	want := ramAddressWord(7, 2, globalWord)
	if g.Expected != uint64(want) || g.Actual != uint64(want)^0x01 {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", g.Expected, g.Actual, want, want^0x01)
	}
}

// Graceful stop mid-permuted-fill: cancel lands after beat 8, so
// exactly the first two chunks of pass 2's permutation were
// written. Verify must check exactly those two (in reverse write
// order), must NOT flag the third (it legitimately holds pass 1's
// pattern), and the partial pass earns nothing.
func TestRamPermGracefulStopMidFill(t *testing.T) {
	rep := ramTestReporter(t)
	seq := traversalSeq(rep)
	visit2 := ramPermOrder(7, 2, seq)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 8

	it, err := runRamPerm(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (the interrupted pass must not count)", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1", rep.ramPasses)
	}
	for j, c := range visit2 {
		v := rep.regions[c.region].Block.Bytes()
		base := rep.regions[c.region].Grant.Offset/4 + uint64(c.start)/4
		pass := uint64(2)
		if j >= 2 {
			pass = 1 // never refilled this pass: honest old-pattern territory
		}
		for i := c.start; i+4 <= c.end; i += 4 {
			want := ramAddressWord(7, pass, base+uint64(i-c.start)/4)
			if got := wordAt(v, i); got != want {
				t.Fatalf("visit2[%d] (region %d chunk %d) word at %d = %08x, want pass %d pattern %08x",
					j, c.region, c.start, i, got, pass, want)
			}
		}
	}
}

// Timed run: accounting exact, zero false mismatches across
// multiple permuted passes (the bijection must never double-fill or
// skip a chunk - a skip would show up as a false mismatch on the
// verify sweep, which is exactly the honesty guard here).
func TestRamPermPassesAndCoverage(t *testing.T) {
	rep := ramTestReporter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamPerm(ctx, rep)
	if err != nil {
		t.Fatalf("runRamPerm: %v", err)
	}
	if it == 0 {
		t.Fatal("no permuted passes completed in 300ms")
	}
	const all = uint64(192 << 10)
	if rep.ramPasses != it || rep.adds != it || rep.ramFound != 0 {
		t.Errorf("accounting drift: iters=%d passes=%d adds=%d found=%d", it, rep.ramPasses, rep.adds, rep.ramFound)
	}
	if rep.ramUnique != all || rep.ramTotal != it*all || rep.verified != all {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d", rep.ramUnique, rep.ramTotal, rep.verified)
	}
}

// ---- shared guards for both kernels ----

func TestRamTraversalUpfrontCancel(t *testing.T) {
	for _, run := range []func(context.Context, Reporter) (uint64, error){runRamReverse, runRamPerm} {
		rep := ramTestReporter(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		it, err := run(ctx, rep)
		if err != nil {
			t.Fatalf("upfront cancel: %v", err)
		}
		if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
			t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
				it, rep.adds, rep.ramPasses, rep.verified)
		}
	}
}

func TestRamTraversalRefusesWithoutEngine(t *testing.T) {
	for _, run := range []func(context.Context, Reporter) (uint64, error){runRamReverse, runRamPerm} {
		rep := &regionReporter{}
		_, err := run(context.Background(), rep)
		if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
			t.Errorf("err = %v, want the no-engine refusal", err)
		}
	}
}

func TestRamTraversalRegistryEntries(t *testing.T) {
	for name, ver := range map[string]string{"ram-reverse": "ram-reverse-v1", "ram-perm": "ram-perm-v1"} {
		w, ok := workloads[name]
		if !ok {
			t.Fatalf("%s missing from the workload registry", name)
		}
		if w.KernelVersion != ver || !w.RequiresMemory || w.FaultClass != FaultMemory {
			t.Errorf("registry entry wrong: %+v", w)
		}
		if got := KernelVersionOf(name); got != ver {
			t.Errorf("KernelVersionOf(%s) = %q", name, got)
		}
		if strings.TrimSpace(w.Description) == "" {
			t.Errorf("%s: empty description", name)
		}
	}
}
