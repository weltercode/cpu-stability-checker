package workload

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

// regionReporter is the child-side reporter shape for the migrated
// mem prototype: it hands out engine regions and records the
// verified byte count the kernel reports per completed pass.
type regionReporter struct {
	regions  []memtest.AllocatedRegion
	verified uint64
	adds     uint64
	beats    uint64
	seed     uint64
	// ram evidence sink (mirrors what the child coreCounter does):
	// counts every mismatch, keeps the first journal.MaxRamFindings.
	ramPasses  uint64
	ramUnique  uint64
	ramTotal   uint64
	ramFound   int
	ramTrunc   bool
	ramDetails []ramFindingLite
	cancel     context.CancelFunc // called from Beat after N beats (deterministic cancel point)
	cancelAt   uint64             // 1-based beat number that cancels; 0 = never
	flip       func()             // called from Beat at flipAt (deterministic corruption point)
	flipAt     uint64             // 1-based beat number that flips; 0 = never
}

// ramFindingLite captures what the kernel handed the reporter, so
// tests can check identity, offsets and phases without importing the
// journal (the child maps these later).
type ramFindingLite struct {
	Algorithm string
	Seed      uint64
	Pass      uint64
	Region    string
	Offset    uint64
	Expected  uint64
	Actual    uint64
	Phase     string
}

// The compile-time assertion matters: runMem discovers regions through
// a runtime type assertion, so a fake that silently stops satisfying
// RegionsReporter would fall back to the heap path and the region
// tests would pass vacuously. This line makes that a compile error.
var (
	_ RegionsReporter = (*regionReporter)(nil)
	_ RamReporter     = (*regionReporter)(nil)
)

func (r *regionReporter) Add(uint64) { r.adds++ }
func (r *regionReporter) Beat() {
	r.beats++
	if r.flip != nil && r.flipAt > 0 && r.beats == r.flipAt {
		r.flip()
	}
	if r.cancel != nil && r.cancelAt > 0 && r.beats == r.cancelAt {
		r.cancel()
	}
}
func (r *regionReporter) MemRegions() []memtest.AllocatedRegion { return r.regions }
func (r *regionReporter) MemVerified(b uint64)                  { r.verified = b }
func (r *regionReporter) RamSeed() uint64                       { return r.seed }

func (r *regionReporter) ReportRamMismatch(m RamMismatch) {
	r.ramFound++
	if len(r.ramDetails) >= 16 {
		r.ramTrunc = true
		return
	}
	r.ramDetails = append(r.ramDetails, ramFindingLite{
		Algorithm: m.Algorithm, Seed: m.Seed, Pass: m.Pass, Region: m.Region,
		Offset: m.Offset, Expected: uint64(m.Expected), Actual: uint64(m.Actual), Phase: m.Phase,
	})
}

func (r *regionReporter) RamPass(bytes uint64) {
	r.ramPasses++
	r.ramUnique = bytes
	r.ramTotal += bytes
}

func openTestEngine(t *testing.T, sizes ...uint64) *memtest.Engine {
	t.Helper()
	alloc, err := memtest.NewAllocator()
	if errors.Is(err, memtest.ErrUnsupported) {
		t.Skipf("no native allocator on this platform: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var grants []memtest.Grant
	var off uint64
	for i, s := range sizes {
		grants = append(grants, memtest.Grant{
			Worker: 0, Region: i, Role: memtest.RoleSource, Offset: off, Requested: s,
		})
		off += s
	}
	e, err := memtest.Open(grants, alloc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return e
}

// The migrated prototype must run the same pattern kernel over
// native regions: every byte written and read back, iterations and
// verified bytes both accounted, nothing silently skipped.
func TestRunMemRegionsFullPasses(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions()}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runMem(ctx, rep)
	if err != nil {
		t.Fatalf("runMem with regions: %v", err)
	}
	if it == 0 {
		t.Fatal("no iterations completed in 300ms")
	}
	if rep.verified != (128+64)<<10 {
		t.Errorf("verified = %d, want every byte of both regions (%d)", rep.verified, (128+64)<<10)
	}
	if rep.adds != it || rep.beats != it {
		t.Errorf("accounting drift: iters=%d adds=%d beats=%d", it, rep.adds, rep.beats)
	}
}

// The flip test is the semantic pin for the region kernel: if the
// compiler forwarded stored values through the verification read
// (the DSE hazard measured earlier in this project), a flipped byte
// in real memory would go UNDETECTED and this test would fail. A
// pass means the stores and loads really hit memory.
func TestMemBytesKernelDetectsFlip(t *testing.T) {
	e := openTestEngine(t, 4096, 4097) // 4097: exercises the word-tail path too
	views := make([][]byte, 0, 2)
	for _, r := range e.Regions() {
		views = append(views, r.Block.Bytes())
	}
	for _, v := range views {
		memFillBytes(v)
		if err := memVerifyBytes(v); err != nil {
			t.Fatalf("clean region reported: %v", err)
		}
	}
	v := views[1]
	v[len(v)-1] ^= 0x40 // flip the tail byte
	if err := memVerifyBytes(v); err == nil {
		t.Error("flipped tail byte not detected")
	}
	v[len(v)-1] ^= 0x40
	v[12] ^= 0x01 // flip one bit of a word byte
	if err := memVerifyBytes(v); err == nil {
		t.Error("flipped word byte not detected")
	}
}

// Without engine regions the prototype keeps its Go buffer - the
// same kernel, same verification - so in-process mode keeps working
// while the journal simply has no mem accounting for that slot.
func TestRunMemFallsBackToHeap(t *testing.T) {
	rep := &regionReporter{} // MemRegions returns nil: no engine
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	it, err := runMem(ctx, rep)
	if err != nil {
		t.Fatalf("runMem without regions: %v", err)
	}
	if it == 0 {
		t.Fatal("no iterations completed in 200ms")
	}
	if rep.verified != 0 {
		t.Errorf("heap path must not report engine-verified bytes: %d", rep.verified)
	}
}
