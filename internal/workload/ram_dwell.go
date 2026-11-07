package workload

// P07d: fill -> dwell -> verify. The data is written, the kernel
// then PAUSES (no memory traffic from this kernel during the dwell
// window), and only afterwards reads everything back. What this
// honestly proves: the data survived a quiet interval inside a
// user-mode process - nothing overwrote it, and whatever the
// caches and memory controller did during the pause left it intact.
//
// What it does NOT claim: a clean physical DRAM retention test.
// Without confirming residency (no page-locking promise, no cache
// flush control) the final read may be served partly from cache,
// and the plan forbids promising more than the mechanism supports.
// The dwell window is also NOT a Rowhammer pattern: DRAM row
// addressing is not controlled or confirmed here, so the schedule
// is described as activity/pause periods, never as hammering.
//
// The dwell target per pass is base + jitter(seed, pass): a
// reproducible pseudo-random pause (golden-pinned pure function),
// so the pause schedule replays exactly for the same seed while
// still not being a constant. A cancel that lands during the dwell
// shortens it, the verify still runs over everything written (no
// written byte goes unchecked), and the pass is NOT counted: the
// pause contract it promises was not kept.

import (
	"context"
	"fmt"
	"time"
)

const ramDwellKernel = "ram-dwell-v1"

// Production dwell spec (ram-dwell-v1): 250 ms base plus 0..125 ms
// seeded jitter per pass. These constants are part of the kernel's
// coverage story and pinned by test; changing them bumps the
// version. Tests override via DwellParamsReporter to stay fast -
// the override changes the pause length, never the data contract.
const (
	ramDwellBaseMs   = 250
	ramDwellJitterMs = 125
)

// ramDwellSlice bounds how long the kernel sleeps before checking
// cancellation again (a responsiveness detail, not a data spec).
const ramDwellSlice = 25 * time.Millisecond

// DwellParamsReporter is the optional override used by tests (and
// any future harness) to shorten the pause without touching the
// data contract. Zero/negative jitter means "no jitter".
type DwellParamsReporter interface {
	RamDwell() (base, jitter time.Duration)
}

// ramDwellTarget is the versioned pause schedule: splitmix64 over
// (seed, pass) mapped into [0, jitter). Pure and golden-pinned:
// the same seed replays the same pause sequence.
func ramDwellTarget(seed, pass uint64, base, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return base
	}
	x := seed + 0x9E3779B97F4A7C15*(pass+1)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return base + time.Duration(x%uint64(jitter))
}

// runRamDwell implements fill -> dwell -> verify over engine
// regions. The expected-word function is the pinned ram-address
// spec (position-derived: every position distinct, so nothing can
// pass as a neighbour's data after the pause).
func runRamDwell(ctx context.Context, rep Reporter) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	regions, totalBytes := ramRegions(rr.MemRegions())
	rp, hasRP := rep.(RamReporter)
	seed := rr.RamSeed()
	fn := ramAddressWord

	base, jitter := time.Duration(ramDwellBaseMs)*time.Millisecond, time.Duration(ramDwellJitterMs)*time.Millisecond
	if dp, ok := rep.(DwellParamsReporter); ok {
		base, jitter = dp.RamDwell()
	}

	var iters, pass uint64
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++

		// --- fill (chunk-cancellable, prefix-tracked: the shared
		// family discipline).
		filled := make([]int, len(regions))
		graceful := false
		for vi := range regions {
			for off := 0; off < len(regions[vi].view); off += ramChunkBytes {
				end := min(off+ramChunkBytes, len(regions[vi].view))
				ramFillChunk(regions[vi].view[off:end], seed, pass, regions[vi].off+uint64(off), fn)
				filled[vi] = end
				rep.Beat()
				if ctx.Err() != nil {
					graceful = true
					break
				}
			}
			if graceful {
				break
			}
		}
		if graceful {
			// Cancel during fill: verify the filled prefix, stop
			// uncounted (no dwell happened, but written bytes must
			// not go unchecked either).
			if bad, vi, off := ramDwellVerify(regions, seed, pass, fn, filled, rep, rp, hasRP); bad > 0 {
				return iters, ramDwellFault(bad, pass, regions, vi, off)
			}
			return iters, nil
		}

		// --- dwell: no memory traffic from this kernel. Sleeping
		// in bounded slices keeps cancellation responsive; each
		// slice beats so the heartbeat shows a live, waiting
		// process (liveness, not data progress - the stall detector
		// must not blame a legitimately paused kernel).
		target := ramDwellTarget(seed, pass, base, jitter)
		dwellInterrupted := false
		if target > 0 {
			deadline := time.Now().Add(target)
			for {
				left := time.Until(deadline)
				if left <= 0 {
					break
				}
				if left > ramDwellSlice {
					left = ramDwellSlice
				}
				time.Sleep(left)
				rep.Beat()
				if ctx.Err() != nil {
					dwellInterrupted = true
					break
				}
			}
		}

		// --- verify (read-only, not interruptible): everything
		// written gets read back even when the dwell was cut short.
		bad, vi, off := ramDwellVerify(regions, seed, pass, fn, nil, rep, rp, hasRP)
		if bad > 0 {
			// Data did not survive the pause (or the read path):
			// the fault outranks any cancellation, same family rule.
			return iters, ramDwellFault(bad, pass, regions, vi, off)
		}
		if dwellInterrupted {
			// The promised pause was not kept: partial, NOT counted,
			// NOT credited - even though the data verified clean.
			return iters, nil
		}

		iters++
		if hasRP {
			rp.RamPass(totalBytes)
		}
		rr.MemVerified(totalBytes)
		rep.Add(1)
	}
}

// ramDwellVerify reads every region back against the expected
// words; limit (when non-nil) caps each region to the filled prefix
// (the mid-fill graceful path only). Returns the total mismatch
// count; findings carry full identity.
func ramDwellVerify(regions []ramRegion, seed, pass uint64, fn ramWordFn, limit []int, rep Reporter, rp RamReporter, hasRP bool) (bad int, firstVi, firstOff int) {
	for vi := range regions {
		extent := len(regions[vi].view)
		if limit != nil && limit[vi] < extent {
			extent = limit[vi]
		}
		for off := 0; off < extent; off += ramChunkBytes {
			end := min(off+ramChunkBytes, extent)
			n := ramVerifyChunk(regions[vi].view[off:end], seed, pass, regions[vi].off+uint64(off), fn,
				func(local int, exp, act uint32) {
					if !hasRP {
						return
					}
					rp.ReportRamMismatch(RamMismatch{
						Algorithm: ramDwellKernel, Seed: seed, Pass: pass,
						Region: regions[vi].id, Offset: uint64(off + local),
						Expected: uint64(exp), Actual: uint64(act), Phase: "verify",
					})
				})
			if n > 0 {
				if bad == 0 {
					firstVi, firstOff = vi, off
				}
				bad += n
				return bad, firstVi, firstOff // evidence intact: abort before any later write
			}
			rep.Beat()
		}
	}
	return 0, 0, 0
}

func ramDwellFault(bad int, pass uint64, regions []ramRegion, vi, off int) error {
	return fmt.Errorf(
		"%s: %d mismatch(es) in pass %d (first in region %s at logical offset %d; data did not survive the pass; details in the journal ram evidence)",
		ramDwellKernel, bad, pass, regions[vi].id, off)
}
