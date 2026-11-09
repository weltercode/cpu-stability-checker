//go:build windows

package sysinfo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"
)

// rsmbTableData fetches the SMBIOS structure table through
// GetSystemFirmwareTable('RSMB'). The payload is RawSMBIOSData: an
// 8-byte header (used20-calling-method, major, minor, DMI revision,
// then a DWORD length OF THE STRUCTURE DATA - a byte count, not a
// record count; the pre-rewrite ratedBaseMHz read its low half as a
// count and walked 1904 "records" through a 1904-byte table) followed
// by the structures themselves. If the declared length overruns the
// payload we clamp to the payload: parsing fewer real bytes cannot
// fabricate records (every emitted field is validated), and refusing
// outright would hide fields the firmware really published.

func rsmbTableData() (data []byte, version string, err error) {
	const rsmb = 0x52534D42 // 'RSMB'
	size, _, _ := procFirmwareTbl.Call(rsmb, 0, 0, 0)
	if size == 0 {
		return nil, "", errors.New("SMBIOS not available")
	}
	buf := make([]byte, size)
	n, _, _ := procFirmwareTbl.Call(rsmb, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(size))
	if int(n) != len(buf) || len(buf) < 8 {
		return nil, "", errors.New("SMBIOS table truncated")
	}
	dataLen := int(binary.LittleEndian.Uint32(buf[4:]))
	if dataLen <= 0 || 8+dataLen > len(buf) {
		dataLen = len(buf) - 8
	}
	return buf[8 : 8+dataLen], fmt.Sprintf("%d.%d", buf[1], buf[2]), nil
}

// CollectMemoryProfile observes the machine's memory identity from
// SMBIOS (Type 16 array + Type 17 devices). Read-only firmware query,
// no WMI, no driver, no stress: safe to call once at run start. The
// result carries only what firmware wrote; serial and asset identity
// never enter it (see MemoryDevice), and an error means the whole
// profile is unknown - callers must disclose that, never substitute
// defaults.
func CollectMemoryProfile() (MemoryProfile, error) {
	data, version, err := rsmbTableData()
	if err != nil {
		return MemoryProfile{}, err
	}
	return parseMemoryProfile(data, version), nil
}
