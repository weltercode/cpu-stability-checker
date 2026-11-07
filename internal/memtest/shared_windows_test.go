//go:build windows

package memtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// sharedPattern is a deterministic byte stream both processes can
// re-derive without sharing any code state: tag picks the stream,
// position mixes in.
func sharedPattern(tag byte, n uint64) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = tag ^ byte(uint64(i)*131+uint64(i>>8)*7)
	}
	return p
}

const sharedChildEnv = "MEMTEST_SHARED_CHILD"

// TestSharedMappingChild is the child half of TestSharedMappingSameBytes.
// It only runs when the parent re-executed this test binary with the
// inherited handle in the environment; standalone runs skip it.
//
// Exit codes are distinct so a failure names the step: 3 open,
// 4 parent bytes not visible, 5 view, 6 unmap.
func TestSharedMappingChild(t *testing.T) {
	spec := os.Getenv(sharedChildEnv)
	if spec == "" {
		t.Skip("shared-mapping child protocol: not invoked as a child")
	}
	parts := strings.Fields(spec)
	if len(parts) != 3 {
		t.Fatalf("child spec %q: want %s=<handle> <size> <span>", spec, sharedChildEnv)
	}
	h, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("child handle %q: %v", parts[0], err)
	}
	size, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("child size %q: %v", parts[1], err)
	}
	span, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		t.Fatalf("child span %q: %v", parts[2], err)
	}

	m, err := OpenSharedMapping(uintptr(h), size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: open: %v\n", err)
		os.Exit(3)
	}
	v, err := m.View(0, span)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: view: %v\n", err)
		os.Exit(5)
	}
	if !bytes.Equal(v, sharedPattern('A', span)) {
		fmt.Fprintln(os.Stderr, "child: parent-written bytes are NOT visible in the shared view")
		os.Exit(4)
	}
	w, err := m.View(span, span)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: view2: %v\n", err)
		os.Exit(5)
	}
	copy(w, sharedPattern('B', span))
	if err := m.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "child: close: %v\n", err)
		os.Exit(6)
	}
	fmt.Println("child: ok")
}

// TestSharedMappingSameBytes is the P07e foundation proof: two real
// processes, one pagefile-backed section, the same physical bytes in
// both directions. The child sees what the parent wrote before the
// spawn, and the parent sees what the child wrote before it exited.
//
// Process exit is the publication point here: teardown unmaps the
// child's view and every store is then visible. A LIVE handoff needs
// a documented barrier (P07e2 adds it); this test deliberately does
// not pretend concurrency-free ordering proves anything about that.
func TestSharedMappingSameBytes(t *testing.T) {
	const size = 256 * 1024
	const span = 64 * 1024

	m, err := CreateSharedMapping(size)
	if err != nil {
		t.Fatalf("CreateSharedMapping: %v", err)
	}
	defer m.Close()

	v, err := m.View(0, span)
	if err != nil {
		t.Fatalf("parent view: %v", err)
	}
	copy(v, sharedPattern('A', span))

	// The child is this same test binary, re-executed with the
	// handle passed through AdditionalInheritedHandles (ExtraFiles
	// is not supported on Windows). The child is short-lived and the
	// parent waits for it, so no orphan can survive this test.
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedMappingChild$")
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%d %d %d", sharedChildEnv, m.Handle(), size, span))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(m.Handle())},
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed: %v\nstdout: %s\nstderr: %s", err, out.String(), errb.String())
	}

	w, err := m.View(span, span)
	if err != nil {
		t.Fatalf("parent view2: %v", err)
	}
	if !bytes.Equal(w, sharedPattern('B', span)) {
		t.Errorf("child-written bytes are NOT visible in the parent view")
	}
}

// PublicationBarrier must succeed on this platform (the call itself
// is the P07e publication primitive; a refusal here would make every
// handoff a setup error). The build quirk (FALSE with last-error
// SUCCESS on build 26300) is documented at the function.
func TestPublicationBarrierSmoke(t *testing.T) {
	if err := PublicationBarrier(); err != nil {
		t.Errorf("PublicationBarrier: %v", err)
	}
}

func TestSharedMappingRefusals(t *testing.T) {
	if _, err := CreateSharedMapping(0); err == nil {
		t.Error("CreateSharedMapping(0) was accepted")
	}
	if _, err := CreateSharedMapping(1 << 62); err == nil {
		t.Error("CreateSharedMapping(4 EiB) was accepted")
	}

	m, err := CreateSharedMapping(64 * 1024)
	if err != nil {
		t.Fatalf("CreateSharedMapping: %v", err)
	}
	defer m.Close()

	// The bogus-handle refusal deliberately does NOT live here: on a
	// verified Windows environment MapViewOfFile answered the garbage
	// handle 0x1234 by RAISING STATUS_INVALID_HANDLE (0xc0000008) and
	// killed the whole test process (audit A02), while another build
	// returns NULL with ERROR_INVALID_HANDLE. Everything in this test
	// otherwise uses ordinary parent-created handles with documented
	// clean answers; the one unpredictable answer runs as data in a
	// probe child - see TestSharedMappingBogusHandleProbe.
	if _, err := OpenSharedMapping(m.Handle(), 128*1024); err == nil {
		t.Error("a view larger than the section was accepted")
	}

	// Allocator bounds and aliasing refusals happen up front, before
	// any Commit: a grant past the mapping end or an overlapping pair
	// must never reach a kernel write.
	if _, err := m.Allocator([]Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Offset: 60 * 1024, Requested: 8 * 1024},
	}); err == nil {
		t.Error("grant past the mapping end was accepted")
	}
	if _, err := m.Allocator([]Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 4096},
		{Worker: 1, Region: 0, Role: RoleDest, Offset: 2048, Requested: 4096},
	}); err == nil {
		t.Error("overlapping shared grants were accepted")
	}
	if _, err := m.Allocator([]Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 0},
	}); err == nil {
		t.Error("zero-size shared grant was accepted")
	}
}

func TestSharedAllocatorEngine(t *testing.T) {
	const size = 128 * 1024
	m, err := CreateSharedMapping(size)
	if err != nil {
		t.Fatalf("CreateSharedMapping: %v", err)
	}
	defer m.Close()

	grants := []Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 64 * 1024},
		{Worker: 1, Region: 0, Role: RoleDest, Offset: 64 * 1024, Requested: 64 * 1024},
	}
	a, err := m.Allocator(grants)
	if err != nil {
		t.Fatalf("Allocator: %v", err)
	}
	e, err := Open(grants, a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Blocks land exactly at the grant offsets inside the view: the
	// same Grant set names the same physical bytes everywhere.
	for i, g := range grants {
		want := m.base + uintptr(g.Offset)
		if got := e.Regions()[i].Block.Addr; got != want {
			t.Errorf("region %d addr = %#x, want %#x (base+offset)", i, got, want)
		}
	}
	if req, alloc := e.Totals(); req != size || alloc != size {
		t.Errorf("Totals = %d/%d, want %d/%d", req, alloc, size, size)
	}
	prep, err := e.Prepare(context.Background(), 0x5C)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.Touched != size {
		t.Errorf("Prepare touched %d bytes, want %d", prep.Touched, size)
	}
	// Write through one region, read the same bytes through the
	// mapping view: one section, one set of pages.
	src := e.Regions()[0].Block.Bytes()
	view, err := m.View(0, 64*1024)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if !bytes.Equal(src, view) {
		t.Errorf("region bytes differ from the mapping view at the same offset")
	}
	if err := e.Close(); err != nil {
		t.Errorf("engine Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v (must be safe)", err)
	}
	// The engine closed the allocator, which closed the mapping; the
	// deferred m.Close() is now a no-op by construction.

	// Commit/grant order is positional and mismatches are loud.
	m2, err := CreateSharedMapping(size)
	if err != nil {
		t.Fatalf("CreateSharedMapping(2): %v", err)
	}
	defer m2.Close()
	a2, err := m2.Allocator(grants)
	if err != nil {
		t.Fatalf("Allocator(2): %v", err)
	}
	bad := []Grant{
		{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 32 * 1024}, // != 64 KiB
		{Worker: 1, Region: 0, Role: RoleDest, Offset: 96 * 1024, Requested: 32 * 1024},
	}
	if _, err := Open(bad, a2); err == nil {
		t.Error("Commit/grant size mismatch was accepted")
	} else if !strings.Contains(err.Error(), "does not match grant") {
		t.Errorf("mismatch error = %v, want the loud grant-mismatch refusal", err)
	}
	// Open's failure path closed a2 (and m2); a second Close is safe.
	if err := a2.Close(); err != nil {
		t.Errorf("allocator Close after failed Open: %v", err)
	}

	// More Commits than grants is a refusal, never an anonymous extra.
	m3, err := CreateSharedMapping(4096)
	if err != nil {
		t.Fatalf("CreateSharedMapping(3): %v", err)
	}
	defer m3.Close()
	a3, err := m3.Allocator([]Grant{{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 4096}})
	if err != nil {
		t.Fatalf("Allocator(3): %v", err)
	}
	if _, err := a3.Commit(4096); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if _, err := a3.Commit(4096); err == nil {
		t.Error("Commit beyond the grant list was accepted")
	}
	if err := a3.Release(Block{Addr: m3.base + 8192, Requested: 16}); err == nil {
		t.Error("Release of a foreign block was accepted")
	}
	if err := a3.Close(); err != nil {
		t.Errorf("a3 Close: %v", err)
	}
}

// sharedProbeEnv re-executes this test binary as a refusal-probe child.
const sharedProbeEnv = "MEMTEST_HANDLE_PROBE"

// TestSharedMappingRefusalProbe is the probe child for the one
// refusal whose ANSWER FORM is not portable (audit A02): the bogus
// handle 0x1234 is not a handle at all, and MapViewOfFile's answer
// varies by Windows build - a verified environment saw the kernel
// raise STATUS_INVALID_HANDLE (0xc0000008), an unhandled exception
// that killed the whole test process, while this machine returns NULL
// with ERROR_INVALID_HANDLE. Both answers are legitimate data; the
// suite process must never be the thing that finds out which form it
// gets on this particular machine.
//
// Child exit codes are distinct protocol facts, the same discipline as
// the shared-mapping child above:
// 7 = refused with a normal Go error (the documented clean path),
// 8 = the bogus handle was ACCEPTED (a real defect - always loud),
// 0xc0000008 = the exception form (the audit's observed build).
func TestSharedMappingRefusalProbe(t *testing.T) {
	if os.Getenv(sharedProbeEnv) != "bogus" {
		t.Skip("handle-probe child protocol: not invoked as a probe child")
	}
	if _, err := OpenSharedMapping(0x1234, 4096); err != nil {
		fmt.Fprintf(os.Stderr, "probe: bogus handle refused with a normal error: %v\n", err)
		os.Exit(7)
	}
	fmt.Fprintln(os.Stderr, "probe: bogus handle was ACCEPTED")
	os.Exit(8)
}

// TestSharedMappingBogusHandleProbe keeps the A02 contract end to end:
// the risky call runs in a probe child (re-exec of this binary, the
// same house pattern the P07e shared-mapping tests use), the parent
// reads the outcome as data, and under NO build behavior does a
// negative probe take the suite down with it.
func TestSharedMappingBogusHandleProbe(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedMappingRefusalProbe$")
	cmd.Env = append(os.Environ(), sharedProbeEnv+"=bogus")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		t.Fatalf("probe child exited cleanly - a probe child that does not probe is a protocol failure: stdout %q stderr %q",
			out.String(), errb.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("probe child never started: %v", err)
	}
	switch code := exit.ExitCode(); code {
	case 7:
		t.Logf("probe form on this build: clean Go-error refusal - %s", strings.TrimSpace(errb.String()))
	case 0xC0000008:
		// The exception form is a real Windows behavior, not a bug of
		// ours: recorded honestly, the suite stays alive, and this is
		// exactly what the audit's environment would report.
		t.Logf("probe form on this build: STATUS_INVALID_HANDLE exception (0xc0000008) - the audit's observed form")
	case 8:
		t.Fatalf("the bogus handle was ACCEPTED - a real refusal defect: stdout %q stderr %q", out.String(), errb.String())
	default:
		t.Fatalf("probe child ended unexpectedly: exit %#x stdout %q stderr %q", code, out.String(), errb.String())
	}
}
