package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManifest builds a minimal running manifest for tests.
func newTestManifest(id string) *sessionManifest {
	return &sessionManifest{
		Version:   manifestVersion,
		SessionID: id,
		Kind:      "run",
		Started:   time.Now().UTC().Format(time.RFC3339),
		State:     manifestRunning,
	}
}

// Two sessions started in the same second must stay distinct: the
// id is a UTC second plus 64 random bits (menu plan section 11).
func TestSessionIDSameSecondDistinct(t *testing.T) {
	a, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two ids collide: %q", a)
	}
	if len(a) != len("20060102-150405-")+16 || !strings.Contains(a, "-") {
		t.Errorf("id shape drifted: %q", a)
	}
}

// A manifest is created running and finalized once: the finalize
// rewrites its OWN file (same id) and fills the terminal fields.
// Until then Finished/Exit/Validation stay absent - a history record
// never invents a finish time or an exit code.
func TestManifestCreateThenFinalize(t *testing.T) {
	chdirTemp(t)
	m := newTestManifest("2026-10-17T000000Z-aaaa")
	m.Runs = []manifestRun{{Name: "pass 1", Log: "ok.log", Journal: "ok.log.journal.jsonl"}}
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
	got, err := loadSessionManifest(m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != manifestRunning || got.Finished != "" || got.Exit != nil || got.Validation != "" {
		t.Errorf("running manifest filled terminal fields: %+v", got)
	}

	exit := 0
	m.State = "completed"
	m.Finished = time.Now().UTC().Format(time.RFC3339)
	m.Exit = &exit
	m.Validation = "verified"
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
	got, err = loadSessionManifest(m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" || got.Finished == "" || got.Exit == nil || *got.Exit != 0 ||
		got.Validation != "verified" || len(got.Runs) != 1 {
		t.Errorf("finalize lost fields: %+v", got)
	}
	// One session, one file: finalize rewrote, it did not fork.
	entries, _ := os.ReadDir(historyDir)
	if len(entries) != 1 {
		t.Errorf("history holds %d files after finalize, want 1", len(entries))
	}
}

// One id, one session: a file whose content belongs to a different
// id is never overwritten by a new save.
func TestManifestCollisionRefused(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Crafted by hand: the filename says X, the content says Y.
	if err := os.WriteFile(manifestPath("X"),
		[]byte(`{"version":1,"session_id":"Y","kind":"run","started":"x","state":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionManifest(newTestManifest("X")); err == nil {
		t.Fatal("overwrote another session's manifest")
	}
}

// A corrupt manifest never breaks the list: it gets its own warning
// while every readable session stays visible, newest first. Temp
// files and strays are not counted as manifests.
func TestListIsolatesCorruption(t *testing.T) {
	chdirTemp(t)
	old := newTestManifest("20260101-000000-0000000000000001")
	mid := newTestManifest("20260601-000000-0000000000000002")
	// Same second as mid, different random part: both must survive.
	sameSec := newTestManifest("20260601-000000-ffffffffffffffff")
	for _, m := range []*sessionManifest{old, mid, sameSec} {
		if err := saveSessionManifest(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(historyDir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(historyDir, "20260601-000000-aaaaaaaaaaaa.json.tmp1"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, warns := listSessionManifests()
	if len(list) != 3 {
		t.Fatalf("list = %d manifests, want 3: %+v (warns %v)", len(list), list, warns)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "broken.json") {
		t.Errorf("corruption warnings = %v, want exactly broken.json", warns)
	}
	if list[0].SessionID[:8] != "20260601" || list[2].SessionID[:8] != "20260101" {
		t.Errorf("not newest-first: %s .. %s", list[0].SessionID, list[2].SessionID)
	}
	if list[0].SessionID == list[1].SessionID {
		t.Error("same-second sessions merged")
	}
}

// Reading history is read-only by contract: no manifest is
// rewritten, no file (even a corrupt one) is deleted.
func TestHistoryReadIsReadOnly(t *testing.T) {
	chdirTemp(t)
	if err := saveSessionManifest(newTestManifest("20260101-000000-0000000000000001")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(historyDir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	entries, _ := os.ReadDir(historyDir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(historyDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		before[e.Name()] = string(b)
	}

	listSessionManifests()
	listSessionManifests()

	after := map[string]string{}
	entries, _ = os.ReadDir(historyDir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(historyDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		after[e.Name()] = string(b)
	}
	if len(before) != len(after) {
		t.Fatalf("read changed the file set: %d -> %d", len(before), len(after))
	}
	for name, content := range before {
		if after[name] != content {
			t.Errorf("%s was rewritten by a history read", name)
		}
	}
}

// A manifest from a newer tool is reported, not reinterpreted; a
// missing history directory is simply no history, not an error.
func TestFutureVersionAndMissingDir(t *testing.T) {
	chdirTemp(t)
	if list, warns := listSessionManifests(); list != nil || warns != nil {
		t.Errorf("missing history dir: list=%v warns=%v", list, warns)
	}
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath("2099-1-1"),
		[]byte(`{"version":99,"session_id":"2099-1-1","kind":"run","started":"x","state":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	list, warns := listSessionManifests()
	if len(list) != 0 {
		t.Errorf("future manifest listed: %+v", list)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "newer tool") {
		t.Errorf("future manifest warning = %v", warns)
	}
}

// An anonymous history record is refused at the door: an empty id
// would silently merge sessions into one unnamed file.
func TestSaveWithoutIDRefused(t *testing.T) {
	chdirTemp(t)
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion, State: manifestRunning}); err == nil {
		t.Fatal("saved a manifest without a session id")
	}
}
