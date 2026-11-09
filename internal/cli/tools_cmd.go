package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/toolsetup"
)

// toolsCmd manages the third-party stress tools: what is present, and
// downloading + registering the pinned versions. We never ship the
// tools: the user's machine fetches them from the authors' own sites,
// and the pinned SHA-256 pins down exactly the build we tested.
func toolsCmd(args []string) int {
	if len(args) == 0 {
		toolsUsage()
		return 2
	}
	switch args[0] {
	case "status":
		return toolsStatus(args[1:])
	case "install":
		return toolsInstall(args[1:])
	case "detect":
		return toolsDetect(args[1:])
	default:
		toolsUsage()
		return 2
	}
}

func toolsUsage() {
	fmt.Print(`Usage: cpu-stability tools <status|install|detect> [args]

  tools status                 which external tools are configured and present
  tools install <name>         download the pinned official build, verify its
                               SHA-256, unpack and register it in config.json
                               (-dir path, default tools/<name>; -force re-downloads)
  tools detect <dir>...        register an existing local install (no download)

Known tools:
`)
	for _, t := range toolsetup.Pinned {
		fmt.Printf("  %-11s pinned %s  %s\n", t.Name, t.Version, t.URL)
	}
}

// defaultExe is the launcher each tool is identified by.
func defaultExe(name string) string {
	switch name {
	case "prime95":
		return "prime95.exe"
	case "ryzen-smu-cli":
		return "ryzen-smu-cli.exe"
	}
	return "y-cruncher.exe"
}

// registerTool writes the config entry for one pinned tool: CO
// backends register as the top-level co_tool (never an external
// workload, never stress presets) and get the honest prerequisites
// reminder; stress tools keep the external-workload registration.
func registerTool(cfgPath string, t toolsetup.Tool, installDir string) error {
	if t.Role != "co" {
		if err := toolsetup.Register(cfgPath, t.Name, defaultSpec(t, installDir)); err != nil {
			return err
		}
		registerPresets(cfgPath, installDir, t.Name)
		return nil
	}
	abs, err := filepath.Abs(filepath.Join(installDir, defaultExe(t.Name)))
	if err != nil {
		abs = filepath.Join(installDir, defaultExe(t.Name))
	}
	if err := toolsetup.RegisterCoTool(cfgPath, abs); err != nil {
		return err
	}
	fmt.Printf("Registered co_tool %q in %s.\n", abs, cfgPath)
	if found, detail := toolsetup.DesktopRuntime8(); found {
		fmt.Printf("  .NET 8 Desktop Runtime: %s\n", detail)
	} else {
		fmt.Printf("  WARNING .NET 8 Desktop Runtime: %s\n", detail)
		fmt.Println("  the pinned build is framework-dependent: install the runtime or the tool will fail to start")
	}
	fmt.Println("  every run needs elevation (the tool itself refuses without admin).")
	fmt.Println("  read-only checks: cpu-stability co probe | co topology | co read")
	return nil
}

// defaultSpec is the registration entry install/detect writes: the same
// shape the README documents, with absolute paths under the install dir.
func defaultSpec(t toolsetup.Tool, dir string) map[string]any {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	spec := map[string]any{"dir": abs}
	switch t.Name {
	case "prime95":
		spec["command"] = filepath.Join(abs, "prime95.exe")
		spec["args"] = []any{"-t"}
		spec["description"] = "Prime95 torture test (GUI; core count and FFT range live in prime.txt)"
		spec["adapter"] = "prime95-v30.19"
		spec["fault_file"] = "results.txt"
		spec["fault_pattern"] = "(?i)hardware failure"
		spec["single_instance"] = true
		spec["write_files"] = map[string]any{"prime.txt": toolsetup.PrimeTortureConfig("1", "16384")}
	default: // y-cruncher: the launcher picks the mode binary for this CPU
		spec["command"] = filepath.Join(abs, "y-cruncher.exe")
		spec["args"] = []any{"config", "stress-default.json"}
		spec["description"] = "y-cruncher stress test (auto mode for this CPU; all tests, 30s each)"
		spec["single_instance"] = true
		spec["adapter"] = "ycruncher-v0.8"
		spec["status_pattern"] = `Running (\w+): Passed`
		spec["status_sequence"] = yCruncherTests
		spec["write_files"] = map[string]any{"stress-default.json": toolsetup.StressConfig(runtime.NumCPU(), 30, yCruncherTests)}
	}
	return spec
}

// yCruncherTests: all ten v0.8 algorithms, the CoreCycler default.
var yCruncherTests = []string{"BKT", "BBP", "SFT", "SFTv4", "SNT", "SVT", "FFT", "FFTv4", "N63", "VT3"}

func toolsInstall(args []string) int {
	// Accept the name before or after the flags: Go's flag package stops
	// at the first positional argument, and "install prime95 -dir ..." is
	// the natural order people type.
	var name string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-dir" || a == "-config":
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case name == "":
			name = a
		}
	}
	fs := flag.NewFlagSet("tools install", flag.ContinueOnError)
	dir := fs.String("dir", "", "install directory (default tools/<name>)")
	cfgPath := fs.String("config", config.DefaultPath, "config file to register in")
	force := fs.Bool("force", false, "re-download even if the tool is already present")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	t, ok := toolsetup.ByName(name)
	if !ok {
		fmt.Printf("Unknown tool %q. Known: ", name)
		for _, k := range toolsetup.Pinned {
			fmt.Printf("%s ", k.Name)
		}
		fmt.Println()
		return 2
	}
	if runtime.GOOS != "windows" && t.Name != "prime95" {
		// The pinned archives are the Windows builds; say so honestly.
		fmt.Printf("Pinned %s build is Windows-only on this platform: install manually or use built-in workloads.\n", t.Name)
		return 2
	}
	installDir := *dir
	if installDir == "" {
		installDir = filepath.Join("tools", t.Name)
	}
	exe := filepath.Join(installDir, defaultExe(t.Name))
	if _, err := os.Stat(exe); err == nil && !*force {
		fmt.Printf("%s already installed at %s (use -force to re-download).\n", t.Name, installDir)
		if err := registerTool(*cfgPath, t, installDir); err != nil {
			fmt.Fprintf(os.Stderr, "cannot register in %s: %v\n", *cfgPath, err)
			return 2
		}
		if t.Role != "co" {
			fmt.Printf("Registered %q in %s.\n", t.Name, *cfgPath)
		}
		return 0
	}

	fmt.Println(t.LicenseNote)
	fmt.Printf("Downloading %s %s from the official site...\n", t.Name, t.Version)
	archive, sha, err := toolsetup.Fetch(t, filepath.Join(os.TempDir(), "cpu-stability-tools"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	fmt.Printf("SHA-256 %s\n", sha)
	if err := toolsetup.UnpackZip(archive, installDir); err != nil {
		fmt.Fprintf(os.Stderr, "unpack %s: %v\n", archive, err)
		return 2
	}
	os.Remove(archive)
	if _, err := os.Stat(exe); err != nil {
		fmt.Fprintf(os.Stderr, "downloaded archive does not contain %s - unpacked into %s, check its layout\n", defaultExe(t.Name), installDir)
		return 2
	}
	if err := registerTool(*cfgPath, t, installDir); err != nil {
		fmt.Fprintf(os.Stderr, "cannot register in %s: %v\n", *cfgPath, err)
		return 2
	}
	if t.Role == "co" {
		fmt.Printf("Installed %s %s into %s.\n", t.Name, t.Version, installDir)
		return 0
	}
	fmt.Printf("Installed %s %s into %s and registered workload %q in %s.\n", t.Name, t.Version, installDir, t.Name, *cfgPath)
	fmt.Println("Pick it in the menu, or: cpu-stability run -a " + t.Name + " -duration 5m")
	return 0
}

// registerPresets adds the ready-made variants for one tool (Prime95
// FFT presets, y-cruncher mode presets) alongside the base entry.
func registerPresets(cfgPath, dir, toolName string) {
	prefix := "prime95-"
	if toolName != "prime95" {
		prefix = "yc-"
	}
	for _, p := range toolsetup.AllPresets(dir) {
		if !strings.HasPrefix(p.Name, prefix) {
			continue
		}
		if err := toolsetup.Register(cfgPath, p.Name, p.Spec); err != nil {
			fmt.Fprintf(os.Stderr, "cannot register %s in %s: %v\n", p.Name, cfgPath, err)
			continue
		}
		fmt.Printf("  + %-18s %s\n", p.Name, p.Desc)
	}
}

func toolsDetect(args []string) int {
	if len(args) == 0 {
		fmt.Println("Usage: cpu-stability tools detect <dir>...")
		return 2
	}
	code := 0
	for _, dir := range args {
		found := false
		for _, t := range toolsetup.Pinned {
			if _, err := os.Stat(filepath.Join(dir, defaultExe(t.Name))); err != nil {
				continue
			}
			if err := registerTool(config.DefaultPath, t, dir); err != nil {
				fmt.Fprintf(os.Stderr, "cannot register %s from %s: %v\n", t.Name, dir, err)
				code = 2
				continue
			}
			if t.Role == "co" {
				fmt.Printf("Detected %s in %s.\n", t.Name, dir)
			} else {
				fmt.Printf("Detected %s in %s, registered workload %q.\n", t.Name, dir, t.Name)
			}
			found = true
		}
		if !found {
			fmt.Printf("%s: no known tool launcher found\n", dir)
			code = 1
		}
	}
	return code
}

func toolsStatus(args []string) int {
	cfg, err := config.LoadOrDefault(config.DefaultPath)
	if err != nil {
		// R15: status about configured tools needs a readable config.
		// On silent defaults this screen would honestly-print a lie:
		// "no external workloads configured" about a file that simply
		// could not be read.
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		return 2
	}
	if cfg.CoTool != "" {
		state := "MISSING"
		if _, err := os.Stat(cfg.CoTool); err == nil {
			state = "ok"
		}
		fmt.Printf("  %-12s %-8s %s\n", "co_tool", state, cfg.CoTool)
	}
	if len(cfg.External) == 0 {
		if cfg.CoTool == "" {
			fmt.Println("No external workloads configured (tools install <name> adds one).")
		}
		return 0
	}
	names := make([]string, 0, len(cfg.External))
	for name := range cfg.External {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := cfg.External[name]
		state := "MISSING"
		if _, err := os.Stat(spec.Command); err == nil {
			state = "ok"
		}
		fmt.Printf("  %-12s %-8s %s\n", name, state, spec.Command)
	}
	return 0
}
