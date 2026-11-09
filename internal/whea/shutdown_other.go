//go:build !windows

package whea

import "errors"

// The unexpected-shutdown witness records (Kernel-Power 41 / EventLog
// 6008) are a Windows System event log facility. On every other platform
// the entry point names its own refusal honestly - the witness then says
// "could not look, and here is why", never zero-fills.
var errShutdownUnsupported = errors.New("unexpected-shutdown witness records (Kernel-Power 41 / EventLog 6008) are a Windows System event log facility: not present on this platform")

func QueryShutdownWitness(limit int) ([]Event, error) {
	return nil, errShutdownUnsupported
}
