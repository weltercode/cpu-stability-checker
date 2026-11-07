package workload

// P07c: copy/verify - data MOVEMENT between paired engine regions.
// The memory engine already grants every worker a source AND a
// destination region (subprocess/mem.go partitions both roles); this
// kernel pairs them by role and moves the pass's pattern from
// source to destination with verification on both ends:
//
//   - the source is filled with the DESTINATION's image: the word
//     expected at source-local offset k is f(seed, pass, destination
//     global word k). The source is transport, not a pattern of its
//     own - what it must hold is defined by where the data is going.
//   - the source is verified BEFORE the transfer (an independent
//     read against that image): a broken source faults before any
//     byte is copied, so the destination is never blamed for source
//     damage, and never overwritten with it.
//   - blocks move chunk by chunk; the destination is verified AFTER
//     the transfer against the SAME independently reconstructed
//     image - not against "whatever the source held".
//
// Block identity and generation are carried by the content itself:
// the expected word is derived from the destination's global index
// (block ID) and the pass number (generation). A swapped block
// reads its neighbour's per-position values and faults; a stale
// block from an older pass reads the older generation's values and
// faults. No header can silently claim a wrong identity, because
// the data IS the identity claim, checked word by word.
//
// Cancellation follows the family contract: fill and copy are
// chunk-cancellable, and a graceful stop verifies exactly what was
// written (the source prefix, or the copied destination prefix)
// before returning with the partial pass uncounted. The untouched
// destination suffix legitimately holds the previous pass's
// generation - stale, but honest territory, never flagged and
// never credited.

import (
	"context"
	"fmt"
	"sort"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

const ramCopyKernel = "ram-copy-v1"

const (
	kernelPhaseVerifySrc  = "verify-src"  // independent source read before transfer
	kernelPhaseVerifyDest = "verify-dest" // independent destination read after transfer
)

// ramPair is one source/destination pair: equal-length regions of
// the same worker, verified to pair up before any byte moves.
type ramPair struct {
	src, dst ramRegion
}

// ramCopyPairs groups regions by worker and pairs the i-th source
// with the i-th destination (ordered by global offset), requiring
// equal lengths. Anything else is a setup error said out loud:
// silently testing "whatever regions exist" would quietly change
// what the profile promises.
func ramCopyPairs(raw []memtest.AllocatedRegion) ([]ramPair, error) {
	byWorker := map[int][]memtest.AllocatedRegion{}
	for _, r := range raw {
		byWorker[r.Grant.Worker] = append(byWorker[r.Grant.Worker], r)
	}
	workers := make([]int, 0, len(byWorker))
	for w := range byWorker {
		workers = append(workers, w)
	}
	sort.Ints(workers)

	var pairs []ramPair
	for _, w := range workers {
		var srcs, dsts []ramRegion
		for _, r := range byWorker[w] {
			switch r.Grant.Role {
			case memtest.RoleSource:
				srcs = append(srcs, ramRegion{view: r.Block.Bytes(), id: ramRegionID(r), off: r.Grant.Offset})
			case memtest.RoleDest:
				dsts = append(dsts, ramRegion{view: r.Block.Bytes(), id: ramRegionID(r), off: r.Grant.Offset})
			}
		}
		sort.Slice(srcs, func(i, j int) bool { return srcs[i].off < srcs[j].off })
		sort.Slice(dsts, func(i, j int) bool { return dsts[i].off < dsts[j].off })
		if len(srcs) == 0 && len(dsts) == 0 {
			continue // control-only worker: nothing to pair, nothing promised
		}
		if len(srcs) != len(dsts) {
			return nil, fmt.Errorf("%s: worker %d has %d source region(s) but %d destination region(s): copy needs one destination per source",
				ramCopyKernel, w, len(srcs), len(dsts))
		}
		for i := range srcs {
			if len(srcs[i].view) != len(dsts[i].view) {
				return nil, fmt.Errorf("%s: worker %d pair %d: source %s is %d bytes but destination %s is %d bytes: the image mapping is 1:1 by local offset",
					ramCopyKernel, w, i, srcs[i].id, len(srcs[i].view), dsts[i].id, len(dsts[i].view))
			}
			pairs = append(pairs, ramPair{src: srcs[i], dst: dsts[i]})
		}
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("%s: no source/destination region pairs (this kernel moves data between paired engine regions; without a destination there is no transfer to verify)",
			ramCopyKernel)
	}
	return pairs, nil
}

// runRamCopy implements the copy/verify contract. The expected-word
// function is the pinned ram-address spec: position-derived, so
// every destination position demands a different word and neither a
// swapped block nor a stale generation can pass as anything else.
func runRamCopy(ctx context.Context, rep Reporter) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	pairs, err := ramCopyPairs(rr.MemRegions())
	if err != nil {
		return 0, err
	}
	rp, hasRP := rep.(RamReporter)
	seed := rr.RamSeed()
	fn := ramAddressWord

	var movedBytes uint64 // unique verified range per full pass: source AND destination
	for _, p := range pairs {
		movedBytes += uint64(len(p.src.view)) + uint64(len(p.dst.view))
	}

	var iters, pass uint64
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++

		// --- phase 1: fill each source with the destination image
		// (chunk-cancellable; filled[pi] tracks the written prefix).
		filled := make([]int, len(pairs))
		graceful := false
		for pi := range pairs {
			p := pairs[pi]
			for off := 0; off < len(p.src.view); off += ramChunkBytes {
				end := min(off+ramChunkBytes, len(p.src.view))
				// baseOff = the DESTINATION's global offset: the word
				// at source-local offset is defined by the destination
				// global position it must eventually occupy.
				ramFillChunk(p.src.view[off:end], seed, pass, p.dst.off+uint64(off), fn)
				filled[pi] = end
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
			// Verify only the filled source prefix (image expected),
			// then stop uncounted - written source bytes never go
			// unchecked, unwritten ones are not this pass's business.
			if bad, id, off := ramCopyVerifySweep(pairs, seed, pass, fn, true, filled, rep, rp, hasRP, kernelPhaseVerifySrc); bad > 0 {
				return iters, ramCopyFault(bad, pass, id, off)
			}
			return iters, nil
		}

		// --- phase 2: verify every source IN FULL before any byte
		// moves (read-only, not interruptible: bounded work). A
		// broken source faults HERE, before the transfer - the
		// destination is never blamed for, nor overwritten by,
		// source damage.
		if bad, id, off := ramCopyVerifySweep(pairs, seed, pass, fn, true, nil, rep, rp, hasRP, kernelPhaseVerifySrc); bad > 0 {
			return iters, ramCopyFault(bad, pass, id, off)
		}

		// --- phase 3: transfer source -> destination, chunk by
		// chunk (cancellable; copied[pi] tracks the moved prefix).
		copied := make([]int, len(pairs))
		for pi := range pairs {
			p := pairs[pi]
			for off := 0; off < len(p.src.view); off += ramChunkBytes {
				end := min(off+ramChunkBytes, len(p.src.view))
				ramCopyChunk(p.dst.view[off:end], p.src.view[off:end])
				copied[pi] = end
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

		// --- phase 4: verify the destination against the SAME
		// independently reconstructed image. On a graceful copy stop
		// this still checks the entire copied prefix (written-but-
		// unchecked destination bytes would be coverage the run
		// never earned); the untouched suffix holds the previous
		// pass's generation and is not this pass's business.
		limit := copied
		if !graceful {
			limit = nil
		}
		if bad, id, off := ramCopyVerifySweep(pairs, seed, pass, fn, false, limit, rep, rp, hasRP, kernelPhaseVerifyDest); bad > 0 {
			// The fault outranks the graceful stop, same family rule.
			return iters, ramCopyFault(bad, pass, id, off)
		}
		if graceful {
			return iters, nil // partial pass: NOT counted, NOT credited
		}

		iters++
		if hasRP {
			rp.RamPass(movedBytes)
		}
		rr.MemVerified(movedBytes)
		rep.Add(1)
	}
}

// ramCopyVerifySweep verifies either the sources (src=true, against
// the destination-image expectation at the destination's global
// offsets) or the destinations (src=false, against their own global
// offsets). limit (when non-nil) caps each pair at limit[pi] bytes.
// Findings carry the verified region's own identity and local
// offset: evidence says WHERE it was seen, not where the data was
// headed.
func ramCopyVerifySweep(pairs []ramPair, seed, pass uint64, fn ramWordFn, src bool, limit []int, rep Reporter, rp RamReporter, hasRP bool, phase string) (bad int, firstRegionID string, firstOff int) {
	for pi := range pairs {
		p := pairs[pi]
		view, baseOff, id := p.dst.view, p.dst.off, p.dst.id
		if src {
			view, baseOff, id = p.src.view, p.dst.off, p.src.id // the image is defined by the destination's offsets
		}
		extent := len(view)
		if limit != nil && limit[pi] < extent {
			extent = limit[pi]
		}
		for off := 0; off < extent; off += ramChunkBytes {
			end := min(off+ramChunkBytes, extent)
			n := ramVerifyChunk(view[off:end], seed, pass, baseOff+uint64(off), fn,
				func(local int, exp, act uint32) {
					if !hasRP {
						return
					}
					rp.ReportRamMismatch(RamMismatch{
						Algorithm: ramCopyKernel, Seed: seed, Pass: pass,
						Region: id, Offset: uint64(off + local),
						Expected: uint64(exp), Actual: uint64(act), Phase: phase,
					})
				})
			if n > 0 {
				if bad == 0 {
					firstRegionID, firstOff = id, off
				}
				bad += n
				return bad, firstRegionID, firstOff // evidence intact: abort before any later write
			}
			rep.Beat()
		}
	}
	return 0, "", 0
}

func ramCopyFault(bad int, pass uint64, regionID string, off int) error {
	return fmt.Errorf(
		"%s: %d mismatch(es) in pass %d (first in region %s at logical offset %d; details in the journal ram evidence)",
		ramCopyKernel, bad, pass, regionID, off)
}

// ramCopyChunk moves one chunk src->dst. The noinline barrier keeps
// this a real memory-to-memory transfer: the flip tests are the
// semantic pin that the copy is not elided or forwarded (the P04
// lesson - a source-level claim is not proof).
//
//go:noinline
func ramCopyChunk(dst, src []byte) {
	for i := range dst {
		dst[i] = src[i]
	}
}
