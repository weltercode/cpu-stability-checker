package toolsetup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipBytes builds an in-memory zip with the given name->content entries.
func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func shaOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFetchVerifiesChecksum(t *testing.T) {
	archive := zipBytes(t, map[string]string{"prime95.exe": "fake"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/p95.zip" {
			http.NotFound(w, r)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Run("matching checksum unpacks", func(t *testing.T) {
		tool := Tool{Name: "prime95", URL: srv.URL + "/p95.zip", SHA256: shaOf(archive)}
		path, sha, err := Fetch(tool, dir)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if sha != shaOf(archive) {
			t.Errorf("reported sha %s, want %s", sha, shaOf(archive))
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("archive not kept at %s: %v", path, err)
		}
	})
	t.Run("mismatching checksum refuses", func(t *testing.T) {
		tool := Tool{Name: "prime95", URL: srv.URL + "/p95.zip", SHA256: "deadbeef"}
		_, _, err := Fetch(tool, dir)
		if err == nil {
			t.Fatal("Fetch accepted a wrong checksum")
		}
	})
	t.Run("missing page errors", func(t *testing.T) {
		tool := Tool{Name: "prime95", URL: srv.URL + "/nope.zip"}
		if _, _, err := Fetch(tool, dir); err == nil {
			t.Fatal("Fetch accepted HTTP 404")
		}
	})
}

func TestUnpackZip(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(archive, zipBytes(t, map[string]string{
		"prime95.exe":   "MZfake",
		"sub/note.txt":  "hello",
		"../escape.txt": "nope",
	}), 0o644)

	dir := t.TempDir()
	if err := UnpackZip(archive, dir); err == nil {
		t.Fatal("zip-slip entry was unpacked without complaint")
	}

	// Without the slip entry everything lands inside and stays put.
	clean := filepath.Join(t.TempDir(), "a.zip")
	os.WriteFile(clean, zipBytes(t, map[string]string{
		"prime95.exe":  "MZfake",
		"sub/note.txt": "hello",
	}), 0o644)
	if err := UnpackZip(clean, dir); err != nil {
		t.Fatalf("UnpackZip: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "note.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("nested entry = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prime95.exe")); err != nil {
		t.Errorf("top-level entry missing: %v", err)
	}
}

func TestRegisterKeepsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"test":"general","future_key":{"kept":true}}`), 0o644)

	if err := Register(path, "prime95", map[string]any{"command": "x"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Second registration replaces, not duplicates.
	if err := Register(path, "prime95", map[string]any{"command": "y"}); err != nil {
		t.Fatalf("Register again: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config is not valid JSON after Register: %v", err)
	}
	if doc["test"] != "general" {
		t.Errorf("existing key lost: %v", doc["test"])
	}
	if _, ok := doc["future_key"]; !ok {
		t.Error("unknown key was dropped")
	}
	ext := doc["external"].(map[string]any)
	if len(ext) != 1 {
		t.Fatalf("external entries = %v, want exactly one (replaced)", ext)
	}
	if ext["prime95"].(map[string]any)["command"] != "y" {
		t.Errorf("entry not replaced: %v", ext["prime95"])
	}
}

// RegisterCoTool sets the top-level co_tool key and never touches the
// external map: the CO backend is not a stress workload, and a
// mis-registered backend would make run -a ryzen-smu-cli possible.
func TestRegisterCoToolKeepsDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"test":"general","external":{"prime95":{"command":"keepme"}}}`), 0o644)

	if err := RegisterCoTool(path, `C:\tools\ryzen-smu-cli\ryzen-smu-cli.exe`); err != nil {
		t.Fatalf("RegisterCoTool: %v", err)
	}
	if err := RegisterCoTool(path, `C:\other\ryzen-smu-cli.exe`); err != nil {
		t.Fatalf("RegisterCoTool again: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config is not valid JSON after RegisterCoTool: %v", err)
	}
	if doc["test"] != "general" {
		t.Errorf("existing key lost: %v", doc["test"])
	}
	if doc["co_tool"] != `C:\other\ryzen-smu-cli.exe` {
		t.Errorf("co_tool = %v, want the replaced absolute path", doc["co_tool"])
	}
	ext := doc["external"].(map[string]any)
	if ext["prime95"].(map[string]any)["command"] != "keepme" {
		t.Errorf("external map was disturbed: %v", ext)
	}
	if _, leaked := ext["ryzen-smu-cli"]; leaked {
		t.Error("CO backend leaked into the external workload map")
	}
}

// The pinned CO backend must be marked as role "co" (so install and
// detect register it as co_tool, never as a workload) and carry the
// measured release-channel pin.
func TestPinnedCoToolEntry(t *testing.T) {
	tool, ok := ByName("ryzen-smu-cli")
	if !ok {
		t.Fatal("ryzen-smu-cli is not pinned")
	}
	if tool.Role != "co" {
		t.Errorf("Role = %q, want co", tool.Role)
	}
	if tool.SHA256 == "" {
		t.Error("the CO backend archive must be checksum-enforced, not just reported")
	}
	if !strings.Contains(tool.URL, "releases/download/0.1.3/ryzen-smu-cli-0.1.3.zip") {
		t.Errorf("URL = %s, want the pinned 0.1.3 release asset", tool.URL)
	}
	if !strings.Contains(tool.LicenseNote, "GPL-3.0") || !strings.Contains(tool.LicenseNote, ".NET 8 Desktop Runtime") {
		t.Errorf("license/prerequisite note incomplete: %s", tool.LicenseNote)
	}
}

// The registered presets must speak the protocols measured against the
// pinned builds: version-specific adapter selectors, and prime.txt
// keys Prime95 actually understands (the P01-era torture_time and
// torture_mem keys were not real Prime95 options and were silently
// ignored - measured against 30.19b20).
func TestPresetsUseMeasuredProtocols(t *testing.T) {
	p95 := prime95Preset(t.TempDir(), "p95-sm", "small", "8", "16")
	if got := p95.Spec["adapter"]; got != "prime95-v30.19" {
		t.Errorf("prime95 preset adapter = %v, want prime95-v30.19", got)
	}
	files, _ := p95.Spec["write_files"].(map[string]any)
	cfg, _ := files["prime.txt"].(string)
	for _, want := range []string{"StressTester=1", "UsePrimenet=0", "MinTortureFFT=8", "MaxTortureFFT=16", "TortureTime=", "EnableSetAffinity=0"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("prime.txt missing %q:\n%s", want, cfg)
		}
	}
	for _, bad := range []string{"torture_time", "torture_mem"} {
		if strings.Contains(cfg, bad) {
			t.Errorf("prime.txt uses the non-option key %q (Prime95 ignores it):\n%s", bad, cfg)
		}
	}
	yc := yCruncherPreset(t.TempDir(), "yc-sm", "stress", "", 30, []string{"BKT"})
	if got := yc.Spec["adapter"]; got != "ycruncher-v0.8" {
		t.Errorf("y-cruncher preset adapter = %v, want ycruncher-v0.8", got)
	}
}
