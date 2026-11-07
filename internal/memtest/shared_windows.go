//go:build windows

package memtest

import (
	"fmt"
	"sort"
	"syscall"
)

var (
	procCreateFileMapping = modKernel32Alloc.NewProc("CreateFileMappingW")
	procMapViewOfFile     = modKernel32Alloc.NewProc("MapViewOfFile")
	procUnmapViewOfFile   = modKernel32Alloc.NewProc("UnmapViewOfFile")
	procSetHandleInfo     = modKernel32Alloc.NewProc("SetHandleInformation")
	procCloseHandle       = modKernel32Alloc.NewProc("CloseHandle")
	procFlushWriteBuffers = modKernel32Alloc.NewProc("FlushProcessWriteBuffers")
)

const (
	// FILE_MAP_ALL_ACCESS (winnt.h): the standard rights plus all
	// section map rights. We map read-write because handoff has both
	// writers and readers in different processes.
	fileMapAllAccess = 0x000F001F
	// HANDLE_FLAG_INHERIT (winbase.h).
	handleFlagInherit = 0x00000001
	// INVALID_HANDLE_VALUE as the file handle means "back this
	// section with the system page file": no named object, no
	// namespace entry, nothing that can collide or linger after the
	// run. Children receive the section by handle inheritance, never
	// by name.
	invalidHandleValue = ^uintptr(0)
	// pageGranularity: pagefile-backed sections commit at page
	// granularity. The 64 KiB allocation granularity only constrains
	// MapViewOfFile offsets, and we always map at offset 0, so
	// subviews are plain pointer arithmetic with no alignment rule.
	pageGranularity = 4096
)

// SharedMapping is one anonymous pagefile-backed section mapped whole
// into this process. It is the P07e foundation: the parent and its
// worker children see the SAME physical pages, so block ownership can
// move between worker processes without copying anything.
//
// Ownership rules, deliberately strict:
//   - the section object is created once, in the parent
//     (CreateSharedMapping);
//   - children receive the HANDLE by inheritance: the handle is
//     marked inheritable here, and the launcher must pass it in
//     syscall.SysProcAttr.AdditionalInheritedHandles. Cmd.ExtraFiles
//     is NOT supported on Windows and must never be used for this;
//   - every process maps exactly the declared size with
//     MapViewOfFile: the OS refuses a view larger than the section
//     (ERROR_VIEW_SIZE), so the bounds are enforced by the kernel,
//     not by trusting arithmetic over argv;
//   - the section lives until the LAST handle closes: a parent that
//     closes its handle first never pulls pages out from under a
//     child, and the Job Object kill-on-close guarantees children
//     never outlive the parent to keep pages committed on their own.
type SharedMapping struct {
	handle syscall.Handle
	base   uintptr
	size   uint64
}

// CreateSharedMapping creates the section in the calling (parent)
// process, maps it whole, and marks the handle inheritable so it can
// be handed to worker children. Every failure path releases whatever
// was partially created: no handle, no view, no leak.
func CreateSharedMapping(size uint64) (*SharedMapping, error) {
	if size == 0 {
		return nil, fmt.Errorf("memtest: shared mapping size must be > 0")
	}
	if size > maxInt {
		// Checked BEFORE the syscall: the view is addressed through
		// Go slice windows, and a size that does not fit the address
		// space must never reach the ABI as a wrapped uintptr.
		return nil, fmt.Errorf("memtest: shared mapping size %d exceeds the address space of this process", size)
	}
	h, _, errno := procCreateFileMapping.Call(
		invalidHandleValue, 0, pageReadWrite,
		uintptr(uint32(size>>32)), uintptr(uint32(size)), 0)
	if h == 0 {
		// Section creation can fail for commit exhaustion exactly
		// like VirtualAlloc: classify it the same way so callers map
		// it to a resource condition, never to a CPU fault.
		return nil, fmt.Errorf("memtest: CreateFileMapping(pagefile, %d bytes): %w (%w)",
			size, classifyCommit(errno), errno)
	}
	m := &SharedMapping{handle: syscall.Handle(h), size: size}
	base, _, errno := procMapViewOfFile.Call(h, fileMapAllAccess, 0, 0, uintptr(size))
	if base == 0 {
		procCloseHandle.Call(h)
		return nil, fmt.Errorf("memtest: MapViewOfFile(shared, %d bytes): %w", size, errno)
	}
	m.base = base
	if r, _, errno := procSetHandleInfo.Call(h, handleFlagInherit, handleFlagInherit); r == 0 {
		// Inheritance is the whole point of the handle: without the
		// flag AdditionalInheritedHandles passes nothing usable.
		m.Close()
		return nil, fmt.Errorf("memtest: SetHandleInformation(INHERIT) on shared mapping: %w", errno)
	}
	return m, nil
}

// OpenSharedMapping is the child side: it maps the inherited handle's
// section. A bogus or never-inherited handle fails with
// ERROR_INVALID_HANDLE - that is a wiring bug (the handle was not in
// AdditionalInheritedHandles), reported as an error, never a crash.
// Mapping exactly size bytes lets the kernel refuse (ERROR_VIEW_SIZE)
// when the parent's section is smaller than the declared size.
func OpenSharedMapping(handle uintptr, size uint64) (*SharedMapping, error) {
	if size == 0 {
		return nil, fmt.Errorf("memtest: shared mapping size must be > 0")
	}
	if size > maxInt {
		return nil, fmt.Errorf("memtest: shared mapping size %d exceeds the address space of this process", size)
	}
	base, _, errno := procMapViewOfFile.Call(handle, fileMapAllAccess, 0, 0, uintptr(size))
	if base == 0 {
		return nil, fmt.Errorf("memtest: MapViewOfFile(inherited handle %#x, %d bytes): %w "+
			"(the handle must be passed via AdditionalInheritedHandles)", handle, size, errno)
	}
	return &SharedMapping{handle: syscall.Handle(handle), base: base, size: size}, nil
}

// Handle returns this process's section handle as a plain uintptr so
// cross-platform call sites compile. On Windows it is the inheritable
// handle to place in SysProcAttr.AdditionalInheritedHandles. Valid
// until Close.
func (m *SharedMapping) Handle() uintptr { return uintptr(m.handle) }

// Size is the declared mapping size in bytes.
func (m *SharedMapping) Size() uint64 { return m.size }

// View returns the byte window [off, off+n) of the mapping. The data
// lives in the shared section (no Go pointers inside); the bounds are
// checked here. The noinline lesson from Block.Bytes applies
// unchanged: the window is produced by calling Block.Bytes on a
// synthetic block, so the call boundary keeps stores through the view
// conservatively ordered.
func (m *SharedMapping) View(off, n uint64) ([]byte, error) {
	end, ok := addChecked(off, n)
	if !ok || end > m.size {
		return nil, fmt.Errorf("memtest: view [%d,%d) is outside the shared mapping (%d bytes)", off, off+n, m.size)
	}
	b := Block{Addr: m.base + uintptr(off), Requested: n, Allocated: n}
	return b.Bytes(), nil
}

// Close unmaps the view and closes THIS process's handle to the
// section. Safe twice, safe after partial construction, and safe to
// call while another process still holds a handle (the section
// object survives until its last handle closes).
func (m *SharedMapping) Close() error {
	var firstErr error
	if m.base != 0 {
		if r, _, errno := procUnmapViewOfFile.Call(m.base); r == 0 && firstErr == nil {
			firstErr = fmt.Errorf("memtest: UnmapViewOfFile(%#x): %w", m.base, errno)
		}
		m.base = 0
	}
	if m.handle != 0 {
		if r, _, errno := procCloseHandle.Call(uintptr(m.handle)); r == 0 && firstErr == nil {
			firstErr = fmt.Errorf("memtest: CloseHandle(shared mapping): %w", errno)
		}
		m.handle = 0
	}
	return firstErr
}

// sharedAllocator hands out the grant ranges of one SharedMapping as
// Blocks. It is an ordinary Allocator - Engine.Open, Prepare, Totals
// and Close all work unchanged - with two deliberate differences:
//
//   - Commit is positional: Engine.Open iterates grants in order, so
//     the n-th Commit must carry the size of the n-th grant. A size
//     mismatch is a wiring bug and is refused loudly, never patched
//     over by allocating something else;
//   - Release and Close never VirtualFree anything: the view is not
//     the backing store, the section is. Individual regions are not
//     the unit of ownership - the mapping is, and closing it here
//     closes the handle this process holds.
//
// Blocks land at base+Grant.Offset, so the same Grant set names the
// same physical bytes in every process that maps the section - which
// is exactly what block handoff between workers needs.
type sharedAllocator struct {
	m      *SharedMapping
	grants []Grant
	next   int
}

// Allocator validates the grant set against THIS mapping (the child
// validated against the budget, but the mapping is the real bound)
// and re-checks disjointness: overlapping grants would alias two
// regions onto the same bytes, the one failure mode handoff must
// never have. Validation happens up front, before any Commit.
func (m *SharedMapping) Allocator(grants []Grant) (Allocator, error) {
	for _, g := range grants {
		if g.Requested == 0 {
			return nil, fmt.Errorf("memtest: shared grant worker=%d region=%d has zero size", g.Worker, g.Region)
		}
		end, ok := addChecked(g.Offset, g.Requested)
		if !ok || end > m.size {
			return nil, fmt.Errorf("memtest: shared grant worker=%d region=%d range [%d,%d) is outside the mapping (%d bytes)",
				g.Worker, g.Region, g.Offset, g.Offset+g.Requested, m.size)
		}
	}
	byOffset := make([]Grant, len(grants))
	copy(byOffset, grants)
	sort.Slice(byOffset, func(i, j int) bool { return byOffset[i].Offset < byOffset[j].Offset })
	for i := 1; i < len(byOffset); i++ {
		prev := byOffset[i-1]
		end, ok := addChecked(prev.Offset, prev.Requested)
		if !ok || end > byOffset[i].Offset {
			return nil, fmt.Errorf("memtest: shared grant ranges overlap at offset %d (worker=%d region=%d)",
				byOffset[i].Offset, byOffset[i].Worker, byOffset[i].Region)
		}
	}
	return &sharedAllocator{m: m, grants: append([]Grant(nil), grants...)}, nil
}

func (a *sharedAllocator) AllocationGranularity() uint64 { return pageGranularity }

func (a *sharedAllocator) Commit(size uint64) (Block, error) {
	if a.next >= len(a.grants) {
		return Block{}, fmt.Errorf("memtest: shared allocator received more Commit calls (%d) than grants (%d)",
			a.next+1, len(a.grants))
	}
	g := a.grants[a.next]
	if size != g.Requested {
		return Block{}, fmt.Errorf("memtest: shared Commit(%d) does not match grant worker=%d region=%d size %d "+
			"(Commit order must follow grant order)", size, g.Worker, g.Region, g.Requested)
	}
	a.next++
	// Allocated == Requested exactly: the view is byte-exact with no
	// OS rounding. The physical pages commit on first touch, which
	// Engine.Prepare performs and accounts honestly.
	return Block{Addr: a.m.base + uintptr(g.Offset), Requested: g.Requested, Allocated: g.Requested}, nil
}

// Release accepts only blocks this allocator handed out and frees
// nothing: the section is the unit of ownership, not the region.
func (a *sharedAllocator) Release(b Block) error {
	if b.Addr < a.m.base {
		return fmt.Errorf("memtest: Release(%#x): block was not handed out by this allocator", b.Addr)
	}
	rel := uint64(b.Addr - a.m.base)
	end, ok := addChecked(rel, b.Requested)
	if !ok || end > a.m.size {
		return fmt.Errorf("memtest: Release(%#x): block is not inside the shared mapping", b.Addr)
	}
	for _, g := range a.grants {
		gEnd, ok := addChecked(g.Offset, g.Requested)
		if ok && rel >= g.Offset && end <= gEnd {
			return nil
		}
	}
	return fmt.Errorf("memtest: Release(%#x): block was not handed out by this allocator", b.Addr)
}

// Close releases this process's view and handle. Safe twice and safe
// after partial Open failures (Engine.Open closes the allocator).
func (a *sharedAllocator) Close() error { return a.m.Close() }

// PublicationBarrier is the documented Windows mechanism that makes
// this thread's memory writes visible to every processor in the
// system (FlushProcessWriteBuffers: it sends an IPI to every thread
// and flushes write buffers, the same primitive Windows itself uses
// to implement memory-model guarantees).
//
// It is the P07e publication rule, stated once where the memory
// layer can enforce it: a writer calls it BEFORE acking WRITTEN, a
// reader calls it AFTER receiving LEASE_READ and before its first
// load of the block. The pipe round-trip orders the messages; this
// call is what orders the DATA, on a mechanism Microsoft documents -
// not on an assumption about x86 or about syscalls being fences.
//
// Measured on Windows 11 build 26300: the call returns FALSE even
// when it succeeds, with last-error left at SUCCESS (the documented
// "TRUE on success" is not what this build does; the runtime itself
// ignores the return value). The honest success signal is therefore
// "no error code was set": a real failure surfaces as a non-SUCCESS
// last error, and only that is reported as one. The BOOL is not
// trusted either way.
func PublicationBarrier() error {
	_, _, errno := procFlushWriteBuffers.Call()
	en, isErrno := errno.(syscall.Errno)
	if isErrno && en != 0 {
		return fmt.Errorf("memtest: FlushProcessWriteBuffers: %w", errno)
	}
	if !isErrno && errno != nil {
		return fmt.Errorf("memtest: FlushProcessWriteBuffers: %w", errno)
	}
	return nil
}
