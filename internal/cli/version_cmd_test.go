package cli

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// version (queue R16) is informational: it answers on every machine,
// never starts a load, and never prints a blank or a zero where an
// observation is missing.
func TestVersionAlwaysAnswers(t *testing.T) {
	code, out := captureAnswer(t, func() int { return versionCmd(nil) })
	if code != 0 {
		t.Fatalf("versionCmd exit = %d, want 0 (identity answers even on a machine it knows nothing about):\n%s", code, out)
	}
	for _, want := range []string{"cpu-stability-checker", "build:", "runtime:", "cpu:", "os:", "bios:", "memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("version answer missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `""`) {
		t.Errorf("an unobserved identity field printed as an empty string instead of unknown:\n%s", out)
	}
	if !strings.Contains(out, "modified:") {
		t.Errorf("the build identity must state the modified (dirty-tree) flag:\n%s", out)
	}
}

// The answer also has to be reachable through the real dispatcher and
// documented in the usage text: an undocumented command is a command
// nobody can find, and a documented one that is not wired is a lie in
// the help.
func TestVersionIsDispatchedAndDocumented(t *testing.T) {
	code, out := captureAnswer(t, func() int { return Run([]string{"version", "-json"}) })
	if code != 0 {
		t.Fatalf("Run(version -json) exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, `"schema_version"`) {
		t.Errorf("dispatched -json did not answer with the envelope:\n%s", out)
	}
	_, usage := captureAnswer(t, func() int { printUsage(); return 0 })
	if !strings.Contains(usage, "cpu-stability version [-json]") {
		t.Errorf("usage text must document the version command:\n%s", usage)
	}
}

func TestVersionRefusesPositionals(t *testing.T) {
	if code := versionCmd([]string{"something-extra"}); code != 2 {
		t.Errorf("a stray positional = %d, want a refusal (2)", code)
	}
}

// The -json shape: one machine-readable document whose optional fields
// are absent when unobserved, whose memory block is always recorded,
// and which carries the reading caveat in the document itself (an
// automated consumer must not be the one place the observation/diagnosis
// line disappears).
func TestVersionJSONAnswerShape(t *testing.T) {
	code, out := captureAnswer(t, func() int { return versionCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("versionCmd(-json) exit = %d:\n%s", code, out)
	}
	var ans versionAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("stdout is not one valid JSON document: %v\n%s", err, out)
	}
	if ans.SchemaVersion != infoSchemaVersion {
		t.Errorf("schema_version = %d, want %d", ans.SchemaVersion, infoSchemaVersion)
	}
	if ans.GoVersion == "" || ans.GOOS == "" || ans.GOARCH == "" {
		t.Errorf("runtime identity must always be knowable to the binary: %+v", ans)
	}
	if ans.Reading == "" {
		t.Error("the JSON answer dropped the reading caveat")
	}
	if ans.Memory == nil {
		t.Fatal("memory block missing: provenance must never be silent about memory")
	}
	if len(ans.Memory.Devices) == 0 && ans.Memory.UnknownReason == "" {
		t.Errorf("memory block neither observes anything nor names a skip: %+v", ans.Memory)
	}
	// Structural identity discipline: the memory object cannot carry a
	// serial or asset field, because the collector never reads those
	// strings. (The document's prose says so explicitly; this pin is
	// about the DATA.)
	mb, err := json.Marshal(ans.Memory)
	if err != nil {
		t.Fatalf("marshal memory: %v", err)
	}
	for _, banned := range []string{"serial", "asset"} {
		if strings.Contains(strings.ToLower(string(mb)), banned) {
			t.Errorf("memory answer carries a %q field: %s", banned, mb)
		}
	}
}

// Rendering pinned against a synthetic firmware record (no live
// firmware needed): the three speed fields stay three separate
// observations, per-slot detail is visible, and the renderer never
// reaches for a diagnosis word.
func TestVersionHumanRendersObservationsNotDiagnoses(t *testing.T) {
	mb, spd, maxcap, conf, arr := 49152, 4800, 5600, 1100, 262154
	f := versionFacts{
		GoVer: "go1.27.0", GOOS: "windows", GOARCH: "amd64",
		Machine: sysinfo.MachineIdentity{CPUName: "Test CPU", OSVersion: "Test OS 1.0"},
		Memory: &journal.RunMemoryFields{
			SMBIOSVersion: "3.4", ArrayMaxCapacityMB: &arr, RejectedCandidates: 1,
			Devices: []journal.RunMemoryDevice{
				{Locator: "DIMM 1", BankLocator: "P0 CHANNEL A", Manufacturer: "G.Skill",
					PartNumber: "F5-5200J4040A48G",
					SizeMB:     &mb, SPDMaxMTs: &spd, MaxCapMTs: &maxcap, ConfiguredMTs: &conf},
			},
		},
	}
	_, out := captureAnswer(t, func() int { printVersionHuman(f); return 0 })
	for _, want := range []string{
		`cpu:      "Test CPU"`, "go1.27.0 windows/amd64",
		`slot "DIMM 1" bank "P0 CHANNEL A" G.Skill:`,
		"spd 4800", "max-cap 5600", "configured-as-written 1100",
		"1 malformed record(s) refused",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"running speed", "effective", "overclock",
		"mismatch", "above rating", "below rating"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("version output diagnosed instead of observing (%q):\n%s", banned, out)
		}
	}

	// A refused collector: the answer must name the reason.
	f2 := f
	f2.Memory = &journal.RunMemoryFields{UnknownReason: "SMBIOS not available"}
	_, out2 := captureAnswer(t, func() int { printVersionHuman(f2); return 0 })
	if !strings.Contains(out2, "memory unknown (SMBIOS not available)") {
		t.Errorf("a refused collector must be reported by name:\n%s", out2)
	}
	if strings.Contains(out2, "spd ") {
		t.Errorf("a refused collector left invented memory numbers:\n%s", out2)
	}

	// An unstamped binary says its identity is unrecoverable instead of
	// inventing one.
	f3 := f
	f3.Meta = journal.RunMetaFields{}
	f3.Memory = &journal.RunMemoryFields{UnknownReason: "no firmware table here"}
	_, out3 := captureAnswer(t, func() int { printVersionHuman(f3); return 0 })
	if !strings.Contains(out3, "no VCS stamp") {
		t.Errorf("an unstamped build must disclose that:\n%s", out3)
	}
	if !strings.Contains(out3, "rev unknown") || !strings.Contains(out3, "modified: unknown") {
		t.Errorf("unstamped build fields must print as unknown, never blanks:\n%s", out3)
	}
}

// The same answer has to be reachable from the menu (queue R16: an
// identity surface a user cannot find is a surface that will not get
// used when an evidence thread needs one).
func TestDiagnosticsMenuOffersIdentity(t *testing.T) {
	code, out := captureAnswer(t, func() int {
		return diagnosticsMenu(bufio.NewReader(strings.NewReader("8\nq\n")),
			&screenUI{}, config.Default(), nil)
	})
	if code != 0 {
		t.Fatalf("diagnostics menu exit = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "cpu-stability-checker") || !strings.Contains(out, "memory") {
		t.Errorf("menu item 8 must print the identity answer:\n%s", out)
	}
	if !strings.Contains(out, "as firmware describes it") {
		t.Errorf("the menu must label the item as a firmware observation, not a check:\n%s", out)
	}
}
