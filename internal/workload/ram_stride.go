package workload

// P07b2: modulo/stride - the word-granularity address-line sweep.
// Each pass picks a stride from a fixed versioned schedule, writes
// the INVERTED base pattern across every residue class in turn
// (class = global word index mod stride), and finishes with one
// full class-aware verify: every word must read base XOR-inverted
// exactly when its class has already been flipped this pass.
//
// Why residue iteration and not one modular cycle: when the stride
// is not coprime with the region length, a single cycle silently
// skips whole offset classes. Iterating all residues 0..S-1 covers
// every word exactly once for ANY stride and ANY length - the
// plan's "full traversal of all offset classes" requirement, and
// the tail bytes (partial words) join the class of their word.
//
// The final full verify is what makes coupling attempts visible:
// a strided write that corrupts a NEIGHBOUR in another class would
// be re-overwritten and masked if each class only verified itself;
// the closing full read checks every word against the exact flip
// state it should be in, no matter which sweep broke it.
//
// Cancellation lands only at class boundaries (a started class
// sweep runs to completion - bounded work), and the class-aware
// verify then checks exactly the flipped-and-unflipped state the
// fill left behind: written bytes never go unchecked, and the
// partial pass is never credited.

import (
	"context"
	"fmt"
)

const ramStrideKernel = "ram-stride-v1"

// ramStrideSchedule is the versioned stride cycle: pass p uses
// ramStrideSchedule[(p-1) % len]. The list mixes small strides,
// powers of two and non-coprime-with-length cases on purpose;
// changing the list changes the kernel's coverage, so it is pinned
// by golden test and requires a version bump.
var ramStrideSchedule = []uint64{2, 3, 4, 5, 8, 16, 32}

// runRamStride implements the modulo/stride contract on engine
// memory. The base pattern is the pinned ram-address word function
// (position-derived, so every position is distinct and a wrong
// write at one position can never be masked by another's value).
func runRamStride(ctx context.Context, rep Reporter) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	regions, totalBytes := ramRegions(rr.MemRegions())
	rp, hasRP := rep.(RamReporter)
	seed := rr.RamSeed()
	fn := ramAddressWord

	var iters, pass uint64
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++
		stride := ramStrideSchedule[(pass-1)%uint64(len(ramStrideSchedule))]

		// --- base fill (write-only, sequential chunks): every word
		// gets the base pattern for this pass. Same prefix/cancel
		// discipline as the shared engine.
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
			// No class flipped yet: verify ONLY the filled prefix
			// against the BASE expectation (doneClasses=0) - the
			// unfilled bytes legitimately still hold the previous
			// pass's state and are not this pass's business. Then
			// stop with the partial pass uncounted.
			if bad, firstVi, firstOff := ramStrideVerify(regions, seed, pass, fn, stride, 0, filled, rep, rp, hasRP, kernelPhaseStrideBase); bad > 0 {
				return iters, ramStrideFault(ramStrideKernel, bad, pass, regions, firstVi, firstOff)
			}
			return iters, nil
		}

		// --- flip sweeps: one residue class at a time, whole
		// partition per class. Cancellation is honored at class
		// boundaries only; doneClasses tracks exactly which classes
		// the flips completed.
		doneClasses := uint64(0)
		for r := uint64(0); r < stride; r++ {
			for vi := range regions {
				ramFlipStrideChunk(regions[vi].view, seed, pass, regions[vi].off, stride, r, fn)
			}
			doneClasses++
			rep.Beat()
			if ctx.Err() != nil {
				graceful = true
				break
			}
		}

		// --- full class-aware verify (read-only, not interruptible):
		// every word must read base inverted iff its class was
		// flipped. This is the closing verdict that catches
		// cross-class corruption from any sweep.
		bad, firstVi, firstOff := ramStrideVerify(regions, seed, pass, fn, stride, doneClasses, nil, rep, rp, hasRP, kernelPhaseVerify)
		if bad > 0 {
			// A fault outranks a graceful stop, same rule as the
			// shared engine: broken data decides the verdict.
			return iters, ramStrideFault(ramStrideKernel, bad, pass, regions, firstVi, firstOff)
		}
		if graceful {
			// Partial pass: NOT counted, coverage NOT credited -
			// but every written byte was just verified above.
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

const (
	kernelPhaseStrideBase = "verify-fill" // graceful mid-base-fill check
	kernelPhaseVerify     = "verify"      // the closing class-aware sweep
)

// ramStrideVerify runs the class-aware verify over every region in
// chunks. doneClasses decides the expected inversion per word;
// limit (when non-nil) caps each region at limit[vi] bytes - the
// mid-base-fill graceful path checks only the filled prefix, the
// closing sweep checks everything (limit=nil). It returns the total
// mismatch count plus the first offending (region, chunk offset)
// for the fault message.
func ramStrideVerify(regions []ramRegion, seed, pass uint64, fn ramWordFn, stride, doneClasses uint64, limit []int, rep Reporter, rp RamReporter, hasRP bool, phase string) (bad int, firstVi, firstOff int) {
	for vi := range regions {
		extent := len(regions[vi].view)
		if limit != nil && limit[vi] < extent {
			extent = limit[vi]
		}
		for off := 0; off < extent; off += ramChunkBytes {
			end := min(off+ramChunkBytes, extent)
			n := ramVerifyChunkStrided(regions[vi].view[off:end], seed, pass, regions[vi].off+uint64(off), fn, stride, doneClasses,
				func(local int, exp, act uint32) {
					if !hasRP {
						return
					}
					rp.ReportRamMismatch(RamMismatch{
						Algorithm: ramStrideKernel, Seed: seed, Pass: pass,
						Region: regions[vi].id, Offset: uint64(off + local),
						Expected: uint64(exp), Actual: uint64(act), Phase: phase,
					})
				})
			if n > 0 {
				if bad == 0 {
					firstVi, firstOff = vi, off
				}
				bad += n
				// Read-only tail done inside the chunk; abort the
				// sweep before anything could overwrite evidence.
				return bad, firstVi, firstOff
			}
			rep.Beat()
		}
	}
	return 0, 0, 0
}

func ramStrideFault(kernel string, bad int, pass uint64, regions []ramRegion, vi, off int) error {
	return fmt.Errorf(
		"%s: %d mismatch(es) in pass %d (first in region %s at logical offset %d; details in the journal ram evidence)",
		kernel, bad, pass, regions[vi].id, off)
}

// ramFlipStrideChunk writes the INVERTED expected word at every
// word (and tail byte lane) whose GLOBAL word index is congruent to
// residue modulo stride. Region offsets are page-aligned, so the
// global word index is exact at every byte.
//
//go:noinline
func ramFlipStrideChunk(v []byte, seed, pass, baseOff uint64, stride, residue uint64, fn ramWordFn) {
	for i := 0; i+4 <= len(v); i += 4 {
		gw := (baseOff + uint64(i)) / 4
		if gw%stride != residue {
			continue
		}
		w := ^fn(seed, pass, gw)
		v[i], v[i+1], v[i+2], v[i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		gw := (baseOff + uint64(i)) / 4
		if gw%stride != residue {
			continue
		}
		w := ^fn(seed, pass, gw)
		v[i] = byte(w >> (8 * (i % 4)))
	}
}

// ramVerifyChunkStrided compares every word (and tail byte lane)
// against base XOR-inverted exactly when its global word class is
// below doneClasses - the exact state the flip sweeps must have
// produced. Like ramVerifyChunk it never stops at the first bad
// word: the whole chunk is read, every mismatch is reported, and
// the count comes back so the sweep aborts with evidence intact.
//
//go:noinline
func ramVerifyChunkStrided(v []byte, seed, pass, baseOff uint64, fn ramWordFn, stride, doneClasses uint64, report func(localOff int, expected, actual uint32)) int {
	bad := 0
	for i := 0; i+4 <= len(v); i += 4 {
		gw := (baseOff + uint64(i)) / 4
		want := fn(seed, pass, gw)
		if gw%stride < doneClasses {
			want = ^want
		}
		got := uint32(v[i]) | uint32(v[i+1])<<8 | uint32(v[i+2])<<16 | uint32(v[i+3])<<24
		if got != want {
			bad++
			if report != nil {
				report(i, want, got)
			}
		}
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		gw := (baseOff + uint64(i)) / 4
		w := fn(seed, pass, gw)
		if gw%stride < doneClasses {
			w = ^w
		}
		want := byte(w >> (8 * (i % 4)))
		if v[i] != want {
			bad++
			if report != nil {
				report(i, uint32(want), uint32(v[i]))
			}
		}
	}
	return bad
}
