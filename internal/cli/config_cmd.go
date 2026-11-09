package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// configCmd answers "what settings does this machine have, and where
// do they come from?" (R18, owner queue 2026-11-09). Bare `config` and
// `config show` render ONE shared inspection model: the real file path,
// the state of the file, and the effective value of every top-level
// setting with its local meaning - replacing the old raw-JSON dump.
// The model deliberately stops at "configured values and local
// meanings": compiled run plans, resolved core sets and admission
// budgets belong to the plan machinery (and to 'scenario -dry-run'),
// and re-deriving them here would fork the interpretation the runner
// owns. Machine facts are checked by reading directories only (path
// existence of configured tools) - never by launching anything.
func configCmd(args []string) int {
	if len(args) >= 1 && args[0] == "init" {
		// The sample is a starting point, not a licence to destroy:
		// config.json is where a machine's core selectors, durations,
		// memory budget and registered external-tool paths live. An
		// overwrite would silently discard the work `tools install`
		// and the wizards did, so the default answer is no and the
		// operator has to say otherwise.
		force := false
		for _, a := range args[1:] {
			if a == "-force" || a == "--force" {
				force = true
				continue
			}
			fmt.Fprintf(os.Stderr, "config init: unknown argument %q (only -force is accepted)\n", a)
			return 2
		}
		if _, err := os.Stat(config.DefaultPath); err == nil && !force {
			fmt.Fprintf(os.Stderr, "config init refused: %s already exists and holds this machine's settings (cores, durations, memory budget, registered tools). Edit that file, or pass -force to replace it with the sample.\n", config.DefaultPath)
			return 2
		}
		cfg := config.Sample(nil)
		if err := cfg.Save(config.DefaultPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Printf("wrote %s (edit it to preset cores/workloads, then re-run)\n", config.DefaultPath)
		return 0
	}

	// R18: unknown subcommands refuse, and refuse BEFORE any
	// configuration is read - "what are my settings?" is not answered
	// from a command nobody typed, whatever state the file is in.
	// Before this, `config frobnicate` fell into the raw dump.
	rest := args
	if len(args) >= 1 && args[0] == "show" {
		rest = args[1:]
	}
	for _, a := range rest {
		fmt.Fprintf(os.Stderr, "config: unknown argument %q (the view takes none; 'config init [-force]' writes the sample)\n", a)
		return 2
	}
	return configShow()
}

// configShow renders the shared inspection model for the three honest
// states (the same law R15 installed at every load site): absent file =
// built-in defaults, said out loud; present-but-unreadable = refusal
// with the reason, because that file holds this machine's deliberate
// plan; parsed = the merged values with their local meanings.
func configShow() int {
	abs := config.DefaultPath
	if a, err := filepath.Abs(config.DefaultPath); err == nil {
		abs = a
	}

	if _, err := os.Stat(config.DefaultPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// The file may exist and be unreachable; guessing
			// "absent" from a permission error would answer from
			// defaults about a plan that could still be there.
			fmt.Fprintf(os.Stderr, "configuration error: cannot stat %s: %v\n", config.DefaultPath, err)
			return 2
		}
		fmt.Printf("config file: %s - no file there\n", abs)
		fmt.Println("state: built-in defaults in effect. The configuration is a saved preference, so absence is legitimate - but it is said, never hidden behind a defaults printout dressed as this machine's plan.")
		fmt.Println("first run: 'cpu-stability config init' writes an editable sample (cores, durations, memory budget, external tools).")
		fmt.Println()
		showConfigValues(config.Default())
		return 0
	}

	cfg, err := config.LoadStrict(config.DefaultPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		fmt.Fprintf(os.Stderr, "config file: %s is PRESENT but unreadable. It holds this machine's deliberate plan (cores, durations, memory budget, registered tools); answering from defaults would describe a run that never was the planned one, so there is no answer to show.\n", abs)
		fmt.Fprintln(os.Stderr, "Fix the file, or remove it to run on defaults (every screen then says so), or replace it deliberately with 'config init -force'.")
		return 2
	}

	fmt.Printf("config file: %s\n", abs)
	fmt.Println("state: present and parsed. The values below are what every command on this machine will use; keys the file leaves out fall back to built-in defaults - documented merge behaviour, disclosed here once instead of guessed per line (which keys were supplied is not distinguishable from their values, and this view does not pretend otherwise).")
	fmt.Println()
	showConfigValues(cfg)
	fmt.Println("\nThis file is what every command loads. Edit it to change any value; 'config init' refuses to overwrite it, so your choices are never silently replaced.")
	return 0
}

// showConfigValues prints the effective Config - the merged snapshot
// the caller is actually holding (one load, no helper re-reading the
// file) - each line stating the value AS WRITTEN and what it means
// locally. Where a value is a defect, the line says so and names what
// the runner will do (refuse by name, or its documented fallback); no
// line ever shows a repaired or invented value.
func showConfigValues(cfg config.Config) {
	w := newCfgPrinter()
	fmt.Println("values in effect:")

	if len(cfg.Cores) == 0 {
		w.line("cores", "empty in the file: every run resolves all selected cores from the detected topology ('cpu-stability topology' shows the set here)")
	} else {
		w.line("cores", fmt.Sprintf("logical CPUs %s as configured (%d named; each run resolves them against the detected topology)", formatCPUList(cfg.Cores), len(cfg.Cores)))
	}
	w.line("default test", fmt.Sprintf("%q (the menu/scripted default when no -test is given)", cfg.Test))
	w.line("workload A / B", fmt.Sprintf("%q / %q (the two default workload slots)", cfg.WorkloadA, cfg.WorkloadB))

	dur := fmt.Sprintf("%q per run: the load window used when no -duration is given", cfg.Duration)
	if _, derr := time.ParseDuration(cfg.Duration); derr != nil {
		// Named defect, not a repair: state the documented fallback
		// the runner takes, so nobody mistakes it for the written intent.
		dur = fmt.Sprintf("%q as written is NOT parseable - a scripted 'run' without -duration falls back to 30s (ParsedDuration's documented floor), which is NOT what this file asked for; fix it", cfg.Duration)
	}
	w.line("duration", dur)

	w.line("workers per core", fmt.Sprintf("%d", max(cfg.Workers, 1)))
	if cfg.Workers <= 0 {
		w.ind("empty or non-positive in the file: the built-in floor is 1")
	}

	switch cfg.Execution {
	case "", config.ExecutionAuto:
		w.line("execution policy", "auto: the resolver picks threaded vs per-process per workload's hard requirements")
	case config.ExecutionThreaded, config.ExecutionSubprocess, config.ExecutionRotate:
		w.line("execution policy", cfg.Execution+": an explicit settings-level override; one contradicting a workload's requirement is refused before any load (plan.ResolveExecution), never quietly switched")
	default:
		w.line("execution policy", fmt.Sprintf("%q is NOT in the vocabulary auto/threaded/subprocess/rotate - every run refuses it at setup (exit 2), nothing will start until fixed", cfg.Execution))
	}

	w.line("rotate", onOff(cfg.Rotate)+" (per-process mode: one core runs at a time in exclusive windows)")
	w.line("stop on fault", onOff(cfg.StopOnFault)+faulNote(cfg.StopOnFault))
	w.line("live metrics", onOff(cfg.ShowMetrics)+" (memory/OS stats in the live UI)")
	w.line("telemetry", onOff(cfg.Telemetry)+" (per-second clock/idle lines during runs)")

	if cfg.ControlCore == nil {
		w.line("control core", "auto: the helper threads get the first CPU no worker occupies")
	} else if *cfg.ControlCore == -1 {
		w.line("control core", "off: helper threads stay unpinned and float")
	} else {
		w.line("control core", fmt.Sprintf("explicitly pinned to logical CPU %d", *cfg.ControlCore))
	}
	switch cfg.ReserveControl {
	case "", "none":
		w.line("reserve control", "none: the requested worker set is never shrunk; if helpers need headroom they float (auto gives up honestly)")
	case "logical":
		w.line("reserve control", "logical: when the auto control core would starve, the highest-numbered logical CPU is removed from the worker set for the helpers - the coverage loss IS the experiment, named in plan hash and journal")
	default:
		w.line("reserve control", fmt.Sprintf("%q is unknown (vocabulary: none/logical) - runs refuse it at setup", cfg.ReserveControl))
	}

	if cfg.MaxTemp <= 0 {
		w.line("temperature limit", "off (0)")
	} else {
		w.line("temperature limit", fmt.Sprintf("%.1f C on the shared panel sensor; a missing or stale reading STOPS the run (unknown never reads as under-limit)", cfg.MaxTemp))
	}

	w.line("journal file", fmt.Sprintf("%s (from \"log_file\" %q, journal suffix rule applied)", journal.Path(cfg.LogFile), cfg.LogFile))

	beat := cfg.BeatEvery
	if beat == "" {
		beat = "1s (built-in default)"
	}
	w.line("beat interval", beat+" - how often core_beat records hit the journal; the live UI is unaffected")

	w.line("ram seed", fmt.Sprintf("%d for the ram-* family (0 is a valid seed, not \"unset\")", cfg.RamSeed))

	switch cfg.ParkingPolicy {
	case "", "off":
		w.line("parking policy", "off globally: parking evidence is never promised (a phase may still carry its own word)")
	case "try", "required":
		w.line("parking policy", cfg.ParkingPolicy+": the global default for prepared-session phases (observed on measured idle windows; 'required' makes a full success depend on the evidence)")
	default:
		w.line("parking policy", fmt.Sprintf("%q is unknown (vocabulary: off/try/required) - phases inheriting it refuse at setup", cfg.ParkingPolicy))
	}

	showMemoryLine(w, cfg)

	switch cfg.MemoryLock {
	case "", "off":
		w.line("memory lock", "off: engine pages are never pinned")
	case "try":
		w.line("memory lock", "try: pinning is attempted and reported honestly either way")
	case "required":
		w.line("memory lock", "required: an OS refusal makes the run a setup error (exit 2), never a quieter run")
	default:
		w.line("memory lock", fmt.Sprintf("%q is unknown (vocabulary: off/try/required) - runs refuse it at setup", cfg.MemoryLock))
	}

	showToolLine(w, "CO backend", cfg.CoTool, "empty: no Curve Optimizer tool configured - the tool never searches PATH by itself")
	showExternalTools(w, cfg.External)

	if len(cfg.Phases) == 0 {
		w.line("scenario phases", "none configured: the 'scenario' command refuses until a \"phases\" list exists (presets like 'quick-check' are built-in, not config phases)")
	} else {
		names := make([]string, 0, len(cfg.Phases))
		for i, ph := range cfg.Phases {
			if ph.Name == "" {
				names = append(names, fmt.Sprintf("phase %d (unnamed)", i+1))
				continue
			}
			names = append(names, ph.Name)
		}
		w.line("scenario phases", fmt.Sprintf("%d: %s ('cpu-stability scenario -dry-run' compiles each into its concrete run)", len(cfg.Phases), strings.Join(names, ", ")))
	}
}

// showMemoryLine states the memory-engine budget as written plus what
// it means, never an auto-resolved number (that is computed at run
// admission, not here) and never a silently dropped invalid value.
func showMemoryLine(w *cfgPrinter, cfg config.Config) {
	if strings.TrimSpace(cfg.Memory) == "" {
		w.line("memory engine", "off: no \"memory\" budget configured, RAM verification never silently happens (RAM-promising presets will be refused up front, by name)")
		return
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Memory), "auto") {
		w.line("memory engine", "auto: the budget is computed at run admission from this machine's free memory - deliberately NOT computed here (a config view must not pretend to know the machine moment the run will see)")
		return
	}
	req, err := cfg.MemoryRequest()
	if err != nil {
		w.line("memory engine", fmt.Sprintf("%q as written is NOT a valid budget: every run attempt refuses up front (exit 2): %v - the value stays shown as written, nothing is guessed around it", cfg.Memory, err))
		return
	}
	w.line("memory engine", fmt.Sprintf("%q accepted as written: exactly %s - runs will ask for this budget and it is never quietly shrunk (admission can still refuse it as too big for the machine then, honestly)", cfg.Memory, human.Bytes(req.Bytes)))
}

// showToolLine reports a configured executable path and whether the
// FILE exists. Reading the directory is the whole extent of the check:
// nothing is ever launched, and existence never means runnable,
// licensed or version-correct.
func showToolLine(w *cfgPrinter, label, path, emptyNote string) {
	if path == "" {
		w.line(label, emptyNote)
		return
	}
	w.line(label, fmt.Sprintf("%s  %s", path, existenceNote(path)))
	w.ind("checked by reading the directory only; the tool was never executed, and a present file is not a promise it runs correctly")
}

func showExternalTools(w *cfgPrinter, ext map[string]workload.ExternalSpec) {
	if len(ext) == 0 {
		w.line("external tools", "none registered")
		return
	}
	names := make([]string, 0, len(ext))
	for name := range ext {
		names = append(names, name)
	}
	sort.Strings(names)
	w.line("external tools", fmt.Sprintf("%d registered (paths checked by reading directories only - never executed; relative paths resolve against the current working directory):", len(names)))
	for _, name := range names {
		spec := ext[name]
		wrap := fmt.Sprintf("%s -> %s  %s", name, spec.Command, existenceNote(spec.Command))
		if spec.Adapter != "" {
			wrap += "  adapter=" + spec.Adapter
		}
		w.ind(wrap)
	}
	w.ind("existence here never means the tool runs correctly; 'cpu-stability tools status' is where each adapter's honest state is reported")
}

// existenceNote distinguishes the three facts the filesystem can give:
// present file, confirmed missing, and uncheckable (a stat error is
// NOT silently missing, and never a guess about what's there).
func existenceNote(path string) string {
	switch st, err := os.Stat(path); {
	case err == nil && st.Mode().IsRegular():
		return "[file present]"
	case err == nil:
		return "[present but NOT a regular file]"
	case errors.Is(err, os.ErrNotExist):
		return "[FILE MISSING - any run using it will refuse it by name]"
	default:
		return fmt.Sprintf("[cannot check: %v]", err)
	}
}

type cfgPrinter struct{}

func newCfgPrinter() *cfgPrinter { return &cfgPrinter{} }

func (p *cfgPrinter) line(label, value string) {
	fmt.Printf("  %-17s %s\n", label+":", value)
}

func (p *cfgPrinter) ind(detail string) {
	fmt.Printf("  %17s  %s\n", " ", "- "+detail)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func faulNote(stop bool) string {
	if stop {
		return " - a confirmed fault aborts the run"
	}
	return " - a confirmed fault is journaled and the run keeps going (per-phase stop_on_fault can still override)"
}

// formatCPUList echoes the selector as configured; long lists are
// truncated with an honest count, never drawn as a range they may not
// be (the file order is the user's).
func formatCPUList(cores []int) string {
	limit := 12
	if len(cores) <= limit {
		parts := make([]string, len(cores))
		for i, c := range cores {
			parts[i] = strconv.Itoa(c)
		}
		return strings.Join(parts, ",")
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = strconv.Itoa(cores[i])
	}
	return fmt.Sprintf("%s,... (+%d more, exact list stays in the file)", strings.Join(parts, ","), len(cores)-limit)
}
