package sysinfo

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// sbRec assembles one SMBIOS record: type, length (4 + body), handle,
// body bytes (fields from offset 0x04 up), then the string area
// (NUL-terminated strings + closing NUL).
func sbRec(typ byte, handle uint16, body []byte, strs ...string) []byte {
	rec := []byte{typ, byte(4 + len(body))}
	rec = binary.LittleEndian.AppendUint16(rec, handle)
	rec = append(rec, body...)
	for _, s := range strs {
		rec = append(rec, s...)
		rec = append(rec, 0)
	}
	return append(rec, 0)
}

// t17Body builds a Type 17 formatted area. off = offset - 4.
func t17Body(arr, err, totalW, dataW, sizeW uint16, ff, dset, loc, bank byte,
	mtype byte, detail, speed uint16, mfr, serial, asset, part byte, attrs byte,
	extMB uint32, clk20, clk22 uint16) []byte {
	body := make([]byte, 0x24) // through offset 0x27 (covers clk22 at 0x22)
	b := func(off int) int { return int(off) - 4 }
	binary.LittleEndian.PutUint16(body[b(0x04):], arr)
	binary.LittleEndian.PutUint16(body[b(0x06):], err)
	binary.LittleEndian.PutUint16(body[b(0x08):], totalW)
	binary.LittleEndian.PutUint16(body[b(0x0A):], dataW)
	binary.LittleEndian.PutUint16(body[b(0x0C):], sizeW)
	body[b(0x0E)] = ff
	body[b(0x0F)] = dset
	body[b(0x10)] = loc
	body[b(0x11)] = bank
	body[b(0x12)] = mtype
	binary.LittleEndian.PutUint16(body[b(0x13):], detail)
	binary.LittleEndian.PutUint16(body[b(0x15):], speed)
	body[b(0x17)] = mfr
	body[b(0x18)] = serial
	body[b(0x19)] = asset
	body[b(0x1A)] = part
	body[b(0x1B)] = attrs
	binary.LittleEndian.PutUint32(body[b(0x1C):], extMB)
	binary.LittleEndian.PutUint16(body[b(0x20):], clk20)
	binary.LittleEndian.PutUint16(body[b(0x22):], clk22)
	return body
}

func t16Body(capMB uint32) []byte {
	body := make([]byte, 0x13) // record length 0x17 (through 0x16)
	b := func(off int) int { return int(off) - 4 }
	body[b(0x04)] = 0x03 // array type: other (unvalidated label, not collected)
	body[b(0x05)] = 0x03 // use: system memory
	body[b(0x06)] = 0x03 // ecc: none
	binary.LittleEndian.PutUint32(body[b(0x0B):], capMB)
	return body
}

// cat joins records into one table image.
func cat(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

func ptrEq(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %d", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %d, want %d", name, *got, want)
	}
}

func ptrNil(t *testing.T, name string, got *int) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want nil (unknown)", name, *got)
	}
}

// A spec-shaped table: one Type 16 array and two Type 17 slots (one
// populated 48 GB module encoded as 0x7FFF + Extended Size, one empty
// slot) must decode with the exact numbers, and only the referenced
// array's capacity is claimed.
func TestParseMemoryProfileStrictFixture(t *testing.T) {
	table := sbRec(0x10, 0x000B, t16Body(262154))
	table = append(table, sbRec(0x11, 0x0010,
		t17Body(0x000B, 0x000F, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0x4080,
			4800, 3, 4, 0, 5, 0x02, 49152, 5600, 1100),
		"DIMM 1", "P0 CHANNEL A", "Unknown", "SER-FAKE-0001", "F5-5200J4040A48G              ")...)
	table = append(table, sbRec(0x11, 0x0013,
		t17Body(0x000B, 0x0012, 0xFFFF, 0xFFFF, 0x0000, 0x02, 0, 1, 2, 0x02, 0,
			0, 3, 3, 3, 3, 0, 0, 0, 0),
		"DIMM 0", "P0 CHANNEL B", "Unknown")...)

	prof := parseMemoryProfile(table, "3.4")
	if prof.SMBIOSVersion != "3.4" {
		t.Errorf("version = %q", prof.SMBIOSVersion)
	}
	if prof.RejectedCandidates != 0 {
		t.Errorf("rejected = %d, want 0 for a well-formed table: %+v", prof.RejectedCandidates, prof)
	}
	ptrEq(t, "array max MB", prof.ArrayMaxCapacityMB, 262154)
	if len(prof.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(prof.Devices))
	}
	d := prof.Devices[0]
	if d.Locator != "DIMM 1" || d.BankLocator != "P0 CHANNEL A" || d.Manufacturer != "Unknown" {
		t.Errorf("strings = %q / %q / %q", d.Locator, d.BankLocator, d.Manufacturer)
	}
	if d.PartNumber != "F5-5200J4040A48G" {
		t.Errorf("part = %q, want padded blanks trimmed", d.PartNumber)
	}
	ptrEq(t, "device size MB", d.SizeMB, 49152)
	ptrEq(t, "spd mts", d.SPDMaxMTs, 4800)
	ptrEq(t, "max cap mts", d.MaxCapMTs, 5600)
	ptrEq(t, "configured as written", d.ConfiguredMTs, 1100)
	e := prof.Devices[1]
	ptrEq(t, "empty slot size", e.SizeMB, 0)
	ptrNil(t, "empty slot spd", e.SPDMaxMTs)

	// Identity discipline: serial and asset contents must not be
	// reachable anywhere in the profile (the collector never copies
	// those string slots).
	blob := fmt.Sprintf("%+v", prof)
	for _, secret := range []string{"SER-FAKE-0001"} {
		if strings.Contains(blob, secret) {
			t.Errorf("profile leaked identity string %q", secret)
		}
	}
}

// The firmware-verified layout from this project's dev board: Type 17
// records carry a vendor tail (length 0x5c), and several records are
// followed by a padded extra NUL that a sequential walk eats as a
// header. The scan must produce the same decode regardless.
func TestParseMemoryProfilePaddedFirmwareFixture(t *testing.T) {
	tail := make([]byte, 0x34) // vendor tail past offset 0x27: record len 0x5c
	pop := sbRec(0x11, 0x0010, append(t17Body(0x000B, 0x000F, 64, 64, 0x7FFF,
		0x09, 0, 1, 2, 0x22, 0x4080, 4800, 3, 4, 0, 5, 0x02, 49152, 5600, 1100), tail...),
		"DIMM 1", "P0 CHANNEL A", "Unknown", "SER-FAKE-0002", "F5-5200J4040A48G")
	table := sbRec(0x10, 0x000B, t16Body(262154))
	table = append(table, 0) // the padding NUL this firmware writes
	table = append(table, pop...)
	table = append(table, 0)                                          // and again
	table = append(table, sbRec(0x12, 0x000F, make([]byte, 0x13))...) // a record of another type

	prof := parseMemoryProfile(table, "3.4")
	if len(prof.Devices) != 1 {
		t.Fatalf("devices = %d, want 1 (padding must not desync the scan): %+v", len(prof.Devices), prof)
	}
	ptrEq(t, "device size MB", prof.Devices[0].SizeMB, 49152)
	ptrEq(t, "array max MB", prof.ArrayMaxCapacityMB, 262154)
}

// Candidates must fail closed: an out-of-range string index, an
// unprintable string, or an array handle no validated Type 16 carries
// all disqualify (or hollow out) the claim.
func TestParseMemoryProfileRejectsInvalidCandidates(t *testing.T) {
	// (a) part string index beyond the set
	bad1 := sbRec(0x11, 0x0010,
		t17Body(0x000B, 0, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0, 4800,
			1, 1, 0, 9, 0, 49152, 5600, 5600), "DIMM 1", "BANK", "Unknown")
	// (b) unprintable locator (junk-byte candidate)
	bad2 := sbRec(0x11, 0x0011,
		t17Body(0x000B, 0, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0, 4800,
			3, 3, 0, 3, 0, 49152, 5600, 5600), "DIM\x01M", "BANK", "Unknown")
	// (c) well-formed but references an array nothing declares
	orphan := sbRec(0x11, 0x0012,
		t17Body(0x00EE, 0, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0, 4800,
			3, 3, 0, 3, 0, 49152, 5600, 5600), "DIMM 1", "BANK", "Unknown")
	prof := parseMemoryProfile(cat(bad1, bad2, orphan), "3.4")
	if prof.RejectedCandidates != 2 {
		t.Errorf("rejected = %d, want 2", prof.RejectedCandidates)
	}
	if prof.ArrayMaxCapacityMB != nil {
		t.Errorf("array capacity = %d, want nil (unreferenced)", *prof.ArrayMaxCapacityMB)
	}
	// the orphan record survives structurally but claims no capacity?
	// no: its own size field IS its own claim and stays; the ARRAY
	// claim is what the cross-check gates. Size stays as an
	// observation, which is honest and must stay pinned.
	if len(prof.Devices) != 1 {
		t.Fatalf("devices = %d, want 1 (orphan keeps its own fields)", len(prof.Devices))
	}
	ptrEq(t, "orphan size MB", prof.Devices[0].SizeMB, 49152)
}

// Hostile noise must never panic and must never yield confident
// garbage: every emitted number still passes the parser's own gates.
func TestParseMemoryProfileHostileNoise(t *testing.T) {
	noise := make([]byte, 0, 2048)
	for i := 0; i < 2048; i++ {
		noise = append(noise, byte(i*37%251)^0x11)
		if i%97 == 0 {
			noise = append(noise, 0x11, 0x5c, 0x00, 0x0b) // stray type-17-ish runs
		}
		if i%61 == 0 {
			noise = append(noise, 0, 0)
		}
	}
	prof := parseMemoryProfile(noise, "")
	for _, d := range prof.Devices {
		for _, s := range []string{d.Locator, d.BankLocator, d.Manufacturer, d.PartNumber} {
			if !printableASCII(s) {
				t.Errorf("emitted unprintable string %q", s)
			}
		}
		if d.SizeMB != nil && (*d.SizeMB < 0 || *d.SizeMB > 8_000_000) {
			t.Errorf("emitted implausible size %d MB", *d.SizeMB)
		}
		for _, sp := range []*int{d.SPDMaxMTs, d.MaxCapMTs, d.ConfiguredMTs} {
			if sp != nil && (*sp < 100 || *sp > 32000) {
				t.Errorf("emitted implausible MT/s %d", *sp)
			}
		}
	}
}

// Speed words outside every plausible MT/s range are dropped to nil
// while the record itself survives - a firmware voltage written into
// a speed slot must not masquerade as a data rate.
func TestParseMemoryProfileDropsImplausibleSpeeds(t *testing.T) {
	rec := sbRec(0x11, 0x0010,
		t17Body(0x000B, 0, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0,
			4800, 3, 3, 0, 3, 0, 49152, 65535, 3),
		"DIMM 1", "BANK", "Unknown")
	prof := parseMemoryProfile(rec, "3.4")
	if len(prof.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(prof.Devices))
	}
	ptrNil(t, "out-of-range max cap", prof.Devices[0].MaxCapMTs)
	ptrNil(t, "below-range configured", prof.Devices[0].ConfiguredMTs)
	ptrEq(t, "spd still kept", prof.Devices[0].SPDMaxMTs, 4800)
}

// Identity discipline as a standalone pin: a record that carries
// serial and asset strings must validate AND neither string may be
// reachable anywhere in the profile.
func TestMemoryProfileNeverCarriesSerialOrAsset(t *testing.T) {
	table := sbRec(0x11, 0x0010,
		t17Body(0x000B, 0x000F, 64, 64, 0x7FFF, 0x09, 0, 1, 2, 0x22, 0x4080,
			4800, 3, 4, 5, 3, 0x02, 49152, 5600, 1100),
		"DIMM 1", "P0 CHANNEL A", "Unknown", "SERIAL-FAKE-LEAKCHECK", "ASSET-FAKE-LEAKCHECK")
	prof := parseMemoryProfile(table, "3.4")
	if len(prof.Devices) != 1 {
		t.Fatalf("fixture must validate: %d devices, rejected %d",
			len(prof.Devices), prof.RejectedCandidates)
	}
	blob := fmt.Sprintf("%+v %+v", prof, prof.Devices[0])
	for _, secret := range []string{"SERIAL-FAKE-LEAKCHECK", "ASSET-FAKE-LEAKCHECK"} {
		if strings.Contains(blob, secret) {
			t.Errorf("profile leaked identity string %q", secret)
		}
	}
}

func t4Body(socketIdx, familyIdx, mfrIdx, idIdx, verIdx, voltIdx byte, maxMHz uint16) []byte {
	body := make([]byte, 0x2C) // record length 0x30 (through 0x2F)
	body[0] = socketIdx        // 0x04 Socket (string)
	// body[1..8]: CPUID QWORD - binary bytes, deliberately zero
	body[9] = 0x01                                   // 0x0D Processor Type: Central Processor
	body[10] = familyIdx                             // 0x0E Family (string)
	body[12] = mfrIdx                                // 0x10 Manufacturer (string)
	body[13] = idIdx                                 // 0x11 Processor ID (string)
	body[14] = verIdx                                // 0x12 Version (string)
	body[15] = voltIdx                               // 0x13 Voltage (string)
	binary.LittleEndian.PutUint16(body[18:], maxMHz) // 0x16 Max Speed
	return body
}

func TestParseRatedProcessorMHz(t *testing.T) {
	good := sbRec(0x04, 0x001A, t4Body(1, 2, 2, 0, 3, 4, 4300),
		"AM5", "Advanced Micro Devices, Inc.", "AMD Ryzen 9 9950X", "0.V")
	t.Run("single record", func(t *testing.T) {
		mhz, err := parseRatedProcessorMHz(good)
		if err != nil || mhz != 4300 {
			t.Fatalf("got %d, %v; want 4300, nil", mhz, err)
		}
	})
	t.Run("agreeing records", func(t *testing.T) {
		mhz, err := parseRatedProcessorMHz(cat(good, good))
		if err != nil || mhz != 4300 {
			t.Fatalf("got %d, %v", mhz, err)
		}
	})
	t.Run("disagreement refuses", func(t *testing.T) {
		other := sbRec(0x04, 0x001B, t4Body(1, 2, 2, 0, 3, 4, 4400),
			"AM5", "Advanced Micro Devices, Inc.", "AMD Ryzen 9 9950X", "0.V")
		if _, err := parseRatedProcessorMHz(cat(good, other)); err == nil {
			t.Fatal("disagreeing records must refuse, not guess")
		}
	})
	t.Run("junk candidates ignored", func(t *testing.T) {
		// the kind of false candidates this machine's old walk met:
		// a 0x04 byte in string garbage with an implausible speed
		junk := sbRec(0x04, 0x4C04, t4Body(0, 0, 0, 0, 0, 0, 0))
		junk2 := sbRec(0x04, 0x006B, t4Body(1, 1, 1, 1, 1, 1, 28498), "garbage", "more garbage")
		mhz, err := parseRatedProcessorMHz(cat(junk, junk2, good))
		if err != nil || mhz != 4300 {
			t.Fatalf("junk filtered: got %d, %v", mhz, err)
		}
	})
	t.Run("padded table still parses", func(t *testing.T) {
		table := append(append([]byte{}, good...), 0, 0, 0) // NUL padding + noise
		mhz, err := parseRatedProcessorMHz(table)
		if err != nil || mhz != 4300 {
			t.Fatalf("padded: got %d, %v", mhz, err)
		}
	})
	t.Run("nothing publishable is an error", func(t *testing.T) {
		if _, err := parseRatedProcessorMHz([]byte{0x11, 0x22, 0x00, 0x01, 0x00, 0x00, 0x00}); err == nil {
			t.Fatal("want an error, not a zero-filled base")
		}
	})
}
