package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Per-process mode gives every writer its own file: the parent owns
// the main journal, each child owns its fragment. ReadAll stitches the
// story back together, and a registered fragment that produced nothing
// reads as a hole (Missing), never as silence.
func TestReadAllMergesFragments(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "j.jsonl")

	f, err := OpenFile(main)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "run-A")
	jw.RunStart("general", "", []int{0, 1, 2}, 1, "1s", true)
	jw.Sources([]int{0, 1, 2})

	if err := os.MkdirAll(FragmentsDir(main, "run-A"), 0o755); err != nil {
		t.Fatal(err)
	}
	// core 0 told its whole story in its own fragment:
	ff, err := OpenFile(FragmentPath(main, "run-A", 0))
	if err != nil {
		t.Fatal(err)
	}
	fj := NewWriter(ff, "run-A")
	fj.CoreStart(0, "int")
	fj.CoreDone(0, OK, "")
	ff.Close()
	// core 1 started in its fragment but never finished; the parent
	// marked it interrupted in the main file after the run-end scan:
	f1, err := OpenFile(FragmentPath(main, "run-A", 1))
	if err != nil {
		t.Fatal(err)
	}
	f1j := NewWriter(f1, "run-A")
	f1j.CoreStart(1, "int")
	f1.Close()
	jw.CoreDone(1, Interrupted, "worker stopped by parent")
	// core 2: registered, fragment never even opened: a hole.

	jw.RunEnd(1)
	f.Close()

	recs, iss := ReadAll(main)
	if !iss.Empty() {
		t.Errorf("clean fragment layout reported damage: %+v", iss.Found)
	}
	rep := RecoverRuns(recs)
	if len(rep) != 1 {
		t.Fatalf("runs = %d, want 1", len(rep))
	}
	r := rep[0]
	if len(r.Missing) != 1 || r.Missing[0] != 2 {
		t.Errorf("Missing = %v, want [2] (registered, never reported)", r.Missing)
	}
	byCore := map[int]CoreOutcome{}
	for _, c := range r.Cores {
		byCore[c.Core] = c
	}
	if c, ok := byCore[0]; !ok || c.Status != OK {
		t.Errorf("core 0 = %+v, want ok from its fragment", c)
	}
	// The fragment's core_start must not downgrade the parent's
	// interrupted ending to "crashed" just because fragments are read
	// after the main file.
	if c, ok := byCore[1]; !ok || c.Status != Interrupted {
		t.Errorf("core 1 = %+v, want interrupted (kept, not downgraded)", c)
	}

	var out strings.Builder
	PrintReport(&out, rep)
	if !strings.Contains(out.String(), "MISSING WORKER FRAGMENTS") {
		t.Errorf("missing fragment not stated loudly:\n%s", out.String())
	}
}

// A run whose parent died before opening the main journal still has a
// story: the fragments are readable on their own. Only when neither
// the main file nor any fragment exists is the journal truly absent.
func TestReadAllFragmentsWithoutMain(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "gone.journal.jsonl")
	if err := os.MkdirAll(FragmentsDir(main, "run-B"), 0o755); err != nil {
		t.Fatal(err)
	}
	ff, err := OpenFile(FragmentPath(main, "run-B", 3))
	if err != nil {
		t.Fatal(err)
	}
	fj := NewWriter(ff, "run-B")
	fj.CoreStart(3, "int")
	fj.CoreDone(3, OK, "")
	ff.Close()

	recs, iss := ReadAll(main)
	if !iss.Empty() {
		t.Errorf("fragments-only story flagged as damage: %+v", iss.Found)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want the two fragment records", len(recs))
	}
	rep := RecoverRuns(recs)
	if len(rep) != 1 || len(rep[0].Cores) != 1 || rep[0].Ended {
		t.Errorf("fragments-only recovery: %+v", rep)
	}

	// Nothing at all: the hard read error returns.
	if _, iss := ReadAll(filepath.Join(dir, "nothing.journal.jsonl")); iss.ReadErr == "" {
		t.Error("absent journal must report a read error")
	}
}
