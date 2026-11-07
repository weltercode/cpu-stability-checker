//go:build windows

package memtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// The Windows structs must match the platform ABI exactly: a Go-side
// layout drift would make VirtualQuery/GetSystemInfo write through a
// shape we no longer match.
func TestWindowsStructABIs(t *testing.T) {
	if unsafe.Sizeof(systemInfo{}) != 48 {
		t.Errorf("systemInfo size = %d, want 48 (SYSTEM_INFO on amd64)", unsafe.Sizeof(systemInfo{}))
	}
	if unsafe.Sizeof(memoryBasicInfo{}) != 48 {
		t.Errorf("memoryBasicInfo size = %d, want 48 (MEMORY_BASIC_INFORMATION on amd64)", unsafe.Sizeof(memoryBasicInfo{}))
	}
	if off := unsafe.Offsetof(memoryBasicInfo{}.RegionSize); off != 24 {
		t.Errorf("RegionSize offset = %d, want 24", off)
	}
	if off := unsafe.Offsetof(systemInfo{}.dwAllocationGranularity); off != 40 {
		t.Errorf("dwAllocationGranularity offset = %d, want 40", off)
	}
}

// A committed block must be real memory: touch the first and last
// requested byte and read them back. If MEM_COMMIT lied, this is
// where it shows up - as a crash the test suite never survives.
func TestCommitTouchRelease(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	const size = 100000 // deliberately not a granularity multiple
	b, err := a.Commit(size)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if b.Addr == 0 {
		t.Fatal("Commit returned a null address")
	}
	if b.Requested != size {
		t.Errorf("Requested = %d, want %d", b.Requested, size)
	}
	if b.Allocated < size {
		t.Errorf("Allocated %d < Requested %d: accounting cannot report less than was asked", b.Allocated, size)
	}
	// Measured on this build (26300): VirtualAlloc rounds to page
	// granularity (100000 -> 102400), NOT the 64 KiB GetSystemInfo
	// advertises - so the honest assertion is page-multiple, and the
	// number came from VirtualQuery, not from a constant.
	if b.Allocated%4096 != 0 {
		t.Errorf("Allocated %d is not a multiple of the page size", b.Allocated)
	}

	view := b.Bytes()
	if len(view) != size {
		t.Fatalf("Bytes() view length = %d, want %d", len(view), size)
	}
	view[0] = 0x5A
	view[size-1] = 0xA5
	if view[0] != 0x5A || view[size-1] != 0xA5 {
		t.Error("committed memory did not hold written values")
	}

	if err := a.Release(b); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close after Release: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
}

// Zero and address-space-exceeding sizes are refused before any
// syscall: a wrapped uintptr on a 32-bit build would commit a
// different amount than requested, and the plan forbids silent
// size games.
func TestCommitRejectsInvalidSizes(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Commit(0); err == nil {
		t.Error("Commit(0) accepted")
	}
	if _, err := a.Commit(^uint64(0)); err == nil {
		t.Error("Commit(MaxUint64) accepted")
	}
}

// A request the machine cannot honor must come back as an error
// (wrapping ErrNoCommit for the documented commit-exhaustion codes),
// never as a smaller block and never as a crash. The allocator stays
// usable afterwards.
func TestCommitHugeFailsHonestly(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Commit(1 << 62)
	if err == nil {
		t.Fatal("a 4 EiB commit succeeded on this machine - the test machine is a lie")
	}
	if !errors.Is(err, ErrNoCommit) {
		t.Logf("huge commit error (not ErrNoCommit, still an honest refusal): %v", err)
	}
	b, err := a.Commit(65536)
	if err != nil {
		t.Errorf("allocator unusable after a failed huge commit: %v", err)
	} else if err := a.Release(b); err != nil {
		t.Errorf("Release after recovery: %v", err)
	}
}

// Releasing a block this allocator never handed out is an error, not
// undefined behavior: ownership is tracked.
func TestReleaseForeignBlock(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Release(Block{Addr: 0x1000, Requested: 4096}); err == nil {
		t.Error("Release of a foreign block was accepted")
	}
}

// Close after partial failures releases everything still owned:
// commit three blocks, release one, Close must clean up the rest
// (verified by the empty ownership map and a clean second Close).
func TestCloseAfterPartialFailure(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	var blocks []Block
	for i := 0; i < 3; i++ {
		b, err := a.Commit(65536 * uint64(i+1))
		if err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
		blocks = append(blocks, b)
	}
	if err := a.Release(blocks[1]); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w, ok := a.(*winAllocator); ok && len(w.blocks) != 0 {
		t.Errorf("Close left %d blocks owned", len(w.blocks))
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Engine.Open against the REAL allocator: small grants become real
// committed regions (touched to prove the commit), and Close
// releases every one of them.
func TestOpenCloseRealAllocator(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	res, err := Partition(Budget{Bytes: 4 << 20}, Layout{Workers: 2, Roles: []Role{RoleSource, RoleDest}},
		TrialParams{MaxPct: 70, MinOSReserve: 4 << 30, CommitReserve: 2 << 30, PerWorkerAllowance: 0}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(res.Grants, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(e.Regions()) != 4 {
		t.Fatalf("regions = %d, want 4", len(e.Regions()))
	}
	for _, r := range e.Regions() {
		v := r.Block.Bytes()
		v[0] = 0x2A
		v[len(v)-1] = 0x17
		if v[0] != 0x2A || v[len(v)-1] != 0x17 {
			t.Errorf("region worker=%d region=%d did not hold written values", r.Worker, r.Region)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Prepare on real committed regions: every requested byte is touched
// exactly once, the pattern is actually in memory, and the elapsed
// time is accounted separately from any measured run.
func TestPrepareRealRegions(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open([]Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Requested: 256 << 10},
		{Worker: 0, Region: 1, Role: RoleDest, Requested: 128 << 10},
	}, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	rep, err := e.Prepare(context.Background(), opaqueFill())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if rep.Touched != (256<<10 + 128<<10) {
		t.Errorf("touched = %d, want %d", rep.Touched, 256<<10+128<<10)
	}
	// No Elapsed>0 assertion here: measured on this machine the
	// monotonic clock advances in ~0.5 ms steps, and a 384 KiB pass
	// over already-resident pages legitimately fits inside one step
	// (it measured 0s both inside Prepare and around the call). The
	// non-zero accounting assertion lives in TestPrepareCancelMidPass,
	// where hundreds of MiB cannot fit one step.
	for _, r := range e.Regions() {
		b := r.Block.Bytes()
		if byteAt(b, 0) != wantFill() || byteAt(b, len(b)-1) != wantFill() {
			t.Errorf("region worker=%d region=%d was not fully touched", r.Worker, r.Region)
		}
	}
}

// lastFill records the opaque fill actually used, so the pattern
// check compares against the runtime value rather than a constant
// the compiler could forward through the stores.
var lastFill byte

// opaqueFill returns a byte the compiler cannot know at build time.
// Without opacity, the compiler can (and on this toolchain did)
// forward the stored constant through the test's own verification
// read, delete the load, and then delete the stores as dead - the
// pattern check would pass vacuously while no memory was touched.
func opaqueFill() byte {
	lastFill = byte(os.Getpid() & 0xFF)
	return lastFill
}

// wantFill reads the expected fill through a call boundary: without
// it the compiler ties the stored value and the compared value to
// the same GetPid call, folds the pattern check to true, deletes the
// load, and then deletes the stores - the check would pass vacuously
// while no memory was touched (measured on this toolchain).
//
//go:noinline
func wantFill() byte { return lastFill }

// byteAt reads one byte through a call boundary the optimizer cannot
// fold into the store that produced it.
//
//go:noinline
func byteAt(b []byte, i int) byte { return b[i] }

// A context that is already done stops Prepare before it touches
// anything: the pass is cancellable, and a cancelled pass is not a
// partial success.
func TestPrepareCancelledUpfront(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open([]Grant{{Worker: 0, Region: 0, Role: RoleSource, Requested: 1 << 20}}, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := e.Prepare(ctx, 0xA5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.Touched != 0 {
		t.Errorf("touched = %d after an upfront cancel, want 0", rep.Touched)
	}
}

// A cancel landing mid-pass stops the pass: the error is
// context.Canceled and the touched count is the honest partial -
// strictly less than the total, never the total.
func TestPrepareCancelMidPass(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	const total = 256 << 20 // 256 MiB: touching takes far longer than the cancel delay
	e, err := Open([]Grant{{Worker: 0, Region: 0, Role: RoleSource, Requested: total}}, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	rep, err := e.Prepare(ctx, 0xA5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.Touched >= total {
		t.Errorf("touched %d covers the whole region despite the cancel", rep.Touched)
	}
	if rep.Elapsed <= 0 {
		t.Errorf("first-touch elapsed = %v: a pass this large cannot fit one clock step, so a zero duration would mean the time was not accounted", rep.Elapsed)
	}
	t.Logf("mid-pass cancel touched %d of %d bytes in %v", rep.Touched, total, rep.Elapsed)
}

// try and required on real small regions: locking 128 KiB is within
// any sane working-set quota, so both must report success with the
// limits known - and the report must not claim large pages.
func TestLockRealSmallRegionsSucceed(t *testing.T) {
	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open([]Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Requested: 64 << 10},
		{Worker: 0, Region: 1, Role: RoleDest, Requested: 64 << 10},
	}, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer e.Close()

	rep, err := e.Lock(LockTry)
	if err != nil {
		t.Fatalf("try: %v", err)
	}
	if rep.FailedRegions != 0 || rep.Failure != "" {
		t.Errorf("try reported failures on lockable regions: %+v", rep)
	}
	if rep.LockedBytes == 0 {
		t.Errorf("try locked nothing: %+v", rep)
	}
	if !rep.WorkingSetKnown || rep.WorkingSetMax == 0 {
		t.Errorf("working-set limits not reported: %+v", rep)
	}
	if rep.LargePages != LargePagesP05 {
		t.Errorf("LargePages = %q, want %q", rep.LargePages, LargePagesP05)
	}
	if _, err := e.Lock(LockRequired); err != nil {
		t.Errorf("required on lockable regions: %v", err)
	}
}

// The first-touch stores must exist in the machine code, not just in
// the source: measured on this toolchain, the optimizer deleted a
// fill loop it could prove nobody reads - and folded the test's own
// "verify the pattern" load into a constant true, making the check
// pass vacuously. This test builds the package's test binary and
// objdumps touchChunk, requiring real stores to memory. (objdump on
// the go-test temp binary is not an option - no pclntab; the -c
// build is, measured.)
func TestTouchChunkMachineCode(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not found next to the test: %v", err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package dir")
		}
		dir = parent
	}
	probe := filepath.Join(t.TempDir(), "memtest_probe.test.exe")
	t.Logf("toolchain: %s %s/%s; building test binary probe in %s", runtime.Version(), runtime.GOOS, runtime.GOARCH, dir)
	// The build target is the explicit import path, not ".": a compiled
	// test binary's cwd is wherever the harness launched it (the 'go
	// test' driver happens to run inside the package dir), and "."
	// would silently build whatever package that cwd holds - the go
	// tool exits 0 with "no test files" and the probe never exists.
	cmd := exec.Command(goTool, "test", "-c", "-o", probe, "github.com/weltercode/cpu-stability-checker/internal/memtest")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v\n%s", err, out)
	}
	out, err := exec.Command(goTool, "tool", "objdump", "-s", "touchChunk", probe).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go tool objdump: %v, stderr: %s", err, ee.Stderr)
		}
		t.Fatalf("go tool objdump: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "touchChunk") {
		t.Fatalf("objdump found no touchChunk text - the barrier did not survive:\n%s", text[:min(2000, len(text))])
	}
	// A store of an immediate (or register) into a memory operand:
	// MOVB $0xa5, 0(R8)(R12*1) or vector equivalents. Stack spills
	// alone would not satisfy this: the fill value must reach the
	// buffer through the slice's base+index addressing.
	storeImm := regexp.MustCompile(`(?i)\bMOV(B|W|L|Q|DQU|UPS|DQA)[A-Z0-9]*\s+\$0x[0-9a-f]+,\s*[^,]*\(`)
	storeReg := regexp.MustCompile(`(?i)\bMOV(B|W|L|Q|DQU|UPS|DQA)[A-Z0-9]*\s+[\w$]+,\s*[^,]*\([^,]*\*\d+\)`)
	stores := 0
	for _, line := range strings.Split(text, "\n") {
		if storeImm.MatchString(line) || storeReg.MatchString(line) {
			stores++
		}
	}
	if stores == 0 {
		t.Errorf("no buffer stores in the emitted touchChunk: the fill was optimized away:\n%s", text[:min(3000, len(text))])
	}
	t.Logf("machine code check: %d store lines in touchChunk", stores)
}
