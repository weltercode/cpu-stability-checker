package workload

import "math/bits"

// The int kernel runs three independent chains per batch so a single
// ALU fault has three different ways to show up, each checked by a
// reference computed with different mathematics:
//
//	x: LCG x*A+B            - reference: 2x2 matrix fast exponentiation
//	y: rotl/xor affine map  - reference: GF(2) affine-map exponentiation
//	v: divide-and-spread    - reference: magic multiply-high division
//
// The kernel does NOT claim to cover every execution unit: it covers
// the integer adder/multiplier path (x), the shifter/XOR path (y) and
// the hardware divider (v), each against an independent reference.
// All arithmetic is mod 2^64 by construction (Go's uint64 wrap), so
// overflow is defined behavior, not a fault.

// --- y: the rotate/xor affine chain ------------------------------------------

// yC is the constant XORed in each step; any fixed odd value works,
// this one is the documented golden-ratio constant used elsewhere.
const yC = uint64(0x9E3779B97F4A7C15)

// yStep is the hot-loop step: y = rotl(y,13) ^ rotl(y,29) ^ yC.
func yStep(y uint64) uint64 {
	return bits.RotateLeft64(y, 13) ^ bits.RotateLeft64(y, 29) ^ yC
}

// gf2Affine is an affine map over GF(2)^64: x -> L(x) ^ c, where the
// linear part L is stored as the images of the 64 basis vectors. The
// y chain is exactly such a map (rotations and XOR are GF(2)-linear),
// so iterating it n times is exponentiating this map - a completely
// different computation (bit-matrix composition) from the iterative
// shift/xor hot path, and it catches stuck bits and cross-bit faults
// the iteration could hide.
type gf2Affine struct {
	img [64]uint64 // L(e_i) for each bit position i
	c   uint64     // the constant part
}

// yBase is the one-step map of the y chain.
func yBase() gf2Affine {
	var m gf2Affine
	for i := 0; i < 64; i++ {
		e := uint64(1) << i
		m.img[i] = bits.RotateLeft64(e, 13) ^ bits.RotateLeft64(e, 29)
	}
	m.c = yC
	return m
}

// apply evaluates L(x) ^ c by XORing the images of the set bits.
func (m gf2Affine) apply(x uint64) uint64 {
	r := m.c
	for i := 0; i < 64; i++ {
		if x&(1<<i) != 0 {
			r ^= m.img[i]
		}
	}
	return r
}

// compose returns m1(m2(x)): the images compose column by column and
// the constants fold as L1(c2) ^ c1.
func composeGF2(m1, m2 gf2Affine) gf2Affine {
	var out gf2Affine
	lin := func(x uint64) uint64 {
		r := uint64(0)
		for i := 0; i < 64; i++ {
			if x&(1<<i) != 0 {
				r ^= m1.img[i]
			}
		}
		return r
	}
	for i := 0; i < 64; i++ {
		out.img[i] = lin(m2.img[i])
	}
	out.c = lin(m2.c) ^ m1.c
	return out
}

// powGF2 raises the map to the n-th power by binary exponentiation:
// the n-step reference for the y chain, computed in O(64^2 log n)
// word operations regardless of n.
func powGF2(m gf2Affine, n uint64) gf2Affine {
	result := gf2Affine{} // identity
	for i := 0; i < 64; i++ {
		result.img[i] = 1 << i
	}
	for n > 0 {
		if n&1 == 1 {
			result = composeGF2(m, result)
		}
		m = composeGF2(m, m)
		n >>= 1
	}
	return result
}

// --- v: the divide-and-spread chain -------------------------------------------

// vStep is the hot-loop step: v = v/7 + (v%7)*13. The division and
// remainder run on the hardware divider (the unit under test); the
// value shrinks by a factor of ~7 per step until the remainder term
// dominates, so v stays bounded and the chain never legitimately
// overflows.
func vStep(v uint64) uint64 {
	q := v / 7
	return q + (v-q*7)*13
}

// magicDiv7 divides by 7 with a multiply-high and no divider: the
// reference path must not use the very unit it checks. The constant is
// ceil(2^66/7) with a shift of 2; that estimate is never below the
// true quotient and at most one too high, so a single correction
// (compare the product, adjust once) makes it exact - and the
// correction itself uses only multiply and compare, never the divider.
// The sweep test pins the result against the hardware divider across
// boundary and sweep values before anything trusts it.
func magicDiv7(v uint64) uint64 {
	hi, _ := bits.Mul64(v, 0x924924924924924A)
	q := hi >> 2
	if p := q * 7; p > v {
		q-- // the estimate came out one high
	} else if v-p >= 7 {
		q++ // defensive: never expected for this constant, but cheap
	}
	return q
}

// vRefAdvance runs n steps of the v chain using only multiply, shift,
// add and subtract (magicDiv7 and v-q*7 for the remainder): a
// different arithmetic path than the hardware div in the hot loop.
func vRefAdvance(v uint64, n uint64) uint64 {
	for i := uint64(0); i < n; i++ {
		q := magicDiv7(v)
		v = q + (v-q*7)*13
	}
	return v
}
