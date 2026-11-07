package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

type SessionMode string

const (
	sessionAllCore SessionMode = "all-core"
	sessionPerCore SessionMode = "per-core"
)

// SessionOptions describes an interactive sequence of one or more runner
// invocations. Physical cores are normalized indices shown by `topology`.
// The JSON tags double as the on-disk recipe format for lastrun.json:
// after a crash the menu can replay this exact choice without questions.
type SessionOptions struct {
	Mode           SessionMode   `json:"mode"`
	PhysicalCores  []int         `json:"physical_cores"`
	Workload       string        `json:"workload"`
	ThreadsPerCore int           `json:"threads_per_core"`
	Duration       time.Duration `json:"duration_ns"`
	Passes         int           `json:"passes"`
	StopOnFault    bool          `json:"stop_on_fault"`
	ShowMetrics    bool          `json:"show_metrics"`
	LogFile        string        `json:"log_file"`
	PerProcess     bool          `json:"per_process"` // one pinned subprocess per logical CPU
	Rotate         bool          `json:"rotate"`      // with PerProcess: exclusive windows, one core at a time
	// Execution policy and the resolver's reason (U01, menu plan
	// section 4): provenance for WHY this worker mode. "" = the recipe
	// predates the field (unknown, never assumed auto). The concrete
	// booleans above stay what actually runs: a replay never
	// re-resolves and never applies today's settings override to
	// yesterday's request.
	Execution       string  `json:"execution,omitempty"`
	ExecutionReason string  `json:"execution_reason,omitempty"`
	ControlCore     *int    `json:"control_core,omitempty"`
	ReserveControl  string  `json:"reserve_control,omitempty"` // helper-headroom policy, carried like the control core itself
	MaxTemp         float64 `json:"max_temp,omitempty"`
	Telemetry       bool    `json:"telemetry,omitempty"`
	BeatEvery       string  `json:"beat_interval,omitempty"`
	// RAM-family settings belong to the session's promise, not to the
	// machine's current config (audit A03): the memory engine budget,
	// the page-lock policy and the ram-* seed are snapshotted here and
	// carried to the executor. Without them a menu-picked ram-* run
	// reached the preflight refusal with an empty budget even when
	// config.json had one configured.
	Memory     string `json:"memory,omitempty"`      // "auto" or an exact size; "" = engine off
	MemoryLock string `json:"memory_lock,omitempty"` // "" / "off" = never, "try", "required"
	RamSeed    uint64 `json:"ram_seed,omitempty"`    // 0 is a valid seed (the default), not "unset"
}

// sessionDefaults snapshots durable settings so repeating a session keeps
// its safety and monitoring policy even if config.json changes afterwards.
func sessionDefaults(cfg config.Config) SessionOptions {
	s := SessionOptions{StopOnFault: cfg.StopOnFault, ShowMetrics: cfg.ShowMetrics,
		LogFile: cfg.LogFile, MaxTemp: cfg.MaxTemp, Telemetry: cfg.Telemetry, BeatEvery: cfg.BeatEvery,
		Memory: cfg.Memory, MemoryLock: cfg.MemoryLock, RamSeed: cfg.RamSeed, ReserveControl: cfg.ReserveControl}
	if cfg.ControlCore != nil {
		core := *cfg.ControlCore
		s.ControlCore = &core
	}
	return s
}

func sessionRunOptions(s SessionOptions, run sessionRun, logFile string) (plan.Options, error) {
	cfg := config.Default()
	cfg.Test, cfg.WorkloadA, cfg.WorkloadB = "general", s.Workload, s.Workload
	cfg.Cores, cfg.Workers, cfg.Duration = run.logicalCPUs, 1, s.Duration.String()
	cfg.LogFile, cfg.StopOnFault, cfg.ShowMetrics = logFile, s.StopOnFault, s.ShowMetrics
	cfg.ControlCore, cfg.MaxTemp, cfg.Telemetry, cfg.BeatEvery = s.ControlCore, s.MaxTemp, s.Telemetry, s.BeatEvery
	cfg.ReserveControl = s.ReserveControl
	cfg.Memory, cfg.MemoryLock, cfg.RamSeed = s.Memory, s.MemoryLock, s.RamSeed
	opt, err := plan.OptionsFromConfig(cfg)
	opt.PerProcess, opt.Rotate = s.PerProcess, s.Rotate
	return opt, err
}

type sessionRun struct {
	pass         int
	physicalCore *int // nil for one simultaneous all-core run
	logicalCPUs  []int
}

func parseNumberSet(value string, limit int) ([]int, error) {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return nil, fmt.Errorf("no selectable values are available")
	}
	if strings.EqualFold(value, "all") || value == "" {
		out := make([]int, limit)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}

	seen := make(map[int]struct{})
	var out []int
	add := func(value int) error {
		if value < 0 || value >= limit {
			return fmt.Errorf("%d is outside the available range 0..%d", value, limit-1)
		}
		if _, exists := seen[value]; exists {
			return nil
		}
		seen[value] = struct{}{}
		out = append(out, value)
		return nil
	}

	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, fmt.Errorf("empty item in %q", value)
		}
		if !strings.Contains(token, "-") {
			n, err := strconv.Atoi(token)
			if err != nil {
				return nil, fmt.Errorf("%q is neither a number nor a range", token)
			}
			if err := add(n); err != nil {
				return nil, err
			}
			continue
		}

		bounds := strings.Split(token, "-")
		if len(bounds) != 2 {
			return nil, fmt.Errorf("%q is not a valid inclusive range", token)
		}
		first, errFirst := strconv.Atoi(strings.TrimSpace(bounds[0]))
		last, errLast := strconv.Atoi(strings.TrimSpace(bounds[1]))
		if errFirst != nil || errLast != nil || first > last {
			return nil, fmt.Errorf("%q is not a valid ascending range", token)
		}
		for n := first; n <= last; n++ {
			if err := add(n); err != nil {
				return nil, err
			}
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("select at least one value")
	}
	return out, nil
}

func selectedHaveSMT(topology sysinfo.Topology, selected []int) bool {
	for _, index := range selected {
		if core, ok := physicalCore(topology, index); ok && len(core.LogicalCPUs) > 1 {
			return true
		}
	}
	return false
}

func physicalCore(topology sysinfo.Topology, index int) (sysinfo.PhysicalCore, bool) {
	for _, core := range topology.PhysicalCores {
		if core.Index == index {
			return core, true
		}
	}
	return sysinfo.PhysicalCore{}, false
}

func logicalCPUsForPhysicalCore(topology sysinfo.Topology, index, threads int) ([]int, error) {
	core, ok := physicalCore(topology, index)
	if !ok {
		return nil, fmt.Errorf("physical core %d does not exist", index)
	}
	if threads < 1 {
		return nil, fmt.Errorf("threads per core must be at least 1")
	}
	if len(core.LogicalCPUs) < threads {
		return nil, fmt.Errorf("physical core %d exposes only %d logical CPU(s), cannot select %d", index, len(core.LogicalCPUs), threads)
	}

	siblings := append([]sysinfo.LogicalCPU(nil), core.LogicalCPUs...)
	sort.Slice(siblings, func(i, j int) bool {
		if siblings[i].Group != siblings[j].Group {
			return siblings[i].Group < siblings[j].Group
		}
		return siblings[i].Number < siblings[j].Number
	})
	out := make([]int, 0, threads)
	for _, cpu := range siblings[:threads] {
		if cpu.Group != 0 {
			return nil, fmt.Errorf("logical CPU g%d:%d requires processor-group affinity, which is not implemented yet", cpu.Group, cpu.Number)
		}
		out = append(out, int(cpu.Number))
	}
	return out, nil
}

func buildSessionRuns(session SessionOptions, topology sysinfo.Topology) ([]sessionRun, error) {
	if session.Mode != sessionAllCore && session.Mode != sessionPerCore {
		return nil, fmt.Errorf("unknown session mode %q", session.Mode)
	}
	if session.Passes < 1 {
		return nil, fmt.Errorf("passes must be at least 1")
	}
	if session.Duration <= 0 {
		return nil, fmt.Errorf("duration must be positive")
	}
	if len(session.PhysicalCores) == 0 {
		return nil, fmt.Errorf("select at least one physical core")
	}
	if _, ok := workload.Get(session.Workload); !ok {
		return nil, fmt.Errorf("unknown workload %q", session.Workload)
	}

	logicalByCore := make(map[int][]int, len(session.PhysicalCores))
	for _, core := range session.PhysicalCores {
		logical, err := logicalCPUsForPhysicalCore(topology, core, session.ThreadsPerCore)
		if err != nil {
			return nil, err
		}
		logicalByCore[core] = logical
	}

	var runs []sessionRun
	for pass := 1; pass <= session.Passes; pass++ {
		if session.Mode == sessionAllCore {
			var logical []int
			for _, core := range session.PhysicalCores {
				logical = append(logical, logicalByCore[core]...)
			}
			runs = append(runs, sessionRun{pass: pass, logicalCPUs: logical})
			continue
		}
		for _, core := range session.PhysicalCores {
			coreCopy := core
			runs = append(runs, sessionRun{
				pass:         pass,
				physicalCore: &coreCopy,
				logicalCPUs:  append([]int(nil), logicalByCore[core]...),
			})
		}
	}
	return runs, nil
}

func writeSessionPlan(w io.Writer, session SessionOptions, topology sysinfo.Topology) {
	runs, err := buildSessionRuns(session, topology)
	if err != nil {
		fmt.Fprintf(w, "Cannot build session plan: %v\n", err)
		return
	}
	modeDescription := "all selected physical cores run simultaneously"
	if session.Mode == sessionPerCore {
		modeDescription = "selected physical cores run one after another"
	}
	execution := "in-process worker threads"
	if session.PerProcess {
		execution = "one pinned subprocess per logical CPU"
		if session.Rotate {
			execution = "rotating subprocesses: exclusive window per core, wake latency measured"
		}
	}
	// U01: the plan shows not just the mode but WHY - the policy and
	// the resolver's reason. A recipe predating the field shows the
	// mode alone (unknown provenance, no fabricated reason).
	if r := strings.TrimSpace(session.ExecutionReason); r != "" {
		execution += fmt.Sprintf(" (policy %s: %s)", session.Execution, r)
	}
	estimated := session.Duration * time.Duration(len(runs))

	wl, _ := workload.Get(session.Workload)
	fmt.Fprintln(w, "==============================================================================")
	fmt.Fprintln(w, " CPU Stability Checker — interactive session plan")
	fmt.Fprintln(w, "==============================================================================")
	fmt.Fprintf(w, "Test mode: ........................... %s — %s\n", session.Mode, modeDescription)
	fmt.Fprintf(w, "Execution: ........................... %s\n", execution)
	fmt.Fprintf(w, "Physical cores (zero-based): ........ %s\n", human.Ints(session.PhysicalCores))
	fmt.Fprintf(w, "Threads per physical core: .......... %d\n", session.ThreadsPerCore)
	if session.ReserveControl == "logical" {
		// Coverage is part of the promise this screen confirms: the
		// policy is named here, and every run's journal states the
		// cpu set that actually loaded.
		fmt.Fprintln(w, "Helper headroom: .................. reserve_control=logical: with no free cpu the highest-numbered logical cpu leaves the worker set (watchdog/journal/panel get their own cpu; coverage drops by one, the journal names the set)")
	}
	fmt.Fprintf(w, "Load algorithm: ..................... %s — %s\n", session.Workload, wl.Description)
	if session.Memory != "" {
		// The budget is part of the promise the user confirms: show it
		// (and the seed for RAM families) before the first byte of load,
		// never only in the engine's own log.
		lock := strings.TrimSpace(session.MemoryLock)
		if lock == "" || lock == "off" {
			lock = "off"
		}
		fmt.Fprintf(w, "Memory engine: ...................... budget %s, page lock %s\n", session.Memory, lock)
		if wl.RequiresMemory {
			fmt.Fprintf(w, "RAM test seed: ........................ %d (0 is a valid seed)\n", session.RamSeed)
		}
	}
	fmt.Fprintf(w, "Duration of each run: ............... %s\n", session.Duration)
	fmt.Fprintf(w, "Complete passes: .................... %d\n", session.Passes)
	fmt.Fprintf(w, "Total individual runs: .............. %d\n", len(runs))
	fmt.Fprintf(w, "Estimated active test time: ......... %s\n", estimated)
	fmt.Fprintf(w, "Stop on workload fault: ............. %s\n", human.Enabled(session.StopOnFault))
	fmt.Fprintf(w, "Live metrics: ....................... %s\n", human.Enabled(session.ShowMetrics))
	if session.MaxTemp > 0 {
		fmt.Fprintf(w, "Temperature control: ................ %.1f°C; shared sensor, stops on data loss\n", session.MaxTemp)
	} else {
		fmt.Fprintln(w, "Temperature control: ................ off (max_temp=0)")
	}
	fmt.Fprintf(w, "Log naming pattern: ................. %s\n", sessionLogPattern(session.LogFile, session.Mode))
	fmt.Fprintln(w, "==============================================================================")
}

// runSessionWithPanel runs every planned run of the session. ext is
// the external-tool spec map the session resolves against (config or
// recipe snapshot): it only feeds the plan hash (A11 - the registered
// command path is part of the experiment's identity), never a second
// source of execution truth.
// recipeID binds the outcome receipt to the saved recipe (C3): the
// menu/repeat paths pass the id they saved/replayed, "" means no
// receipt is kept (standalone/test callers).
func runSessionWithPanel(session SessionOptions, topology sysinfo.Topology, panel *dashboard.Console, ext map[string]workload.ExternalSpec, recipeID string) int {
	// B08: the session holds the run lock across ALL passes: the
	// per-pass join in executeOptWithPanel would flicker the OS
	// lock between passes and let another load slip into the gap.
	// The refusal arrives before any attempt is armed.
	relLock, lerr := acquireRunLockShared()
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "session refused: %v\n", lerr)
		return 2
	}
	defer relLock()
	// U02: the session gets its unique id and a running manifest
	// before anything is built or loaded. A setup failure below is
	// still history (setup_error), and the id names the child log
	// files, so two sessions started in the same second stay
	// distinct on disk too (menu plan section 11). A random-source
	// failure refuses: a bare timestamp fallback could silently
	// merge two sessions into one history record.
	st, serr := newSessionTracker("session", nil, nil)
	if serr != nil {
		fmt.Printf("Cannot start session: %v\n", serr)
		newAttemptReceipt(recipeID, 0, nil, nil).finishSetup(2)
		return 2
	}
	req := lastRun{Kind: lastRunSession, Session: &session, Config: &config.Config{External: ext}}
	// Bind the saved recipe this session replays: the menu/repeat
	// paths pass the id they saved, and the manifest embeds that
	// recipe only when it is exactly this request. Otherwise the
	// manifest embeds the session as built here (the actual
	// choices, unbound to any recipe id) - never a guess.
	if lr, err := loadLastRun(); err == nil && recipeID != "" && lr.RecipeID == recipeID {
		st.m.RecipeID, st.m.ProfileID, st.m.ProfileVersion = lr.RecipeID, lr.ProfileID, lr.ProfileVersion
		st.m.Request = lr
	} else {
		st.m.Request = &req
	}
	runs, err := buildSessionRuns(session, topology)
	if err != nil {
		fmt.Printf("Cannot start session: %v\n", err)
		newAttemptReceipt(recipeID, 0, nil, nil).finishSetup(2)
		st.finishSetup(2)
		return 2
	}

	sessionID := st.m.SessionID
	options := make([]plan.Options, len(runs))
	logs := make([]string, 0, len(runs))
	hashes := make([]string, 0, len(runs))
	manifestRuns := make([]manifestRun, 0, len(runs))
	for i, run := range runs {
		options[i], err = sessionRunOptions(session, run, sessionLogFile(session.LogFile, sessionID, run))
		if err != nil {
			fmt.Printf("Cannot start session: %v\n", err)
			newAttemptReceipt(recipeID, 0, nil, nil).finishSetup(2)
			st.finishSetup(2)
			return 2
		}
		options[i].PlanHash = planHash(req, options[i])
		options[i].SessionID = sessionID
		logs = append(logs, options[i].LogFile)
		hashes = append(hashes, options[i].PlanHash)
		manifestRuns = append(manifestRuns, manifestRun{
			Name:    fmt.Sprintf("pass %d", run.pass),
			Log:     options[i].LogFile,
			Journal: journal.Path(options[i].LogFile),
		})
	}
	st.setRuns(manifestRuns)
	// Managed stream (nil-safe): the plan as resolved, then the
	// session starting under its fresh identity.
	currentEvents.emit("resolved", "", resolvedPayload{Runs: len(runs), PlanHashes: hashes}, false)
	currentEvents.emit("started", "", startedPayload{Runs: len(runs), Cycles: 1}, false)
	fmt.Printf("\nStarting session %s with %d individual run(s).\n", sessionID, len(runs))
	tr := newAttemptReceipt(recipeID, len(runs), logs, hashes)

	// An interactive menu keeps its panel alive through the Enter prompt.
	// Standalone callers own a panel only for the duration of the session.
	if panel == nil {
		panel = dashboard.New(os.Stdout)
		if panel.Active() {
			panel.Start()
		}
		defer panel.Stop()
	}

	unverified := 0 // runs that ended clean but never confirmed their own checking
	results := make([]plan.Result, 0, len(runs))
	for index, run := range runs {
		label := "all selected physical cores"
		if run.physicalCore != nil {
			label = fmt.Sprintf("physical core %d", *run.physicalCore)
		}
		panel.Print(fmt.Sprintf("\nRun %d/%d | pass %d/%d | %s | logical CPU %s\n",
			index+1, len(runs), run.pass, session.Passes, label, human.Ints(run.logicalCPUs)))
		phaseID := fmt.Sprintf("pass %d | %s", run.pass, label)
		currentEvents.setPhase(phaseID)
		currentEvents.emit("phase_started", phaseID, sessionPhaseStartedPayload{LogicalCPUs: run.logicalCPUs}, false)

		res := executeOptWithPanel(options[index], panel)
		results = append(results, res)
		emitPhaseResult(fmt.Sprintf("pass %d | %s", run.pass, label), res)
		if res.Exit != 0 {
			panel.Print(fmt.Sprintf("Session stopped during run %d/%d (exit=%d, verification=%s).\n",
				index+1, len(runs), res.Exit, verificationLabel(res.Validation)))
			tr.finish(res.Exit, results)
			st.finish(res.Exit, results)
			return res.Exit
		}
		// Exit 0 with an unconfirmed verification claim is said out
		// loud here: the session ended clean, but this run never
		// proved its own checking, and "passed" would invent that.
		if !verificationConfirmed(res.Validation) {
			unverified++
			panel.Print(fmt.Sprintf("Run %d/%d ended clean but verification=%s — not counted as passed.\n",
				index+1, len(runs), verificationLabel(res.Validation)))
		}
	}
	if unverified == 0 {
		panel.Print(fmt.Sprintf("\nSession completed: %d/%d runs passed (verification confirmed).\n", len(runs), len(runs)))
	} else {
		panel.Print(fmt.Sprintf("\nSession completed: %d/%d runs ended clean, %d without confirmed verification.\n",
			len(runs), len(runs), unverified))
	}
	tr.finish(0, results)
	st.finish(0, results)
	return 0
}

func sessionLogPattern(base string, mode SessionMode) string {
	corePart := ""
	if mode == sessionPerCore {
		corePart = "-coreNN"
	}
	dir, file := filepath.Split(base)
	ext := filepath.Ext(file)
	name := strings.TrimSuffix(file, ext)
	// The session id is a UTC second plus random bits (U02): the
	// random part is what keeps two sessions started in the same
	// second from sharing one set of log files.
	return filepath.Join(dir, name+"-YYYYMMDD-HHMMSS-RANDOM-passNN"+corePart+ext)
}

func sessionLogFile(base, sessionID string, run sessionRun) string {
	dir, file := filepath.Split(base)
	ext := filepath.Ext(file)
	name := strings.TrimSuffix(file, ext)
	suffix := fmt.Sprintf("-%s-pass%02d", sessionID, run.pass)
	if run.physicalCore != nil {
		suffix += fmt.Sprintf("-core%02d", *run.physicalCore)
	}
	return filepath.Join(dir, name+suffix+ext)
}
