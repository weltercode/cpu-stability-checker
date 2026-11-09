package sysinfo

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

// SMBIOS reading lives here in two layers: a pure parser that only
// looks at the table bytes it is given (fully testable with fixtures,
// on any OS), and a platform fetcher (memory_windows.go) that pulls
// the raw firmware table. perf_windows.go's rated processor speed uses
// the same fetcher.
//
// WHY CANDIDATE SCANNING AND NOT A SEQUENTIAL WALK: the spec says
// records are contiguous - formatted area, string area, next record -
// and a walk from offset 8 should work. It does not on this project's
// dev-board firmware (ASRock X870, verified live 2026-11-09): several
// UEFI-injected records are followed by a PADDED NUL run (the old
// sequential walk in perf_windows.go desynced exactly there, and the
// pre-rewrite ratedBaseMHz consumed a padding NUL as a type-0 header
// and misread the following record's type byte as a length). One bad
// boundary corrupts every record after it. So we do not walk; we scan
// for candidate headers of exactly the record types we consume and
// validate each candidate internally: string indices within range,
// printable string content, size/extended-size consistency, and (for
// memory devices) an array handle that a validated Type 16 record
// actually carries. Candidates that fail validation are counted, never
// emitted - an unvalidated record must not put fake MB or MT/s numbers
// into provenance. Residual risk, stated plainly: a byte sequence can
// pass validation by accident; the cross-checks (array-handle chain,
// printability, unit plausibility) are what keep that risk low, and
// every emitted field is still only ever "what the firmware wrote".

// MemoryDevice is one SMBIOS Type 17 (memory device) slot exactly as
// the firmware wrote it. Every numeric pointer field is nil when the
// firmware did not report it (or the record is too short to carry it)
// - never a zero-filled guess; an explicit 0 on SizeMB means "slot
// reported empty", nil means "unknown".
//
// Serial number and asset tag are deliberately NOT part of this
// struct: they are personal identity, and this tool's evidence
// (journals, reports, bundles) is written to be shared. The parser
// reads the string indices but never copies those two strings.
//
// The three speed pointers are the three speed-shaped fields Type 17
// carries, reported under the names the current spec puts at those
// offsets - observations of what firmware wrote, NOT a diagnosis. Real
// firmware was observed (this dev box, SMBIOS 3.4, verified live) to
// write: SPD speed 4800 at 0x15, max capability 5600 at 0x20, and
// 1100 (a voltage-like, implausible value) at 0x22 where the spec
// says "configured memory clock speed" goes. We record all three
// verbatim and let the reader compare; the collector NEVER computes
// or claims an "effective running speed" and never says above/below
// rating.
type MemoryDevice struct {
	Locator      string // Device Locator string, as declared ("DIMM 1" etc.)
	BankLocator  string // Bank Locator string, as declared ("P0 CHANNEL A")
	Manufacturer string // Manufacturer string as declared; kits often say "Unknown"
	PartNumber   string // Part Number string as declared, trailing blanks trimmed

	SizeMB        *int // installed capacity in MB; 0 = empty slot reported
	SPDMaxMTs     *int // Type 17 "Speed" (offset 0x15): module speed as written
	MaxCapMTs     *int // Type 17 "Max Speed Capability" (0x20, SMBIOS 2.7+)
	ConfiguredMTs *int // Type 17 "Configured Clock Speed" (0x22, 2.7+) verbatim
}

// MemoryProfile is the memory identity observed from SMBIOS Type 16
// (memory array) and Type 17 (devices) records. Absent pointers mean
// "not observed", never "verified to be zero".
type MemoryProfile struct {
	SMBIOSVersion      string // "3.4" style, from the table header; "" = unknown
	ArrayMaxCapacityMB *int   // Type 16 max capacity in MB, referenced array
	Devices            []MemoryDevice
	RejectedCandidates int // candidate records that failed validation
}

// parseMemoryProfile validates memory records from raw SMBIOS table
// data (structures only, without the 8-byte RawSMBIOSData header).
// version is the "major.minor" string from the header, purely for
// labelling. Field offsets are gated on each record's own declared
// length, which is the honest limit of what that record can claim.
func parseMemoryProfile(data []byte, version string) MemoryProfile {
	prof := MemoryProfile{SMBIOSVersion: version}

	// Pass 1: validated Type 17 candidates.
	type cand17 struct {
		dev      MemoryDevice
		arrHdl   uint16
		rejected bool
	}
	var cands []cand17
	for p := 0; p+4 <= len(data); p++ {
		if data[p] != 0x11 {
			continue
		}
		l := int(data[p+1])
		if l < 0x10 || p+l > len(data) {
			continue
		}
		strs, ok := smbiosStrings(data, p+l)
		if !ok {
			prof.RejectedCandidates++
			continue
		}
		strIdx := func(off int) (string, bool) {
			if p+off >= p+l {
				return "", false
			}
			i := data[p+off]
			if i == 0 {
				return "", true // field says "no string" - legitimate
			}
			if int(i) > len(strs) {
				return "", false // index beyond the set: malformed
			}
			return strs[i-1], true
		}
		// Structural validation: every string this record can reference
		// must be inside its own set, and every referenced string must
		// be printable text (junk-byte candidates fail here).
		dev := MemoryDevice{}
		bad := false
		for _, spec := range []struct {
			off  int
			dest *string
			must bool // printable when present
		}{
			{0x10, &dev.Locator, true},
			{0x11, &dev.BankLocator, true},
			{0x17, &dev.Manufacturer, true},
			{0x1A, &dev.PartNumber, true},
		} {
			if p+spec.off >= p+l {
				continue // record too short for this field: unknown
			}
			s, ok := strIdx(spec.off)
			if !ok {
				bad = true
				break
			}
			if spec.must && s != "" && !printableASCII(s) {
				bad = true
				break
			}
			if spec.dest != nil {
				*spec.dest = trimSpaces(s)
			}
		}
		// serial number (0x18) and asset tag (0x19) indices are
		// validated (a bogus index marks the record malformed) but
		// their contents are never copied - identity stays out.
		if !bad {
			for _, off := range []int{0x18, 0x19} {
				if p+off < p+l {
					if _, ok := strIdx(off); !ok {
						bad = true
					}
				}
			}
		}
		if bad {
			prof.RejectedCandidates++
			continue
		}

		// Size: WORD at 0x0C with the documented encodings; 0x7FFF
		// means "see Extended Size DWORD at 0x1C (MB)".
		sizeW := binary.LittleEndian.Uint16(data[p+0x0C:])
		var sizeMB *int
		switch {
		case sizeW == 0x7FFF:
			if l >= 0x20 {
				ext := binary.LittleEndian.Uint32(data[p+0x1C:])
				if ext > 0 && ext <= 8_000_000 { // <= 8 TB in MB
					mb := int(ext)
					sizeMB = &mb
				} // else: claims 0x7FFF but the extended field is junk: unknown
			}
		case sizeW&0x8000 != 0:
			raw := int(sizeW & 0x7FFF)
			if raw == 0 {
				zero := 0
				sizeMB = &zero // not-installed encoding (0x8000)
			} else {
				mb := raw // factor bit set: MB
				sizeMB = &mb
			}
		case sizeW == 0:
			zero := 0
			sizeMB = &zero // a size-0 slot: reported empty
		default:
			mb := int(sizeW) / 1024 // factor bit clear: KB (spec 2.3+)
			if int(sizeW) < 1024 {
				// under 1 MB and below the smallest real module:
				// more likely "unknown" than a fact
				sizeMB = nil
			} else {
				sizeMB = &mb
			}
		}
		dev.SizeMB = sizeMB

		// The three speed WORDs, verbatim. A value outside
		// [100, 32000] MT/s is not MT/s in any DDR generation we
		// target, so it is dropped (nil) rather than stored as a fake
		// data rate; the record itself stays.
		spdWord := func(off int) *int {
			if l < off+2 {
				return nil
			}
			w := int(binary.LittleEndian.Uint16(data[p+off:]))
			if w == 0 {
				return nil // firmware did not report
			}
			if w < 100 || w > 32000 {
				return nil
			}
			return &w
		}
		dev.SPDMaxMTs = spdWord(0x15)
		dev.MaxCapMTs = spdWord(0x20)
		dev.ConfiguredMTs = spdWord(0x22)

		arrHdl := binary.LittleEndian.Uint16(data[p+0x04:])
		cands = append(cands, cand17{dev: dev, arrHdl: arrHdl})
	}

	// Pass 2: accept only Type 16 records whose handle a validated
	// Type 17 references - the array-handle chain is the cross-check
	// that separates real memory arrays from 0x10 bytes in string
	// garbage.
	referenced := map[uint16]bool{}
	for _, c := range cands {
		referenced[c.arrHdl] = true
	}
	for p := 0; p+4 <= len(data); p++ {
		if data[p] != 0x10 {
			continue
		}
		l := int(data[p+1])
		if l < 0x0F || p+l > len(data) {
			continue
		}
		if _, ok := smbiosStrings(data, p+l); !ok {
			continue
		}
		hdl := binary.LittleEndian.Uint16(data[p+2:])
		if !referenced[hdl] {
			continue
		}
		if prof.ArrayMaxCapacityMB != nil {
			continue // first referenced array wins; duplicates: leave unknown-vs-first honest
		}
		// Maximum Capacity DWORD at 0x0B: MB with bit31 = "extended
		// capacity is authoritative" (SMBIOS 2.7). Extended Maximum
		// Capacity DWORD lives at 0x13 when the record is long enough.
		capDW := binary.LittleEndian.Uint32(data[p+0x0B:])
		var mb *int
		if capDW&(1<<31) != 0 && l >= 0x17 {
			ext := binary.LittleEndian.Uint32(data[p+0x13:])
			if ext > 0 && ext <= 8_000_000 { // <= 8 TB in MB
				v := int(ext)
				mb = &v
			}
		} else {
			v := int(capDW &^ (1 << 31))
			if v > 0 && v <= 8_000_000 {
				mb = &v
			}
		}
		prof.ArrayMaxCapacityMB = mb
	}

	for _, c := range cands {
		prof.Devices = append(prof.Devices, c.dev)
	}
	return prof
}

// parseRatedProcessorMHz scans Type 4 (processor) records for the
// firmware-declared rated maximum core speed (Max Speed WORD at
// 0x16). It applies the same discipline as the memory collector:
// candidate scan, string validation, plausibility bound. Every
// record that reports a speed must agree; disagreement is ambiguous
// calibration, and guessing would put fake MHz numbers on the panel,
// so the function refuses and the caller stays on the honest tsc
// label.
//
// Type 4 field discipline (verified against this machine's live
// record at offset 1546): Socket at 0x04, CPUID QWORD at 0x05 (BINARY
// - never treat those bytes as string indices), Processor Type BYTE
// at 0x0D, Processor Family WORD at 0x0E (also NOT a string),
// Manufacturer 0x10, Processor ID 0x11, Version 0x12, Voltage 0x13
// (string indices), Max Speed WORD 0x16, Current Speed WORD 0x18.
//
// The validation checks ONLY Max Speed (plausibility bound) and the
// Socket string index, because this board's firmware (ASRock/AMI,
// verified live 2026-11-09) writes corrupted values in OTHER slots:
// Processor Type byte 0xFB, Processor ID string index 141, Version
// string index 100 - all beyond its own six-string set. Demanding
// those fields validate would throw away the one real, correct fact
// (Max Speed 4300 on a 9950X) on a firmware quirk. Junk candidates
// mostly fail the speed bound outright; if some junk survivor ever
// disagreed with a real record, the disagreement refusal surfaces it
// loudly and the caller degrades to the honest tsc label - the
// fail-safe direction (less calibration, never a fake base).
func parseRatedProcessorMHz(data []byte) (int, error) {
	base := 0
	for p := 0; p+4 <= len(data); p++ {
		if data[p] != 0x04 {
			continue
		}
		l := int(data[p+1])
		if l < 0x18 || p+l > len(data) {
			continue
		}
		strs, ok := smbiosStrings(data, p+l)
		if !ok {
			continue
		}
		max := int(binary.LittleEndian.Uint16(data[p+0x16:]))
		if max < 100 || max > 20000 {
			continue // not a plausible processor speed: not a real record
		}
		// The socket string must at least be self-consistent: a real
		// populated record names its socket ("AM5" here).
		if i := data[p+0x04]; i != 0 {
			if int(i) > len(strs) || !printableASCII(strs[i-1]) {
				continue
			}
		}
		if base == 0 {
			base = max
		} else if base != max {
			return 0, fmt.Errorf("SMBIOS processor records disagree (%d vs %d MHz)", base, max)
		}
	}
	if base == 0 {
		return 0, fmt.Errorf("SMBIOS publishes no rated processor speed")
	}
	return base, nil
}

// smbiosStrings strictly parses a record's string area starting at s:
// a run of NUL-terminated strings closed by one more NUL. ok=false
// means the area runs past the table (truncated table).
func smbiosStrings(data []byte, s int) (strs []string, ok bool) {
	q := s
	for q < len(data) && data[q] != 0 {
		e := q
		for e < len(data) && data[e] != 0 {
			e++
		}
		strs = append(strs, string(data[q:e]))
		q = e + 1
	}
	if q >= len(data) {
		return nil, false
	}
	return strs, true
}

// ival renders a *int for display: nil prints as "unknown" (never as
// 0 - that would turn an unobserved value into a confident number).
func ival(p *int) string {
	if p == nil {
		return "unknown"
	}
	return strconv.Itoa(*p)
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func trimSpaces(s string) string {
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}
