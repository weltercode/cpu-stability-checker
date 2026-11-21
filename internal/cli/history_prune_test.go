package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// R37 (owner decision 2026-11-18): history is cleanable - automatic
// retention by config history_keep, and an explicit clear from the
// history view. These tests pin the fail-closed half: what must NEVER
// be touched, and every removal stated out loud.

func seedSession(t *testing.T, id, log string) {
	t.Helper()
	if log != "" {
		writeFile(t, log, "a log line\n")
		writeFile(t, journal.Path(log), `{"record":1}`+"\n")
		writeFile(t, filepath.Join(journal.Path(log)+workersSuffix, "run-a", "core0.jsonl"), "{}\n")
	}
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: id,
		Kind: "run", Started: "2026-01-01T00:00:00Z", State: "completed",
		Runs: func() []manifestRun {
			if log == "" {
				return nil
			}
			return []manifestRun{{Name: "r", Log: log}}
		}()})
}

func manifestExists(id string) bool {
	_, err := os.Stat(manifestPath(id))
	return err == nil
}

// The owner's shape: keep the last N records AND their whole evidence
// bundles; older sessions lose manifest + log + journal + fragments.
func TestRetentionKeepsLastNWithBundles(t *testing.T) {
	chdirTemp(t)
	writeDefaultConfig(t, `{"history_keep":2}`)
	seedSession(t, "20260101-000001-0000000000000001", "s1.log")
	seedSession(t, "20260101-000002-0000000000000002", "s2.log")
	seedSession(t, "20260101-000003-0000000000000003", "s3.log")

	pruneHistory()

	// The oldest record and its whole bundle are gone.
	for _, gone := range []string{"s1.log", "s1.log" + journalSuffix,
		"s1.log" + journalSuffix + workersSuffix,
		manifestPath("20260101-000001-0000000000000001")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("oldest bundle member still there: %s", gone)
		}
	}
	// The two kept records keep every file.
	for _, keep := range []string{"s2.log", "s3.log"} {
		for _, f := range []string{keep, keep + journalSuffix, keep + journalSuffix + workersSuffix} {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("kept bundle member missing: %s (%v)", f, err)
			}
		}
	}
	for _, id := range []string{"20260101-000002-0000000000000002", "20260101-000003-0000000000000003"} {
		if !manifestExists(id) {
			t.Errorf("kept manifest missing: %s", id)
		}
	}
}

// A shared log (and its derived journal/tree) survives when a newer
// kept record references the same files.
func TestRetentionSharedEvidenceSurvivesYounger(t *testing.T) {
	chdirTemp(t)
	writeDefaultConfig(t, `{"history_keep":1}`)
	seedSession(t, "20260101-000001-0000000000000001", "shared.log")
	seedSession(t, "20260101-000002-0000000000000002", "shared.log")

	pruneHistory()

	if manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("the older record should have been removed")
	}
	for _, f := range []string{"shared.log", "shared.log" + journalSuffix, "shared.log" + journalSuffix + workersSuffix} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("shared evidence removed while a kept session still names it: %s", f)
		}
	}
}

// Fail-closed policy vocabulary: unknown policy NEVER deletes.
func TestRetentionPolicyNeverDeletesUnderUnknownPolicy(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{"zero is not keep-nothing", `{"history_keep":0}`},
		{"below the vocabulary", `{"history_keep":-5}`},
		{"broken config", "{not json"},
		{"unlimited", `{"history_keep":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			writeDefaultConfig(t, tc.config)
			ids := []string{"20260101-000001-0000000000000001", "20260101-000002-0000000000000002",
				"20260101-000003-0000000000000003", "20260101-000004-0000000000000004",
				"20260101-000005-0000000000000005"}
			for i, id := range ids {
				seedSession(t, id, fmt.Sprintf("s%d.log", i+1))
			}
			pruneHistory()
			for i, id := range ids {
				if !manifestExists(id) {
					t.Errorf("record %s removed under policy %q", id, tc.config)
				}
				if _, err := os.Stat(fmt.Sprintf("s%d.log", i+1)); err != nil {
					t.Errorf("evidence removed under policy %q: s%d.log", tc.config, i+1)
				}
			}
		})
	}
}

// No config.json at all: the DEFAULT (last 3) applies and discloses
// itself as the default in its own right - and it still prunes.
func TestRetentionDefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	for i := 1; i <= 5; i++ {
		seedSession(t, fmt.Sprintf("20260101-00000%d-000000000000000%d", i, i), fmt.Sprintf("s%d.log", i))
	}
	pruneHistory()
	for i := 1; i <= 2; i++ { // the two oldest fall to the default keep-3
		if manifestExists(fmt.Sprintf("20260101-00000%d-000000000000000%d", i, i)) {
			t.Errorf("default retention did not prune s%d", i)
		}
	}
	for i := 3; i <= 5; i++ {
		if !manifestExists(fmt.Sprintf("20260101-00000%d-000000000000000%d", i, i)) {
			t.Errorf("default retention pruned too much (s%d)", i)
		}
		if _, err := os.Stat(fmt.Sprintf("s%d.log", i)); err != nil {
			t.Errorf("kept session lost its evidence (s%d)", i)
		}
	}
}

// An unreadable record is never counted, never auto-removed, and it
// blocks the evidence it names: ownership unproven, nothing touched.
func TestRetentionUnreadableNeverCountedAndBlocksOwnership(t *testing.T) {
	chdirTemp(t)
	writeDefaultConfig(t, `{"history_keep":1}`)
	seedSession(t, "20260101-000001-0000000000000001", "blocked.log")
	seedSession(t, "20260101-000002-0000000000000002", "kept.log")
	// The corrupt record names blocked.log too (invalid JSON on
	// purpose): blocked.log AND its derived journal/tree stay.
	corrupt := filepath.Join(historyDir, "20260101-000009-0000000000000009.json")
	writeFile(t, corrupt, `{"version":1,"session_id":"20260101-000009-0000000000000009","runs":[{"log":"blocked.log"}]},oops`)

	pruneHistory()

	if !manifestExists("20260101-000009-0000000000000009") {
		t.Errorf("an unreadable record was auto-removed")
	}
	for _, f := range []string{"blocked.log", "blocked.log" + journalSuffix, "blocked.log" + journalSuffix + workersSuffix} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("unreadable record may own it, yet it was removed: %s", f)
		}
	}
	// The readable victim's own manifest is still pruned (it IS owned
	// and readable); only its evidence is blocked.
	if manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("the readable older record should still be pruned")
	}
	// The corrupt file must not count toward the retention count:
	// kept.log's session (1 readable kept of 2 readable) untouched.
	if !manifestExists("20260101-000002-0000000000000002") {
		t.Errorf("unreadable file stole a retention slot")
	}
}

// A workers tree only gets RemoveAll when it matches the public
// shape exactly. Deeply nested entries or stray files at the tree
// root keep the WHOLE tree (RemoveAll would walk blindly), while the
// rest of that session's bundle is still removed.
func TestRetentionTreeShapeGuard(t *testing.T) {
	chdirTemp(t)
	writeDefaultConfig(t, `{"history_keep":1}`)
	// v1: a subdirectory below the run level (unexpected).
	seedSession(t, "20260101-000001-0000000000000001", "v1.log")
	writeFile(t, filepath.Join("v1.log"+journalSuffix+workersSuffix, "run-a", "sub", "deep.jsonl"), "{}\n")
	// v2: a stray file at the tree root (unexpected).
	seedSession(t, "20260101-000002-0000000000000002", "v2.log")
	writeFile(t, filepath.Join("v2.log"+journalSuffix+workersSuffix, "stray.txt"), "not a fragment\n")
	seedSession(t, "20260101-000003-0000000000000003", "kept.log")

	pruneHistory()

	for _, gone := range []string{"v1.log", "v1.log" + journalSuffix, "v2.log", "v2.log" + journalSuffix} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("bundle member not removed: %s", gone)
		}
	}
	for _, stay := range []string{
		filepath.Join("v1.log"+journalSuffix+workersSuffix, "run-a", "sub", "deep.jsonl"),
		filepath.Join("v2.log"+journalSuffix+workersSuffix, "stray.txt")} {
		if _, err := os.Stat(stay); err != nil {
			t.Errorf("tree with unexpected shape must stay whole, lost: %s (%v)", stay, err)
		}
	}
	if !manifestExists("20260101-000003-0000000000000003") {
		t.Errorf("the kept session lost its record")
	}
}

// Declared paths outside the working directory are left exactly where
// they are - pruning never reaches into other places.
func TestRetentionOutsidePathStays(t *testing.T) {
	chdirTemp(t)
	far := filepath.Join(t.TempDir(), "far.log")
	if err := os.WriteFile(far, []byte("not ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDefaultConfig(t, `{"history_keep":1}`)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion,
		SessionID: "20260101-000001-0000000000000001", Kind: "run",
		Started: "2026-01-01T00:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "far", Log: far}}})
	seedSession(t, "20260101-000002-0000000000000002", "near.log")

	pruneHistory()

	if _, err := os.Stat(far); err != nil {
		t.Errorf("a declared outside path was touched: %v", err)
	}
	if manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("the outside-declaring record itself should still be pruned")
	}
}

// Sol's discriminating trap: a link/junction inside the root pointing
// OUT must never be followed into - every external byte stays intact.
// Windows junctions (no privileges needed) exercise the pin here;
// other platforms use symlinks and skip honestly if refused.
func TestRetentionNeverFollowsLinksOutOfRoot(t *testing.T) {
	chdirTemp(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.log")
	if err := os.WriteFile(sentinel, []byte("must survive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", "extlink", outside).CombinedOutput()
		if jerr != nil {
			t.Skipf("junction creation refused on this machine (%v: %s)", jerr, out)
		}
	} else if serr := os.Symlink(outside, "extlink"); serr != nil {
		t.Skipf("this machine refuses unprivileged link creation (%v): the physical-escape guard stays pinned only where the OS allows it", serr)
	}
	writeDefaultConfig(t, `{"history_keep":1}`)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion,
		SessionID: "20260101-000001-0000000000000001", Kind: "run",
		Started: "2026-01-01T00:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "l", Log: filepath.Join("extlink", "sentinel.log")}}})
	seedSession(t, "20260101-000002-0000000000000002", "near.log")

	pruneHistory()

	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("retention followed a link out of the root and touched an external file: %v", err)
	}
}

// Sol's discriminating check (implementation review 2026-11-18): a
// directory ALIAS inside the root - a junction pointing at "." -
// gives every protected file a fresh lexical spelling while it stays
// physically itself. Protection must follow the PHYSICAL identity:
// a manifest naming "alias/config.json" must never delete the
// configuration. Windows junctions need no privileges, so this runs
// on machines where the symlink pin skips.
func TestProtectionFollowsAliasInsideRoot(t *testing.T) {
	chdirTemp(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		out, jerr := exec.Command("cmd", "/c", "mklink", "/J", "alias", cwd).CombinedOutput()
		if jerr != nil {
			t.Skipf("junction creation refused on this machine (%v: %s)", jerr, out)
		}
	} else if serr := os.Symlink(cwd, "alias"); serr != nil {
		t.Skipf("this machine refuses link creation (%v)", serr)
	}
	writeDefaultConfig(t, `{"history_keep":1}`)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion,
		SessionID: "20260101-000001-0000000000000001", Kind: "run",
		Started: "2026-01-01T00:00:00Z", State: "completed",
		Runs: []manifestRun{{Name: "alias", Log: filepath.Join("alias", "config.json")}}})
	seedSession(t, "20260101-000002-0000000000000002", "near.log")

	pruneHistory()

	if _, err := os.Stat(config.DefaultPath); err != nil {
		t.Errorf("config.json was deleted through an alias (physical protection bypassed): %v", err)
	}
}

// The hook: pruning fires from a SUCCESSFUL finalize - never from
// started/updated saves. Same-second ids keep only one record total
// (which one is id-order dependent; the COUNT is the pin).
func TestFinalizeTriggersRetention(t *testing.T) {
	chdirTemp(t)
	writeDefaultConfig(t, `{"history_keep":1}`)
	st1, err := newSessionTracker("run", nil, []manifestRun{{Name: "a", Log: "t1.log"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, "t1.log", "one\n")
	writeFile(t, "t1.log"+journalSuffix, "{}\n")
	st1.finish(0, nil)

	st2, err := newSessionTracker("run", nil, []manifestRun{{Name: "b", Log: "t2.log"}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, "t2.log", "two\n")
	writeFile(t, "t2.log"+journalSuffix, "{}\n")
	st2.finish(0, nil)

	list, _ := listSessionManifests()
	if len(list) != 1 {
		t.Fatalf("finalize did not trigger retention: %d records, want 1", len(list))
	}
	// Exactly one evidence set survives, whichever id won the order:
	// the pruned session's files must have gone with its manifest.
	one := func(set string) bool {
		_, err := os.Stat(set)
		return err == nil
	}
	if one("t1.log") == one("t2.log") {
		t.Errorf("evidence sets do not follow the records: t1=%v t2=%v", one("t1.log"), one("t2.log"))
	}
}

// CLI clear: the preview is a named refusal, -yes is the second word
// of the decision, and the read views never combine with it.
func TestClearCLIRefusalsAndConfirmation(t *testing.T) {
	chdirTemp(t)
	seedSession(t, "20260101-000001-0000000000000001", "c1.log")
	seedSession(t, "20260101-000002-0000000000000002", "")
	writeDefaultConfig(t, `{"history_keep":3}`)
	for _, s := range []string{lastrunPath, lastResultPath, findingsPath} {
		writeFile(t, s, `{"keep":"me"}`)
	}
	// A corrupt record and a crashed writer's temp file: the record
	// goes ("all entries"), the temp stays (belongs to no record).
	writeFile(t, filepath.Join(historyDir, "broken.json"), "{oops")
	writeFile(t, filepath.Join(historyDir, "20260101-000003-0000000000000003.json.tmp4242"), "half")

	code, out := captureAnswer(t, func() int { return historyCmd([]string{"-clear"}) })
	if code != 2 {
		t.Fatalf("preview without -yes exit = %d, want 2:\n%s", code, out)
	}
	if !strings.Contains(out, "This would remove ALL 2 session record(s)") || !strings.Contains(out, "-clear -yes") {
		t.Fatalf("preview missing or wrong:\n%s", out)
	}
	if !manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("a preview deleted something")
	}
	if _, err := os.Stat("c1.log"); err != nil {
		t.Errorf("a preview touched evidence")
	}

	for _, args := range [][]string{{"-yes"}, {"-clear", "-json"}} {
		if code, _ := captureAnswer(t, func() int { return historyCmd(args) }); code != 2 {
			t.Errorf("history %v exit = %d, want 2", args, code)
		}
	}
	if !manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("a refused form deleted something")
	}

	code, out = captureAnswer(t, func() int { return historyCmd([]string{"-clear", "-yes"}) })
	if code != 0 {
		t.Fatalf("confirmed clear exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "history cleared: 3 record(s)") {
		t.Errorf("summary wrong (readable 2 + corrupt 1):\n%s", out)
	}
	if !strings.Contains(out, "broken.json was removed") {
		t.Errorf("the corrupt record's caveat is missing:\n%s", out)
	}
	if !strings.Contains(out, "temp file(s) in history/ stay") {
		t.Errorf("the temp file's note is missing:\n%s", out)
	}
	for _, id := range []string{"20260101-000001-0000000000000001", "20260101-000002-0000000000000002"} {
		if manifestExists(id) {
			t.Errorf("confirmed clear left readable record %s", id)
		}
	}
	for _, gone := range []string{"c1.log", "c1.log" + journalSuffix, "c1.log" + journalSuffix + workersSuffix,
		filepath.Join(historyDir, "broken.json")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("confirmed clear left: %s", gone)
		}
	}
	// Never-touched set survives: singletons, config, temps, the dir.
	for _, keep := range []string{lastrunPath, lastResultPath, findingsPath, config.DefaultPath} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("protected file lost: %s (%v)", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(historyDir, "20260101-000003-0000000000000003.json.tmp4242")); err != nil {
		t.Errorf("the crashed writer's temp file was touched")
	}
	if _, err := os.Stat(historyDir); err != nil {
		t.Errorf("the history directory itself was removed")
	}
}

// The menu route: 'c' previews, and ONLY the exact word "clear"
// commits. A wrong word cancels without touching a byte.
func TestMenuClearNeedsTheExactWord(t *testing.T) {
	chdirTemp(t)
	seedSession(t, "20260101-000001-0000000000000001", "m.log")
	writeFile(t, lastrunPath, `{"keep":"me"}`)

	// Cancelled first: 'nope' is not the word.
	if code, out := menuLoopCapture(t, "6\nc\nnope\nq\n0\n"); code != 0 {
		t.Fatalf("cancelled clear menu exit = %d:\n%s", code, out)
	}
	if !manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("a cancelled menu clear removed records")
	}
	if _, err := os.Stat("m.log"); err != nil {
		t.Errorf("a cancelled menu clear touched evidence")
	}

	// Confirmed: the exact word.
	if code, out := menuLoopCapture(t, "6\nc\nclear\nq\n0\n"); code != 0 {
		t.Fatalf("confirmed clear menu exit = %d:\n%s", code, out)
	}
	if manifestExists("20260101-000001-0000000000000001") {
		t.Errorf("the confirmed menu clear left the record")
	}
	for _, gone := range []string{"m.log", "m.log" + journalSuffix, "m.log" + journalSuffix + workersSuffix} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("menu clear left evidence behind: %s", gone)
		}
	}
	if _, err := os.Stat(lastrunPath); err != nil {
		t.Errorf("the menu clear touched the replay slot")
	}
}

func menuLoopCapture(t *testing.T, input string) (int, string) {
	t.Helper()
	return captureAnswer(t, func() int { return menuLoop(bufio.NewReader(strings.NewReader(input)), config.Default()) })
}

// The machine answer for retention: the config round-trip keeps the
// key (an operator's -1 can never be silently lost by a settings
// save), and the vocabulary constants are what the CLI promises.
func TestHistoryKeepConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	if cfg.HistoryKeep != config.HistoryKeepDefault {
		t.Fatalf("default retention drifted: %d", cfg.HistoryKeep)
	}
	cfg.HistoryKeep = config.HistoryKeepAll
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := config.LoadStrict(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.HistoryKeep != config.HistoryKeepAll {
		t.Errorf("history_keep lost through Save/Load: %d", back.HistoryKeep)
	}
	// And the key is invisible-but-harmless to old receipts: a
	// version-2 recipe without it still loads.
	var raw map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["history_keep"]; !ok || v.(float64) != -1 {
		t.Errorf("saved config missing history_keep -1: %v", raw["history_keep"])
	}
}
