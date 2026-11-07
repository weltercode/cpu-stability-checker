package workload

// P07e: block ownership handoff between worker processes over ONE
// shared section. The data contract continues the ram-* family:
// expected word = f(seed, generation, global word index) - the
// generation plays the role of the pass number, and the global word
// index spans the whole partition (grant offset + local offset), so
// repartitioning never changes what a position must contain.
//
// Identity rides in the content, exactly as in ram-copy: the word at
// a position depends on its generation, so a reader granted an older
// generation reads the older values and faults; a reader granted the
// wrong block reads its neighbour's per-position values and faults.
// No header can silently claim a wrong identity.
//
// Unlike every other ram-* entry this kernel does NOT run as a
// standalone phase: ownership leases need a coordinator, and a
// phase-mode Run would be a lie about who orders the writes. It runs
// only as session leases (LEASE_WRITE / LEASE_READ, see
// internal/transition and internal/subprocess): the parent arbiter
// serializes ownership, the writer fills one block and publishes it
// (FlushProcessWriteBuffers before the WRITTEN ack), a reader
// verifies it independently (flush after the LEASE_READ command,
// before the first load). The tester never races, so a mismatch here
// is evidence about the memory path, never about tester ordering.
//
// Lease operations are bounded by one block: they are short by
// construction, so cancellation applies between operations (the
// command loop), not inside a block fill.

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

// HandoffKernelVersion names the handoff kernel spec. The mixing
// below IS the reproduction spec: golden vectors pin it, and
// changing it requires bumping the version, never the other way
// around.
const HandoffKernelVersion = "ram-handoff-v1"

const handoffPhaseLeaseRead = "lease-read"

// HandoffWord is the ram-handoff-v1 expected-word spec: splitmix64
// finalizer fed with (seed, generation, global word index), reported
// through the LOW bits - deliberately a different spec from
// ram-address (high bits) and ram-random (xorshift stream), so the
// three kernels stress different value structure.
func HandoffWord(seed, gen, wordIdx uint64) uint32 {
	x := seed + 0x9E3779B97F4A7C15*(wordIdx+1) + 0xC2B2AE3D27D4EB4F*(gen+1)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return uint32(x) // the low half: distinct from ram-address by spec
}

// HandoffWrite fills view with the handoff image for (seed, gen),
// word i holding HandoffWord(seed, gen, wordBase+i). wordBase is the
// region's global word offset (grant offset / 4), so the same Grant
// set means the same words in every process. A view whose length is
// not a whole number of words is a plumbing bug: refused loudly as a
// setup condition, never partially filled.
func HandoffWrite(view []byte, seed, gen, wordBase uint64) error {
	if len(view) == 0 || len(view)%4 != 0 {
		return NewSetupError(fmt.Errorf("%s: view length %d is not a whole number of words", HandoffKernelVersion, len(view)))
	}
	handoffFill(view, seed, gen, wordBase)
	return nil
}

// handoffFill is the store loop. The noinline barrier is load-bearing
// for the same reason the engine chunk primitives carry it: through
// a call boundary the view is an opaque slice and the stores cannot
// be tracked, reordered or elided by the optimizer.
//
//go:noinline
func handoffFill(view []byte, seed, gen, wordBase uint64) {
	for i := 0; i+4 <= len(view); i += 4 {
		binary.LittleEndian.PutUint32(view[i:], HandoffWord(seed, gen, wordBase+uint64(i>>2)))
	}
}

// HandoffVerify reads view against the independently reconstructed
// image for (seed, gen): every word is recomputed, never compared
// against "whatever the writer kept". It stops at the FIRST mismatch
// (the arbiter taints the block; re-reading a damaged block buys
// nothing) and returns the number of words examined up to and
// including the mismatch plus the structured evidence. regionID names
// the grant identity for the evidence (never a physical claim).
//
// A malformed view (empty, not a whole number of words) is a setup
// error, not a vacuous pass and not a hardware-looking finding: the
// third return says so and no mismatch is invented.
func HandoffVerify(view []byte, seed, gen, wordBase uint64, regionID string) (checks uint64, mismatch *RamMismatch, setupErr error) {
	if len(view) == 0 || len(view)%4 != 0 {
		return 0, nil, NewSetupError(fmt.Errorf("%s: view length %d is not a whole number of words", HandoffKernelVersion, len(view)))
	}
	c, mm := handoffCheck(view, seed, gen, wordBase, regionID)
	return c, mm, nil
}

// handoffCheck carries the same noinline barrier as handoffFill: the
// verification loads must stay real.
//
//go:noinline
func handoffCheck(view []byte, seed, gen, wordBase uint64, regionID string) (uint64, *RamMismatch) {
	var checks uint64
	for i := 0; i+4 <= len(view); i += 4 {
		want := HandoffWord(seed, gen, wordBase+uint64(i>>2))
		got := binary.LittleEndian.Uint32(view[i:])
		checks++
		if got != want {
			return checks, &RamMismatch{
				Algorithm: HandoffKernelVersion,
				Seed:      seed,
				Pass:      gen, // the generation plays the pass role in this family member
				Region:    regionID,
				Offset:    uint64(i), // logical offset within the block
				Expected:  uint64(want),
				Actual:    uint64(got),
				Phase:     handoffPhaseLeaseRead,
			}
		}
	}
	return checks, nil
}

// runRamHandoffPhase refuses the slot-loop phase shape: this kernel only
// runs as session leases over a shared mapping. Registered so the
// registry knows its version, its memory need and its fault class - but
// a TRIGGER-style phase run is a wiring bug and says so, never quietly
// "succeeds" without any shared block in sight. A scenario handoff phase
// (S5) is a different shape: the coordinator (subprocess.ExecuteHandoffResult)
// drives the leases over one inherited section, and the plan layer
// (plan.ValidateHandoff) pins that shape at compile.
func runRamHandoffPhase(ctx context.Context, rep Reporter) (uint64, error) {
	return 0, NewSetupError(fmt.Errorf("%s runs only as session leases over a shared mapping (P07e), not as a standalone phase", HandoffKernelVersion))
}

// HandoffRegionID names one engine region by its grant identity for
// lease evidence (the same shape as the engine kernels' region ids:
// identity strings, never physical addresses).
func HandoffRegionID(r memtest.AllocatedRegion) string {
	return fmt.Sprintf("worker=%d region=%d role=%s offset=%d len=%d",
		r.Grant.Worker, r.Grant.Region, r.Grant.Role, r.Grant.Offset, r.Grant.Requested)
}
