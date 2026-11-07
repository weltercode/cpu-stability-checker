package workload

// P07b: ordered traversals - the same expected data as the address
// kernel (f(seed, pass, global word index)), visited in a different
// ORDER. What changes is the access pattern the memory system sees:
// reverse sweeps exercise descending writes/reads, and a seeded
// block permutation scatters 64 KiB blocks so sequential prefetch
// and address locality cannot be assumed anywhere in the pass. The
// data contract is untouched: repartitioning the budget still never
// changes what a position must contain, and verify always checks
// exactly the chunks the fill completed, in any order.

import (
	"context"
)

const (
	ramReverseKernel = "ram-reverse-v1"
	ramPermKernel    = "ram-perm-v1"
)

// runRamReverse alternates sweep directions: odd passes fill
// ascending and even passes fill descending, and the verify sweep
// always runs OPPOSITE to the fill. Both directions are exercised
// as writes, not just reads; the order schedule is part of the
// versioned spec (golden-pinned).
func runRamReverse(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRunOrdered(ctx, rep, ramReverseKernel, ramAddressWord, ramReverseFillOrder, true)
}

// ramReverseFillOrder is the ram-reverse-v1 fill schedule: pass
// parity picks the direction. Pure function of (seed, pass, seq) -
// seed is unused because the direction schedule is deterministic by
// spec; pinning that here keeps the signature honest.
func ramReverseFillOrder(seed, pass uint64, seq []ramChunkRef) []ramChunkRef {
	_ = seed
	if pass%2 == 1 {
		return seq // ascending fill (the shared default order)
	}
	out := make([]ramChunkRef, len(seq))
	for i, c := range seq {
		out[len(seq)-1-i] = c
	}
	return out
}

// runRamPerm visits whole blocks (the 64 KiB chunks) in a seeded
// permutation that changes per pass and reproduces exactly from
// (seed, pass): scattered block writes, read back in the reverse of
// the write order. The permutation is a bijection - every chunk
// exactly once per pass, no matter how the budget was partitioned.
func runRamPerm(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRunOrdered(ctx, rep, ramPermKernel, ramAddressWord, ramPermOrder, true)
}

// ramPermOrder is the ram-perm-v1 order spec: a full-cycle LCG
// x -> 5x + c (mod 2^k) over the next power of two >= len(seq),
// rejecting indices past the chunk count. The parameters satisfy the
// Hull-Dobell conditions (c odd; 5-1 divisible by 4 and by 2), so
// one cycle visits every residue exactly once and rejection leaves
// every chunk index exactly once - a bijection, not a heuristic
// shuffle. The seeded start is the splitmix64 finalizer over
// (seed, pass). This mixing IS the reproduction spec: golden vectors
// pin it, and changing it requires bumping the kernel version, never
// the other way around.
func ramPermOrder(seed, pass uint64, seq []ramChunkRef) []ramChunkRef {
	n := len(seq)
	M := uint64(1)
	for M < uint64(n) {
		M <<= 1
	}
	mask := M - 1

	x := seed + 0x9E3779B97F4A7C15*(pass+1)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31

	const c = 0x9E3779B97F4A7C15 // odd: the Hull-Dobell c condition
	out := make([]ramChunkRef, 0, n)
	for range M {
		x = (5*x + c) & mask
		if x < uint64(n) {
			out = append(out, seq[x])
			if len(out) == n {
				break
			}
		}
	}
	// Unreachable by the full-cycle proof (M steps cover every
	// residue), but a short list here would silently shrink the
	// pass's coverage - so the defensive path completes the
	// permutation in address order instead of trusting the proof.
	if len(out) != n {
		taken := make(map[ramChunkRef]bool, n)
		for _, c := range out {
			taken[c] = true
		}
		for _, c := range seq {
			if !taken[c] {
				out = append(out, c)
			}
		}
	}
	return out
}
