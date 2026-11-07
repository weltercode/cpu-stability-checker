package workload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
	"time"
)

// TestAffineReferenceMatchesIteration proves the int workload's cross-check is
// correct: the fast-exponentiation reference must equal the plain iterative
// LCG. If it did not, the int workload would false-positive (or, worse, the
// reference could hide a real arithmetic fault).
func TestAffineReferenceMatchesIteration(t *testing.T) {
	const n = 100_000
	var x uint64 = 1
	for i := 0; i < n; i++ {
		x = x*lcgA + lcgB
	}
	if want := affineN(1, lcgA, lcgB, n); x != want {
		t.Fatalf("affine reference mismatch: iterative=%x reference=%x", x, want)
	}
}

// TestFloatStaysFinite is the regression guard for the false-positive bug: the
// float workload must never legitimately overflow to Inf/NaN, otherwise the
// finiteness check would report a fault on healthy hardware.
func TestFloatStaysFinite(t *testing.T) {
	a, b, c, d := 0.7, 1.3, 0.9, 1.0
	for i := 0; i < 1_000_000; i++ {
		a, b, c, d = floatStep(a, b, c, d)
		if !finite(a, b, c, d) {
			t.Fatalf("float went non-finite at iter %d: a=%v b=%v c=%v d=%v", i, a, b, c, d)
		}
	}
}

// TestFloatWindowMatchesLoop proves the redundant-recompute helper advances the
// accumulators identically to the hot loop, so a mismatch truly means a fault.
func TestFloatWindowMatchesLoop(t *testing.T) {
	const steps = 4096
	a, b, c, d := 0.7, 1.3, 0.9, 1.0
	ra, rb, rc, rd := a, b, c, d
	for i := 0; i < steps; i++ {
		ra, rb, rc, rd = floatStep(ra, rb, rc, rd)
	}
	wa, wb, wc, wd := floatWindow(a, b, c, d, steps)
	if ra != wa || rb != wb || rc != wc || rd != wd {
		t.Fatalf("floatWindow mismatch: loop=%v %v %v %v window=%v %v %v %v",
			ra, rb, rc, rd, wa, wb, wc, wd)
	}
}

// TestMemVerifyDetectsBitFlip proves the RAM detector accepts a clean buffer and
// rejects one with a single corrupted element.
func TestMemVerifyDetectsBitFlip(t *testing.T) {
	buf := make([]uint32, 1024)
	for i := range buf {
		buf[i] = memPattern(i)
	}
	if err := memVerify(buf); err != nil {
		t.Fatalf("clean buffer should pass: %v", err)
	}
	buf[512] ^= 1
	if err := memVerify(buf); err == nil {
		t.Fatalf("corrupted buffer should be detected")
	}
}

// TestSha256KAT pins the known-answer vectors so the sha256 workload's
// correctness probe is guaranteed against a stable reference.
func TestSha256KAT(t *testing.T) {
	for _, k := range sha256KAT {
		if got := sha256.Sum256(k.in); got != k.want {
			t.Fatalf("KAT failed for %q: got %x want %x", string(k.in), got, k.want)
		}
	}
}

// TestIntDetectsFault proves the int workload flags a corrupted accumulator
// against the anchored reference (the reference is fixed to the true start, so
// any drift is caught).
func TestIntDetectsFault(t *testing.T) {
	// Simulate a healthy run of a few batches, then corrupt x the way a
	// hardware fault would, and confirm the anchored reference disagrees.
	const batch = 4096
	var x uint64 = 1
	for i := 0; i < batch*3; i++ {
		x = x*lcgA + lcgB
	}
	x ^= 0x8000000000000000 // flip the top bit — a plausible transient fault
	if want := affineN(1, lcgA, lcgB, batch*3); x == want {
		t.Fatalf("corrupted accumulator was not detected")
	}
}

// TestFloatDetectsFault proves the finiteness probe catches a non-finite value.
func TestFloatDetectsFault(t *testing.T) {
	if finite(math.Inf(1), 1.0, 1.0, 1.0) {
		t.Fatalf("finite() should report Inf as non-finite")
	}
	if finite(math.NaN(), 1.0, 1.0, 1.0) {
		t.Fatalf("finite() should report NaN as non-finite")
	}
	if !finite(0.7, 1.3, 0.9, 1.0) {
		t.Fatalf("finite() should accept normal values")
	}
}

// P04 accounting: one Add per verified block, and the final verification
// counts the non-empty tail exactly once. The returned iteration count
// makes the expectation exact rather than timing-dependent: int verifies
// every 64th batch plus the tail, float every 4th batch plus a partial
// tail when one exists, float-exact verifies whole atomic blocks,
// sha256 and mem verify every iteration/block.
func TestVerifiedBlockAccounting(t *testing.T) {
	cases := []struct {
		name string
		run  func(context.Context, Reporter) (uint64, error)
		want func(iters uint64) uint64
	}{
		{"int", runInt, func(n uint64) uint64 {
			if n%64 == 0 {
				return n / 64
			}
			return n/64 + 1
		}},
		// float verifies every 4th batch; the stop-time tail check
		// adds one more only when the last window was partial.
		{"float", runFloat, func(n uint64) uint64 {
			if n%4 == 0 {
				return n / 4
			}
			return n/4 + 1
		}},
		// float-exact blocks are atomic: every block is fully
		// computed and verified, so adds == blocks exactly.
		{"float-exact", runFloatExact, func(n uint64) uint64 { return n }},
		{"sha256", runSha256, func(n uint64) uint64 { return n }},
		{"mem", runMem, func(n uint64) uint64 { return n }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rep := &recordingReporter{}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			iters, err := tt.run(ctx, rep)
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if iters == 0 {
				t.Skip("window too short to produce iterations")
			}
			adds, _ := rep.snapshot()
			if want := tt.want(iters); adds != want {
				t.Errorf("adds = %d for %d iterations, want %d (verified blocks only)", adds, iters, want)
			}
		})
	}
}

// The kernel identity must be reachable for the journal: built-ins carry
// their version, an adapter-registered external entry carries the adapter
// name, and a legacy entry stays unknown ("") instead of pretending.
func TestKernelVersionIdentities(t *testing.T) {
	for name, want := range map[string]string{
		"int":         "int-chains-v2",
		"float":       "float-repeated-v1",
		"float-exact": "float-exact-v1",
		"sha256":      "sha256-block-v2",
		"mem":         "mem-pattern-v1",
		"mem-bw":      "membw-load-v2",
		"ghost":       "",
	} {
		if got := KernelVersionOf(name); got != want {
			t.Errorf("KernelVersionOf(%q) = %q, want %q", name, got, want)
		}
	}
	RegisterExternal(map[string]ExternalSpec{
		"kv-legacy":  {Command: "x"},
		"kv-adapted": {Command: "x", Adapter: "ycruncher-v0.8"},
	})
	t.Cleanup(func() { Delete("kv-legacy"); Delete("kv-adapted") })
	if got := KernelVersionOf("kv-legacy"); got != "" {
		t.Errorf("legacy external kernel = %q, want unknown", got)
	}
	if got := KernelVersionOf("kv-adapted"); got != "ycruncher-v0.8" {
		t.Errorf("adapted external kernel = %q, want the adapter name", got)
	}
}

// The scalar reference must match the published example digests from
// FIPS 180-4 (the external authority), and must agree with
// crypto/sha256 across every padding-boundary size - the reference is
// only usable as a reference because it is anchored, not because our
// code wrote it.
func TestSha256RefAnchored(t *testing.T) {
	published := []struct {
		in   string
		want string
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq", "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"},
	}
	for _, p := range published {
		if got := sha256Ref([]byte(p.in)); hex.EncodeToString(got[:]) != p.want {
			t.Errorf("sha256Ref(%q...) = %x, want published %s", p.in[:min(12, len(p.in))], got, p.want)
		}
	}
	// every padding-boundary size, then the corpus itself
	for n := 0; n <= 200; n++ {
		in := make([]byte, n)
		for i := range in {
			in[i] = byte(i * 7)
		}
		if sha256Ref(in) != sha256.Sum256(in) {
			t.Fatalf("reference disagrees with crypto/sha256 at size %d", n)
		}
	}
	for _, in := range sha256Corpus {
		if sha256Ref(in) != sha256.Sum256(in) {
			t.Fatalf("reference disagrees on corpus size %d", len(in))
		}
	}
}

// The corpus is fixed by its generator: replaying the seed replays the
// exact bytes (golden digest below), so digests stay comparable across
// runs and machines, and a generator change is a visible corpus change.
func TestSha256CorpusGolden(t *testing.T) {
	var all []byte
	for _, in := range buildSha256Corpus() {
		all = append(all, in...)
	}
	const golden = "b46184c572db9036ded437214aff5bf86e6378ab934d8bd573a0094b0f3ae3c5"
	if got := sha256Ref(all); hex.EncodeToString(got[:]) != golden {
		t.Fatalf("corpus golden = %x (if the generator changed on purpose, re-anchor this value)", got)
	}
	if sha256.Sum256(all) != sha256Ref(all) {
		t.Error("corpus digest must agree between both implementations")
	}
}

// Corrupting the main work's result - the very fault this kernel
// exists to catch - must fail the block and name the input size.
func TestSha256BlockDetectsCorruption(t *testing.T) {
	if err := sha256Block(sha256Corpus, sha256.Sum256); err != nil {
		t.Fatalf("clean block reported %v", err)
	}
	corrupt := func(in []byte) [32]byte {
		h := sha256.Sum256(in)
		h[0] ^= 0x01 // one bit off the main work's result
		return h
	}
	err := sha256Block(sha256Corpus, corrupt)
	if err == nil {
		t.Fatal("corrupted main-work digests passed the block")
	}
	if !strings.Contains(err.Error(), "0-byte input") {
		t.Errorf("error = %v, want it to name the failing input size", err)
	}
}

// The float-exact reference must agree with the FPU path BITWISE at
// every step, across many full cycles of all four recurrences (17,
// 33, ~5 and ~10 steps): exactness is the whole design, so any
// divergence - including a rounding trap at a wrap point - is a bug.
func TestFloatExactReferenceMatches(t *testing.T) {
	var a, b, c, d float64 = 0, 0, 1, 1
	var A, B, C, D int64 = 0, 0, 4, 4
	for i := 0; i < 20000; i++ {
		a, b, c, d = exactStep(a, b, c, d)
		A, B, C, D = exactRefStep(A, B, C, D)
		if !exactMatch(a, b, c, d, A, B, C, D) {
			t.Fatalf("FP and integer reference diverged at step %d: fp=(%v,%v,%v,%v) ref=(%v,%v,%v,%v)",
				i, a, b, c, d, float64(A)/2, float64(B)/4, float64(C)/4, float64(D)/4)
		}
	}
}

// Corrupting any accumulator by one ulp (or flipping d's sign) must
// fail the bitwise comparison: there is no epsilon that could hide a
// flipped result.
func TestFloatExactDetectsCorruption(t *testing.T) {
	var a, b, c, d float64 = 0, 0, 1, 1
	var A, B, C, D int64 = 0, 0, 4, 4
	for i := 0; i < 5000; i++ {
		a, b, c, d = exactStep(a, b, c, d)
		A, B, C, D = exactRefStep(A, B, C, D)
	}
	if !exactMatch(a, b, c, d, A, B, C, D) {
		t.Fatal("clean state must match the reference")
	}
	if exactMatch(math.Nextafter(a, 2), b, c, d, A, B, C, D) {
		t.Error("1-ulp corruption of a went undetected")
	}
	if exactMatch(a, math.Nextafter(b, -2), c, d, A, B, C, D) {
		t.Error("1-ulp corruption of b went undetected")
	}
	if exactMatch(a, b, math.Nextafter(c, 0), d, A, B, C, D) {
		t.Error("1-ulp corruption of c went undetected")
	}
	if exactMatch(a, b, c, -d, A, B, C, D) {
		t.Error("sign corruption of d went undetected")
	}
}

// The plan's "ошибка в последнем float-пакете": a fault in the last
// partial window must be caught by the stop-time tail check, and a
// clean partial window must pass it.
func TestFloatTailCheckDetectsLastPacket(t *testing.T) {
	const rem = 3 // batches since the last checkpoint
	pa, pb, pc, pd := 0.7, 1.3, 0.9, 1.0
	a, b, c, d := floatWindow(pa, pb, pc, pd, rem*floatBatch)
	if !floatTailCheck(pa, pb, pc, pd, a, b, c, d, rem) {
		t.Fatal("clean partial window rejected")
	}
	if floatTailCheck(pa, pb, pc, pd, math.Nextafter(a, 2), b, c, d, rem) {
		t.Error("corrupted last packet passed the tail check")
	}
}

// cancelOnBeat cancels the context the first time the kernel reports
// progress: for kernels that beat after a batch but before its
// verification (int, float) the cancellation lands exactly between
// compute and verify; for kernels whose beat follows a whole verified
// block it lands just after the block boundary.
type cancelOnBeat struct {
	recordingReporter
	cancelsLeft int
	cancel      context.CancelFunc
}

func (r *cancelOnBeat) Beat() {
	r.recordingReporter.Beat()
	if r.cancelsLeft > 0 {
		r.cancelsLeft--
		r.cancel()
	}
}

// Cancellation between compute and verify (the plan's test): the
// bounded verification of work already computed still completes and
// counts - it is never skipped because ctx is done, never fabricated
// for work that was not done, and a clean cancellation is not a
// fault. Deterministic: the reporter itself cancels at a known beat.
func TestCancelBetweenComputeAndVerify(t *testing.T) {
	cases := []struct {
		name      string
		run       func(context.Context, Reporter) (uint64, error)
		wantIters uint64
		wantAdds  uint64
	}{
		// batch computed, cancel before its verify: the stop-time
		// tail check still runs and counts the partial window.
		{"int", runInt, 1, 1},
		{"float", runFloat, 1, 1},
		// these verify whole blocks before beating: the block that
		// completed is verified and counted, nothing partial follows.
		{"float-exact", runFloatExact, 1, 1},
		{"sha256", runSha256, 1, 1},
		{"mem", runMem, 1, 1},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rep := &cancelOnBeat{cancelsLeft: 1, cancel: cancel}
			iters, err := tt.run(ctx, rep)
			if err != nil {
				t.Fatalf("run() error = %v, want nil: a clean cancellation is not a fault", err)
			}
			if iters != tt.wantIters {
				t.Errorf("iters = %d, want %d", iters, tt.wantIters)
			}
			adds, _ := rep.snapshot()
			if adds != tt.wantAdds {
				t.Errorf("adds = %d, want %d: the verified work counts, nothing else does", adds, tt.wantAdds)
			}
		})
	}
}
