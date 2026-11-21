package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureArtifacts(t *testing.T, args []string) (int, string) {
	t.Helper()
	return captureAnswer(t, func() int { return artifactsCmd(args) })
}

// writeFile seeds one artifact with content (the inventory counts
// bytes; the content never needs to be a valid journal).
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Sol's discriminating check (R36 plan review): references are built
// from DECLARED manifest paths, never from paths that happen to exist
// on disk. A manifest that stores only the LOG name (its journal
// field unset) still references the derived journal - and the
// journal that IS on disk must come out referenced, not UNREFERENCED.
func TestArtifactsDerivedReferenceAndDeclaredAbsent(t *testing.T) {
	cwd := chdirTemp(t)
	writeFile(t, "x.log", "a log line\n")
	writeFile(t, "x.log.journal.jsonl", "{\"record\":1}\n")
	writeFile(t, filepath.Join("x.log.journal.jsonl.workers", "run-1", "core0.jsonl"), "{}\n")
	writeFile(t, filepath.Join("x.log.journal.jsonl.workers", "run-1", "core1.jsonl"), "{}\n{}\n")
	writeFile(t, filepath.Join("x.log.journal.jsonl.workers", "run-2", "core0.jsonl"), "{}\n")

	// Journal field deliberately EMPTY: only the log is stored, so the
	// journal reference only exists if the scan derives it.
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-1", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "run", Log: "x.log"}}})
	// A manifest that lists a journal this directory no longer holds:
	// the reference stands even though the file is gone.
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-2", Kind: "session",
		Started: "2026-10-16T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "gone", Journal: "gone.log.journal.jsonl"}}})
	// The second Sol trap (review 2026-11-17): a run entered with an
	// ABSOLUTE path stores absolute paths in its manifest. The file
	// stands right here under its bare name - matching must resolve
	// declared and observed spellings onto one identity, or the live
	// journal reads UNREFERENCED and both spellings become phantom
	// "declared absent" entries.
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-3", Kind: "run",
		Started: "2026-10-15T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "abs", Log: filepath.Join(cwd, "x.log")}}})

	code, text := captureArtifacts(t, nil)
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	// The group and its tree: one evidence story, counted once.
	if !strings.Contains(text, "journal x.log.journal.jsonl —") {
		t.Fatalf("journal group missing:\n%s", text)
	}
	if !strings.Contains(text, "workers tree x.log.journal.jsonl.workers — 2 run dir(s), 3 fragment(s)") {
		t.Errorf("fragment tree not counted as journal content:\n%s", text)
	}
	// The derived reference: the journal line's own block must carry
	// the session ids (not an UNREFERENCED label).
	li := strings.Index(text, "journal x.log.journal.jsonl —")
	block := text[li:min(li+500, len(text))]
	if !strings.Contains(block, "references: history manifest(s):") {
		t.Errorf("Sol trap sprung: a log-only manifest entry left the derived journal UNREFERENCED:\n%s", block)
	}
	for _, want := range []string{"S-1", "S-3"} {
		if !strings.Contains(block, want) {
			t.Errorf("reference %s lost (relative or absolute spelling must find the same artifact):\n%s", want, block)
		}
	}
	if strings.Contains(block, "UNREFERENCED") {
		t.Errorf("the derived journal was labeled UNREFERENCED:\n%s", block)
	}
	// The absolute spelling of a PRESENT file must never surface as
	// a phantom declared-absent entry.
	abs := filepath.Join(cwd, "x.log")
	if strings.Contains(text, "no longer holds: "+abs) || strings.Contains(text, "no longer holds: "+abs+journalSuffix) {
		t.Errorf("a live file's absolute spelling was reported as missing:\n%s", text)
	}
	// Declared-but-absent stays a named fact, and the reference stands.
	if !strings.Contains(text, "manifest S-2 lists a journal this directory no longer holds: gone.log.journal.jsonl") {
		t.Errorf("declared-absent journal not stated:\n%s", text)
	}

	// Read-only proof: the scan wrote nothing and touched nothing.
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"x.log", "x.log.journal.jsonl", "x.log.journal.jsonl.workers", "history"} {
		if !strings.Contains(joined, want) {
			t.Errorf("artifact %q vanished (the scan must not touch anything): %s", want, joined)
		}
	}
	if strings.Contains(joined, ".tmp") || strings.Contains(joined, ".lock") {
		t.Errorf("the scan created scratch files: %s", joined)
	}
}

// An unreferenced artifact is labeled with its LIMIT, never with a
// verdict: the screen must carry the "not a deletion-safety
// assessment" rule and must not speak the words that would imply one.
func TestArtifactsUnreferencedCarriesItsLimit(t *testing.T) {
	chdirTemp(t)
	writeFile(t, "solo.log.journal.jsonl", "{\"record\":1}\n")

	code, text := captureArtifacts(t, nil)
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	if !strings.Contains(text, "references: UNREFERENCED") {
		t.Errorf("unreferenced not labeled:\n%s", text)
	}
	if !strings.Contains(text, "NOT a deletion-safety assessment") {
		t.Errorf("the limit of UNREFERENCED is missing:\n%s", text)
	}
	for _, forbidden := range []string{"safe to remove", "can be deleted", "prune", "junk", "stale"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the screen speaks deletion vocabulary %q:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, "removes nothing") {
		t.Errorf("the read-only promise is missing:\n%s", text)
	}
}

// Trees without a main journal, loose logs, singletons that are
// simply absent (stated, never zeroed) and export directories are all
// rendered as observations with their unknowns named.
func TestArtifactsOrphanTreeSingletonsAndExports(t *testing.T) {
	chdirTemp(t)
	// A fragment tree whose main journal file is not here: the story
	// may still be complete INSIDE the fragments.
	writeFile(t, filepath.Join("z.log.journal.jsonl.workers", "run-9", "core0.jsonl"), "{}\n")
	writeFile(t, "loose.log", "no journal sibling\n")
	writeFile(t, filepath.Join("bug-report-test", "manifest.txt"), "bundle\n")
	writeFile(t, filepath.Join("bug-report-test", "journal.txt"), "scrubbed\n")

	code, text := captureArtifacts(t, nil)
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	for _, want := range []string{
		"fragment tree z.log.journal.jsonl.workers — main journal file NOT present here",
		"loose log loose.log —",
		"lastrun.json — not present",
		"lastresult.json — not present",
		"findings.json — not present",
		"history/ — 0 manifest(s)",
		"bug-report-test — 2 file(s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
}

// The machine answer keeps the same model: derived references,
// counted trees, the UNREFERENCED rule text, and totals labeled as
// scan-only.
func TestArtifactsJSONShape(t *testing.T) {
	chdirTemp(t)
	writeFile(t, "x.log", "log\n")
	writeFile(t, "x.log.journal.jsonl", "{\"record\":1}\n")
	writeFile(t, filepath.Join("x.log.journal.jsonl.workers", "run-1", "core0.jsonl"), "{}\n")
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-1", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Log: "x.log"}}})

	code, text := captureArtifacts(t, []string{"-json"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	var got artifactsScan
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, text)
	}
	if got.Schema != 1 || len(got.Groups) != 1 {
		t.Fatalf("shape wrong: %s", text)
	}
	g := got.Groups[0]
	if g.Journal != "x.log.journal.jsonl" || !g.Present || g.Workers == nil ||
		g.Workers.RunDirs != 1 || g.Workers.Fragments != 1 {
		t.Errorf("group wrong: %+v", g)
	}
	if len(g.Refs.Sessions) != 1 || g.Refs.Sessions[0] != "S-1" {
		t.Errorf("derived reference lost in JSON: %+v", g.Refs)
	}
	if got.UnrefRule == "" || len(got.Scope) == 0 || len(got.Rules) == 0 {
		t.Errorf("the honesty model must ride in the machine answer too")
	}
	if got.Totals.Groups != 1 || got.Totals.ObservedBytes == 0 {
		t.Errorf("totals wrong: %+v", got.Totals)
	}
}

// Setup refusals and the honest empty scan.
func TestArtifactsRefusalsAndEmpty(t *testing.T) {
	chdirTemp(t)
	if code, text := captureArtifacts(t, []string{"extra"}); code != 2 {
		t.Errorf("positional exit = %d, want 2:\n%s", code, text)
	}
	code, text := captureArtifacts(t, nil)
	if code != 0 {
		t.Fatalf("empty scan exit = %d, want 0 (an empty inventory is still an inventory):\n%s", code, text)
	}
	if !strings.Contains(text, "journal evidence: none found") {
		t.Errorf("empty history not stated:\n%s", text)
	}
}
