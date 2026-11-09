package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// R16: the provenance mapper must be a MIRROR. Every pointer it is
// given stays a pointer (nil = firmware did not report), a reported
// zero stays zero, and nothing is summed, averaged or reconciled on
// the way into the journal: an interpretation added here would be an
// interpretation the collector explicitly refused to make.
func TestMemoryProvenanceIsAMirror(t *testing.T) {
	mb, spd, maxcap, conf, arr := 49152, 4800, 5600, 1100, 262154
	zero := 0
	p := sysinfo.MemoryProfile{
		SMBIOSVersion:      "3.4",
		ArrayMaxCapacityMB: &arr,
		RejectedCandidates: 2,
		Devices: []sysinfo.MemoryDevice{
			{Locator: "DIMM 1", BankLocator: "P0 CHANNEL A", Manufacturer: "Unknown",
				PartNumber: "F5-5200J4040A48G",
				SizeMB:     &mb, SPDMaxMTs: &spd, MaxCapMTs: &maxcap, ConfiguredMTs: &conf},
			{Locator: "DIMM 0", BankLocator: "P0 CHANNEL B", SizeMB: &zero},
			{Locator: "DIMM 1", BankLocator: "P0 CHANNEL B"}, // nothing reported at all
		},
	}
	f := MemoryProvenance(p)
	if f == nil {
		t.Fatal("MemoryProvenance returned nil for an observed profile")
	}
	if f.SMBIOSVersion != "3.4" || f.RejectedCandidates != 2 {
		t.Errorf("header fields not mirrored: %+v", f)
	}
	if f.ArrayMaxCapacityMB == nil || *f.ArrayMaxCapacityMB != arr {
		t.Errorf("array capacity not mirrored: %v", f.ArrayMaxCapacityMB)
	}
	if len(f.Devices) != 3 {
		t.Fatalf("devices = %d, want all 3 records in table order", len(f.Devices))
	}
	d0 := f.Devices[0]
	if d0.Locator != "DIMM 1" || d0.BankLocator != "P0 CHANNEL A" ||
		d0.Manufacturer != "Unknown" || d0.PartNumber != "F5-5200J4040A48G" {
		t.Errorf("strings not mirrored: %+v", d0)
	}
	for _, tc := range []struct {
		name string
		got  *int
		want int
	}{
		{"size_mb", d0.SizeMB, 49152},
		{"spd_speed_mts", d0.SPDMaxMTs, 4800},
		{"max_speed_capability_mts", d0.MaxCapMTs, 5600},
		{"configured_clock_mts_as_written", d0.ConfiguredMTs, 1100},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s = %v, want %d", tc.name, tc.got, tc.want)
		}
	}
	if f.Devices[1].SizeMB == nil || *f.Devices[1].SizeMB != 0 {
		t.Errorf("a slot firmware reported empty must keep an explicit 0: %+v", f.Devices[1])
	}
	d2 := f.Devices[2]
	if d2.SizeMB != nil || d2.SPDMaxMTs != nil || d2.MaxCapMTs != nil || d2.ConfiguredMTs != nil {
		t.Errorf("unreported fields became numbers: %+v", d2)
	}

	// No interpretation smuggled in as a convenience field: nothing
	// summed, no "total", no derived "effective" rate.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"total", "effective", "running", "aggregate", "rated"} {
		if strings.Contains(strings.ToLower(string(b)), banned) {
			t.Errorf("mapper produced %q, which is an interpretation, not an observation: %s", banned, b)
		}
	}

	// Identity stays out of the record: there is no serial or asset
	// field anywhere in the shape (the collector never reads those
	// strings, so this is a structural pin, not a filter).
	for _, banned := range []string{"serial", "asset"} {
		if strings.Contains(strings.ToLower(string(b)), banned) {
			t.Errorf("memory provenance carries a %q field: %s", banned, b)
		}
	}
}

// An observed-but-empty profile is still an observation: it must
// arrive as a record that says so, not as an absent field a reader
// could mistake for "never looked".
func TestMemoryProvenanceKeepsEmptyProfile(t *testing.T) {
	f := MemoryProvenance(sysinfo.MemoryProfile{SMBIOSVersion: "2.8"})
	if f == nil {
		t.Fatal("an observed empty profile must not be dropped")
	}
	if f.Devices != nil {
		t.Errorf("invented devices: %+v", f.Devices)
	}
}

// The collector side: on every platform the answer is a record, and a
// platform that cannot look says why in the record. This is a live
// call (one read-only firmware query, no load), so it pins the shape
// rather than this machine's numbers.
func TestCollectMemoryProvenanceAlwaysRecords(t *testing.T) {
	f := CollectMemoryProvenance()
	if f == nil {
		t.Fatal("CollectMemoryProvenance returned nil: memory must never be silent in provenance")
	}
	if len(f.Devices) == 0 && f.UnknownReason == "" {
		t.Errorf("record says nothing about memory and names no reason: %+v", f)
	}
	if f.UnknownReason != "" && len(f.Devices) != 0 {
		t.Errorf("a refusal cannot also carry observations: %+v", f)
	}
}

// RunProvenance carries the memory block through the run_meta record
// (it rides alongside the build/config/machine identity it already
// carried).
func TestRunProvenanceCarriesMemory(t *testing.T) {
	m := RunProvenance(Options{RamSeed: 1})
	if m.Memory == nil {
		t.Fatal("run_meta provenance lost the memory block")
	}
	if len(m.Memory.Devices) == 0 && m.Memory.UnknownReason == "" {
		t.Errorf("memory block neither observes nor names a skip: %+v", m.Memory)
	}
}
