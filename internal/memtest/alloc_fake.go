package memtest

import (
	"fmt"
	"sync"
)

// FakeAllocator is a deterministic stand-in for the native allocator,
// exported for cross-package tests (the plan's "fake allocator fails
// at the Nth block" and low-commit scenarios). It is NOT used by any
// production path: NewAllocator only ever returns the platform one.
type FakeAllocator struct {
	mu       sync.Mutex
	next     uintptr
	blocks   map[uintptr]uint64
	failOn   int    // commit number (1-based) that must fail; 0 = never
	cap      uint64 // total committed bytes allowed; 0 = unlimited
	commits  int
	releases int
	gran     uint64
}

// NewFakeAllocator returns a fake with 4096 granularity.
func NewFakeAllocator() *FakeAllocator {
	return &FakeAllocator{next: 0x10000, blocks: make(map[uintptr]uint64), gran: 4096}
}

// FailOnCommit makes the n-th Commit call (1-based) fail with an
// ErrNoCommit-wrapped error.
func (f *FakeAllocator) FailOnCommit(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn = n
}

// CapTotal makes any Commit that would push total committed bytes
// above cap fail with ErrNoCommit - the low-commit simulation.
func (f *FakeAllocator) CapTotal(cap uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cap = cap
}

// Stats reports commits, releases, live blocks and total allocated
// bytes so tests can assert "everything committed was released".
func (f *FakeAllocator) Stats() (commits, releases, live int, allocated uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total uint64
	for _, v := range f.blocks {
		total += v
	}
	return f.commits, f.releases, len(f.blocks), total
}

func (f *FakeAllocator) AllocationGranularity() uint64 { return f.gran }

func (f *FakeAllocator) Commit(size uint64) (Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size == 0 {
		return Block{}, fmt.Errorf("memtest fake: commit size must be > 0")
	}
	f.commits++
	if f.failOn != 0 && f.commits == f.failOn {
		return Block{}, fmt.Errorf("memtest fake: commit %d forced to fail: %w", f.commits, ErrNoCommit)
	}
	rounded := size + (f.gran-size%f.gran)%f.gran
	if f.cap != 0 {
		var total uint64
		for _, v := range f.blocks {
			total += v
		}
		if total+rounded > f.cap {
			return Block{}, fmt.Errorf("memtest fake: commit %d exceeds cap %d: %w", size, f.cap, ErrNoCommit)
		}
	}
	addr := f.next
	f.next += uintptr(rounded) // fake sizes are test-scale; no uintptr overflow in practice
	f.blocks[addr] = rounded
	return Block{Addr: addr, Requested: size, Allocated: rounded}, nil
}

func (f *FakeAllocator) Release(b Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, owned := f.blocks[b.Addr]; !owned {
		return fmt.Errorf("memtest fake: Release(%#x): block not owned", b.Addr)
	}
	delete(f.blocks, b.Addr)
	f.releases++
	return nil
}

func (f *FakeAllocator) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases += len(f.blocks)
	f.blocks = make(map[uintptr]uint64)
	return nil
}
