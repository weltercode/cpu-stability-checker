package workload

import "testing"

// The magic-number division must agree with the hardware divider on
// every boundary and a wide sweep before any reference trusts it.
func TestMagicDiv7MatchesDivider(t *testing.T) {
	boundaries := []uint64{0, 1, 6, 7, 8, 14, 15, 1 << 32, (1 << 32) + 7, 1 << 63, 1<<63 + 7, ^uint64(0), ^uint64(0) - 7}
	for _, v := range boundaries {
		if magicDiv7(v) != v/7 {
			t.Errorf("magicDiv7(%d) = %d, want %d", v, magicDiv7(v), v/7)
		}
	}
	x := uint64(1)
	for i := 0; i < 200000; i++ {
		x = x*lcgA + lcgB
		for _, v := range []uint64{x, x - 1, x + 1, x / 7 * 7} {
			if magicDiv7(v) != v/7 {
				t.Fatalf("magicDiv7(%d) = %d, want %d", v, magicDiv7(v), v/7)
			}
		}
	}
}

// The GF(2) exponentiation must equal plain iteration of yStep for
// step counts around the powers and the verification interval.
func TestGF2PowMatchesIteration(t *testing.T) {
	seeds := []uint64{0, 1, ^uint64(0), 0x5555555555555555, 0x1234567887654321}
	counts := []uint64{0, 1, 2, 63, 64, 65, 4096, 64 * 4096}
	for _, seed := range seeds {
		for _, n := range counts {
			y := seed
			for i := uint64(0); i < n; i++ {
				y = yStep(y)
			}
			if got := powGF2(yBase(), n).apply(seed); got != y {
				t.Fatalf("powGF2(%d).apply(%x) = %x, iteration gives %x", n, seed, got, y)
			}
		}
	}
}

// The three chains must match their independent references from
// boundary seeds: zero, one, all-ones and the alternating patterns.
// (The plan's deterministic vectors and seed replay.)
func TestIntChainsMatchReferences(t *testing.T) {
	seeds := []struct{ x0, y0, v0 uint64 }{
		{1, 0x1234567887654321, 0xDEADBEEFCAFEBABE},
		{0, 0, 0},
		{1, 1, 1},
		{^uint64(0), ^uint64(0), ^uint64(0)},
		{0x5555555555555555, 0xAAAAAAAAAAAAAAAA, 0x5555555555555555},
	}
	const n = 10000
	for _, s := range seeds {
		x, y, v := s.x0, s.y0, s.v0
		for i := 0; i < n; i++ {
			x = x*lcgA + lcgB
			y = yStep(y)
			v = vStep(v)
		}
		if want := affineN(s.x0, lcgA, lcgB, n); x != want {
			t.Errorf("LCG seed %x: x=%x want %x", s.x0, x, want)
		}
		if want := powGF2(yBase(), n).apply(s.y0); y != want {
			t.Errorf("rot/xor seed %x: y=%x want %x", s.y0, y, want)
		}
		if want := vRefAdvance(s.v0, n); v != want {
			t.Errorf("divide seed %x: v=%x want %x", s.v0, v, want)
		}
	}
}

// Corrupting any chain's state the way a transient fault would must
// be caught by that chain's reference - each chain independently.
func TestIntChainsDetectCorruption(t *testing.T) {
	const n = 5000
	// x: already covered by TestIntDetectsFault; y and v here.
	var y uint64 = 0x1234567887654321
	for i := 0; i < n; i++ {
		y = yStep(y)
	}
	y ^= 1 << 17 // flip one bit mid-run
	if want := powGF2(yBase(), n).apply(0x1234567887654321); y == want {
		t.Error("corrupted rotate/xor chain was not detected")
	}
	var v uint64 = 0xDEADBEEFCAFEBABE
	for i := 0; i < n; i++ {
		v = vStep(v)
	}
	v ^= 1 << 3
	if want := vRefAdvance(0xDEADBEEFCAFEBABE, n); v == want {
		t.Error("corrupted divide chain was not detected")
	}
}

// The v chain must stay bounded: the divide-and-spread recurrence
// converges, so a legitimate run never overflows and a huge value is
// always fault evidence, never drift.
func TestVChainStaysBounded(t *testing.T) {
	v := ^uint64(0)
	for i := 0; i < 1000; i++ {
		v = vStep(v)
	}
	for i := 0; i < 10000; i++ {
		v = vStep(v)
		if v > 1<<20 {
			t.Fatalf("v chain not bounded: v=%d at step %d", v, i)
		}
	}
}
