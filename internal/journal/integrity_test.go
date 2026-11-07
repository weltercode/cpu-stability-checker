package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The decoder must never silently swallow damage: every dropped line
// is counted, a truncated tail is flagged, and the valid records still
// come back so the story is known-incomplete rather than fake-clean.
func TestParseAllIssues(t *testing.T) {
	cases := []struct {
		name             string
		in               string
		wantRecs         int
		wantMalformed    int
		wantMissing      int
		wantUnterminated bool
		wantReadErr      bool
	}{
		{
			name:     "clean journal",
			in:       "{\"type\":\"run_start\",\"run_id\":\"r1\"}\n{\"type\":\"run_end\",\"run_id\":\"r1\",\"exit\":0}\n",
			wantRecs: 2,
		},
		{
			name:          "torn line in the middle is counted, not hidden",
			in:            "{\"type\":\"run_start\",\"run_id\":\"r1\"}\n{\"type\":\"core_don\",\"run_i\n{\"type\":\"run_end\",\"run_id\":\"r1\",\"exit\":0}\n",
			wantRecs:      2,
			wantMalformed: 1,
		},
		{
			name:        "record without type or run id is counted",
			in:          "{\"type\":\"run_start\",\"run_id\":\"r1\"}\n{\"core\":0,\"result\":\"ok\"}\n{\"type\":\"run_end\",\"run_id\":\"r1\",\"exit\":0}\n",
			wantRecs:    2,
			wantMissing: 1,
		},
		{
			name:             "writer died mid-append: tail is truncated",
			in:               "{\"type\":\"run_start\",\"run_id\":\"r1\"}\n{\"type\":\"run_end\",\"run_",
			wantRecs:         1,
			wantMalformed:    1,
			wantUnterminated: true,
		},
		{
			name:        "a line too long to be a record is a read error",
			in:          "{\"type\":\"run_start\",\"run_id\":\"r1\"}\n" + strings.Repeat("x", 70000) + "\n",
			wantRecs:    1,
			wantReadErr: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			recs, iss := ParseAll(strings.NewReader(tt.in), "test.journal.jsonl")
			if len(recs) != tt.wantRecs {
				t.Errorf("records = %d, want %d", len(recs), tt.wantRecs)
			}
			if iss.Malformed != tt.wantMalformed {
				t.Errorf("Malformed = %d, want %d", iss.Malformed, tt.wantMalformed)
			}
			if iss.MissingField != tt.wantMissing {
				t.Errorf("MissingField = %d, want %d", iss.MissingField, tt.wantMissing)
			}
			if iss.Unterminated != tt.wantUnterminated {
				t.Errorf("Unterminated = %v, want %v", iss.Unterminated, tt.wantUnterminated)
			}
			if (iss.ReadErr != "") != tt.wantReadErr {
				t.Errorf("ReadErr = %q, want set=%v", iss.ReadErr, tt.wantReadErr)
			}
			if tt.wantMalformed+tt.wantMissing > 0 && len(iss.Found) == 0 {
				t.Error("issues carry no provenance detail")
			}
			for _, pi := range iss.Found {
				if pi.Source != "test.journal.jsonl" || pi.Line < 1 {
					t.Errorf("issue provenance: %+v", pi)
				}
			}
		})
	}
}

// Every record this build writes carries the schema stamp; old records
// (no stamp) still parse and read as legacy.
func TestSchemaStamping(t *testing.T) {
	var buf bytes.Buffer
	j := NewWriter(&buf, "r1")
	j.RunStart("general", "", []int{0}, 1, "1s", false)
	j.RunEnd(0)
	recs := Parse(bytes.NewReader(buf.Bytes()))
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Schema != SchemaVersion {
			t.Errorf("record %s schema = %d, want %d", r.Type, r.Schema, SchemaVersion)
		}
	}
	rep := RecoverRuns(recs)
	if len(rep) != 1 || rep[0].Schema != SchemaVersion {
		t.Fatalf("recovered schema lost: %+v", rep)
	}

	// Legacy journal: no schema field anywhere -> the run is marked
	// pre-versioning and the report says so instead of claiming zeros.
	var out strings.Builder
	PrintReport(&out, RecoverRuns(Parse(strings.NewReader(
		`{"type":"run_start","run_id":"old","test":"general"}`+"\n"+
			`{"type":"run_end","run_id":"old","exit":0}`+"\n"))))
	if !strings.Contains(out.String(), "PRE-VERSIONING") {
		t.Errorf("legacy run not marked:\n%s", out.String())
	}
}

// A journal whose tail was torn by a dead writer must be delimited on
// open: the damaged line stays damaged and countable, and the next
// record still parses instead of gluing onto the torn bytes.
func TestOpenFileDelimitsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"run_start","run_id":"r0","test":"gen`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j := NewWriter(f, "r1")
	j.RunStart("general", "", []int{0}, 1, "1s", false)
	j.RunEnd(0)
	f.Close()

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	recs, iss := ParseAll(rf, path)
	if len(recs) != 2 {
		t.Errorf("new records = %d, want 2 (torn tail must not glue them): %v", len(recs), iss.Found)
	}
	if iss.Malformed != 1 {
		t.Errorf("torn tail not counted as damage: Malformed = %d", iss.Malformed)
	}
}
