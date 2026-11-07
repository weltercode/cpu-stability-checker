package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMetaRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	jw := NewWriter(f, "rid-m1")
	jw.RunStart("general", "", []int{0}, 1, "5s", false)
	seed := uint64(0) // seed 0 is a valid seed, not "unset"
	modified := false
	jw.RunMeta(RunMetaFields{
		ModuleVer: "v1.2.3", BuildRev: "abc123", BuildTime: "2026-10-15T00:00:00Z",
		BuildModified: &modified, ConfigHash: "deadbeef", Seed: &seed,
		CPUName: "Test CPU", OSVersion: "Windows NT 10.0 build 1",
	})
	jw.RunEnd(0)
	f.Close()

	recs, iss := ReadAll(path)
	if !iss.Empty() {
		t.Fatalf("issues = %+v, want intact", iss)
	}
	runs := RecoverRuns(recs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	m := runs[0].Meta
	if m == nil {
		t.Fatal("recovered Meta = nil, want the provenance record")
	}
	if m.ModuleVer != "v1.2.3" || m.BuildRev != "abc123" || m.ConfigHash != "deadbeef" {
		t.Errorf("provenance build fields = %+v, want the written values", m)
	}
	if m.Seed == nil || *m.Seed != 0 {
		t.Errorf("provenance seed = %v, want an explicit 0 (valid seed, not unset)", m.Seed)
	}
	if m.BuildModified == nil || *m.BuildModified {
		t.Errorf("build_modified = %v, want explicit false", m.BuildModified)
	}

	var buf bytes.Buffer
	PrintReport(&buf, runs)
	out := buf.String()
	for _, want := range []string{"provenance: build v1.2.3 rev abc123 (modified: no)", "config sha256 deadbeef", "seed 0", `"Test CPU"`} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

// A journal without a run_meta record (any older run) must report
// provenance as unknown - never a blank, never a fabricated identity.
func TestRunMetaAbsentIsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	jw := NewWriter(f, "rid-m2")
	jw.RunStart("general", "", []int{0}, 1, "5s", false)
	jw.RunEnd(0)
	f.Close()

	recs, _ := ReadAll(path)
	runs := RecoverRuns(recs)
	if runs[0].Meta != nil {
		t.Fatalf("Meta = %+v, want nil for a journal without run_meta", runs[0].Meta)
	}
	var buf bytes.Buffer
	PrintReport(&buf, runs)
	if !strings.Contains(buf.String(), "provenance: unknown") {
		t.Errorf("report must say provenance is unknown:\n%s", buf.String())
	}
}

// BuildProvenance must not invent a stamp for a test binary built
// without VCS stamping semantics it cannot observe: whatever it
// returns, empty fields stay empty (unknown), and the call never
// panics.
func TestBuildProvenanceNeverPanics(t *testing.T) {
	m := BuildProvenance()
	if m.ModuleVer == "" && m.BuildRev == "" && m.ConfigHash != "" {
		t.Error("ConfigHash filled by BuildProvenance, which cannot know the config")
	}
}
