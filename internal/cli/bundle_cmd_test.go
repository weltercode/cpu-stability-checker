package cli

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/sanitize"
)

// TestBundleStripsLiveIdentity is the R09 acceptance: a bundle built
// next to a journal whose evidence mentions this machine's real
// hostname, real user and real home path must contain NONE of them in
// any artifact — while the evidence itself (kinds, fault results, the
// deep custom path) survives verbatim. Sanitization that destroys
// evidence is worthless; sanitization that misses identity is worse.
func TestBundleStripsLiveIdentity(t *testing.T) {
	dir := chdirTemp(t)

	liveHost, err := os.Hostname()
	if err != nil || liveHost == "" {
		t.Skip("hostname is not observable on this machine: nothing to prove stripped")
	}
	liveHome, err := os.UserHomeDir()
	if err != nil || liveHome == "" {
		t.Skip("home dir is not observable on this machine: nothing to prove stripped")
	}
	liveUser := ""
	if u, uerr := user.Current(); uerr == nil {
		liveUser = u.Username
	}
	// A needle deliberately deeper than the home dir: proves the
	// scrubber removes the WHOLE matched path, not a basename dance.
	deep := filepath.Join(liveHome, "AppData", "Local", "Temp", "OPENCODE")

	cfg := map[string]any{
		"test":       "general",
		"workload_a": "int",
		"duration":   "1s",
		"log_file":   "cpu-stability.log",
		"co_tool":    deep + `\ryzen-smu-cli.exe`,
		"external":   map[string]any{"prime95": map[string]any{"path": `C:\Tools\Prime95\prime95.exe`}},
		"memory":     "",
		"ram_seed":   0,
	}
	rawCfg, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.json", rawCfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("lastresult.json",
		[]byte(`{"msg":"session on `+liveHost+` stored at `+deep+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	jpath := filepath.Join(dir, "bug-report-journal.jsonl")
	f, err := os.Create(jpath)
	if err != nil {
		t.Fatal(err)
	}
	w := journal.NewWriter(f, "bundle-e2e-run")
	w.RunStart("general", "", []int{3}, 1, "1s", false)
	seed := uint64(1)
	w.RunMeta(journal.RunMetaFields{
		ModuleVer: "v0.0.0-test", ConfigHash: "cafe", PlanHash: "feed",
		Seed: &seed, CPUName: "Test CPU", OSVersion: "test", SessionID: "s-1",
	})
	w.CoreStart(3, "int")
	w.CoreDone(3, journal.Fault,
		"synthetic evidence line mentioning "+liveHost+" and "+liveUser+" and path "+deep)
	// A second fault line in upper case: the scrubber must not need
	// the exact case the OS happens to report.
	w.CoreDone(1, journal.Fault, "SECOND FAULT AT "+strings.ToUpper(deep))
	w.RunEndValidation(1, "partial")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "bundle")
	if code := bundleCmd([]string{"-out", out, jpath}); code != 0 {
		t.Fatalf("bundle exit = %d, want 0", code)
	}

	rules := sanitize.Rules(sanitize.FromOS())
	if len(rules) == 0 {
		t.Skip("no identity observed: the scrub would be vacuous")
	}

	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	need := map[string]bool{"manifest.txt": false, "journal.jsonl": false, "report.txt": false,
		"report.json": false, "config.json": false, "lastresult.json": false,
		"topology.json": false, "caps.json": false, "caps.txt": false}
	for _, n := range names {
		need[n] = true
	}
	for n, seen := range need {
		if !seen {
			t.Errorf("bundle missing artifact %s (have %v)", n, names)
		}
	}

	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(out, n))
		if err != nil {
			t.Fatal(err)
		}
		if sanitize.Contains(string(raw), rules) {
			t.Errorf("artifact %s still contains live identity", n)
		}
	}

	// Evidence survived: the fault results and the non-identity part
	// of the custom tool path are verbatim; only identity segments
	// became tokens.
	jb, err := os.ReadFile(filepath.Join(out, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(jb)
	if !strings.Contains(js, journal.Fault) || !strings.Contains(js, "synthetic evidence line") {
		t.Errorf("journal evidence was damaged:\n%s", js)
	}
	if !strings.Contains(js, "SECOND FAULT AT [home]") {
		// The deep path was JSON-escaped (\\) inside the journal
		// string: the escaped needle form must collapse the whole
		// home prefix, in any case.
		t.Errorf("expected the upper-case deep home path folded to %q, got:\n%s", sanitize.HomeToken, js)
	}

	// The manifest documents what it replaced by length only (no raw
	// echo), and its self-check passed clean.
	mb, err := os.ReadFile(filepath.Join(out, "manifest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ms := string(mb)
	if !strings.Contains(ms, "self_check") || !strings.Contains(ms, ": clean") {
		t.Errorf("manifest self-check lines missing:\n%s", ms)
	}
	if strings.Contains(ms, "STILL CONTAINS LIVE IDENTITY") {
		t.Errorf("manifest admits a live identity survived:\n%s", ms)
	}

	// The machine paths that carry NO identity stay as they are: the
	// scrubber removes identity, it does not redact the world. And
	// the identity-bearing config value must show the token, not the
	// live home path, with the remainder of the path preserved.
	cb, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs := string(cb)
	if !strings.Contains(cs, `C:\\Tools\\Prime95\\prime95.exe`) {
		t.Errorf("non-identity tool path should stay verbatim (in its JSON-escaped form):\n%s", cs)
	}
	if !strings.Contains(cs, `[home]\\AppData\\Local\\Temp\\OPENCODE\\ryzen-smu-cli.exe`) {
		t.Errorf("co_tool home segment should fold to the token, remainder verbatim:\n%s", cs)
	}
}

// TestBundleRefusesMissingJournal: setup before anything is written —
// the bundle must not half-write a directory from a journal that does
// not exist.
func TestBundleRefusesMissingJournal(t *testing.T) {
	chdirTemp(t)
	out := filepath.Join(t.TempDir(), "never")
	if code := bundleCmd([]string{"-out", out, "no-such-journal.jsonl"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		// MkdirAll may create the directory before the journal check;
		// what must NOT exist is any artifact.
		if entries, _ := os.ReadDir(out); len(entries) != 0 {
			t.Errorf("artifacts written for an unreadable journal: %v", entries)
		}
	}
}
