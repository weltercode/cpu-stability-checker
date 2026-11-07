//go:build !windows && !linux

package sysinfo

// No identity collector on other platforms: every field stays unknown,
// and the journal/report says so instead of inventing values.
func machineIdentity() MachineIdentity {
	return MachineIdentity{}
}
