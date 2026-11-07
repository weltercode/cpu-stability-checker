//go:build windows

package sysinfo

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var procGetSystemCPUSetInformation = kernel32.NewProc("GetSystemCpuSetInformation")
var procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")

// logicalRef addresses one hardware thread the way the raw records do.
type logicalRef struct {
	group  uint16
	number uint8
}

// l3CacheIDs maps every logical CPU to a per-call-stable L3 instance id by
// walking the cache relationship records: cores that share one L3 form a
// CCX (the CCD unit on AMD). The CPU Set API used for the rest of the
// topology carries no cache information, so this second API is required.
func l3CacheIDs() (map[logicalRef]uint32, error) {
	const (
		relationAll   = 0xffff // all relationship types, not RelationProcessorCore
		relationCache = 2
		level3        = 3
		// header(8) + CACHE_DESCRIPTOR(8) + GROUP_AFFINITY(16)
		cacheRecordSize = 32
	)

	// ReturnLength is in/out: on input it must carry the buffer size (a NULL
	// buffer or a zero size is rejected with ERROR_INSUFFICIENT_BUFFER), and
	// on failure it reports the size actually needed.
	size := 4096
	var buf []byte
	for attempt := 0; ; attempt++ {
		buf = make([]byte, size)
		required := uint32(size)
		r, _, errno := procGetLogicalProcessorInformationEx.Call(
			relationAll,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&required)),
		)
		runtime.KeepAlive(buf)
		if r != 0 {
			if int(required) > len(buf) {
				return nil, fmt.Errorf("processor information size %d exceeds buffer %d", required, len(buf))
			}
			buf = buf[:required]
			break
		}
		const errorInsufficientBuffer = 122
		if errno == syscall.Errno(errorInsufficientBuffer) && required > uint32(size) && attempt < 5 {
			size = int(required)
			continue
		}
		return nil, fmt.Errorf("GetLogicalProcessorInformationEx failed: %w", errno)
	}

	out := make(map[logicalRef]uint32)
	var l3seq uint32
	for offset := 0; offset < len(buf); {
		if len(buf)-offset < 8 {
			return nil, fmt.Errorf("truncated record header at byte %d", offset)
		}
		relationship := binary.LittleEndian.Uint32(buf[offset : offset+4])
		recordSize := int(binary.LittleEndian.Uint32(buf[offset+4 : offset+8]))
		if recordSize < 8 || offset+recordSize > len(buf) {
			return nil, fmt.Errorf("invalid record size %d at byte %d", recordSize, offset)
		}

		if relationship == relationCache && recordSize >= cacheRecordSize {
			level := buf[offset+8]
			if level == level3 {
				if recordSize >= 56 {
					// Windows 11 26H2 grew cache records to 56 bytes: a
					// CACHE_TYPE dword at +16, a GroupCount word at +38 and
					// one KAFFINITY per processor group starting at +40.
					groupCount := int(binary.LittleEndian.Uint16(buf[offset+38 : offset+40]))
					for g := 0; g < groupCount; g++ {
						start := offset + 40 + 8*g
						if start+8 > offset+recordSize {
							break
						}
						mask := binary.LittleEndian.Uint64(buf[start : start+8])
						for bit := uint8(0); bit < 64; bit++ {
							if mask&(1<<bit) != 0 {
								out[logicalRef{group: uint16(g), number: bit}] = l3seq
							}
						}
					}
				} else {
					// Classic layout: GROUP_AFFINITY at +16.
					group := binary.LittleEndian.Uint16(buf[offset+16 : offset+18])
					mask := binary.LittleEndian.Uint64(buf[offset+24 : offset+32])
					for bit := uint8(0); bit < 64; bit++ {
						if mask&(1<<bit) != 0 {
							out[logicalRef{group: group, number: bit}] = l3seq
						}
					}
				}
				l3seq++
			}
		}
		offset += recordSize
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no L3 cache relationships reported")
	}
	return out, nil
}

// discoverRawLogicalCPUs uses the Windows CPU Set API because one record gives
// us the processor group, logical index, physical-core index, NUMA node and a
// CPU Set ID suitable for later thread assignment.
func discoverRawLogicalCPUs() ([]rawLogicalCPU, string, bool, error) {
	var required uint32
	procGetSystemCPUSetInformation.Call(
		0,
		0,
		uintptr(unsafe.Pointer(&required)),
		0,
		0,
	)
	if required == 0 {
		return nil, "", false, fmt.Errorf("GetSystemCpuSetInformation returned an empty buffer size")
	}

	buf := make([]byte, required)
	r, _, callErr := procGetSystemCPUSetInformation.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&required)),
		0,
		0,
	)
	runtime.KeepAlive(buf)
	if r == 0 {
		return nil, "", false, fmt.Errorf("GetSystemCpuSetInformation failed: %w", callErr)
	}
	if int(required) > len(buf) {
		return nil, "", false, fmt.Errorf("CPU Set data grew from %d to %d bytes", len(buf), required)
	}

	const (
		cpuSetInformationType = 0
		minimumRecordSize     = 32
	)

	var out []rawLogicalCPU
	for offset := 0; offset < int(required); {
		if int(required)-offset < 8 {
			return nil, "", false, fmt.Errorf("truncated CPU Set record header at byte %d", offset)
		}
		recordSize := int(binary.LittleEndian.Uint32(buf[offset : offset+4]))
		recordType := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
		if recordSize < 8 || offset+recordSize > int(required) {
			return nil, "", false, fmt.Errorf("invalid CPU Set record size %d at byte %d", recordSize, offset)
		}

		if recordType == cpuSetInformationType {
			if recordSize < minimumRecordSize {
				return nil, "", false, fmt.Errorf("CPU Set record is only %d bytes", recordSize)
			}
			record := buf[offset : offset+recordSize]
			out = append(out, rawLogicalCPU{
				cpuSetID:        binary.LittleEndian.Uint32(record[8:12]),
				cpuSetIDKnown:   true,
				group:           binary.LittleEndian.Uint16(record[12:14]),
				number:          record[14],
				coreIndex:       record[15],
				numaNode:        record[17],
				efficiencyClass: record[18],
				parked:          record[19]&1 != 0,
			})
		}
		offset += recordSize
	}

	if len(out) == 0 {
		return nil, "", false, fmt.Errorf("Windows returned no CpuSetInformation records")
	}

	// Cache topology comes from a different API; if it fails we simply keep
	// CCX unknown instead of failing the whole discovery.
	if l3, err := l3CacheIDs(); err == nil {
		for i := range out {
			if id, ok := l3[logicalRef{group: out[i].group, number: out[i].number}]; ok {
				out[i].l3ID = id
				out[i].l3Known = true
			}
		}
	}
	return out, "windows-cpu-sets", false, nil
}
