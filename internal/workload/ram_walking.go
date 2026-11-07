package workload

// P07a: walking bits - the walking-ones / walking-zeros family in
// engine memory. Each pass holds ONE bit position of every word at
// one state while every other bit sits at the opposite state, so
// across the 64-pass cycle each stored bit is driven to 1 and to 0
// with its neighbours held constant: the classic stuck-at coverage
// over the engine's logical words. (Logical coverage only: nothing
// here claims a physical data line, pin or DIMM trace - the engine
// only ever sees byte offsets.)
//
// The pass schedule IS the spec (ram-walking-v1): golden vectors pin
// it, and changing the schedule requires bumping the kernel version,
// never the other way around.

import (
	"context"
)

const ramWalkingKernel = "ram-walking-v1"

// ramWalkingCycle is the full walking-bits cycle: 32 walking-ones
// passes (bit i set, all other bits clear), then 32 walking-zeros
// passes (bit i clear, all other bits set).
const ramWalkingCycle = 64

// ramWalkingWord is the walking-bits expected-word function. It
// ignores seed and wordIdx on purpose: the pattern set is exhaustive
// over bit positions by construction, and EVERY word must see every
// bit state - position-dependence would defeat the point (compare
// ram-constant, which also ignores both).
func ramWalkingWord(seed, pass, wordIdx uint64) uint32 {
	c := (pass - 1) % ramWalkingCycle
	if c < 32 {
		return uint32(1) << c // walking one: bit c high, every other bit low
	}
	return ^(uint32(1) << (c - 32)) // walking zero: bit c-32 low, every other bit high
}

// runRamWalking runs the walking-bits family on the shared ram
// engine: every pass is a full write of the cycle's pattern plus a
// separate full verify per word (the shared ram-* contract: read-only
// tail after a mismatch, partial passes never credited, expected
// data independent of worker counts and region partitioning).
func runRamWalking(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRun(ctx, rep, ramWalkingKernel, ramWalkingWord)
}
