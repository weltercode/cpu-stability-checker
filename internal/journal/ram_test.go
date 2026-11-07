package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ram evidence must survive write -> read -> recover untouched: the
// capped details, the uncapped total, the truncation flag, and the
// per-finding fields the reproduction needs. A slot that ran no ram
// kernel must read back nil (unknown), never an empty "clean" record.
func TestRamEvidenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-ram")
	cores := []int{0, 1}
	jw.RunStart("general", "", cores, 1, "5s", true)
	jw.CoreStart(0, "ram-constant")
	jw.CoreStart(1, "float")

	phy := 3
	jw.CoreDoneRam(0, Fault, "ram mismatch seen", 5*time.Second, nil, "", "ram-constant-v1", &RamEvidence{
		Findings: []RamFinding{{
			Algorithm: "ram-constant-v1", Seed: 7, Pass: 2,
			Region: "worker=0 region=0 role=source offset=0", Offset: 4096,
			Expected: 0xAAAAAAAA, Actual: 0xAAAAAAAB, Xor: 1,
			LogicalCPU: 0, PhysicalCPU: &phy,
			Phase: "verify-fwd", When: "2026-10-16T12:00:00.000000000Z",
		}},
		Total:     5,
		Truncated: true,
	})
	jw.CoreDoneChecks(1, OK, "", 5*time.Second, nil, "", "float-repeated-v1")
	jw.RunEnd(1)
	jw.Close()

	records, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := RecoverRuns(records)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	byCore := map[int]*CoreOutcome{}
	for i := range runs[0].Cores {
		c := runs[0].Cores[i]
		byCore[c.Core] = &c
	}

	c0 := byCore[0]
	if c0 == nil || c0.Ram == nil {
		t.Fatalf("core 0 lost its ram evidence: %+v", c0)
	}
	ev := *c0.Ram
	if ev.Total != 5 || !ev.Truncated || len(ev.Findings) != 1 {
		t.Fatalf("evidence drifted: %+v", ev)
	}
	f0 := ev.Findings[0]
	if f0.Algorithm != "ram-constant-v1" || f0.Seed != 7 || f0.Pass != 2 || f0.Offset != 4096 {
		t.Errorf("finding identity drifted: %+v", f0)
	}
	if f0.Expected != 0xAAAAAAAA || f0.Actual != 0xAAAAAAAB || f0.Xor != 1 {
		t.Errorf("finding values drifted: %+v", f0)
	}
	if f0.Phase != "verify-fwd" || f0.When == "" || f0.LogicalCPU != 0 {
		t.Errorf("finding context drifted: %+v", f0)
	}
	if f0.PhysicalCPU == nil || *f0.PhysicalCPU != 3 {
		t.Errorf("physical cpu drifted: %v", f0.PhysicalCPU)
	}

	if c1 := byCore[1]; c1 == nil || c1.Ram != nil {
		t.Errorf("non-ram core must read nil evidence (unknown, not clean): %+v", c1)
	}

	// The human report must say the count is uncapped and the
	// details were capped - never present the 1 detailed as the whole
	// story.
	var buf bytes.Buffer
	PrintReport(&buf, runs)
	out := buf.String()
	for _, want := range []string{"ram mismatches: 5 observed", "1 detailed", "TRUNCATED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q in:\n%s", want, out)
		}
	}
}

// A clean ram run carries evidence with Total 0: "looked, saw
// nothing wrong" is a recorded result, distinct from the nil a
// non-ram slot reads back.
func TestRamEvidenceCleanRunDistinctFromUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-clean")
	jw.RunStart("general", "", []int{0}, 1, "5s", true)
	jw.CoreStart(0, "ram-constant")
	jw.CoreDoneRam(0, OK, "", 5*time.Second, nil, "", "ram-constant-v1", &RamEvidence{Total: 0})
	jw.RunEnd(0)
	jw.Close()

	records, _ := ReadAll(path)
	runs := RecoverRuns(records)
	if len(runs) != 1 || len(runs[0].Cores) != 1 {
		t.Fatalf("recovery lost the run: %+v", runs)
	}
	c := runs[0].Cores[0]
	if c.Ram == nil {
		t.Fatal("clean ram evidence vanished: nil reads as unknown, not clean")
	}
	if c.Ram.Total != 0 || len(c.Ram.Findings) != 0 || c.Ram.Truncated {
		t.Errorf("clean evidence drifted: %+v", c.Ram)
	}
}
