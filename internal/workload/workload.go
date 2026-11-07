// Package workload defines self-verifying units of CPU/memory load and the
// registry that makes them addressable by name everywhere in the tool.
package workload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

// Idle is the special plan value meaning "do nothing on this core" so a test
// can keep some cores quiet (e.g. for single-core isolation).
const Idle = "idle"

// Reporter lets a workload report progress to the run status while it works,
// so the live UI and the watchdog can observe per-core activity in real time.
type Reporter interface {
	// Add counts verified units of work. For self-verifying kernels
	// one Add corresponds to one block whose result was actually
	// checked (P04 accounting): counting unverified iterations as
	// checks would inflate the verified total the report claims.
	// Beat only says the slot is alive and never counts as a check.
	Add(n uint64)
	Beat()
}

// RegionsReporter is the optional extension a Reporter implements when
// it can hand the workload native memory regions from the memory
// engine (P05): the mem prototype then writes and verifies its
// pattern in OS-committed memory, the same API the multi-gigabyte
// engines of P06 will use. Reporters that do not implement it are
// unaffected - runMem keeps its own small Go buffer, and the journal
// simply has no mem_done record for that slot (unknown, never zero).
// MemVerified lets a region-backed workload state exactly how many
// engine bytes its last completed pass really checked; the child
// carries that into the journal untouched.
type RegionsReporter interface {
	MemRegions() []memtest.AllocatedRegion
	MemVerified(bytes uint64)
	// RamSeed is the configured seed for the ram-* family (P06): the
	// expected data is a function of seed -> region -> pass -> offset,
	// so the same seed reproduces the same words regardless of how
	// many workers or regions the budget happened to split into.
	// Zero is a valid seed, not "unset": the knob defaults to 0 and
	// the journal records it whenever a seed-consuming workload ran.
	RamSeed() uint64
}

// RamMismatch is one observed mismatch reported by a ram-* kernel:
// everything needed to reproduce it, and nothing invented. Offset is
// LOGICAL within the region - the tool cannot see memory wiring, so
// no field here claims a physical or DIMM address. The kernel that
// did the work reports the mismatch; the reporter stamps the
// observation time and the slot's CPU, and caps the details while
// counting every observation (journal.RamEvidence).
type RamMismatch struct {
	Algorithm string // kernel identity, e.g. "ram-constant-v1"
	Seed      uint64
	Pass      uint64 // 1-based pass in which the mismatch was seen
	Region    string // grant identity: worker/region/role/global offset
	Offset    uint64 // logical offset within the region
	Expected  uint64
	Actual    uint64
	Phase     string // fill / verify-fwd / verify-rev / final ...
}

// RamReporter is the optional extension a Reporter implements when it
// collects ram-* evidence: ReportRamMismatch hands one mismatch to
// the reporter (which counts all of them and keeps only the first
// journal.MaxRamFindings details), and RamPass states that one
// completed pass covered exactly bytes distinct engine bytes - the
// reporter accumulates the unique-vs-total counts the plan demands.
// Reporters without it are unaffected: the kernel still faults on
// the first mismatch through the ordinary error path, it just has
// nowhere to leave structured evidence.
type RamReporter interface {
	ReportRamMismatch(m RamMismatch)
	RamPass(bytes uint64)
}

// StatusReporter is the optional extension a Reporter may implement when a
// workload can say what it is doing right now: external tools print their
// test names (y-cruncher announces each finished algorithm), and the live
// panel shows the latest report. Reporters that do not implement it are
// unaffected.
type StatusReporter interface {
	Status(text string)
}

// Evidence vocabulary: what an external adapter's protocol actually
// proved about the tool's self-verification. Strings (not a type) so
// the model layer can compare them without importing this package,
// exactly like the WHEA severity/nature vocabulary. An adapter only
// ever reports a state its protocol confirms; anything unproven stays
// EvUnknown, never a fabricated success.
const (
	EvUnknown      = "unknown"            // the process ran; what it verified is not provable from our side
	EvNotPerformed = "not_performed"      // the tool itself declares no data verification (load-only)
	EvPartial      = "partial"            // protocol start confirmed; completion was not confirmed
	EvConfirmed    = "confirmed-complete" // the tool's own protocol confirmed its verification ran to completion
	EvFailed       = "failed"             // the protocol reported a fault (pairs with a finding)
)

// EvidenceReporter is the optional extension an external adapter uses
// to report what the tool's protocol proved, beyond "the process was
// alive": started, meaningful progress, hardware-fault messages, and a
// completion the adapter's version-specific parser confirmed. The
// engine keeps the last reported state per slot and carries it into
// the journal and the final verdict; a reporter that never hears from
// an adapter leaves the evidence unknown - which is NOT the same as
// "verified" and never becomes partial by accident.
type EvidenceReporter interface {
	ReportEvidence(state string)
}

// Verification names how (and whether) a workload checks its own work.
// It decides what may honestly be counted as a verified check: timer
// beats of a live tool or the bytes moved by a pure loader are not
// checks, while a completed iteration of a self-checking kernel is.
type Verification string

const (
	// VerifyIndependent: results are compared against a reference
	// computed by a different route (affine formula, memory pattern).
	VerifyIndependent Verification = "independent"
	// VerifyRepeated: the window is recomputed and compared with the
	// same algorithm. It catches changed results (hardware faults)
	// but shares any systematic blind spot, so reports say so.
	VerifyRepeated Verification = "repeated"
	// VerifyKAT: known-answer probes run every iteration, but the
	// workload's main output is not compared against a reference
	// (tightening this is P04 work).
	VerifyKAT Verification = "kat"
	// VerifyNone: load only, no data verification (mem-bw).
	VerifyNone Verification = "none"
	// VerifyExternal: a third-party tool; our adapter has no proof of
	// what it verified (its real protocol is P03 work).
	VerifyExternal Verification = "external"
)

// Workload is a self-verifying unit of CPU and/or memory load. Run does work
// until ctx is done, reports progress via rep, and returns a non-nil error if
// it detects an arithmetic or memory fault (rather than just "survived").
type Workload struct {
	Name         string
	Description  string
	Run          func(ctx context.Context, rep Reporter) (uint64, error)
	Verification Verification
	// KernelVersion names this implementation ("int-affine-v1", an
	// adapter name like "ycruncher-v0.8", ...). It rides into the
	// journal's core_done so results produced by different kernels -
	// or by different protocol adapters - are never merged or
	// compared as if they were the same work. "" = unknown (legacy
	// external entries): read as unknown, never as the current code.
	KernelVersion string
	// SingleInstance marks tools that must be one process per run (Prime95's
	// GUI, y-cruncher): they spread over the whole machine by themselves.
	// The in-process engine shares one process across all slots; the
	// per-process engine cannot (each worker is its own process), so the
	// mode choice downgrades to in-process for these workloads.
	SingleInstance bool
	// RequiresMemory marks workloads that verify data in memory-engine
	// regions (the P06 ram-* family): they need a configured memory
	// budget AND per-process mode, because that is where the engine
	// actually allocates. The run refuses them BEFORE starting
	// (plan.PreflightMemoryWorkloads, setup error exit 2) - running
	// them without the regions they promise to verify would be the
	// quiet reduction this project forbids.
	RequiresMemory bool
	// FaultClass says what a fault from this workload is evidence
	// ABOUT (A06) - explicit registry data, never name guessing:
	//   compute:    a wrong computed value indicts the compute path of
	//              the pinned CPU (the classic CPU-stability finding);
	//   memory:    a wrong word in engine memory indicts the memory
	//              (region/offset evidence); the CPU that observed it
	//              is recorded as observed_on only and never enters
	//              the CPU blacklist automatically;
	//   synthetic: the deliberate fault prober: it proves the
	//              reporting pipeline, it is not hardware evidence.
	// "" (unset) reads as compute: custom-registered and external
	// workloads keep the established per-CPU contract.
	FaultClass string
}

// Fault classes (the Workload.FaultClass vocabulary).
const (
	FaultCompute   = "compute"
	FaultMemory    = "memory"
	FaultSynthetic = "synthetic"
)

// FaultClassOf reports what a fault from the named workload is
// evidence about. Unknown names read as compute (the conservative
// established default), never as "no fault class means no blame".
func FaultClassOf(name string) string {
	w, ok := workloads[name]
	if !ok || w.FaultClass == "" {
		return FaultCompute
	}
	return w.FaultClass
}

var workloads = map[string]*Workload{
	"int":          {Name: "int", Description: "integer arithmetic; three chains (LCG, rotate/xor, divide) each checked against an independent reference", Run: runInt, Verification: VerifyIndependent, KernelVersion: "int-chains-v2"},
	"float":        {Name: "float", Description: "floating-point math; re-checks its own window by repeating the same algorithm (not an independent reference)", Run: runFloat, Verification: VerifyRepeated, KernelVersion: "float-repeated-v1"},
	"float-exact":  {Name: "float-exact", Description: "floating-point math on exactly representable values; cross-checked bitwise against an integer reference", Run: runFloatExact, Verification: VerifyIndependent, KernelVersion: "float-exact-v1"},
	"sha256":       {Name: "sha256", Description: "SHA-256 hashing; every digest compared against an independent scalar reference and published answers", Run: runSha256, Verification: VerifyIndependent, KernelVersion: "sha256-block-v2"},
	"mem":          {Name: "mem", Description: "writes and verifies RAM patterns to detect changed bits", Run: runMem, Verification: VerifyIndependent, KernelVersion: "mem-pattern-v1"},
	"ram-constant": {Name: "ram-constant", Description: "constant/checkerboard patterns (00/FF/AA/55) in engine memory: full write, separate full verify per word, then the inverted pattern; needs a memory budget", Run: runRamConstant, Verification: VerifyIndependent, KernelVersion: "ram-constant-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-address":  {Name: "ram-address", Description: "address-derived words in engine memory: expected value = f(seed, pass, global position), so every region holds different data and repartitioning never changes what a position must contain; needs a memory budget", Run: runRamAddress, Verification: VerifyIndependent, KernelVersion: "ram-address-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-moving":   {Name: "ram-moving", Description: "moving inversions in engine memory: fill, forward verify+invert, complement verify+invert back, final read-only verify; every sweep is a full pass and a mismatch stops all writing with the evidence intact; needs a memory budget", Run: runRamMoving, Verification: VerifyIndependent, KernelVersion: "ram-moving-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-random":   {Name: "ram-random", Description: "seeded-random words in engine memory: a fixed xorshift64* stream in counter mode over global positions, every word verified against an independently reconstructed expected value; needs a memory budget", Run: runRamRandom, Verification: VerifyIndependent, KernelVersion: "ram-random-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-walking":  {Name: "ram-walking", Description: "walking bits in engine memory: a 64-pass cycle drives each bit of every word high (all others low) then low (all others high), every pass a full write plus a separate full verify; needs a memory budget", Run: runRamWalking, Verification: VerifyIndependent, KernelVersion: "ram-walking-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-reverse":  {Name: "ram-reverse", Description: "reverse sweeps in engine memory: the address-derived pattern is written ascending on odd passes and descending on even, and verified opposite to the fill; same data contract, different access direction; needs a memory budget", Run: runRamReverse, Verification: VerifyIndependent, KernelVersion: "ram-reverse-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-perm":     {Name: "ram-perm", Description: "seeded block permutation in engine memory: 64 KiB blocks are written in a reproducible scattered order (full-cycle LCG bijection per seed/pass), read back in reverse write order; needs a memory budget", Run: runRamPerm, Verification: VerifyIndependent, KernelVersion: "ram-perm-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-stride":   {Name: "ram-stride", Description: "modulo/stride sweeps in engine memory: each pass inverts the address-derived base pattern across one residue class of a scheduled stride at a time (all classes covered for any stride and length), then one full class-aware verify; needs a memory budget", Run: runRamStride, Verification: VerifyIndependent, KernelVersion: "ram-stride-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-copy":     {Name: "ram-copy", Description: "copy/verify between paired engine regions: the source holds the destination's image and is verified BEFORE the transfer, blocks move chunk by chunk, the destination is verified AFTER against the independently reconstructed image; swapped blocks and stale generations fault by content; needs a memory budget", Run: runRamCopy, Verification: VerifyIndependent, KernelVersion: "ram-copy-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-dwell":    {Name: "ram-dwell", Description: "fill, pause, verify in engine memory: data is written, the kernel dwells for a seeded reproducible pause (250ms base + 0..125ms jitter per pass), then reads everything back; data survival after a quiet interval, not a DRAM retention promise; needs a memory budget", Run: runRamDwell, Verification: VerifyIndependent, KernelVersion: "ram-dwell-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"ram-handoff":  {Name: "ram-handoff", Description: "block ownership handoff between worker processes over one shared section (P07e): runs ONLY as session leases (LEASE_WRITE/LEASE_READ) arbitrated by the parent - in a scenario only as a coordinator-driven handoff phase (handoff_blocks; the shared section geometry is its budget), never as a slot-loop phase run; the tester never races, so a mismatch is memory evidence, not an ordering artifact", Run: runRamHandoffPhase, Verification: VerifyIndependent, KernelVersion: "ram-handoff-v1", RequiresMemory: true, FaultClass: FaultMemory},
	"mem-bw":       {Name: "mem-bw", Description: "memory bandwidth load; does not verify data (reads feed a checksum, not a verdict)", Run: runMemBW, Verification: VerifyNone, KernelVersion: "membw-load-v2"},
	"fault":        {Name: "fault", Description: "always fails; verifies the fault-reporting path itself (used by soak)", Run: runFault, Verification: VerifyNone, KernelVersion: "fault-probe-v1", FaultClass: FaultSynthetic},
}

// runFault is a deterministic negative control: it always reports a fault,
// so 'soak' can prove the fault pipeline (journal record, exit code,
// blacklist) still works cycle after cycle without waiting for the CPU to
// actually misbehave.
func runFault(ctx context.Context, rep Reporter) (uint64, error) {
	return 0, fmt.Errorf("injected fault: this workload always fails")
}

// Register adds or replaces a workload by name. Built-ins are only shadowed
// on purpose.
func Register(w *Workload) {
	workloads[w.Name] = w
}

// Delete removes a workload by name (used by tests and reconfiguration).
func Delete(name string) {
	delete(workloads, name)
}

// Names lists all registered workloads, sorted.
func Names() []string {
	names := make([]string, 0, len(workloads))
	for name := range workloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get looks up a workload by name.
func Get(name string) (*Workload, bool) {
	w, ok := workloads[name]
	return w, ok
}

// IsSingleInstance reports whether the named workload must be one process
// per run. Unknown names are not.
func IsSingleInstance(name string) bool {
	w, ok := workloads[name]
	return ok && w.SingleInstance
}

// CountsIterationsAsChecks reports whether one completed iteration of the
// named workload counts as a confirmed verified check for coverage.
// External tools (whose "iterations" are timer beats or output lines) and
// pure loaders never do; unknown names do not either.
func CountsIterationsAsChecks(name string) bool {
	w, ok := workloads[name]
	if !ok {
		return false
	}
	switch w.Verification {
	case VerifyIndependent, VerifyRepeated, VerifyKAT:
		return true
	}
	return false
}

// VerificationOf returns the named workload's verification class; an
// unknown name has none ("").
func VerificationOf(name string) Verification {
	if w, ok := workloads[name]; ok {
		return w.Verification
	}
	return ""
}

// KernelVersionOf returns the implementation identity that must ride
// into the journal's core_done; "" means unknown (unregistered name,
// legacy external entry) and must never be read as "the current one".
func KernelVersionOf(name string) string {
	if w, ok := workloads[name]; ok {
		return w.KernelVersion
	}
	return ""
}

// RunOne executes a single workload until ctx is cancelled. Idle slots
// simply wait; unknown names are setup errors, never hardware faults.
func RunOne(ctx context.Context, name string, rep Reporter) (uint64, error) {
	if name == Idle {
		<-ctx.Done()
		return 0, nil
	}
	w, ok := Get(name)
	if !ok {
		return 0, NewSetupError(fmt.Errorf("unknown workload %q", name))
	}
	return w.Run(ctx, rep)
}

func finite(vals ...float64) bool {
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// --- int -------------------------------------------------------------------

const (
	lcgA uint64 = 6364136223846793005
	lcgB uint64 = 1442695040888963407
)

// affineN computes the state after n steps of x = x*A + B, i.e. the first
// component of M^n * [x0;1] where M = [[A,B],[0,1]]. All math mod 2^64. This
// is an independent (fast-exponentiation) reference for the iterative loop.
func affineN(x0, A, B, n uint64) uint64 {
	var r00, r01, r10, r11 uint64 = 1, 0, 0, 1
	var b00, b01, b10, b11 uint64 = A, B, 0, 1
	for n > 0 {
		if n&1 == 1 {
			r00, r01, r10, r11 = r00*b00+r01*b10, r00*b01+r01*b11, r10*b00+r11*b10, r10*b01+r11*b11
		}
		b00, b01, b10, b11 = b00*b00+b01*b10, b00*b01+b01*b11, b10*b00+b11*b10, b10*b01+b11*b11
		n >>= 1
	}
	return r00*x0 + r01
}

func runInt(ctx context.Context, rep Reporter) (uint64, error) {
	const batch = 4096
	const verifyEvery = 64
	var x, y, v uint64 = 1, 0x1234567887654321, 0xDEADBEEFCAFEBABE
	// yRef and vRef are checkpointed reference states: they advance
	// only at verification points, so a reference never re-runs the
	// whole history. x needs no checkpoint - affineN anchors it to
	// the fixed start in O(log n).
	var yRef, vRef uint64 = y, v
	// the n-step affine map for one verification interval, computed
	// once by GF(2) exponentiation; the tail builds an on-demand power.
	intervalMap := powGF2(yBase(), verifyEvery*batch)
	var it uint64
	for {
		if ctx.Err() != nil {
			// Final verification closes the non-empty tail: the
			// partial window is checked against all three references
			// and counted once - never dropped, never assumed correct.
			if want := affineN(1, lcgA, lcgB, it*batch); want != x {
				return it, fmt.Errorf("int: LCG fault at iter %d: x=%x want %x", it, x, want)
			}
			if rem := it % verifyEvery; rem > 0 {
				steps := uint64(rem) * batch
				if want := powGF2(yBase(), steps).apply(yRef); want != y {
					return it, fmt.Errorf("int: rotate/xor fault at iter %d: y=%x want %x", it, y, want)
				}
				if want := vRefAdvance(vRef, steps); want != v {
					return it, fmt.Errorf("int: divide-chain fault at iter %d: v=%x want %x", it, v, want)
				}
				rep.Add(1)
			}
			return it, nil
		}
		for i := 0; i < batch; i++ {
			x = x*lcgA + lcgB
			y = yStep(y)
			v = vStep(v)
		}
		it++
		rep.Beat()
		if it%verifyEvery == 0 {
			if want := affineN(1, lcgA, lcgB, it*batch); want != x {
				return it, fmt.Errorf("int: LCG fault at iter %d: x=%x want %x", it, x, want)
			}
			yRef = intervalMap.apply(yRef)
			if yRef != y {
				return it, fmt.Errorf("int: rotate/xor fault at iter %d: y=%x want %x", it, y, yRef)
			}
			vRef = vRefAdvance(vRef, verifyEvery*batch)
			if vRef != v {
				return it, fmt.Errorf("int: divide-chain fault at iter %d: v=%x want %x", it, v, vRef)
			}
			// One Add per verified block: the 64 batches whose three
			// chains all matched their independent references.
			rep.Add(1)
		}
	}
}

// --- float -----------------------------------------------------------------

// floatStep is one bounded FPU update. sin/cos keep every accumulator in a
// small range so the workload never legitimately overflows, which keeps the
// finiteness check a meaningful fault probe.
func floatStep(a, b, c, d float64) (float64, float64, float64, float64) {
	a = math.Sin(a)*2.0 + 0.5
	b = math.Cos(b)*2.0 - 0.5
	c = math.Sin(c)*1.5 + 0.3
	d = d*0.999999 + 1e-6
	return a, b, c, d
}

// floatWindow advances the four accumulators by exactly `steps` from a known
// start, in a separate code path from the hot loop, for redundant verification.
func floatWindow(a, b, c, d float64, steps uint64) (float64, float64, float64, float64) {
	for i := uint64(0); i < steps; i++ {
		a, b, c, d = floatStep(a, b, c, d)
	}
	return a, b, c, d
}

const floatBatch = 4096 // one window batch of floatStep updates

// floatTailCheck verifies the partial window between the checkpoint and
// the hot loop's current state: rem batches recomputed from the
// checkpoint must land bitwise on the live values (same algorithm, same
// order - Go does not fuse FP operations, so the recompute is
// deterministic). The partial window at stop is checked, never dropped
// and never assumed correct.
func floatTailCheck(pa, pb, pc, pd, a, b, c, d float64, rem uint64) bool {
	ra, rb, rc, rd := floatWindow(pa, pb, pc, pd, rem*floatBatch)
	return a == ra && b == rb && c == rc && d == rd
}

func runFloat(ctx context.Context, rep Reporter) (uint64, error) {
	var a, b, c, d float64 = 0.7, 1.3, 0.9, 1.0
	// pa..pd is the checkpoint used to verify the most recent window.
	var pa, pb, pc, pd float64 = a, b, c, d
	var lastVerified uint64
	var it uint64
	for {
		if ctx.Err() != nil {
			if !finite(a, b, c, d) {
				return it, fmt.Errorf("float: non-finite value at iter %d: a=%v b=%v c=%v d=%v", it, a, b, c, d)
			}
			// The partial window at stop is verified, not dropped:
			// recomputing the remaining batches from the checkpoint
			// must land bitwise on where the hot loop ended.
			if rem := it - lastVerified; rem > 0 {
				if !floatTailCheck(pa, pb, pc, pd, a, b, c, d, rem) {
					return it, fmt.Errorf("float: partial window mismatch at iter %d", it)
				}
				rep.Add(1)
			}
			return it, nil
		}
		for i := 0; i < floatBatch; i++ {
			a, b, c, d = floatStep(a, b, c, d)
		}
		it++
		rep.Beat()
		if !finite(a, b, c, d) {
			return it, fmt.Errorf("float: non-finite value at iter %d: a=%v b=%v c=%v d=%v", it, a, b, c, d)
		}
		// Redundant recompute of the last window from the prior
		// checkpoint: one Add per verified block (4 batches), only
		// after the recompute agreed. Finiteness alone is a sanity
		// invariant, not a verified unit of work.
		if it%4 == 0 {
			if ra, rb, rc, rd := floatWindow(pa, pb, pc, pd, 4*floatBatch); a != ra || b != rb || c != rc || d != rd {
				return it, fmt.Errorf("float: redundant recompute mismatch at iter %d", it)
			}
			pa, pb, pc, pd = a, b, c, d
			lastVerified = it
			rep.Add(1)
		}
	}
}

// --- float-exact -------------------------------------------------------------

// The float-exact kernel exercises the FPU add/subtract/multiply/compare
// paths on values chosen so every step is exact in binary64:
//
//	a: always a multiple of 1/2 with 0 <= a <= 8.5   (aligned FP adds)
//	b: always a multiple of 1/4 with -8.25 <= b <= 0 (aligned FP subs)
//	c: always a power of two, 1 <= c <= 32           (exponent-only mul)
//	d: always a power of two, |d| <= 16, sign flips  (mul by -2)
//
// Exactness per operation: adding or subtracting these dyadic values
// never rounds (mantissas stay tiny at these magnitudes), and
// multiplying by a power of two only shifts the exponent. So the FP
// path and the integer reference below must agree BITWISE - there is
// no epsilon anywhere and no rounding assumption to document beyond
// "every operation is exact". Operation order is one step at a time in
// both paths; Go does not fuse FP operations, so no FMA enters.
//
// The reference carries the same recurrences in scaled integers
// (A=2a, B=4b, C=4c, D=4d): integer add/sub/mul and integer compares -
// a different arithmetic unit than the FPU path under test. The branch
// predicates are identical under exact scaling (a > 8.0 iff A > 16,
// b < -8.0 iff B < -32, c > 16.0 iff C > 64, |d| > 8.0 iff |D| > 32),
// so both paths take the same branches by construction.
func exactStep(a, b, c, d float64) (float64, float64, float64, float64) {
	a += 0.5
	if a > 8.0 {
		a -= 8.5
	}
	b -= 0.25
	if b < -8.0 {
		b += 8.25
	}
	c *= 2.0
	if c > 16.0 {
		c *= 0.25
	}
	d *= -2.0
	if d > 8.0 || d < -8.0 {
		d *= -0.25
	}
	return a, b, c, d
}

// exactRefStep is the independent integer reference for exactStep.
// Every division is by a power of two on a value known to be a
// multiple of it, so the integer path is exact too.
func exactRefStep(A, B, C, D int64) (int64, int64, int64, int64) {
	A++
	if A > 16 {
		A -= 17
	}
	B--
	if B < -32 {
		B += 33
	}
	C *= 2
	if C > 64 {
		C /= 4
	}
	D *= -2
	if D > 32 || D < -32 {
		D = -D / 4
	}
	return A, B, C, D
}

// exactMatch compares the FP state against the integer reference
// bitwise: the scaled integers convert back exactly (small integers,
// division by powers of two), so equality is the whole comparison
// rule - no epsilon, no tolerance band.
func exactMatch(a, b, c, d float64, A, B, C, D int64) bool {
	return a == float64(A)/2 && b == float64(B)/4 && c == float64(C)/4 && d == float64(D)/4
}

// runFloatExact counts one Add per verified block: the block's FP
// result must match the integer reference bitwise. Blocks are atomic
// (no ctx check inside one), so there is never a half-checked tail: a
// block is either fully computed and verified, or not started. A
// cancellation landing mid-block still completes and verifies that
// bounded block - never a fabricated fault, never a dropped check.
func runFloatExact(ctx context.Context, rep Reporter) (uint64, error) {
	const block = 1024
	var a, b, c, d float64 = 0, 0, 1, 1
	var A, B, C, D int64 = 0, 0, 4, 4
	var it uint64
	for {
		if ctx.Err() != nil {
			return it, nil
		}
		for i := 0; i < block; i++ {
			a, b, c, d = exactStep(a, b, c, d)
			A, B, C, D = exactRefStep(A, B, C, D)
		}
		if !exactMatch(a, b, c, d, A, B, C, D) {
			return it, fmt.Errorf("float-exact: FPU path disagrees with the integer reference at block %d: fp=(%v,%v,%v,%v) ref=(%v,%v,%v,%v)",
				it, a, b, c, d, float64(A)/2, float64(B)/4, float64(C)/4, float64(D)/4)
		}
		it++
		rep.Add(1)
		rep.Beat()
	}
}

// --- sha256 ----------------------------------------------------------------

func must32(hexStr string) [32]byte {
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != 32 {
		panic("bad sha256 KAT: " + hexStr)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// sha256KAT are well-known SHA-256 digests used as a correctness probe.
var sha256KAT = []struct {
	in   []byte
	want [32]byte
}{
	{[]byte(""), must32("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")},
	{[]byte("abc"), must32("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")},
}

// runSha256's verified block: the main work (crypto/sha256 - the
// SHA-extension/AVX2 path this machine dispatches to) hashes the whole
// corpus and every digest must match the scalar reference; the
// published known answers ride every block as the external-authority
// anchor, checked against BOTH implementations. One Add only after the
// whole block verified.
func runSha256(ctx context.Context, rep Reporter) (uint64, error) {
	var it uint64
	for {
		if ctx.Err() != nil {
			return it, nil
		}
		if err := sha256Block(sha256Corpus, sha256.Sum256); err != nil {
			return it, err
		}
		for _, k := range sha256KAT {
			if got := sha256.Sum256(k.in); got != k.want {
				return it, fmt.Errorf("sha256: published KAT failed for input %q: got %x want %x", string(k.in), got, k.want)
			}
			if ref := sha256Ref(k.in); ref != k.want {
				return it, fmt.Errorf("sha256: scalar reference disagrees with published KAT for %q: got %x want %x", string(k.in), ref, k.want)
			}
		}
		it++
		rep.Add(1)
		rep.Beat()
	}
}

// --- mem -------------------------------------------------------------------

// memPattern writes the deterministic value that belongs at index i.
func memPattern(i int) uint32 {
	return uint32(i) ^ uint32(i>>16) ^ 0xA5A5A5A5
}

// memVerify checks every element against the expected pattern; it returns a
// fault error on the first mismatch. This is the RAM bit-flip detector.
func memVerify(buf []uint32) error {
	for i := range buf {
		if got := buf[i]; got != memPattern(i) {
			return fmt.Errorf("mem: bit flip at index %d: got %08x want %08x", i, got, memPattern(i))
		}
	}
	return nil
}

// runMem writes a deterministic pattern then reads it back and verifies every
// element — the classic way to catch RAM bit-flips. With engine regions
// available (per-process mode with a configured memory budget) the pattern
// lives in OS-committed memory touched before the measured window; without
// them the prototype keeps its small 4 MiB Go buffer. Both paths run the
// same memPattern/memVerify kernel, so the small prototype and the
// multi-gigabyte engines share the API and the checking logic.
func runMem(ctx context.Context, rep Reporter) (uint64, error) {
	if rr, ok := rep.(RegionsReporter); ok {
		if regions := rr.MemRegions(); len(regions) > 0 {
			return runMemRegions(ctx, rep, rr, regions)
		}
	}
	return runMemHeap(ctx, rep)
}

func runMemHeap(ctx context.Context, rep Reporter) (uint64, error) {
	const size = 4 << 20 // 4 MiB per worker
	buf := make([]uint32, size/4)
	var it uint64
	for {
		if ctx.Err() != nil {
			return it, nil
		}
		for i := range buf {
			buf[i] = memPattern(i)
		}
		if err := memVerify(buf); err != nil {
			return it, err
		}
		it++
		rep.Add(1)
		rep.Beat()
	}
}

// runMemRegions runs the same kernel over the engine's regions. The
// views come from Block.Bytes(): native memory, committed by the
// parent's budget and first-touched before this window, so no page
// faults land inside the measurement. Every byte of every region is
// written and read back each pass, and the verified byte count is
// reported through the reporter - never inferred at the journal.
func runMemRegions(ctx context.Context, rep Reporter, rr RegionsReporter, regions []memtest.AllocatedRegion) (uint64, error) {
	views := make([][]byte, len(regions))
	var total uint64
	for i, r := range regions {
		views[i] = r.Block.Bytes()
		total += uint64(len(views[i]))
	}
	var it uint64
	for {
		if ctx.Err() != nil {
			return it, nil
		}
		for _, v := range views {
			memFillBytes(v)
		}
		for _, v := range views {
			if err := memVerifyBytes(v); err != nil {
				return it, err
			}
		}
		it++
		rr.MemVerified(total)
		rep.Add(1)
		rep.Beat()
	}
}

// memFillBytes writes the memPattern word pattern across a native
// region, little-endian, tail bytes included.
//
//go:noinline
func memFillBytes(v []byte) {
	for i := 0; i+4 <= len(v); i += 4 {
		w := memPattern(i / 4)
		v[i], v[i+1], v[i+2], v[i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		v[i] = byte(memPattern(i/4) >> (8 * (i % 4)))
	}
}

// memVerifyBytes reads the region back and checks every byte against
// the pattern; the first mismatch is reported with its offset.
//
//go:noinline
func memVerifyBytes(v []byte) error {
	for i := 0; i+4 <= len(v); i += 4 {
		want := memPattern(i / 4)
		got := uint32(v[i]) | uint32(v[i+1])<<8 | uint32(v[i+2])<<16 | uint32(v[i+3])<<24
		if got != want {
			return fmt.Errorf("mem: word %d: got %08x want %08x", i/4, got, want)
		}
	}
	for i := (len(v) / 4) * 4; i < len(v); i++ {
		if want := byte(memPattern(i/4) >> (8 * (i % 4))); v[i] != want {
			return fmt.Errorf("mem: byte %d: got %02x want %02x", i, v[i], want)
		}
	}
	return nil
}

// bwSink is a package-level accumulator: a write to a global cannot be
// dead-code eliminated, so the checksum - and the loads that produced
// it - must survive codegen.
var bwSink uint64

// memBWSink consumes one iteration's strided checksum.
//
//go:noinline
func memBWSink(sum uint64) { bwSink ^= sum }

// runMemBW is a pure memory bandwidth loader (write then read), no
// verify: it stays an explicit load, and its iterations are never
// counted as checks. The read result is USED, not discarded: a
// strided checksum reduction feeds a global sink, so the accesses
// cannot be optimized away - and the objdump test pins that claim
// against the emitted machine code, because a KeepAlive alone never
// proved the accesses survived.
func runMemBW(ctx context.Context, rep Reporter) (uint64, error) {
	buf := make([]uint64, 1<<20/8)
	var it uint64
	for {
		if ctx.Err() != nil {
			return it, nil
		}
		for i := range buf {
			buf[i]++
		}
		var sum uint64
		for i := 0; i < len(buf); i += 8 {
			sum += buf[i]
		}
		memBWSink(sum)
		it++
		rep.Add(1)
		rep.Beat()
	}
}
