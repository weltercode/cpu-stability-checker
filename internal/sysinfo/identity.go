package sysinfo

// MachineIdentity describes the machine a run happened on, for run
// provenance. Every field is optional: an empty string means "not
// observed on this platform", never "none" and never a guess. BIOS
// identity is deliberately absent: this tool does not collect it yet
// (a WMI Win32_BIOS query is a future collector), and an absent field
// reads as unknown, not as "no BIOS".
type MachineIdentity struct {
	CPUName     string // processor brand string as the OS reports it
	OSVersion   string // OS version string (Windows build / kernel release)
	BIOSVersion string // reserved: never populated today (unknown, not empty-by-verification)
}

// DiscoverMachineIdentity collects what the platform offers cheaply
// and truthfully (mirroring DiscoverTopology's naming). It never runs
// a stress probe and never blocks on a network or a driver.
func DiscoverMachineIdentity() MachineIdentity {
	return machineIdentity()
}
