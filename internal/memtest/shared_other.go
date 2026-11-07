//go:build !windows

package memtest

// SharedMapping is the P07e cross-process section. This program has
// no native shared-mapping layer on non-Windows platforms: the type
// exists so cross-platform call sites compile, and every operation
// refuses with ErrUnsupported instead of faking a Go-heap stand-in
// (the same rule as the native allocator: no pretending).
type SharedMapping struct{}

func CreateSharedMapping(size uint64) (*SharedMapping, error) {
	return nil, ErrUnsupported
}

func OpenSharedMapping(handle uintptr, size uint64) (*SharedMapping, error) {
	return nil, ErrUnsupported
}

func (m *SharedMapping) Handle() uintptr { return 0 }

func (m *SharedMapping) Size() uint64 { return 0 }

func (m *SharedMapping) View(off, n uint64) ([]byte, error) { return nil, ErrUnsupported }

func (m *SharedMapping) Close() error { return ErrUnsupported }

func (m *SharedMapping) Allocator(grants []Grant) (Allocator, error) { return nil, ErrUnsupported }

// PublicationBarrier has no native implementation on this platform:
// there is no shared mapping here either, so no handoff can reach
// the point where a barrier would be needed. The refusal is honest;
// nothing may substitute a Go-level sync and claim cross-process
// store visibility.
func PublicationBarrier() error { return ErrUnsupported }
