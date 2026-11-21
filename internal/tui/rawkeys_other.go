//go:build !windows

package tui

// StartRawKeys has no console-event plumbing on this platform: it
// always reports "not available" and callers keep their buffered
// line input, byte-identical to before R42.
func StartRawKeys() *RawKeys { return nil }

// RawKeys is an empty shell off Windows; StartRawKeys never returns
// one.
type RawKeys struct{}

// Stop is a no-op.
func (rk *RawKeys) Stop() {}

// NextKey is unreachable (StartRawKeys never returns a RawKeys).
func (rk *RawKeys) NextKey() (Key, rune, uint32) { return KeyLost, 0, 1 }

// RaiseCtrlC is unavailable off Windows; callers must handle false.
func RaiseCtrlC() bool { return false }
