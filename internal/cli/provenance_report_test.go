package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// The text report folds runs that share one provenance into a single
// line, and a journal without run_meta records says "unknown" - never
// a fabricated identity.
func TestPrintProvenance(t *testing.T) {
	seed := uint64(7)
	meta := &journal.RunMetaFields{ModuleVer: "v1", BuildRev: "r1", ConfigHash: "h1", Seed: &seed, CPUName: "cpu", OSVersion: "os"}
	var buf bytes.Buffer
	printProvenance(&buf, []journal.RunReport{
		{Meta: meta},
		{Meta: meta},
		{Meta: nil},
	})
	out := buf.String()
	if !strings.Contains(out, "rev r1") || !strings.Contains(out, "· 2 run(s)") {
		t.Errorf("identical provenance must fold with a run count:\n%s", out)
	}
	if !strings.Contains(out, "provenance: unknown (this journal predates run_meta records)") {
		t.Errorf("a run without provenance must read as unknown:\n%s", out)
	}
	if strings.Contains(out, `""`) {
		t.Errorf("empty provenance fields must print as unknown, not blank:\n%s", out)
	}

	// Empty journal: nothing rendered (no runs, no claim).
	buf.Reset()
	printProvenance(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("no runs must render nothing, got %q", buf.String())
	}
}

// A seed of 0 is a valid configured seed, not "unset": the provenance
// line must show 0, not "unknown".
func TestProvenanceSeedZeroIsShown(t *testing.T) {
	seed := uint64(0)
	var buf bytes.Buffer
	printProvenance(&buf, []journal.RunReport{{Meta: &journal.RunMetaFields{BuildRev: "r", Seed: &seed}}})
	out := buf.String()
	if !strings.Contains(out, "seed 0") {
		t.Errorf("seed 0 must be shown as 0, not unknown:\n%s", out)
	}
	// Missing parts (no config hash, no cpu/os) read as unknown.
	if !strings.Contains(out, "config sha256 unknown") || !strings.Contains(out, `cpu "unknown"`) {
		t.Errorf("missing provenance parts must read as unknown:\n%s", out)
	}
}
