package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// R24 (owner UX contract 2026-11-10): the findings inbox behind the red
// menu item. The scan re-reads the durable evidence every pass (newest
// first, "datetime · test type · finding"); findings.json stores ONLY
// the operator's read/dismiss flags; deleting dismisses the
// notification and must not touch one byte of the journals, manifests
// or receipts; and the menu badge never queries the System log - the
// OS story is asked for deliberately, inside one finding's detail.

// fixtureUnfinishedManifest drops one manifest stuck "running" (the
// shape a hard lockup leaves) with a crashed journal behind it, and
// returns the manifest file path.
func fixtureUnfinishedManifest(t *testing.T, id, started string) string {
	t.Helper()
	writeJournalFixture(t, id+".log", crashedRunLines("run-"+id, time.Now(), "hash-"+id)...)
	writeManifestFixture(t, sessionManifest{
		Version: manifestVersion, SessionID: id, Kind: "session",
		Started: started, State: manifestRunning,
		Runs: []manifestRun{{Name: "pass1", Log: id + ".log", Journal: journal.Path(id + ".log")}},
	})
	return filepath.Join(historyDir, id+".json")
}

func TestFindingsScanNewestFirstAndBadge(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	calls := setWitnessSeam(t, func(int) ([]whea.Event, error) {
		t.Error("the findings badge must never query the System log")
		return nil, nil
	})
	fixtureUnfinishedManifest(t, "2026-10-24T06-00-00Z-aaaaaaaaaaaaaaaa", "2026-10-24T06:00:00Z")
	fixtureUnfinishedManifest(t, "2026-10-25T07-51-15Z-bbbbbbbbbbbbbbbb", "2026-10-25T07:51:15Z")

	items := scanFindings()
	if len(items) != 2 {
		t.Fatalf("scan = %d findings, want the two stuck manifests", len(items))
	}
	// Owner's contract: newest first, dated, typed, with the finding.
	if !strings.HasPrefix(items[0].ID, "session:2026-10-25T07-51-15Z") ||
		!strings.HasPrefix(items[1].ID, "session:2026-10-24T06-00-00Z") {
		t.Errorf("findings not newest-first: %s then %s", items[0].ID, items[1].ID)
	}
	if items[0].When.IsZero() || findingWhen(items[0].When) != "2026-10-25 07:51Z" {
		t.Errorf("row date wrong: %q", findingWhen(items[0].When))
	}
	if !strings.Contains(items[0].Error, "state still \"running\"") {
		t.Errorf("finding line not honest: %q", items[0].Error)
	}

	if got := findingsUnreadCount(); got != 2 {
		t.Errorf("unread badge = %d, want 2", got)
	}
	line := findingsMenuItem(nil)
	if !strings.Contains(line, "7) View findings - 2 unread unfinished attempt(s)") {
		t.Errorf("menu item wrong: %q", line)
	}
	if *calls != 0 {
		t.Errorf("badge scanned the System log %d times, want zero", *calls)
	}

	markAllFindingsRead()
	if got := findingsUnreadCount(); got != 0 {
		t.Errorf("after mark-read badge = %d, want 0", got)
	}
	if line = findingsMenuItem(nil); !strings.Contains(line, "none unread") {
		t.Errorf("menu item after mark-read wrong: %q", line)
	}
	// Read flags hide the [new] mark only: the evidence is still
	// unfinished and the scan still lists the full truth.
	if len(scanFindings()) != 2 {
		t.Error("marking read changed the evidence scan")
	}
}

// The delete action dismisses the notification and must not rewrite a
// byte of the evidence: the manifest and journal bytes are compared
// before and after, and the durable scan still sees the stuck session.
func TestFindingsDeleteTouchesOnlyTheNotification(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) { return nil, nil })
	id := "2026-10-25T07-51-15Z-cccccccccccccccc"
	manifestPath := fixtureUnfinishedManifest(t, id, "2026-10-25T07:51:15Z")
	journalPath := journal.Path(id + ".log")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdoutAndRun(t, func() {
		// 7 = inbox, 1 = open newest, d = dismiss, q back, 0 quit.
		if code := menuLoop(bufio.NewReader(strings.NewReader("7\n1\nd\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"d) dismiss this notification (the on-disk evidence stays exactly as it is)",
		"Dismissed: this notification is gone",
		"all findings are dismissed",
		"7) View findings - none unread",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dismiss contract broken, missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2026-10-25 07:51Z · session · state still \"running\"") {
		t.Errorf("list row not 'datetime · type · finding':\n%s", out)
	}

	if got := findingsUnreadCount(); got != 0 {
		t.Errorf("dismissed finding still counted: %d", got)
	}
	if len(scanFindings()) != 1 {
		t.Error("delete rewrote the evidence - the stuck manifest vanished from the scan")
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil || string(manifestAfter) != string(manifestBefore) {
		t.Errorf("dismiss rewrote the manifest on disk (err %v)", err)
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil || string(journalAfter) != string(journalBefore) {
		t.Errorf("dismiss rewrote the journal on disk (err %v)", err)
	}
	b, err := os.ReadFile(findingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var f findingsFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("flag store unreadable: %v", err)
	}
	if f.Version != findingsFileVersion || len(f.Entries) != 1 ||
		f.Entries[0].ID != "session:"+id || !f.Entries[0].Deleted {
		t.Errorf("flag store wrong: %+v", f)
	}
}

// A corrupt flag store loses only operator flags: the findings
// RESURFACE and the screen says so - never the other direction.
func TestFindingsCorruptFlagStoreResurfaces(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) { return nil, nil })
	fixtureUnfinishedManifest(t, "2026-10-25T07-51-15Z-dddddddddddddddd", "2026-10-25T07:51:15Z")
	if err := os.WriteFile(findingsPath, []byte("this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findingsUnreadCount(); got != 1 {
		t.Errorf("corrupt store must mean 'nothing flagged' (resurface), got %d", got)
	}
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("7\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "is not readable") || !strings.Contains(out, "shown again") {
		t.Errorf("the corrupt flag store went unannounced:\n%s", out)
	}
}

// A clean directory: the inbox opens honestly ("none unread", "no
// evidence here"), and none of this ever queries the System log or
// writes a flag file.
func TestFindingsCleanDirectoryZeroQueryNoWrites(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	calls := setWitnessSeam(t, func(int) ([]whea.Event, error) {
		t.Error("a clean findings scan must never query the System log")
		return nil, nil
	})
	if got := findingsUnreadCount(); got != 0 {
		t.Errorf("clean directory badge = %d, want 0", got)
	}
	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("7\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"7) View findings - none unread",
		"no unfinished attempt evidence in this directory",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("clean inbox wrong, missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(findingsPath); !os.IsNotExist(err) {
		t.Error("opening an empty inbox wrote a flag file")
	}
	if *calls != 0 {
		t.Errorf("clean inbox queried the System log %d times, want zero", *calls)
	}
}

// The detail keeps every R21 honesty line, scoped to one finding: the
// journal recovery names the crashed cores, the OS records stay time
// candidates with the coverage bound, and reading states what it did
// NOT do.
func TestFindingsDetailKeepsTheHonestStory(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) { return nil, nil })
	id := "2026-10-25T07-51-15Z-eeeeeeeeeeeeeeee"
	fixtureUnfinishedManifest(t, id, "2026-10-25T07:51:15Z")

	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("7\n1\nq\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"finding: session:" + id,
		"journal " + journal.Path(id+".log") + ": run run-" + id,
		"core 5 (int): CRASHED - last seen",
		"does NOT mean the end was clean",
		"(reading changed nothing: these records were only read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detail lost the honest story, missing %q:\n%s", want, out)
		}
	}
}
