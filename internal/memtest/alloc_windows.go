//go:build windows

package memtest

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modKernel32Alloc  = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc  = modKernel32Alloc.NewProc("VirtualAlloc")
	procVirtualFree   = modKernel32Alloc.NewProc("VirtualFree")
	procVirtualQuery  = modKernel32Alloc.NewProc("VirtualQuery")
	procGetSystemInfo = modKernel32Alloc.NewProc("GetSystemInfo")
)

const (
	memCommit     = 0x00001000
	memReserve    = 0x00002000
	memRelease    = 0x00004000
	pageReadWrite = 0x04
)

// winAllocator wraps VirtualAlloc/VirtualFree with ownership
// tracking: every committed block is remembered, so Close can release
// everything after a partial failure, and releasing a block this
// allocator never handed out is an error, not undefined behavior.
type winAllocator struct {
	granularity uint64
	blocks      map[uintptr]uint64 // addr -> allocated bytes
}

func newPlatformAllocator() (Allocator, error) {
	var si systemInfo
	procGetSystemInfo.Call(uintptr(unsafe.Pointer(&si)))
	g := uint64(si.dwAllocationGranularity)
	if g == 0 {
		// Documented Windows behavior is 64 KiB; if the query ever
		// returns zero, fall back to the documented value rather
		// than dividing by zero later.
		g = 65536
	}
	return &winAllocator{granularity: g, blocks: make(map[uintptr]uint64)}, nil
}

// systemInfo mirrors the Windows SYSTEM_INFO structure on amd64
// (48 bytes; the size test pins it). Go's own field alignment matches
// the Windows layout here: the pointers land at offsets 8/16/24
// without explicit padding. Only the fields through
// dwAllocationGranularity are read.
type systemInfo struct {
	dwOemID                     uint32
	dwPageSize                  uint32
	lpMinimumApplicationAddress uintptr
	lpMaximumApplicationAddress uintptr
	dwActiveProcessorMask       uintptr
	dwNumberOfProcessors        uint32
	dwProcessorType             uint32
	dwAllocationGranularity     uint32
	wProcessorLevel             uint16
	wProcessorRevision          uint16
}

// memoryBasicInfo mirrors MEMORY_BASIC_INFORMATION (SIZE_T fields are
// uintptr: pointer-sized by the platform ABI, no silent 32-bit break).
type memoryBasicInfo struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	_                 uint32 // alignment padding for RegionSize
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
	_                 uint32 // trailing alignment
}

func (a *winAllocator) AllocationGranularity() uint64 { return a.granularity }

func (a *winAllocator) Commit(size uint64) (Block, error) {
	if size == 0 {
		return Block{}, fmt.Errorf("memtest: commit size must be > 0")
	}
	if size > uint64(^uintptr(0)) {
		// Checked BEFORE the syscall: on 32-bit builds the size does
		// not even fit the ABI, and wrapping it into uintptr would
		// commit a different (smaller) amount than requested.
		return Block{}, fmt.Errorf("memtest: commit size %d exceeds the address space of this process", size)
	}
	addr, _, errno := procVirtualAlloc.Call(0, uintptr(size), memReserve|memCommit, pageReadWrite)
	if addr == 0 {
		return Block{}, fmt.Errorf("memtest: VirtualAlloc(%d bytes): %w (%w)",
			size, classifyCommit(errno), errno)
	}
	// The real committed size comes from the region itself: the
	// accounting must report what the OS committed, not what we
	// asked for. Rounding is finer than GetSystemInfo advertises
	// (measured: page granularity on build 26300), so no constant
	// may stand in for the query. If VirtualQuery ever fails, the
	// honest fallback is the request itself as a lower bound.
	allocated := size
	if mbi, ok := queryRegion(addr); ok {
		allocated = uint64(mbi.RegionSize)
	}
	a.blocks[addr] = allocated
	return Block{Addr: uintptr(addr), Requested: size, Allocated: allocated}, nil
}

func (a *winAllocator) Release(b Block) error {
	allocated, owned := a.blocks[b.Addr]
	if !owned {
		return fmt.Errorf("memtest: Release(%#x): block was not committed by this allocator", b.Addr)
	}
	r, _, errno := procVirtualFree.Call(b.Addr, 0, memRelease)
	if r == 0 {
		// Still owned: the block remains committed and Close will
		// try again - a failed release never silently forgets it.
		return fmt.Errorf("memtest: VirtualFree(%#x, %d bytes): %w", b.Addr, allocated, errno)
	}
	delete(a.blocks, b.Addr)
	return nil
}

// Close releases every block still owned. Safe on an empty allocator,
// safe to call twice, and safe after partial allocation failures:
// whatever got committed gets released. Deleting while ranging over
// the map is defined Go behavior.
func (a *winAllocator) Close() error {
	var firstErr error
	for addr, allocated := range a.blocks {
		r, _, errno := procVirtualFree.Call(addr, 0, memRelease)
		if r == 0 && firstErr == nil {
			firstErr = fmt.Errorf("memtest: VirtualFree(%#x, %d bytes) during Close: %w", addr, allocated, errno)
		}
		delete(a.blocks, addr)
	}
	return firstErr
}

// classifyCommit maps the documented "the machine cannot give you
// more commit" Win32 codes onto ErrNoCommit so callers can react to
// the resource condition without parsing platform strings.
func classifyCommit(errno error) error {
	en, ok := errno.(syscall.Errno)
	if !ok {
		if errno == nil {
			return fmt.Errorf("VirtualAlloc returned null with no error code")
		}
		return errno
	}
	switch en {
	case 8, 1455, 297:
		// ERROR_NOT_ENOUGH_MEMORY, ERROR_PAGEFILE_QUOTA_EXCEEDED,
		// ERROR_COMMITMENT_LIMIT: all are commit exhaustion.
		return ErrNoCommit
	}
	return errno
}

func queryRegion(addr uintptr) (memoryBasicInfo, bool) {
	var mbi memoryBasicInfo
	r, _, _ := procVirtualQuery.Call(addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
	if r == 0 {
		return memoryBasicInfo{}, false
	}
	return mbi, true
}
