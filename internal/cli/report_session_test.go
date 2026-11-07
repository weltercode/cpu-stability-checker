package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// captureReport runs reportCmd capturing stdout (report output is
// small, so a plain pipe is enough - no concurrent drain needed).
func captureReport(t *testing.T, args []string) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := reportCmd(args)
	w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

// writeSessionJournal writes one complete run into a journal file
// (append mode lets several runs share one file, exactly like the
// shared default log of plain runs).
func writeSessionJournal(t *testing.T, path, runID, sessionID string, appendMode bool) {
	t.Helper()
	var f *os.File
	var err error
	if appendMode {
		f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	} else {
		f, err = os.Create(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	jw := journal.NewWriter(f, runID)
	jw.RunStart("general", "", []int{6}, 1, "30s", false)
	jw.RunMeta(journal.RunMetaFields{SessionID: sessionID, PlanHash: "h-" + runID})
	jw.CoreStart(0, "int")
	jw.CoreDone(0, journal.OK, "")
	jw.RunEnd(0)
	jw.Close()
}

// The core U02 promise: on a journal shared by several sessions,
// report -session aggregates ONLY the runs whose run_meta carries
// this session id; the others are counted and excluded, never
// merged into this session's totals.
func TestReportSessionAttributesByRunMeta(t *testing.T) {
	chdirTemp(t)
	jp := "shared.log.journal.jsonl"
	writeSessionJournal(t, jp, "run-mine", "S-1", false)
	writeSessionJournal(t, jp, "run-theirs", "S-other", true)
	writeSessionJournal(t, jp, "run-legacy", "", true) // predates attribution: unknown, never assumed

	m := &sessionManifest{Version: manifestVersion, SessionID: "S-1", Kind: "run",
		Started: "2026-10-17T00:00:00Z", State: "completed",
		Runs: []manifestRun{{Log: "shared.log", Journal: jp}}}
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}

	code, text := captureReport(t, []string{"-session", "S-1"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	if !strings.Contains(text, "1 run(s) attributed to this session") {
		t.Errorf("attribution line missing from:\n%s", text)
	}
	if !strings.Contains(text, "2 run(s) in the same file belong to other sessions or predate attribution") {
		t.Errorf("foreign runs not stated (they must be counted, not silently merged):\n%s", text)
	}

	code, text = captureReport(t, []string{"-session", "S-1", "-json"})
	if code != 0 {
		t.Fatalf("json exit = %d", code)
	}
	var got struct {
		SessionID  string           `json:"session_id"`
		Runs       int              `json:"attributed_runs"`
		RunDetails []runDetailJSON  `json:"run_details"`
		Cores      []reportCoreJSON `json:"cores"`
		Files      []struct {
			Status        string `json:"status"`
			Attributed    int    `json:"attributed_runs"`
			NotAttributed int    `json:"not_attributed_runs"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, text)
	}
	if got.SessionID != "S-1" || got.Runs != 1 || len(got.RunDetails) != 1 || got.RunDetails[0].RunID != "run-mine" {
		t.Errorf("json attribution wrong: %+v", got)
	}
	if len(got.Files) != 1 || got.Files[0].Status != "ok" || got.Files[0].Attributed != 1 || got.Files[0].NotAttributed != 2 {
		t.Errorf("json file view wrong: %+v", got.Files)
	}
	if len(got.Cores) != 1 || got.Cores[0].Runs != 1 {
		t.Errorf("cores aggregated over foreign runs: %+v", got.Cores)
	}
}

// A journal the manifest lists but the disk no longer has is said
// MISSING: the report stays honest about the hole and never
// substitutes another file for it (section 11).
func TestReportSessionMissingJournalStated(t *testing.T) {
	chdirTemp(t)
	m := &sessionManifest{Version: manifestVersion, SessionID: "S-2", Kind: "session",
		Started: "2026-10-17T00:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "pass 1", Log: "gone.log", Journal: filepath.Join(t.TempDir(), "gone.log.journal.jsonl")}}}
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
	code, text := captureReport(t, []string{"-session", "S-2"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (an honest empty report is still a report):\n%s", code, text)
	}
	if !strings.Contains(text, "MISSING") || !strings.Contains(text, "nothing is substituted") {
		t.Errorf("missing journal not stated as missing:\n%s", text)
	}
	if !strings.Contains(text, "no runs attributed to this session") {
		t.Errorf("empty aggregation not said out loud:\n%s", text)
	}
}

// No manifest, no report: the id is refused with its reason, never
// answered by guessing at some other session's journals.
func TestReportSessionUnknownIDRefused(t *testing.T) {
	chdirTemp(t)
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := reportCmd([]string{"-session", "nope"})
	w.Close()
	os.Stderr = old
	msg, _ := io.ReadAll(r)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, msg)
	}
}

// The session defines its own files: a positional journal path or a
// -diff comparison would silently mix scopes, so both are refused.
func TestReportSessionRejectsPathAndDiff(t *testing.T) {
	chdirTemp(t)
	m := &sessionManifest{Version: manifestVersion, SessionID: "S-3", Kind: "run",
		Started: "2026-10-17T00:00:00Z", State: "completed"}
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
	if code := reportCmd([]string{"-session", "S-3", "other.journal.jsonl"}); code != 2 {
		t.Errorf("session+path exit = %d, want 2", code)
	}
	if code := reportCmd([]string{"-session", "S-3", "-diff", "x.journal.jsonl"}); code != 2 {
		t.Errorf("session+diff exit = %d, want 2", code)
	}
}

// End to end: a real short runCmd produced a manifest and stamped
// its journal; report -session finds the run through that binding.
func TestReportSessionEndToEnd(t *testing.T) {
	chdirTemp(t)
	if code := runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
		"-cores", "0", "-control", "-1", "-log", "ok.log"}); code != 0 {
		t.Fatalf("runCmd exit = %d", code)
	}
	m := onlyManifest(t)
	code, text := captureReport(t, []string{"-session", m.SessionID, "-json"})
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, text)
	}
	var got struct {
		SessionID string           `json:"session_id"`
		State     string           `json:"state"`
		Runs      int              `json:"attributed_runs"`
		Cores     []reportCoreJSON `json:"cores"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, text)
	}
	if got.SessionID != m.SessionID || got.State != "completed" || got.Runs != 1 || len(got.Cores) == 0 {
		t.Errorf("session report wrong: %+v", got)
	}
}

// A crashed session's manifest still reports: state running reads as
// unfinished at read time (nothing is rewritten), and the runs its
// journals already stamped are still attributable.
func TestReportSessionCrashedManifest(t *testing.T) {
	chdirTemp(t)
	st, err := newSessionTracker("run", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	jp := journal.Path("crash.log")
	writeSessionJournal(t, jp, "run-half", st.m.SessionID, false)
	st.m.Runs = []manifestRun{{Log: "crash.log", Journal: jp}}
	if err := saveSessionManifest(&st.m); err != nil {
		t.Fatal(err)
	}
	// No finalize: the "process" died here.
	code, text := captureReport(t, []string{"-session", st.m.SessionID})
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, text)
	}
	if !strings.Contains(text, "unfinished") || !strings.Contains(text, "exit=unknown") {
		t.Errorf("crashed session dressed up as finished:\n%s", text)
	}
	if !strings.Contains(text, "1 run(s) attributed") {
		t.Errorf("the journal's stamped run should still be attributable:\n%s", text)
	}
	m, err := loadSessionManifest(st.m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != manifestRunning || m.Finished != "" {
		t.Errorf("the report rewrote or reinterpreted the manifest: %+v", m)
	}
}
