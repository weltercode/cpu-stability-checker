//go:build !windows

package memtest

// There is deliberately no native allocator here and no Go-heap
// fallback: a Go heap OOM is fatal and unrecoverable, and the plan
// forbids papering over that with recover. Callers see
// ErrUnsupported and the memory engine stays honestly off.
func newPlatformAllocator() (Allocator, error) {
	return nil, ErrUnsupported
}
