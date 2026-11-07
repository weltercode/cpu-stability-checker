// Package toolsetup fetches, verifies and unpacks the third-party stress
// tools (Prime95, y-cruncher) that external workloads drive, and
// registers them in config.json. We never redistribute the tools
// ourselves: the user's machine downloads them from the authors' own
// sites, and pinned SHA-256 sums say exactly which build we tested.
package toolsetup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Tool is one pinned third-party tool: the exact version we know works,
// the official URL, and its published (or measured) SHA-256.
type Tool struct {
	Name        string
	Version     string
	URL         string
	SHA256      string // empty: checksum is reported, not enforced (not published upstream)
	LicenseNote string
	// Role distinguishes an external stress workload ("", the
	// default) from the Curve Optimizer backend ("co"): a CO tool is
	// registered as the top-level co_tool, never as an external
	// workload, and gets no stress presets.
	Role string
}

// Pinned lists the tools 'tools install' knows about. Checksums come
// from the authors' own download pages (Prime95) or were measured over
// HTTPS from the release page (y-cruncher).
var Pinned = []Tool{
	{
		Name:    "prime95",
		Version: "30.19b20",
		URL:     "https://www.mersenne.org/download/software/v30/30.19/p95v3019b20.win64.zip",
		SHA256:  "d9475f2ff3f4a6a701abc49a86a66126cb48abd10bda6fa87039d98fa8756bca",
		LicenseNote: "Prime95 is freeware by Mersenne Research (GIMPS); its EULA is " +
			"shown on first start. We do not ship it - this downloads it from mersenne.org.",
	},
	{
		Name:    "y-cruncher",
		Version: "0.8.7.9547b",
		URL:     "https://github.com/Mysticial/y-cruncher/releases/download/v0.8.7.9547b/y-cruncher.v0.8.7.9547b.zip",
		SHA256:  "3be696c1cc44907f2ea73aac35de4a5d45048255308f34ed4410a743163a4f87", // measured over HTTPS from the release page, 47198240 bytes
		LicenseNote: "y-cruncher is freeware by Alexander J. Yee; downloading it constitutes " +
			"accepting its license. We do not ship it - this downloads it from the release page.",
	},
	{
		Name:    "ryzen-smu-cli",
		Version: "0.1.3",
		// Source is maintained at github.com/sp00n/ryzen-smu-cli (no
		// releases there); prebuilt builds come from the release
		// channel CoreCycler 0.11 links. Pinned tag 0.1.3 -> commit
		// 39413e27658223b27470196ed0f2b35c1d966899.
		URL:    "https://github.com/rawhide-kobayashi/ryzen-smu-cli/releases/download/0.1.3/ryzen-smu-cli-0.1.3.zip",
		SHA256: "0131955f780566d43f464350c65de7f929dd9cb57a583c401fe7545712e98ffb", // measured over HTTPS 2026-10-18, 632207 bytes
		Role:   "co",
		LicenseNote: "ryzen-smu-cli is GPL-3.0 (LICENSE of the pinned release-channel archive; " +
			"source by sp00n at github.com/sp00n/ryzen-smu-cli). We do not ship it - this downloads " +
			"the pinned archive from the release channel. The build is framework-dependent: it " +
			"needs the .NET 8 Desktop Runtime, and every run needs elevation (the tool itself " +
			"refuses without it).",
	},
}

// ByName finds a pinned tool (case-insensitive).
func ByName(name string) (Tool, bool) {
	for _, t := range Pinned {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return Tool{}, false
}

// Fetch downloads the tool archive and verifies its SHA-256 against the
// pinned value (when one is published). It returns the archive path in
// dir and the checksum actually observed, so the user can see it either way.
func Fetch(t Tool, dir string) (archive, sha string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(t.URL)
	if err != nil {
		return "", "", fmt.Errorf("download %s: %w", t.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download %s: HTTP %d", t.URL, resp.StatusCode)
	}

	archive = filepath.Join(dir, filepath.Base(t.URL))
	f, err := os.Create(archive)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return archive, "", fmt.Errorf("download %s: %w", t.URL, err)
	}
	sha = hex.EncodeToString(h.Sum(nil))
	if t.SHA256 != "" && sha != t.SHA256 {
		return archive, sha, fmt.Errorf("checksum mismatch for %s: got %s, expected %s (refusing to unpack)", t.Name, sha, t.SHA256)
	}
	return archive, sha, nil
}

// UnpackZip extracts a zip into dir, refusing entries that would escape
// it (the classic zip-slip trick).
func UnpackZip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	cleanDir := filepath.Clean(dir) + string(os.PathSeparator)
	for _, zf := range zr.File {
		path := filepath.Join(dir, filepath.FromSlash(zf.Name))
		if filepath.Clean(path)+string(os.PathSeparator) != cleanDir && !strings.HasPrefix(filepath.Clean(path), cleanDir) {
			return fmt.Errorf("archive entry %q escapes %s", zf.Name, dir)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(f, rc)
		closeErr := f.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Register adds (or replaces) one external entry in config.json without
// touching anything else in the file: the document is kept as a generic
// map so unknown keys survive.
func Register(configPath, name string, spec map[string]any) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	ext, _ := doc["external"].(map[string]any)
	if ext == nil {
		ext = map[string]any{}
	}
	ext[name] = spec
	doc["external"] = ext
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(out, '\n'), 0o644)
}

// RegisterCoTool sets the top-level co_tool key in config.json,
// keeping everything else in the document exactly as it was (the
// same generic-map discipline as Register): the CO backend is not a
// stress workload and must not land in the external map.
func RegisterCoTool(configPath, exePath string) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("config %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	doc["co_tool"] = exePath
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(out, '\n'), 0o644)
}

// DesktopRuntime8 answers whether the .NET 8 Desktop Runtime the
// framework-dependent CO tool build needs is present. Three honest
// outcomes: found, absent (dotnet ran but lists no 8.x desktop
// runtime), or unknown (dotnet itself not runnable). A missing
// runtime is its own reason - never reported as a missing tool.
func DesktopRuntime8() (found bool, detail string) {
	out, err := exec.Command("dotnet", "--list-runtimes").Output()
	if err != nil {
		return false, "cannot run 'dotnet --list-runtimes' (unknown whether the .NET 8 Desktop Runtime is present): " + err.Error()
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Microsoft.WindowsDesktop.App 8.") {
			return true, strings.TrimSpace(line)
		}
	}
	return false, "no Microsoft.WindowsDesktop.App 8.x in 'dotnet --list-runtimes'"
}

// StressConfig renders the y-cruncher v0.8 stress-test config file for
// the given logical CPUs, algorithms and per-test seconds. v0.8's
// parser is strict JSON (quoted keys, commas) - unlike the older
// lenient format CoreCycler's scripts still write.
func StressConfig(logicalCPUs, secondsPerTest int, tests []string) string {
	cores := ""
	for i := 0; i < logicalCPUs; i++ {
		if i > 0 {
			cores += ","
		}
		cores += fmt.Sprintf("%d", i)
	}
	list := ""
	for i, name := range tests {
		if i > 0 {
			list += ","
		}
		list += fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf(`{"Action":"StressTest","StressTest":{"AllocateLocally":"true","LogicalCores":[%s],"TotalMemory":26567600,"SecondsPerTest":%d,"SecondsTotal":0,"StopOnError":"true","Tests":[%s]}}`,
		cores, secondsPerTest, list)
}

// Preset is one ready-made external workload entry: a name, a short
// description for the menu, and the spec body. Prime95 presets differ
// only by the prime.txt bytes they write before launch (write_files);
// y-cruncher presets pick a mode binary and a stress config.
type Preset struct {
	Name string
	Desc string
	Spec map[string]any
}

// PrimeTortureConfig renders the prime.txt for a Prime95 v30.19
// torture-test run. The option names and values follow the pinned
// build's undoc.txt plus CoreCycler's long field-tested set (measured
// live against 30.19b20 on this machine): StressTester/UsePrimenet
// make it a pure stress run, the FFT range comes from the preset,
// EnableSetAffinity=0 keeps Prime95 from undoing the affinity we
// applied and read back, and the V2x/V3x "converted" flags plus
// ExitOnX keep first-launch dialogs from blocking an unattended run.
// (The old P01-era keys torture_time/torture_mem were not real
// Prime95 options and were silently ignored - measured.)
func PrimeTortureConfig(minFFT, maxFFT string) string {
	return fmt.Sprintf(`StressTester=1
UsePrimenet=0
MinTortureFFT=%s
MaxTortureFFT=%s
TortureTime=1
TortureMem=0
ResultsFileTimestampInterval=60
EnableSetAffinity=0
EnableSetPriority=0
V24OptionsConverted=1
V30OptionsConverted=1
ExitOnX=1
ErrorCheck=1
SumInputsErrorCheck=1
OutputJSON=1
`, minFFT, maxFFT)
}

// prime95Preset builds one torture-test preset. The FFT ranges and the
// one-iteration runtimes follow the presets documented by Prime95 and
// measured in CoreCycler's config comments.
func prime95Preset(dir, name, desc, minFFT, maxFFT string) Preset {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return Preset{
		Name: name,
		Desc: desc,
		Spec: map[string]any{
			"command":         filepath.Join(abs, "prime95.exe"),
			"args":            []any{"-t"},
			"dir":             abs,
			"description":     desc,
			"adapter":         "prime95-v30.19",
			"fault_file":      "results.txt",
			"fault_pattern":   "(?i)hardware failure",
			"single_instance": true,
			"write_files": map[string]any{
				"prime.txt": PrimeTortureConfig(minFFT, maxFFT),
			},
		},
	}
}

// yCruncherPreset builds one stress preset: mode "" uses the launcher
// (auto-selects the right binary for this CPU), otherwise the named
// mode binary under Binaries. The stress config file pins every logical
// CPU, all ten algorithms and the per-test seconds.
func yCruncherPreset(dir, name, desc, mode string, secondsPerTest int, tests []string) Preset {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	command := filepath.Join(abs, "y-cruncher.exe")
	if mode != "" {
		command = filepath.Join(abs, "Binaries", mode+".exe")
	}
	return Preset{
		Name: name,
		Desc: desc,
		Spec: map[string]any{
			"command":         command,
			"args":            []any{"config", "stress-" + name + ".json"},
			"dir":             abs,
			"description":     desc,
			"single_instance": true,
			"adapter":         "ycruncher-v0.8",
			// y-cruncher prints "Running <TAG>: Passed" after every
			// algorithm; the panel shows the next one in the list -
			// the tests run in exactly this order.
			"status_pattern":  `Running (\w+): Passed`,
			"status_sequence": tests,
			"write_files": map[string]any{
				"stress-" + name + ".json": StressConfig(runtime.NumCPU(), secondsPerTest, tests),
			},
		},
	}
}

// AllPresets lists the ready-made entries 'tools install' registers next
// to the base one. Runtimes are CoreCycler's measured one-iteration
// numbers on a 5900X (one thread) - order of magnitude on modern parts.
func AllPresets(dir string) []Preset {
	all := []string{"BKT", "BBP", "SFT", "SFTv4", "SNT", "SVT", "FFT", "FFTv4", "N63", "VT3"}
	return []Preset{
		prime95Preset(dir, "prime95-small", "Prime95 Small FFTs 36K-248K: maximum CPU power/heat (one FFT ~1 min)", "36", "248"),
		prime95Preset(dir, "prime95-heavyshort", "Prime95 HeavyShort 4K-160K: fast full cycle, ~25 min per iteration (AVX2)", "4", "160"),
		prime95Preset(dir, "prime95-huge", "Prime95 Huge 8960K-MAX: stresses memory controller and RAM, ~15-40 min per iteration", "8960", "65536"),
		prime95Preset(dir, "prime95-moderate", "Prime95 Moderate 1344K-4096K: the Curve Optimizer guide preset", "1344", "4096"),
		yCruncherPreset(dir, "yc-p4p", "y-cruncher 04-P4P: scalar integer, least heat, highest boost clocks", "04-P4P", 30, all),
		yCruncherPreset(dir, "yc-zn5", "y-cruncher 24-ZN5 Komari: full Zen5 AVX512 path, all transistors", "24-ZN5 ~ Komari", 30, all),
	}
}
