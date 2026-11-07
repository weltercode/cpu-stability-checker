package memtest

import (
	"errors"
	"math"
	"unsafe"
)

const maxInt = uint64(math.MaxInt)

// ErrUnsupported means this platform has no native allocation layer in
// this program. The memory engine must NOT fall back to the Go heap:
// a Go heap OOM is fatal and unrecoverable, and pretending a Go
// allocation is a committed native buffer would lie about residency.
var ErrUnsupported = errors.New("memtest: native allocation is not supported on this platform")

// ErrNoCommit means the OS refused the commit: physical memory and/or
// page file cannot cover it. It is a resource condition, never a CPU
// fault, and callers must map it to a setup/resource outcome.
var ErrNoCommit = errors.New("memtest: out of commit space or physical memory")

// Block is one native allocation owned by an Allocator. Requested is
// what was asked for; Allocated is what the OS actually committed
// (VirtualAlloc rounds up - measured at page granularity on build
// 26300 - and the accounting reports the real queried number, not
// the wish).
type Block struct {
	Addr      uintptr
	Requested uint64
	Allocated uint64
}

// byteView mirrors the internal layout of a slice header ({data,
// len, cap}). It exists because both direct uintptr->unsafe.Pointer
// conversion and reflect.SliceHeader are vet heuristics aimed at
// GC-managed memory: our data is OS-committed, pinned and owned by
// the Allocator, so the conversion is sound, and saying so in a
// named type is more honest than hiding it.
type byteView struct {
	data uintptr
	len  int
	cap  int
}

// Bytes returns a byte-slice VIEW of the requested range. The data
// lives in native memory (no Go pointers inside it); the slice header
// is only a window. The allocator keeps ownership: Close or Release
// invalidates the view, and using it afterwards is a use-after-free.
//
// The noinline barrier is load-bearing: measured on this toolchain,
// an inlined byteView construction let the optimizer track the data
// word's provenance, treat stores through the view as invisible to
// Go code, and then reorder or delete them across clock reads and
// verification loads (first-touch accounting folded to exactly zero
// while the pattern check still "passed" vacuously). Through a call
// boundary the returned slice is an ordinary opaque Go slice and the
// stores are conservatively ordered.
//
//go:noinline
func (b Block) Bytes() []byte {
	if b.Addr == 0 || b.Requested > maxInt {
		return nil
	}
	v := byteView{data: b.Addr, len: int(b.Requested), cap: int(b.Requested)}
	return *(*[]byte)(unsafe.Pointer(&v))
}

// Allocator hands out committed native memory. Commit either returns
// a real committed block or an error; it never half-succeeds. Close
// releases every block the allocator still owns and is safe to call
// twice and after partial failures.
type Allocator interface {
	Commit(size uint64) (Block, error)
	Release(b Block) error
	Close() error
	// AllocationGranularity is the value the platform reports for
	// allocation rounding. Measured on Windows 11 build 26300,
	// VirtualAlloc actually rounds to PAGE granularity (4 KiB),
	// finer than the 64 KiB GetSystemInfo still advertises - so
	// accounting uses VirtualQuery's real RegionSize, never this
	// number, and nothing may assume allocations are this coarse.
	AllocationGranularity() uint64
}

// NewAllocator returns the platform's native allocator, or an error
// wrapping ErrUnsupported where none exists.
func NewAllocator() (Allocator, error) {
	return newPlatformAllocator()
}
