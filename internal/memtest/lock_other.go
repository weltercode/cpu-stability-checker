//go:build !windows

package memtest

// No page-lock layer here: the honest answer is ErrUnsupported, and
// Lock's required mode turns that into a setup error rather than a
// silently unlocked run.
func lockRegion(addr uintptr, size uint64) error { return ErrUnsupported }

func workingSetLimits() (min, max uint64, ok bool) { return 0, 0, false }
