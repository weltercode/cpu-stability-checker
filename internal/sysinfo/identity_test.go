package sysinfo

import (
	"runtime"
	"testing"
)

// The identity collector must never invent: on platforms with a
// collector the fields are populated, and the BIOS field stays empty
// (unknown, not collected) everywhere today.
func TestDiscoverMachineIdentity(t *testing.T) {
	id := DiscoverMachineIdentity()
	t.Logf("identity: cpu=%q os=%q bios=%q", id.CPUName, id.OSVersion, id.BIOSVersion)
	if id.BIOSVersion != "" {
		t.Error("BIOS identity reported, but this tool does not collect it: the field must stay unknown")
	}
	switch runtime.GOOS {
	case "windows":
		if id.CPUName == "" || id.OSVersion == "" {
			t.Errorf("windows identity = %+v, want a brand string and an NT version", id)
		}
	case "linux":
		if id.OSVersion == "" {
			t.Errorf("linux identity = %+v, want the kernel release", id)
		}
	}
}
