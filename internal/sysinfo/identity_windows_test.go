//go:build windows

package sysinfo

import "testing"

// A missing key or value must read as unknown (""), never as a panic
// or a fabricated string: identity fields are optional everywhere.
func TestRegistryStringMissingIsUnknown(t *testing.T) {
	if got := registryString(`NONEXISTENT\KEY\PATH`, "Nope"); got != "" {
		t.Errorf("registryString(missing) = %q, want empty", got)
	}
	// The real machine's brand string is read through the same call.
	if got := registryString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "ProcessorNameString"); got == "" {
		t.Error("registryString(ProcessorNameString) = empty on a Windows machine, want the brand string")
	}
}
