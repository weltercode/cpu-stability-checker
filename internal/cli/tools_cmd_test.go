package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readConfigDoc reads the config.json the command wrote in the temp
// cwd (config.DefaultPath is relative, chdirTemp makes it local).
func readConfigDoc(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.json invalid: %v", err)
	}
	return doc
}

// detect must register the CO backend as the top-level co_tool, never
// as an external workload: run -a ryzen-smu-cli must stay impossible.
func TestToolsDetectRegistersCoToolNotWorkload(t *testing.T) {
	dir := chdirTemp(t)
	toolDir := filepath.Join(dir, "co")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "ryzen-smu-cli.exe"), []byte("MZfake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := toolsCmd([]string{"detect", toolDir}); code != 0 {
		t.Fatalf("detect exit = %d", code)
	}
	doc := readConfigDoc(t, dir)
	co, _ := doc["co_tool"].(string)
	if !filepath.IsAbs(co) || !strings.HasSuffix(co, "ryzen-smu-cli.exe") {
		t.Errorf("co_tool = %v, want absolute path to the detected exe", doc["co_tool"])
	}
	if ext, ok := doc["external"].(map[string]any); ok {
		if _, leaked := ext["ryzen-smu-cli"]; leaked {
			t.Error("CO backend registered as an external workload")
		}
	}
}

// install with the launcher already present re-registers without any
// download (hermetic: no network in tests) and still lands in
// co_tool, not external.
func TestToolsInstallCoRegistersWithoutRedownload(t *testing.T) {
	dir := chdirTemp(t)
	installDir := filepath.Join(dir, "tools", "ryzen-smu-cli")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "ryzen-smu-cli.exe"), []byte("MZfake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := toolsCmd([]string{"install", "ryzen-smu-cli", "-dir", installDir}); code != 0 {
		t.Fatalf("install exit = %d", code)
	}
	doc := readConfigDoc(t, dir)
	co, _ := doc["co_tool"].(string)
	if !strings.HasSuffix(co, filepath.Join("tools", "ryzen-smu-cli", "ryzen-smu-cli.exe")) {
		t.Errorf("co_tool = %v, want the installed launcher", doc["co_tool"])
	}
	if ext, ok := doc["external"].(map[string]any); ok && len(ext) != 0 {
		t.Errorf("CO install wrote external entries: %v", ext)
	}
}

// A06 visibility: 'tools status' must show a registered-but-stale path
// as MISSING and name it, so the owner can repoint or reinstall the
// entry before any run (repeat above all) tries to load it. A present
// tool keeps its "ok" line: the command reports the disk's truth,
// never a guess. This is the reporting half of the preflight story -
// the refusing half (externalToolMissing naming the path with exit 2)
// is pinned in lastrun_test.
func TestToolsStatusReportsStalePathsMissing(t *testing.T) {
	dir := chdirTemp(t)
	present := filepath.Join(dir, "present-tool.exe")
	if err := os.WriteFile(present, []byte("MZfake"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"co_tool": `C:\nowhere\ryzen-smu-cli.exe`,
		"external": map[string]any{
			"gonesetup": map[string]any{"command": `C:\nowhere\y-cruncher.exe`},
			"present":   map[string]any{"command": present},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdoutAndRun(t, func() {
		if code := toolsStatus(nil); code != 0 {
			t.Errorf("toolsStatus exit code = %d, want 0", code)
		}
	})
	for _, want := range []string{
		`C:\nowhere\y-cruncher.exe`, `C:\nowhere\ryzen-smu-cli.exe`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lost the stale path %q:\n%s", want, out)
		}
	}
	// Every named stale entry reports MISSING on its own line; the
	// present one reports ok and nothing else on that line.
	for _, name := range []string{"co_tool", "gonesetup"} {
		line := statusLine(t, out, name)
		if !strings.Contains(line, "MISSING") {
			t.Errorf("%s line does not say MISSING: %q", name, line)
		}
	}
	line := statusLine(t, out, "present")
	if !strings.Contains(line, "ok") || strings.Contains(line, "MISSING") {
		t.Errorf("present tool line wrong: %q\nfull:\n%s", line, out)
	}
}

// statusLine finds the single status row for a tool name; failing
// loudly when the row is absent keeps the check honest (a missing row
// is not the same as a MISSING row).
func statusLine(t *testing.T, out, name string) string {
	t.Helper()
	found := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), name+" ") || strings.HasPrefix(strings.TrimSpace(l), name+"\t") {
			if found != "" {
				t.Fatalf("two status lines for %q:\n%s", name, out)
			}
			found = l
		}
	}
	if found == "" {
		t.Fatalf("no status line for %q:\n%s", name, out)
	}
	return found
}
