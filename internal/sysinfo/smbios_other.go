//go:build !windows

package sysinfo

import "errors"

// CollectMemoryProfile is Windows-only for now: the fetcher is
// GetSystemFirmwareTable('RSMB'). On other platforms the memory
// profile is simply unknown (the parser itself is pure Go and would
// happily parse a DMI/sysfs-sourced table - wiring that fetcher is
// future work, not a claim made here).
func CollectMemoryProfile() (MemoryProfile, error) {
	return MemoryProfile{}, errors.New("SMBIOS firmware table not available on this platform")
}
