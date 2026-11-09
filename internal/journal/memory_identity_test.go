package journal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// R16: the memory record is an OBSERVATION log. Every speed SMBIOS
// wrote is kept under its own name, unreconciled, because real
// firmware contradicts itself (this dev board writes SPD 4800, max
// capability 5600 and a voltage-like 1100 in the slot the spec calls
// "configured clock speed"). The renderer must never pick one and it
// must never diagnose.
func memoryFixture() *RunMemoryFields {
	mb := 49152
	spd, maxcap, conf := 4800, 5600, 1100
	zero := 0
	arr := 262154
	dev := func(loc, bank string, size *int) RunMemoryDevice {
		d := RunMemoryDevice{
			Locator: loc, BankLocator: bank, Manufacturer: "Unknown",
			PartNumber: "F5-5200J4040A48G",
			SizeMB:     size, SPDMaxMTs: &spd, MaxCapMTs: &maxcap, ConfiguredMTs: &conf,
		}
		if size != nil && *size == 0 {
			d.PartNumber = "Unknown"
			d.SPDMaxMTs, d.MaxCapMTs, d.ConfiguredMTs = nil, nil, nil
		}
		return d
	}
	return &RunMemoryFields{
		SMBIOSVersion:      "3.4",
		ArrayMaxCapacityMB: &arr,
		Devices: []RunMemoryDevice{
			dev("DIMM 0", "P0 CHANNEL A", &zero),
			dev("DIMM 1", "P0 CHANNEL A", &mb),
			dev("DIMM 0", "P0 CHANNEL B", &zero),
			dev("DIMM 1", "P0 CHANNEL B", &mb),
		},
	}
}

func TestMemoryObservationsRenderVerbatim(t *testing.T) {
	line := FormatMemoryObservations(memoryFixture())
	for _, want := range []string{
		"memory", "SMBIOS 3.4", "array max 262154 MB", "4 slot record(s)",
		"2× 49152 MB", `part "F5-5200J4040A48G"`,
		"spd 4800", "max-cap 5600", "configured-as-written 1100",
		"2× reported empty",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("memory line missing %q:\n%s", want, line)
		}
	}
	// No diagnosis vocabulary: the renderer states values, it does not
	// adjudicate them. If a future edit starts calling one number the
	// "running speed", this pin is the thing that catches it.
	for _, banned := range []string{"running speed", "effective", "above", "below",
		"overclock", "mismatch", "inconsistent", "rated"} {
		if strings.Contains(strings.ToLower(line), banned) {
			t.Errorf("memory line uses diagnosis word %q:\n%s", banned, line)
		}
	}
}

// Folding must never swallow a difference: two slots that disagree
// render as separate groups, and the counts say how many of each.
func TestMemoryFoldingKeepsDisagreementVisible(t *testing.T) {
	m := memoryFixture()
	other := 5600
	m.Devices[2].SizeMB = &other
	m.Devices[2].PartNumber = "F5-6000OTHER"
	line := FormatMemoryObservations(m)
	if !strings.Contains(line, "5600 MB part \"F5-6000OTHER\"") {
		t.Errorf("differing slot must render as its own group:\n%s", line)
	}
	if !strings.Contains(line, "2× 49152 MB") || !strings.Contains(line, "reported empty") {
		t.Errorf("counts of the matching groups must survive:\n%s", line)
	}
	if strings.Contains(line, "3× reported empty") {
		t.Errorf("folding hid the differing slot:\n%s", line)
	}
}

// Unknown-safe rendering: an unobserved value never prints as a number
// the machine never reported, while a reported zero (an empty slot) is
// a fact and must not be hidden behind "unknown".
func TestMemoryObservationsUnknownSafe(t *testing.T) {
	if got := FormatMemoryObservations(nil); got != "memory unknown (not observed by this run)" {
		t.Errorf("nil memory = %q, want the named unknown", got)
	}
	refused := FormatMemoryObservations(&RunMemoryFields{UnknownReason: "SMBIOS not available"})
	if refused != "memory unknown (SMBIOS not available)" {
		t.Errorf("refusal must name its reason, got %q", refused)
	}
	empty := FormatMemoryObservations(&RunMemoryFields{SMBIOSVersion: "2.8"})
	if !strings.Contains(empty, "no memory device records published") {
		t.Errorf("looked-and-found-nothing must say so, got %q", empty)
	}

	unknown := FormatMemoryDevice(RunMemoryDevice{Locator: "DIMM 0"})
	for _, want := range []string{"size unknown", "spd unknown", "max-cap unknown",
		"configured-as-written unknown", "part unknown"} {
		if !strings.Contains(unknown, want) {
			t.Errorf("unobserved field missing %q in %q", want, unknown)
		}
	}
	if strings.Contains(unknown, " 0 ") || strings.HasSuffix(unknown, "0 MT/s") {
		t.Errorf("unknown must not print as a zero-valued measurement: %q", unknown)
	}
	zero := 0
	filled := FormatMemoryDevice(RunMemoryDevice{SizeMB: &zero})
	if !strings.Contains(filled, "reported empty") {
		t.Errorf("a reported-empty slot (size 0) is a fact, not unknown: %q", filled)
	}
}

// Refused malformed records are disclosed, and only when there are any.
func TestMemoryObservationsDiscloseRefusedRecords(t *testing.T) {
	m := memoryFixture()
	if strings.Contains(FormatMemoryObservations(m), "refused") {
		t.Errorf("no refused records must be claimed:\n%s", FormatMemoryObservations(m))
	}
	m.RejectedCandidates = 3
	if line := FormatMemoryObservations(m); !strings.Contains(line, "3 malformed record(s) refused") {
		t.Errorf("refused candidates must be disclosed:\n%s", line)
	}
}

// The provenance line carries the memory segment in every case,
// including when there is nothing to carry: silence about memory reads
// as "the machine has none", which is a different claim.
func TestProvenanceLineCarriesMemory(t *testing.T) {
	var buf bytes.Buffer
	printMetaLine := func(m *RunMetaFields) string {
		buf.Reset()
		PrintReport(&buf, []RunReport{{RunID: "r", Test: "general", Ended: true, Exit: 0, Meta: m}})
		return buf.String()
	}
	if out := printMetaLine(&RunMetaFields{}); !strings.Contains(out, "memory unknown (not observed by this run)") {
		t.Errorf("a run_meta without memory must say unknown, not stay silent:\n%s", out)
	}
	m := memoryFixture()
	out := printMetaLine(&RunMetaFields{Memory: m})
	if !strings.Contains(out, "memory · SMBIOS 3.4") || !strings.Contains(out, "configured-as-written 1100") {
		t.Errorf("provenance line must carry the memory observations:\n%s", out)
	}
}

// Append-only schema discipline: the memory block rides through the
// wire format with its pointers intact. A reported zero must survive
// as 0 (omitempty on a pointer omits only nil), and the three speed
// fields keep separate keys - a merged key would be an interpretation
// smuggled into the storage layer.
func TestRunMetaMemoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	jw := NewWriter(f, "rid-mem1")
	jw.RunStart("general", "", []int{0}, 1, "5s", false)
	jw.RunMeta(RunMetaFields{BuildRev: "abc", Memory: memoryFixture()})
	jw.RunEnd(0)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, key := range []string{`"memory"`, `"smbios_version"`, `"array_max_capacity_mb"`,
		`"size_mb"`, `"spd_speed_mts"`, `"max_speed_capability_mts"`,
		`"configured_clock_mts_as_written"`, `"bank_locator"`} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Errorf("journal line missing key %s:\n%s", key, raw)
		}
	}
	if bytes.Contains(raw, []byte(`"unknown_reason"`)) {
		t.Errorf("unknown_reason must be absent when memory was observed:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte(`"size_mb":0`)) {
		t.Errorf("an empty slot's reported 0 must survive, not vanish:\n%s", raw)
	}

	recs, iss := ReadAll(path)
	if !iss.Empty() {
		t.Fatalf("issues = %+v, want intact", iss)
	}
	runs := RecoverRuns(recs)
	if len(runs) != 1 || runs[0].Meta == nil || runs[0].Meta.Memory == nil {
		t.Fatalf("memory block did not survive recovery: %+v", runs[0].Meta)
	}
	got := runs[0].Meta.Memory
	if got.SMBIOSVersion != "3.4" || len(got.Devices) != 4 {
		t.Fatalf("memory block changed in transit: %+v", got)
	}
	for _, d := range got.Devices {
		if d.SizeMB == nil {
			t.Errorf("slot %q lost its size pointer", d.Locator)
			continue
		}
		if *d.SizeMB == 0 {
			if d.SPDMaxMTs != nil || d.MaxCapMTs != nil || d.ConfiguredMTs != nil {
				t.Errorf("empty slot gained speeds it never reported: %+v", d)
			}
			continue
		}
		if *d.SPDMaxMTs != 4800 || *d.MaxCapMTs != 5600 || *d.ConfiguredMTs != 1100 {
			t.Errorf("speed triple changed in transit: %+v", d)
		}
	}
}

// The refusal shape round-trips too: a run on a platform without a
// collector still leaves an evidence record that names why.
func TestRunMetaMemoryRefusalRoundTrip(t *testing.T) {
	b, err := json.Marshal(RunMetaFields{Memory: &RunMemoryFields{UnknownReason: "SMBIOS firmware table not available on this platform"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(b, []byte(`"unknown_reason"`)) || bytes.Contains(b, []byte(`"devices"`)) {
		t.Errorf("refusal record shape wrong: %s", b)
	}
	var back RunMetaFields
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Memory == nil || back.Memory.UnknownReason == "" {
		t.Fatalf("refusal reason lost: %+v", back.Memory)
	}
	if !strings.Contains(FormatMemoryObservations(back.Memory), "not available on this platform") {
		t.Errorf("refusal reason must stay readable: %+v", back.Memory)
	}
}
