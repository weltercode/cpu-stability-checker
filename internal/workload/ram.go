package workload

// P06: the ram-* family - memory tests that verify data in
// memory-engine regions (OS-committed, first-touched before the
// measured window), not on the Go heap. The old "mem" prototype
// stays as the small fast test; these are the real thing.
//
// Shared contract for every ram-* kernel (enforced by ramRun below):
//   - Expected data is derived from seed -> region -> pass -> offset,
//     never from goroutine scheduling order or worker counts: the
//     same config reproduces the same words.
//   - Fill and verify are SEPARATE full passes (the plan's contract:
//     write everything, then read everything back with a direct
//     per-word comparison). A mismatch is never overwritten by the
//     next pattern: the pass aborts and the slot faults.
//   - Work is chunked (ramChunkBytes) so cancellation latency is
//     bounded. A graceful stop (cancel) finishes the current chunk,
//     then VERIFIES what was written before stopping: a partial pass
//     that leaves written-but-unchecked bytes behind would count
//     coverage it never earned.
//   - Mismatches are reported through the optional RamReporter with
//     full reproduction context (the reporter caps details and
//     counts everything); the kernel itself never decides what is
//     "too many" findings.
//   - Offsets are LOGICAL within the region. Nothing here claims a
//     physical or DIMM address.

import (
	"context"
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

// ramChunkBytes bounds how much native memory a ram kernel touches
// between cancellation checks - and bounds the read-only tail after
// a mismatch inside one chunk.
const ramChunkBytes = 64 << 10

// Kernel identity strings for findings and the journal: results from
// different kernel versions are never merged or compared as one.
const (
	ramConstantKernel = "ram-constant-v1"
	ramAddressKernel  = "ram-address-v1"
	ramMovingKernel   = "ram-moving-v1"
	ramRandomKernel   = "ram-random-v1"
)

// ramPatterns is the constant/checkerboard cycle: all zero, all one,
// checkerboard, and its inverse. Each pattern is one full pass:
// write the whole region, then a separate full read verifying every
// word, then the next (inverted) pattern.
var ramPatterns = []uint32{0x00000000, 0xFFFFFFFF, 0xAAAAAAAA, 0x55555555}

// ramWordFn is a kernel's expected-word function: the ONLY thing
// that differs between ram-* algorithms. It must be a pure function
// of (seed, pass, global word index) - the global word index spans
// the whole partition (grant offset + local offset), so a word's
// expected value depends on WHERE it lives, not on which worker or
// region the budget happened to park it in. Repartitioning the same
// budget moves words between workers; it never silently changes what
// a given global position must contain.
type ramWordFn func(seed, pass, wordIdx uint64) uint32

// ramConstantWord ignores seed and position: the whole region holds
// one pattern per pass, cycling 00/FF/AA/55 (constant, inverse,
// checkerboard, inverse checkerboard).
func ramConstantWord(seed, pass, wordIdx uint64) uint32 {
	return ramPatterns[(pass-1)%uint64(len(ramPatterns))]
}

// ramAddressWord derives the expected word from the global position,
// the pass and the seed (the plan's "address derived" contract:
// regions hold different values because their global offsets
// differ). The mixing is the splitmix64 finalizer - a fixed,
// versioned spec inside ram-address-v1: change it and you must bump
// the kernel version, never the other way around.
func ramAddressWord(seed, pass, wordIdx uint64) uint32 {
	x := seed + 0x9E3779B97F4A7C15*(wordIdx+1) + 0xBF58476D1CE4E5B9*(pass+1)
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return uint32(x >> 32) // the high bits mix best
}

// runRamConstant implements the constant/checkerboard contract.
func runRamConstant(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRun(ctx, rep, ramConstantKernel, ramConstantWord)
}

// runRamAddress implements the address-derived contract.
func runRamAddress(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRun(ctx, rep, ramAddressKernel, ramAddressWord)
}

// ramRandomWord is the ram-random-v1 PRNG spec: an xorshift64* step
// (the xorshift64 triple plus the 0x2545F4914F6CDD1D multiply that
// lifts its weak low bits) over a state keyed by (seed, pass,
// global word index) - a deterministic stream in counter mode.
//
// Counter mode is what makes the plan's "reconstruction of the
// expected word independent of the actual contents" cheap and
// honest: the draw at position i is computed on its own, so a
// finding at (seed, pass, offset) reproduces in O(1) without
// replaying the pass, and repartitioning the budget never changes
// what a position must contain. The stream is pseudo-random and
// position-decorrelated (unlike ram-address's avalanche hash):
// adjacent words share no visible structure, which is the point of
// a seeded-random memory test - pattern-independent data catches
// coupling faults that pattern sweeps can miss.
//
// This mixing IS the reproduction spec: golden vectors pin it, and
// changing it requires bumping the kernel version, never the other
// way around.
func ramRandomWord(seed, pass, wordIdx uint64) uint32 {
	x := seed + 0x9E3779B97F4A7C15*(pass+1) + 0xFF51AFD7ED558CCD*(wordIdx+1)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	x *= 0x2545F4914F6CDD1D // xorshift64*: the multiply fixes xorshift64's weak low bits
	return uint32(x >> 32)
}

// runRamRandom implements the seeded-random contract on the shared
// ram engine: full write of the PRNG stream, separate full read
// comparing every word against an independently reconstructed
// expected value.
func runRamRandom(ctx context.Context, rep Reporter) (uint64, error) {
	return ramRun(ctx, rep, ramRandomKernel, ramRandomWord)
}

// runRamMoving implements the moving-inversions contract: fill the
// base pattern, then a FORWARD verify+invert sweep, then a REVERSE
// (complement-expect) verify+invert sweep back to the base pattern,
// then a FINAL read-only verify. Every sweep is a full pass over
// the filled extent - data moves in one direction per sweep, and
// each word is checked against exactly what the previous sweep must
// have left behind. After a mismatch inside a chunk the rest of
// that chunk is READ but never written again (read-only tail), and
// the pass aborts before any later sweep or pass can overwrite the
// evidence.
func runRamMoving(ctx context.Context, rep Reporter) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	regions, totalBytes := ramRegions(rr.MemRegions())
	rp, hasRP := rep.(RamReporter)
	seed := rr.RamSeed()
	fn := ramAddressWord // the base pattern is seed/pass/position-derived, like ram-address

	// The three verify sweeps: what each expects, and whether it
	// writes the inversion. "final" never writes: it is the closing
	// verdict over data the inversions already restored.
	type sweep struct {
		phase        string
		wantInverted bool
		invert       bool
	}
	sweeps := []sweep{
		{"verify-fwd", false, true},
		{"verify-rev", true, true},
		{"verify-final", false, false},
	}

	var iters, pass uint64
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++

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

		// The inversion sweeps are data movement: once a pass has
		// started, its sweeps run to completion over the filled
		// extent (bounded work, and stopping mid-sweep would leave
		// words in a state no later phase could honestly check).
		// Cancellation is honored at pass boundaries; the hard
		// backstop is the parent's kill.
		mismatches := 0
		var firstRegion, firstOffset int
		var firstPhase string
		for _, sw := range sweeps {
			for vi := range regions {
				for off := 0; off < filled[vi]; off += ramChunkBytes {
					end := min(off+ramChunkBytes, filled[vi])
					bad := ramInvertChunk(regions[vi].view[off:end], seed, pass, regions[vi].off+uint64(off),
						fn, sw.wantInverted, sw.invert,
						func(local int, exp, act uint32) {
							if !hasRP {
								return
							}
							rp.ReportRamMismatch(RamMismatch{
								Algorithm: ramMovingKernel, Seed: seed, Pass: pass,
								Region: regions[vi].id, Offset: uint64(off + local),
								Expected: uint64(exp), Actual: uint64(act), Phase: sw.phase,
							})
						})
					if bad > 0 {
						if mismatches == 0 {
							firstRegion, firstOffset, firstPhase = vi, off, sw.phase
						}
						mismatches += bad
						break
					}
					rep.Beat()
				}
				if mismatches > 0 {
					break
				}
			}
			if mismatches > 0 {
				break
			}
		}

		if mismatches > 0 {
			return iters, fmt.Errorf(
				"%s: %d mismatch(es) in pass %d phase %s (first in region %s at logical offset %d; details in the journal ram evidence)",
				ramMovingKernel, mismatches, pass, firstPhase, regions[firstRegion].id, firstOffset)
		}

		if graceful {
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

// ramRegion is one engine region prepared for the kernels: the byte
// view plus the identity string findings carry.
type ramRegion struct {
	view []byte
	id   string
	off  uint64 // grant's global offset: the region's position in the partition
}

func ramRegions(regions []memtest.AllocatedRegion) ([]ramRegion, uint64) {
	out := make([]ramRegion, len(regions))
	var total uint64
	for i, r := range regions {
		out[i].view = r.Block.Bytes()
		out[i].id = ramRegionID(r)
		out[i].off = r.Grant.Offset
		total += uint64(len(out[i].view))
	}
	return out, total
}

// ramRegionID is the identity string every ram finding carries: the
// grant coordinates, not a pointer. Tests pin the exact format -
// journal evidence from old runs must stay readable.
func ramRegionID(r memtest.AllocatedRegion) string {
	return fmt.Sprintf("worker=%d region=%d role=%s offset=%d",
		r.Grant.Worker, r.Grant.Region, r.Grant.Role, r.Grant.Offset)
}

// errNoEngineRegions guards the impossible path: preflight refuses
// ram workloads without the engine, and the child reporter carries
// compile-time interface pins. If it ever fires, it says so plainly
// instead of silently testing the Go heap.
var errNoEngineRegions = fmt.Errorf("ram kernel: no memory-engine regions (this family never runs on the Go heap: heap memory would test allocator bookkeeping, not RAM)")

// ramChunkRef is one chunk of one region in a pass's traversal: the
// ordered unit of fill/verify work. Every ram-* kernel works chunk
// by chunk so cancellation latency stays bounded (ramChunkBytes) and
// the read-only tail after a mismatch stays bounded too.
type ramChunkRef struct {
	region, start, end int
}

// ramSeqChunks is the default traversal: region-major, chunk-minor,
// ascending - byte-identical to what ramRun did before ordered
// traversals existed (the beat maps in the ram-* tests pin it).
func ramSeqChunks(regions []ramRegion) []ramChunkRef {
	var out []ramChunkRef
	for vi, r := range regions {
		for off := 0; off < len(r.view); off += ramChunkBytes {
			out = append(out, ramChunkRef{vi, off, min(off+ramChunkBytes, len(r.view))})
		}
	}
	return out
}

// ramChunkOrder reorders a pass's chunk visits. It must be a PURE
// function of (seed, pass, seq): the same inputs give the same
// order, so a finding reproduces and a graceful stop can verify
// exactly the chunks the fill completed. It must return a
// permutation of seq - every chunk exactly once.
type ramChunkOrder func(seed, pass uint64, seq []ramChunkRef) []ramChunkRef

// ramRun is the shared pass/chunk/cancel/fault engine every ram-*
// kernel runs on. kernelID names the kernel in findings and in the
// fault message; fn is the kernel's expected-word function.
func ramRun(ctx context.Context, rep Reporter, kernelID string, fn ramWordFn) (uint64, error) {
	return ramRunOrdered(ctx, rep, kernelID, fn, nil, false)
}

// ramRunOrdered is the shared engine with an explicit traversal
// story: order reorders the FILL visits (nil = sequential), and
// verifyRev makes the verify sweep run in the reverse of the fill
// order. The honesty rule that survives from the prefix model:
// verify checks EXACTLY the chunks the fill completed (visit[:done]
// in fill order, or its reverse when verifyRev) - never more
// (unwritten bytes are not this pass's business), never less
// (written bytes must not go unchecked). For the sequential order
// this is byte-identical to the old prefix engine, which is what
// lets ram-constant/address/random/moving keep their pinned beats.
func ramRunOrdered(ctx context.Context, rep Reporter, kernelID string, fn ramWordFn, order ramChunkOrder, verifyRev bool) (uint64, error) {
	rr, ok := rep.(RegionsReporter)
	if !ok || len(rr.MemRegions()) == 0 {
		return 0, errNoEngineRegions
	}
	regions, totalBytes := ramRegions(rr.MemRegions())
	rp, hasRP := rep.(RamReporter) // optional sink: without it mismatches still fault, they just leave no structured trace
	seed := rr.RamSeed()
	seq := ramSeqChunks(regions)

	var iters, pass uint64
	for {
		if ctx.Err() != nil {
			return iters, nil
		}
		pass++

		// --- fill phase (write-only): every chunk in traversal
		// order. done counts COMPLETED chunks, so the verify pass
		// below checks exactly what this pass actually wrote -
		// whatever order that was.
		visit := seq
		if order != nil {
			visit = order(seed, pass, seq)
		}
		done := 0
		graceful := false
		for i := range visit {
			c := visit[i]
			ramFillChunk(regions[c.region].view[c.start:c.end], seed, pass, regions[c.region].off+uint64(c.start), fn)
			done = i + 1
			rep.Beat()
			if ctx.Err() != nil {
				graceful = true
				break
			}
		}

		// --- verify phase (read-only): a separate full pass over
		// exactly the completed chunks, per the contract. On a
		// graceful fill stop this still verifies EVERY completed
		// chunk before returning: written-but-unchecked bytes would
		// be coverage the run never earned, so verify is not
		// interruptible here (it is bounded by what fill wrote plus
		// at most one chunk, and the parent's kill on hard cancel
		// is the backstop).
		mismatches := 0
		var firstRegion, firstOffset int
		for i := 0; i < done; i++ {
			c := visit[i]
			if verifyRev {
				c = visit[done-1-i]
			}
			bad := ramVerifyChunk(regions[c.region].view[c.start:c.end], seed, pass, regions[c.region].off+uint64(c.start), fn,
				func(local int, exp, act uint32) {
					if !hasRP {
						return
					}
					rp.ReportRamMismatch(RamMismatch{
						Algorithm: kernelID, Seed: seed, Pass: pass,
						Region: regions[c.region].id, Offset: uint64(c.start + local),
						Expected: uint64(exp), Actual: uint64(act), Phase: "verify",
					})
				})
			if bad > 0 {
				if mismatches == 0 {
					firstRegion, firstOffset = c.region, c.start
				}
				mismatches += bad
				// Read-only tail done: the rest of this chunk was
				// already scanned by ramVerifyChunk (it never
				// stops at the first bad word inside a chunk);
				// now the pass aborts BEFORE any next pattern
				// can overwrite the evidence.
				break
			}
			rep.Beat()
		}

		if mismatches > 0 {
			// A fault outranks a graceful stop: if cancel landed
			// during the verify that found broken data, the verdict
			// is the fault, not the cancellation.
			return iters, fmt.Errorf(
				"%s: %d mismatch(es) in pass %d (first in region %s at logical offset %d; details in the journal ram evidence)",
				kernelID, mismatches, pass, regions[firstRegion].id, firstOffset)
		}

		if graceful {
			// Partial pass: NOT counted, coverage NOT credited.
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

// ramFillChunk writes the expected words across v. baseOff is the
// GLOBAL byte offset of v[0] within the partition, so the expected
// value at a position is the same no matter which region the budget
// parked it in.
//
// The noinline barriers on fill/verify are load-bearing, not
// decoration: this toolchain has measured-deleted fill loops it
// could prove nobody reads and folded verification loads into
// constants. The flip tests are the semantic pin: a forwarded value
// would make a flipped byte undetectable and the test fails.
//
//go:noinline
func ramFillChunk(v []byte, seed, pass, baseOff uint64, fn ramWordFn) {
	for i := 0; i+4 <= len(v); i += 4 {
		w := fn(seed, pass, (baseOff+uint64(i))/4)
		v[i], v[i+1], v[i+2], v[i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		w := fn(seed, pass, (baseOff+uint64(i))/4)
		v[i] = byte(w >> (8 * (i % 4)))
	}
}

// ramVerifyChunk reads v back and compares EVERY word (and tail
// byte) against the expected value directly - no checksum shortcut
// in the base version: the plan forbids replacing the exact
// comparison until a measured two-stage design exists. It does not
// stop at the first bad word: the whole chunk is read (read-only
// tail), every mismatch is handed to report, and the count comes
// back so the caller can abort the pass with the evidence intact.
//
//go:noinline
func ramVerifyChunk(v []byte, seed, pass, baseOff uint64, fn ramWordFn, report func(localOff int, expected, actual uint32)) int {
	bad := 0
	for i := 0; i+4 <= len(v); i += 4 {
		want := fn(seed, pass, (baseOff+uint64(i))/4)
		got := uint32(v[i]) | uint32(v[i+1])<<8 | uint32(v[i+2])<<16 | uint32(v[i+3])<<24
		if got != want {
			bad++
			if report != nil {
				report(i, want, got)
			}
		}
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		w := fn(seed, pass, (baseOff+uint64(i))/4)
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

// ramInvertChunk is the moving-inversions sweep primitive: it
// compares every word (and tail byte) against the phase's expected
// value - the base word, complemented when wantInverted - and while
// the chunk stays clean it writes back the inversion. After the
// FIRST mismatch in the chunk it stops writing entirely: the rest
// of the chunk is read and reported (the read-only tail the plan
// demands), but the evidence is never overwritten by more
// inversions. The count comes back so the caller aborts the pass.
//
//go:noinline
func ramInvertChunk(v []byte, seed, pass, baseOff uint64, fn ramWordFn, wantInverted, invert bool, report func(localOff int, expected, actual uint32)) int {
	bad := 0
	for i := 0; i+4 <= len(v); i += 4 {
		want := fn(seed, pass, (baseOff+uint64(i))/4)
		if wantInverted {
			want = ^want
		}
		got := uint32(v[i]) | uint32(v[i+1])<<8 | uint32(v[i+2])<<16 | uint32(v[i+3])<<24
		if got != want {
			bad++
			if report != nil {
				report(i, want, got)
			}
			continue // read-only from here: no more writes in this chunk
		}
		// The write is gated on bad==0, not just on this word being
		// clean: once ANY mismatch landed in this chunk the whole
		// rest of it is read-only tail - later clean words must not
		// be inverted either, or the evidence freezes too late.
		if invert && bad == 0 {
			w := ^want
			v[i], v[i+1], v[i+2], v[i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
		}
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		w := fn(seed, pass, (baseOff+uint64(i))/4)
		if wantInverted {
			w = ^w
		}
		want := byte(w >> (8 * (i % 4)))
		if v[i] != want {
			bad++
			if report != nil {
				report(i, uint32(want), uint32(v[i]))
			}
			continue
		}
		if invert && bad == 0 {
			v[i] = byte(^w >> (8 * (i % 4)))
		}
	}
	return bad
}
