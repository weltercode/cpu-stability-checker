package workload

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
)

// openTestEngineRoles builds engine regions with explicit roles for
// worker 0 (offsets assigned sequentially, like Partition does).
func openTestEngineRoles(t *testing.T, roles []memtest.Role, size uint64) *memtest.Engine {
	t.Helper()
	alloc, err := memtest.NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	var grants []memtest.Grant
	var off uint64
	for i, role := range roles {
		grants = append(grants, memtest.Grant{Worker: 0, Region: i, Role: role, Offset: off, Requested: size})
		off += size
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

// pairedReporter: one source + one destination region of `size`.
// Beat map per pass (one chunk per phase):
//
//	pass 1: fill-src=1 | verify-src=2 | copy=3 | verify-dst=4
//	pass 2: fill-src=5 | verify-src=6 | copy=7 | verify-dst=8
//
// For size 128K each phase is two chunks, so every phase number
// doubles.
func pairedReporter(t *testing.T, size uint64) (*regionReporter, uint64) {
	t.Helper()
	e := openTestEngineRoles(t, []memtest.Role{memtest.RoleSource, memtest.RoleDest}, size)
	rep := &regionReporter{regions: e.Regions(), seed: 7}
	var dstOff uint64
	for _, r := range e.Regions() {
		if r.Grant.Role == memtest.RoleDest {
			dstOff = r.Grant.Offset
		}
	}
	return rep, dstOff
}

func copyImage(seed, pass, dstBaseWord, localByte uint64) uint32 {
	return ramAddressWord(seed, pass, dstBaseWord+localByte/4)
}

// Pairing is explicit: source-only regions (what the old engine
// helper builds) must be REFUSED, not quietly run as a pattern test.
func TestRamCopyRefusesUnpairedRegions(t *testing.T) {
	e := openTestEngine(t, 64<<10) // all RoleSource, like the legacy helper
	rep := &regionReporter{regions: e.Regions(), seed: 7}
	_, err := runRamCopy(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "0 destination region(s)") {
		t.Errorf("err = %v, want the source-without-destination refusal", err)
	}

	// control-only regions: nothing to pair at all
	e2 := openTestEngineRoles(t, []memtest.Role{memtest.RoleControl}, 4096)
	rep2 := &regionReporter{regions: e2.Regions(), seed: 7}
	if _, err := runRamCopy(context.Background(), rep2); err == nil ||
		!strings.Contains(err.Error(), "no source/destination region pairs") {
		t.Errorf("err = %v, want the no-pairs refusal", err)
	}
}

func TestRamCopyRefusesMismatchedPairs(t *testing.T) {
	// unequal pair lengths: 1:1 image mapping is broken
	alloc, err := memtest.NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	e, err := memtest.Open([]memtest.Grant{
		{Worker: 0, Region: 0, Role: memtest.RoleSource, Offset: 0, Requested: 4096},
		{Worker: 0, Region: 1, Role: memtest.RoleDest, Offset: 4096, Requested: 8192},
	}, alloc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	rep := &regionReporter{regions: e.Regions(), seed: 7}
	if _, err := runRamCopy(context.Background(), rep); err == nil ||
		!strings.Contains(err.Error(), "1:1 by local offset") {
		t.Errorf("err = %v, want the length-mismatch refusal", err)
	}

	// count mismatch: two sources, one destination
	e2 := openTestEngineRoles(t, []memtest.Role{memtest.RoleSource, memtest.RoleSource, memtest.RoleDest}, 4096)
	rep2 := &regionReporter{regions: e2.Regions(), seed: 7}
	if _, err := runRamCopy(context.Background(), rep2); err == nil ||
		!strings.Contains(err.Error(), "one destination per source") {
		t.Errorf("err = %v, want the count-mismatch refusal", err)
	}
}

// Timed run: exact accounting over source AND destination (both are
// independently verified per pass, so the unique verified range is
// the union), and zero false mismatches.
func TestRamCopyPassesAndCoverage(t *testing.T) {
	rep, _ := pairedReporter(t, 64<<10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamCopy(ctx, rep)
	if err != nil {
		t.Fatalf("runRamCopy: %v", err)
	}
	if it == 0 {
		t.Fatal("no copy passes completed in 300ms")
	}
	const moved = uint64(128 << 10) // 64K source + 64K destination
	if rep.ramPasses != it || rep.adds != it || rep.ramFound != 0 {
		t.Errorf("accounting drift: iters=%d passes=%d adds=%d found=%d", it, rep.ramPasses, rep.adds, rep.ramFound)
	}
	if rep.ramUnique != moved || rep.ramTotal != it*moved || rep.verified != moved {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d (want %d per pass)", rep.ramUnique, rep.ramTotal, rep.verified, moved)
	}
}

// Contents after a timed run: both regions hold the destination
// image for the completed pass, with the interrupted next pass's
// image as an honest chunk-aligned prefix wherever it got to (fill
// or copy) - the same two-state layout the family pins.
func TestRamCopyContentsTwoState(t *testing.T) {
	rep, dstOff := pairedReporter(t, 128<<10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamCopy(ctx, rep)
	if err != nil {
		t.Fatalf("runRamCopy: %v", err)
	}
	if it == 0 {
		t.Fatal("no copy passes completed in 300ms")
	}
	dstBase := dstOff / 4
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		state := 0
		for i := 0; i+4 <= len(v); i += 4 {
			cur := copyImage(7, it+1, dstBase, uint64(i))
			prev := copyImage(7, it, dstBase, uint64(i))
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %s byte %d = %08x: not a prefix of image(%d) over image(%d)",
					r.Grant.Role, i, got, it+1, it)
			}
		}
	}
}

// Stale generation: after the copy wrote pass 1's image into the
// destination, overwrite one destination word with the GENERATION 2
// value (a different pass's content). The closing verify must fault
// with the destination's own identity - a stale block cannot claim
// the current generation because the data IS the identity claim.
func TestRamCopyStaleGenerationFault(t *testing.T) {
	rep, dstOff := pairedReporter(t, 64<<10)
	var dstView []byte
	for _, r := range rep.regions {
		if r.Grant.Role == memtest.RoleDest {
			dstView = r.Block.Bytes()
		}
	}
	rep.flipAt = 3 // right after the pass-1 copy chunk completed
	rep.flip = func() {
		binary.LittleEndian.PutUint32(dstView[0:], ramAddressWord(7, 2, dstOff/4)) // wrong generation, right position
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamCopy(ctx, rep)
	if err == nil {
		t.Fatal("stale-generation word in the destination went undetected")
	}
	if it != 0 {
		t.Errorf("completed passes = %d, want 0", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-copy-v1" || g.Seed != 7 || g.Pass != 1 || g.Phase != "verify-dest" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if !strings.Contains(g.Region, "role=destination") {
		t.Errorf("finding region = %q, want the destination's own identity", g.Region)
	}
	if g.Offset != 0 {
		t.Errorf("finding offset = %d, want 0", g.Offset)
	}
	want := ramAddressWord(7, 1, dstOff/4)
	if g.Expected != uint64(want) || g.Actual != uint64(ramAddressWord(7, 2, dstOff/4)) {
		t.Errorf("finding values = %08x/%08x, want gen1 %08x / injected gen2 %08x",
			g.Expected, g.Actual, want, ramAddressWord(7, 2, dstOff/4))
	}
}

// Block swap: after both destination chunks are copied, swap them.
// Every word then reads its neighbour block's per-position image -
// the block ID is the destination index, and the verify refuses the
// swap loudly instead of accepting "the right pattern, wrong block".
func TestRamCopyBlockSwapFault(t *testing.T) {
	rep, _ := pairedReporter(t, 128<<10)
	var dstView []byte
	for _, r := range rep.regions {
		if r.Grant.Role == memtest.RoleDest {
			dstView = r.Block.Bytes()
		}
	}
	rep.flipAt = 6 // pass 1: fill 1,2 | verify-src 3,4 | copy 5,6 - after both copies
	rep.flip = func() {
		tmp := make([]byte, 64<<10)
		copy(tmp, dstView[:64<<10])
		copy(dstView[:64<<10], dstView[64<<10:128<<10])
		copy(dstView[64<<10:128<<10], tmp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamCopy(ctx, rep)
	if err == nil {
		t.Fatal("swapped destination blocks went undetected")
	}
	if it != 0 {
		t.Errorf("completed passes = %d, want 0", it)
	}
	// The first swapped chunk is fully wrong: verify counts every
	// word in it (16384) before aborting the sweep.
	if rep.ramFound < 1000 {
		t.Errorf("found = %d, want the whole swapped chunk counted (>=1000)", rep.ramFound)
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-copy-v1" || g.Pass != 1 || g.Phase != "verify-dest" || g.Offset != 0 {
		t.Errorf("first finding wrong: %+v", g)
	}
}

// The source is verified BEFORE the transfer: corrupt the source
// right after its fill and the fault must name verify-src - and the
// destination must never have been touched (it still holds the
// previous pass's image).
func TestRamCopySrcFaultBeforeTransfer(t *testing.T) {
	rep, dstOff := pairedReporter(t, 128<<10)
	var srcView, dstView []byte
	for _, r := range rep.regions {
		if r.Grant.Role == memtest.RoleSource {
			srcView = r.Block.Bytes()
		} else {
			dstView = r.Block.Bytes()
		}
	}
	// 8 beats per pass (128K pair = 2 chunks per phase):
	// pass 1: fill 1,2 | verify-src 3,4 | copy 5,6 | verify-dst 7,8
	// pass 2: fill 9,10 | verify-src 11,12 | copy 13,14 | verify-dst 15,16
	rep.flipAt = 9 // right after pass 2's first source-fill chunk
	rep.flip = func() { srcView[4] ^= 0x01 }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamCopy(ctx, rep)
	if err == nil {
		t.Fatal("corrupted source was transferred anyway (or went unseen)")
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (pass 1 clean, pass 2 aborted at the source)", it)
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-copy-v1" || g.Pass != 2 || g.Phase != "verify-src" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if !strings.Contains(g.Region, "role=source") {
		t.Errorf("finding region = %q, want the source's own identity", g.Region)
	}
	// The transfer never ran: the destination still holds pass 1's
	// image everywhere - source damage never overwrote it.
	for i := 0; i+4 <= len(dstView); i += 4 {
		want := copyImage(7, 1, dstOff/4, uint64(i))
		if got := binary.LittleEndian.Uint32(dstView[i:]); got != want {
			t.Fatalf("destination byte %d = %08x, want untouched pass-1 image %08x", i, got, want)
		}
	}
}

// Graceful stop mid-copy (128K pair, cancel after pass 2's first
// copy beat): the copied destination prefix must be verified in
// full, the untouched suffix legitimately holds pass 1's generation
// and must NOT be flagged, and the partial pass earns nothing.
func TestRamCopyGracefulMidCopy(t *testing.T) {
	rep, dstOff := pairedReporter(t, 128<<10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 13 // pass 2: copy chunk 1 done (fill 9,10 | verify-src 11,12 | copy 13) -> cancel

	it, err := runRamCopy(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop mid-copy is not a fault: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1 (the interrupted pass must not count)", it)
	}
	if rep.ramPasses != 1 {
		t.Errorf("RamPass credited %d passes, want 1", rep.ramPasses)
	}
	var srcView, dstView []byte
	for _, r := range rep.regions {
		if r.Grant.Role == memtest.RoleSource {
			srcView = r.Block.Bytes()
		} else {
			dstView = r.Block.Bytes()
		}
	}
	for i := 0; i+4 <= len(srcView); i += 4 { // source fully refilled this pass
		want := copyImage(7, 2, dstOff/4, uint64(i))
		if got := binary.LittleEndian.Uint32(srcView[i:]); got != want {
			t.Fatalf("source byte %d = %08x, want pass-2 image %08x", i, got, want)
		}
	}
	for i := 0; i+4 <= len(dstView); i += 4 {
		pass := uint64(2)
		if i >= 64<<10 {
			pass = 1 // stale generation territory: honest, unflagged, uncredited
		}
		want := copyImage(7, pass, dstOff/4, uint64(i))
		if got := binary.LittleEndian.Uint32(dstView[i:]); got != want {
			t.Fatalf("destination byte %d = %08x, want pass-%d image %08x", i, got, pass, want)
		}
	}
}

// Graceful stop mid-source-fill: only the filled source prefix is
// verified (against the image), the copy never runs, and the
// destination keeps the previous generation untouched.
func TestRamCopyGracefulMidFill(t *testing.T) {
	rep, dstOff := pairedReporter(t, 128<<10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 9 // pass 2: fill chunk 1 done -> cancel

	it, err := runRamCopy(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful stop mid-fill is not a fault: %v", err)
	}
	if it != 1 || rep.ramPasses != 1 {
		t.Errorf("partial pass credited: iters=%d passes=%d, want 1/1", it, rep.ramPasses)
	}
	var srcView, dstView []byte
	for _, r := range rep.regions {
		if r.Grant.Role == memtest.RoleSource {
			srcView = r.Block.Bytes()
		} else {
			dstView = r.Block.Bytes()
		}
	}
	for i := 0; i+4 <= len(srcView); i += 4 {
		pass := uint64(2)
		if i >= 64<<10 {
			pass = 1 // never refilled this pass
		}
		want := copyImage(7, pass, dstOff/4, uint64(i))
		if got := binary.LittleEndian.Uint32(srcView[i:]); got != want {
			t.Fatalf("source byte %d = %08x, want pass-%d image %08x", i, got, pass, want)
		}
	}
	for i := 0; i+4 <= len(dstView); i += 4 {
		want := copyImage(7, 1, dstOff/4, uint64(i)) // copy never ran
		if got := binary.LittleEndian.Uint32(dstView[i:]); got != want {
			t.Fatalf("destination byte %d = %08x, want untouched pass-1 image %08x", i, got, want)
		}
	}
}

func TestRamCopyUpfrontCancel(t *testing.T) {
	rep, _ := pairedReporter(t, 64<<10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamCopy(ctx, rep)
	if err != nil {
		t.Fatalf("upfront cancel: %v", err)
	}
	if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
			it, rep.adds, rep.ramPasses, rep.verified)
	}
}

func TestRamCopyRefusesWithoutEngine(t *testing.T) {
	rep := &regionReporter{}
	_, err := runRamCopy(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
		t.Errorf("err = %v, want the no-engine refusal", err)
	}
}

func TestRamCopyRegistryEntry(t *testing.T) {
	w, ok := workloads["ram-copy"]
	if !ok {
		t.Fatal("ram-copy missing from the workload registry")
	}
	if w.KernelVersion != "ram-copy-v1" || !w.RequiresMemory || w.FaultClass != FaultMemory {
		t.Errorf("registry entry wrong: %+v", w)
	}
	if got := KernelVersionOf("ram-copy"); got != "ram-copy-v1" {
		t.Errorf("KernelVersionOf = %q", got)
	}
	if strings.TrimSpace(w.Description) == "" {
		t.Error("registry description empty")
	}
}
