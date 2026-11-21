package workload

// R31 (owner 2026-11-12: "можем повторить этот алгоритм, хотя бы грубо" -
// can we reproduce the TestMem5 algorithm, at least roughly): a
// per-BLOCK verified RAM walk. The public TestMem5 config
// "extreme@anta777" discloses that several of its tests run in fixed
// 4 MB test blocks inside a per-thread window ("the test block should
// fit in it", author statement), with time weights as short as 20% -
// windows far too small to sweep gigabytes per pass. Our whole-region
// ram-* kernels verify only on COMPLETE passes, so a short window on
// GiB regions closes zero verified checks (the mix-trans incident);
// the only honest way to give a short slot real, credited verification
// is to make the verification unit smaller than the region.
//
// This kernel takes the disclosed BLOCK GRANULARITY (Test Block Size
// (Mb)=4 - configuration data, public) and nothing else from that
// config: what the pattern "SimpleTest" does inside MT0.dll is closed
// and is NOT reproduced here, and no detection parity is implied.
//
// The contract, unit first:
//   - One unit = one block: the block is filled with the pinned,
//     position-derived image (the same f(seed, pass, global position)
//     spec as ram-address/ram-copy), then verified IMMEDIATELY, word
//     by word, before any later block is touched. Written bytes never
//     exist without the verification that follows them - the
//     written-never-unchecked rule holds even on cancel and on fault,
//     because fill and verify are one atomic unit.
//   - Only verified blocks are credited: iters and rep.Add count each
//     completed block (this kernel's "verified checks" are blocks),
//     and MemVerified carries the running count of DISTINCT bytes the
//     kernel has verified at least once in this phase. Unlike the
//     whole-region kernels - where a cancelled pass reports nothing -
//     a cancelled block sweep still reports its true partial coverage:
//     Verified=0 would then be a statement the kernel knows to be
//     false.
//   - The journal's pass-coverage channels keep the whole-family
//     meaning: RamPass(totalBytes) is declared only when a FULL sweep
//     of every owned region has completed, so VerifiedUnique/Total
//     still mean "one complete pass covered exactly this" - a phase
//     that never finished a sweep leaves those channels unknown
//     (nil), never zero, and never inflated by partial sweeps.
//   - Cancellation is honored at BLOCK boundaries: a started unit
//     always completes (bounded: ramBlock4Bytes of fill + verify), so
//     the window overrun is one unit at most and nothing half-done is
//     ever credited or left unchecked. Cancellation before any unit
//     fits (starvation, forced kill, a window shorter than one unit
//     under contention) credits zero - an honest partial run, said so
//     by the numbers, exactly like a short window on the whole-region
//     kernels.
//   - A mismatch in a block's verify: the block's own bytes were
//     already read in full (ramVerifyChunk scans the whole chunk and
//     reports every finding), no credit is given, and the phase stops
//     BEFORE any later sweep can refill - the evidence stays where it
//     was found.
//   - Regions smaller than a block simply are one (tail) block; every
//     byte of every owned region is a unit's exact extent, credited
//     once per sweep and never twice per position.

import (
	"context"
	"fmt"
)

const ramBlock4Kernel = "ram-block4-v1"

// ramBlock4Bytes is the block granularity: 4 MB, mirroring the
// "Test Block Size (Mb)=4" number the public Extreme1 config carries
// for its block-form tests. The SIZE is public data; the semantics
// TM5 gives it are not, and this kernel owes nothing to them.
const ramBlock4Bytes = 4 << 20

const kernelPhaseVerifyBlock = "verify-block"

// runRamBlock4 implements the per-block contract above. fn is the
// pinned ram-address word spec: same seed/pass/global-position
// identity every other ram kernel carries, so repartitioning the
// budget never changes what a position must contain, and a stale
// generation reads the previous sweep's words and faults.
func runRamBlock4(ctx context.Context, rep Reporter) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	regions, totalBytes := ramRegions(rr.MemRegions())
	rp, hasRP := rep.(RamReporter)
	seed := rr.RamSeed()
	fn := ramAddressWord

	var iters, pass uint64
	unique := uint64(0) // distinct bytes verified at least once in this phase
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++

		for vi := range regions {
			v := regions[vi].view
			for off := 0; off < len(v); off += ramBlock4Bytes {
				end := min(off+ramBlock4Bytes, len(v))
				blk := v[off:end]
				base := regions[vi].off + uint64(off)

				// fill, then verify the SAME extent immediately: one
				// atomic unit. The beat after the fill is liveness
				// only - cancellation is honored at the unit
				// boundary, never inside a unit.
				ramFillChunk(blk, seed, pass, base, fn)
				rep.Beat()

				bad := ramVerifyChunk(blk, seed, pass, base, fn,
					func(local int, exp, act uint32) {
						if !hasRP {
							return
						}
						rp.ReportRamMismatch(RamMismatch{
							Algorithm: ramBlock4Kernel, Seed: seed, Pass: pass,
							Region: regions[vi].id, Offset: uint64(off + local),
							Expected: uint64(exp), Actual: uint64(act), Phase: kernelPhaseVerifyBlock,
						})
					})
				if bad > 0 {
					// The fault outranks everything: earlier credited
					// units stay credited (they were verified whole),
					// this unit earns nothing, and the kernel stops
					// before the next sweep could refill the scene.
					return iters, fmt.Errorf(
						"%s: %d mismatch(es) in pass %d (first in region %s at logical offset %d; details in the journal ram evidence)",
						ramBlock4Kernel, bad, pass, regions[vi].id, off)
				}

				// --- unit complete: credit exactly once.
				iters++
				if pass == 1 {
					unique += uint64(end - off)
				}
				rr.MemVerified(unique)
				rep.Add(1)
				rep.Beat()
				if ctx.Err() != nil {
					// Block boundary: the honest stop point. What the
					// window managed stands verified; nothing else is
					// touched.
					return iters, nil
				}
			}
		}

		// Full sweep of every owned region completed: the pass-coverage
		// channel declares the whole-region volume, exactly once per
		// sweep - same family meaning as every other ram kernel.
		unique = totalBytes
		if hasRP {
			rp.RamPass(totalBytes)
		}
	}
}
