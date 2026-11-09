package cli

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/subprocess"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// scenarioCmd runs the config's "phases" list as one session: every phase
// is a full run with its own layout, sharing the log and journal files.
// The scenario stops at the first phase that faults or errors — skipping
// past a faulted core and continuing is a later roadmap feature.
func scenarioCmd(args []string) int {
	return scenarioCmdWithUI(args, nil)
}

func scenarioCmdWithUI(args []string, ui *screenUI) int {
	fs := flag.NewFlagSet("scenario", flag.ExitOnError)
	continueOnFault := fs.Bool("continue-on-fault", false, "keep running later phases, excluding cores that faulted")
	cycles := fs.Int("cycles", 1, "run the whole phase list this many times in a row")
	seed := fs.Int64("seed", -1, "shuffle the phase order reproducibly from this seed (fresh shuffle per cycle); -1 keeps the configured order")
	dryRun := fs.Bool("dry-run", false, "print the compiled phase list and exit without applying any load")
	presetName := fs.String("preset", "", "run a built-in scenario preset instead of the config phases: "+strings.Join(presetNames(), ", "))
	fs.Parse(args)
	if *cycles < 1 {
		fmt.Fprintln(os.Stderr, "scenario: -cycles must be at least 1")
		return 2
	}

	cfg, err := config.LoadStrict(config.DefaultPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run 'cpu-stability config init' to create a default config.json.")
		return 2
	}
	workload.RegisterExternal(cfg.External)

	if *presetName != "" {
		p, ok := lookupPreset(*presetName)
		if !ok {
			fmt.Fprintf(os.Stderr, "scenario: unknown preset %q; built-in presets:\n%s", *presetName, presetListText())
			return 2
		}
		// The preset IS the phase list: a configured phases list is
		// overridden visibly, never silently merged (a merged scenario
		// would be neither the preset nor the user's plan).
		//
		// A preset that promises memory verification is refused HERE,
		// before any load, when no budget exists (audit A05): the
		// alternative was a profile quietly dropping its RAM phases
		// (mixed) or dying mid-scenario - neither states what actually
		// ran. The reason names the fix.
		if p.NeedsMemory && strings.TrimSpace(cfg.Memory) == "" {
			fmt.Fprintf(os.Stderr, "scenario: preset %q verifies memory regions and needs a memory budget: set \"memory\" in config.json (\"auto\" or an exact size like \"2GiB\"), or pick another preset\n", p.Name)
			return 2
		}
		cfg.Phases = p.Phases(cfg)
		fmt.Printf("scenario preset %q: %s\n", p.Name, p.Description)
	}
	if len(cfg.Phases) == 0 {
		fmt.Fprintln(os.Stderr, `config.json has no "phases" list; nothing to run (or pass -preset)`)
		return 2
	}

	// Resolve and validate every phase (generators and repetitions
	// included) before applying any load, so a typo in phase 5 fails the
	// scenario up front, not mid-run.
	opts, notices, err := compileScenario(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario: %v\n", err)
		return 2
	}
	for _, n := range notices {
		fmt.Println(n)
	}

	if *dryRun {
		printScenarioPlan(os.Stdout, cfg, opts, *cycles)
		return 0
	}
	// Resource refusals surface BEFORE saving and before any load: a
	// phase the machine cannot honor must not overwrite the last good
	// recipe on its way to a mid-scenario exit 2 (audit A04), and it
	// must not heat cores first.
	if errs := scenarioMemoryRefusals(cfg.External, opts); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "scenario: %s\n", e)
		}
		return 2
	}
	// Saved AFTER compile: a randomized core-cycle wrote its realized seed
	// back into the generator, so the saved plan replays the same walk.
	// And saved FULL (audit A04): the resolved-config snapshot, the
	// cycles, the shuffle seed and the continue policy the command line
	// asked for - repeat replays this request, not today's config.json.
	var seedPtr *int64
	if *seed >= 0 {
		s := *seed
		seedPtr = &s
	}
	lr := scenarioRecipe(cfg, *cycles, seedPtr, *continueOnFault, *presetName)
	return runScenarioRequest(lr, opts, *cycles, *seed, *continueOnFault, ui, true)
}

// scenarioSession builds a generator scenario in a dialog and runs it
// through the exact same compile+execute pipeline as the 'scenario'
// command: the dialog only produces config.Phase values, so there is no
// second execution path that could drift from the config file.
func scenarioSession(in *bufio.Reader, ui *screenUI, cfg config.Config) int {
	w := newWizard(in, ui, "Scenario generator")

	genIndex := w.choose("Scenario generator", "Choose a generator", []string{
		"Burst — drive one core at a time from idle straight into load (wake transient)",
		"Pairs — directed aggressor/observer phases across core pairs",
		"Curve — ramp the number of simultaneously loaded cores (voltage-droop probe)",
		"Cycle — walk every physical core one at a time (SMT policy, replay order)",
		"Preset — run a built-in scenario (CPU / RAM / core-cycle / transitions / mixed)",
	}, 0)
	if genIndex < 0 {
		return 0
	}

	var ph *config.Phase
	presetUsed := "" // provenance only: the phases below are what replays
	if genIndex == 4 {
		// A preset produces ordinary phases too - the same compile
		// pipeline, the same journal, the same repeat. Nothing about
		// running a preset differs from running a hand-written list.
		// A RAM-promising preset without a budget goes back to the
		// list with the reason shown: fixable in config, not fatal,
		// and never a quietly CPU-shrunken "mixed" run (audit A05).
		names := presetNames()
		var p preset
		for {
			idx := w.choose("Scenario preset", "Choose a preset", names, 0)
			if idx < 0 {
				return 0
			}
			cand, ok := lookupPreset(names[idx])
			if !ok {
				fmt.Fprintf(os.Stderr, "scenario: unknown preset %q\n", names[idx])
				return 2
			}
			if cand.NeedsMemory && strings.TrimSpace(cfg.Memory) == "" {
				msg := fmt.Sprintf("preset %q verifies memory regions and needs a memory budget: set \"memory\" in config.json (\"auto\" or a size like \"2GiB\"), or pick another preset", cand.Name)
				if ui.hasScreen() {
					w.note = msg
				} else {
					fmt.Println(msg)
				}
				continue
			}
			p = cand
			break
		}
		cfg.Phases = p.Phases(cfg)
		presetUsed = p.Name
		if !ui.hasScreen() {
			fmt.Printf("preset %q: %s\n", p.Name, p.Description)
		}
	} else {
		nCPU := runtime.NumCPU()
		w.hints = []string{
			fmt.Sprintf("This machine exposes %d logical CPUs, numbered 0 through %d.", nCPU, nCPU-1),
			"Enter a list such as 0,2,5-8. Ranges are inclusive; 'all' selects every CPU.",
		}
		if !ui.hasScreen() {
			fmt.Println("\nLogical CPUs")
			for _, hint := range w.hints {
				fmt.Println(hint)
			}
		}
		var cores []int
		allCores := true
		for {
			value := w.promptLine("Logical CPUs", "all")
			selected, err := parseNumberSet(value, nCPU)
			if err == nil {
				cores = selected
				allCores = strings.EqualFold(strings.TrimSpace(value), "all")
				w.record("Logical CPUs", value)
				break
			}
			if ui.hasScreen() {
				w.note = fmt.Sprintf("Invalid selection: %v", err)
			} else {
				fmt.Printf("Invalid selection: %v\n", err)
			}
		}
		w.hints = nil

		p := config.Phase{Name: "session"}
		ph = &p
		switch genIndex {
		case 0:
			ph.Burst = &config.Burst{
				Cores:        cores,
				Repeat:       w.askPositiveInt("", "Cycles per core", 1),
				LoadDuration: w.promptLine("Load duration per burst", cfg.Duration),
				IdleDuration: w.promptLine("Idle (settle) before each burst", "2s"),
			}
		case 1:
			ph.Pairs = &config.Pairs{
				Cores:    cores,
				Scope:    w.promptLine("Pair scope: ccx (same L3), smt (SMT siblings) or all", "ccx"),
				Duration: w.promptLine("Duration per pair", cfg.Duration),
				Settle:   w.promptLine("Idle before each pair (empty = none)", ""),
			}
		case 2:
			curve := &config.Curve{
				Cores:    cores,
				Duration: w.promptLine("Duration per step", cfg.Duration),
				Settle:   w.promptLine("Common idle before each step", "2s"),
			}
			if steps := w.promptLine("Loaded-core counts per step (empty = doubling)", ""); steps != "" {
				parsed, serr := parseNumberSet(steps, len(cores)+1)
				if serr != nil {
					fmt.Fprintf(os.Stderr, "curve steps: %v\n", serr)
					return 2
				}
				curve.Steps = parsed
			}
			ph.Curve = curve
		case 3:
			order := w.promptLine("Walk order: sequential, seeded or randomized", "sequential")
			cycle := &config.Cycle{
				SMT:      w.promptLine("SMT policy: one (single logical CPU per core) or both (the whole core at once)", "one"),
				Order:    order,
				Load:     w.promptLine("Load workload", cfg.WorkloadA),
				Duration: w.promptLine("Duration per core", cfg.Duration),
				Settle:   w.promptLine("Idle on the core before its load", "2s"),
			}
			if strings.EqualFold(order, "seeded") {
				// Seed 0 is a valid seed here, so the positive-int helper (which
				// rejects 0) is the wrong prompt: take any whole number.
				seed := int64(0)
				for {
					raw := w.promptLine("Shuffle seed (0 is a valid seed)", "0")
					v, perr := strconv.ParseInt(raw, 10, 64)
					if perr == nil {
						seed = v
						break
					}
					fmt.Printf("Invalid seed: %v\n", perr)
				}
				cycle.Seed = &seed
			}
			if !allCores {
				cycle.Cores = cores // restrict the walk to these cores' physical owners
			}
			ph.Cycle = cycle
		}
	}
	cycles := w.askPositiveInt("", "Repeat the whole scenario", 1)
	if cycles <= 0 {
		return 0
	}
	continueOnFault := strings.EqualFold(w.promptLine("Continue after a faulted core, excluding it? (y/N)", "n"), "y")

	if ph != nil {
		cfg.Phases = []config.Phase{*ph}
	}
	opts, notices, err := compileScenario(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario: %v\n", err)
		return 2
	}
	for _, n := range notices {
		fmt.Println(n)
	}
	// Same resource gate as the command line: refuse before asking the
	// user to confirm a run the machine cannot honor, and before the
	// save could displace the last good recipe (audit A04).
	if errs := scenarioMemoryRefusals(cfg.External, opts); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "scenario: %s\n", e)
		}
		return 2
	}
	var buf bytes.Buffer
	printScenarioPlan(&buf, cfg, opts, cycles)
	w.showPlan(strings.Split(strings.TrimRight(buf.String(), "\n"), "\n"))
	if !w.confirm() {
		fmt.Println("Run cancelled. No load was started.")
		return 0
	}
	lr := scenarioRecipe(cfg, cycles, nil, continueOnFault, presetUsed)
	return runScenarioRequest(lr, opts, cycles, -1, continueOnFault, ui, true)
}

// compileScenario resolves and validates every phase — generators and
// repetitions included — before any load, so a typo in phase 5 fails the
// whole scenario up front. Both the 'scenario' command and the
// interactive scenario session share this single pipeline: the dialog
// only produces config.Phase values, never a second execution path.
// The returned notices tell what compile-time decisions the caller should
// show (a randomized core-cycle walk and its realized seed).
func compileScenario(cfg config.Config) ([]plan.Options, []string, error) {
	// Q04: a generator entry is not a run - it expands into phases it
	// builds itself, and none of them is a prepared-session (bursts)
	// phase. A parking word written on the generator could therefore
	// only ever be dropped: that is a promise the entry cannot keep,
	// so refuse it by name instead of letting it evaporate (the same
	// rule OptionsFromPhase enforces on ordinary phases).
	for i, ph := range cfg.Phases {
		if (ph.Burst != nil || ph.Pairs != nil || ph.Curve != nil || ph.Cycle != nil) && ph.ParkingPolicy != "" {
			return nil, nil, fmt.Errorf("phase %q: parking_policy needs a bursts phase that drives prepared sessions, but this entry is a generator that expands into its own ordinary phases; move the word to a burst phase or drop it", phaseLabel(ph.Name, i))
		}
		// The switch scope fields GENERATE groups inside one switch
		// phase; pairs/burst/curve/cycle EXPAND the entry into separate
		// phases - and those expansions would drop the switch words on
		// the floor. Two shapes in one entry define two experiments and
		// pick neither out loud, so refuse before any expansion runs.
		if (ph.SwitchScope != "" || len(ph.SwitchCores) > 0) && (ph.Burst != nil || ph.Pairs != nil || ph.Curve != nil || ph.Cycle != nil) {
			return nil, nil, fmt.Errorf("phase %q: switch_scope/switch_cores generate groups inside one pair-switch phase, while burst/pairs/curve/cycle expand the entry into separate phases - one shape per entry, pick one", phaseLabel(ph.Name, i))
		}
	}
	expanded, err := expandBursts(cfg.Phases, cfg)
	if err != nil {
		return nil, nil, err
	}
	inv, ccx, smt, err := topologyMaps(cfg.Phases)
	if err != nil {
		return nil, nil, err
	}
	expanded, err = expandPairs(expanded, cfg, ccx, smt)
	if err != nil {
		return nil, nil, err
	}
	expanded, err = expandSwitchGroups(expanded, inv, ccx, smt)
	if err != nil {
		return nil, nil, err
	}
	expanded, err = expandCurve(expanded, cfg)
	if err != nil {
		return nil, nil, err
	}
	groups, err := cycleGroups(expanded)
	if err != nil {
		return nil, nil, err
	}
	var notices []string
	expanded, err = expandCycle(expanded, cfg, groups, &notices)
	if err != nil {
		return nil, nil, err
	}
	opts := make([]plan.Options, 0, len(expanded))
	for i, ph := range expandRepeats(expanded) {
		opt, err := plan.OptionsFromPhase(cfg, ph)
		if err != nil {
			return nil, nil, fmt.Errorf("phase %d (%s): %w", i+1, phaseLabel(ph.Name, i), err)
		}
		opts = append(opts, opt)
	}
	return opts, notices, nil
}

// printScenarioPlan renders the compiled phase list for -dry-run: every
// generator-expanded phase with the parameters it will actually run
// with, the machine mapping, the resource plan, the requirements that
// would refuse the run, and an approximate journal size. Wall-clock is
// deliberately not promised - settle, rotation and external tool
// startup add on top - and a phase whose requirements the machine or
// config cannot meet is said so HERE, so a dry-run never promises a run
// that execution would refuse.
func printScenarioPlan(w io.Writer, cfg config.Config, opts []plan.Options, cycles int) {
	fmt.Fprintf(w, "scenario plan: %d phase(s)", len(opts))
	if cycles > 1 {
		fmt.Fprintf(w, " x %d cycle(s)", cycles)
	}
	fmt.Fprintln(w)

	// Machine mapping: what the walk and the pinning will be against.
	if topology, terr := sysinfo.DiscoverTopology(); terr != nil {
		fmt.Fprintf(w, "topology: unavailable (%v) - SMT-aware layouts will refuse if they need it\n", terr)
	} else {
		smt := "off"
		for _, core := range topology.PhysicalCores {
			if len(core.LogicalCPUs) > 1 {
				smt = "on"
				break
			}
		}
		fmt.Fprintf(w, "topology: %d logical / %d physical CPUs, SMT %s (source: %s)\n",
			runtime.NumCPU(), len(topology.PhysicalCores), smt, topology.Source)
	}

	// Resource plan: one memory budget per phase, computed by the
	// parent at phase start - never re-derived per worker.
	if strings.TrimSpace(cfg.Memory) == "" {
		fmt.Fprintln(w, "memory engine: off (no budget configured)")
	} else {
		fmt.Fprintf(w, "memory engine: budget %q, lock %q, seed %d - resolved once per phase, granted exactly to each child\n",
			strings.TrimSpace(cfg.Memory), orDefault(cfg.MemoryLock, "off"), cfg.RamSeed)
	}

	var total time.Duration
	for i, opt := range opts {
		label := opt.Phase
		if label == "" {
			label = fmt.Sprintf("phase %d", i+1)
		}
		mode := "in-process"
		if opt.PerProcess {
			mode = "per-process"
			if opt.Rotate {
				mode += "+rotate"
			}
			if opt.Settle > 0 {
				mode += fmt.Sprintf("+settle%s", opt.Settle)
			}
		}
		cores := "all"
		if len(opt.Cores) > 0 {
			cores = human.Ints(opt.Cores)
		}
		a, b := opt.A, opt.B
		if b == "" {
			b = "-"
		}
		if opt.HandoffBlocks > 0 {
			// The geometry is the promise AND the budget: rounds x
			// blocks define this phase, never the clock. all_or_none is
			// compiled in (the block-to-core partition makes every
			// participant load-bearing), so the plan line says so.
			fmt.Fprintf(w, "  %d) %s: test=%s a=%s cores=%s mode=handoff(%d blocks x %s, %d round(s), seed %d, lock=%q, all_or_none, budget: shared section %s admitted up front)\n",
				i+1, label, opt.TestName, opt.A, cores, opt.HandoffBlocks, human.Bytes(opt.HandoffBlockBytes),
				opt.HandoffRounds, opt.HandoffSeed, orDefault(opt.MemLock, "off"),
				human.Bytes(uint64(opt.HandoffBlocks)*opt.HandoffBlockBytes))
			continue
		}
		if plan.SwitchActive(opt) {
			// The prepared turn schedule, not a duration, defines this
			// phase: state the GROUPS (who loads together), the order
			// word and what the run will MEASURE (command->last STARTED
			// per turn, an upper bound incl the IPC round trip, plus
			// per-member views in the journal). The journal carries the
			// concrete turns before anything spawns.
			groups := make([]string, len(opt.SwitchPairs))
			for gi, group := range opt.SwitchPairs {
				ag, ob := "-", "-"
				if len(group) > 0 {
					ag = strconv.Itoa(group[0])
				}
				if len(group) > 1 {
					ob = strconv.Itoa(group[1])
				}
				groups[gi] = fmt.Sprintf("g%d[aggressor %s, observer %s]", gi, ag, ob)
			}
			span := time.Duration(opt.SwitchRounds) * time.Duration(len(opt.SwitchPairs)) * (opt.SwitchGap + opt.SwitchRun)
			sched := fmt.Sprintf("%d group(s) x %d round(s) gap=%s run=%s", len(opt.SwitchPairs), opt.SwitchRounds, opt.SwitchGap, opt.SwitchRun)
			if opt.SwitchOrder != "" {
				sched += " order=" + opt.SwitchOrder
				if opt.SwitchOrder == "seeded" {
					// The seed is the provenance of the realized order:
					// the journal carries the concrete turns, but the
					// plan line must say which seed produced them.
					sched += fmt.Sprintf(" seed=%d", opt.SwitchSeed)
				}
			}
			if opt.SwitchJitter > 0 {
				sched += fmt.Sprintf(" jitter=±%s seed=%d", opt.SwitchJitter, opt.SwitchSeed)
			}
			if opt.Parking != "" {
				sched += " parking=" + opt.Parking // Q04: the turn gaps ARE the measured idle windows
			}
			fmt.Fprintf(w, "  %d) %s: test=%s a=%s b=%s mode=switch(%s; %s; all_or_none; measures switch latency command->last-started per turn as an upper bound incl IPC) span≈%s\n",
				i+1, label, opt.TestName, a, b, strings.Join(groups, " "), sched, span)
			total += span
			continue
		}
		criterion := fmt.Sprintf("duration=%s", opt.Duration)
		if wl, ok := workload.Get(opt.A); ok && wl.Verification == workload.VerifyExternal {
			criterion = "ends on the tool's own protocol completion (evidence, not the clock)"
		}
		if opt.Bursts > 0 {
			// The prepared schedule, not a duration, defines this phase:
			// show exactly what will be commanded (the journal will carry
			// the concrete windows).
			span := time.Duration(opt.Bursts) * (opt.BurstGap + opt.BurstRun)
			sched := fmt.Sprintf("%d bursts gap=%s run=%s", opt.Bursts, opt.BurstGap, opt.BurstRun)
			if opt.BurstJitter > 0 {
				sched += fmt.Sprintf(" jitter=±%s seed=%d", opt.BurstJitter, opt.BurstSeed)
			}
			if opt.BurstStagger > 0 {
				sched += fmt.Sprintf(" stagger=%s", opt.BurstStagger)
			}
			if opt.Parking != "" {
				sched += " parking=" + opt.Parking // Q04: the plan line states the evidence policy the session will run
			}
			fmt.Fprintf(w, "  %d) %s: test=%s a=%s b=%s cores=%s mode=transitions(%s) span≈%s\n",
				i+1, label, opt.TestName, a, b, cores, sched, span)
			total += span
			continue
		}
		fmt.Fprintf(w, "  %d) %s: test=%s a=%s b=%s cores=%s workers=%d %s mode=%s\n",
			i+1, label, opt.TestName, a, b, cores, opt.Workers, criterion, mode)
		total += opt.Duration
	}
	fmt.Fprintf(w, "configured load time: %s per cycle (settle/rotation/tool startup add on top)\n", total)

	// Requirements the machine or config cannot meet: said now, in the
	// dry-run, because execution would refuse them (exit 2) - a plan
	// that silently "succeeds" on paper is exactly what this tool
	// promises never to do.
	for _, warn := range scenarioRequirements(cfg, opts) {
		fmt.Fprintf(w, "requirement: %s\n", warn)
	}

	records, beatRecords, approxBytes := journalEstimate(opts, cycles, runtime.NumCPU())
	fmt.Fprintf(w, "journal estimate: ~%d record(s), of which ~%d are core_beat (beat_interval grows/shrinks that)\n",
		records, beatRecords)
	fmt.Fprintf(w, "journal size estimate: ~%s (per-type averages measured from this schema's journals; actual size varies)\n",
		human.Bytes(uint64(approxBytes)))
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// scenarioMemoryRefusals lists the HARD refusals the resource
// preflight would make for these phases (transition phases judge slot
// A only: slot B never loads there, see run.go): the memory-engine
// promise the config cannot keep, and external tools whose registered
// command is not on disk. Unlike the dry-run's requirement notes,
// these stop the entry point before saving or loading anything: the
// recipe on disk stays the last one the machine could actually run.
func scenarioMemoryRefusals(ext map[string]workload.ExternalSpec, opts []plan.Options) []string {
	var errs []string
	for i, opt := range opts {
		gate := opt
		if gate.Bursts > 0 {
			gate.B = ""
		}
		if gate.HandoffBlocks > 0 {
			// Handoff phases judge slot A only (the coordinator drives
			// ram-handoff leases there), and their budget is the shared
			// section geometry: admitted HERE, with the same rule the
			// executor applies before mapping (B09) - the dry plan and
			// the run can never disagree about what the machine could
			// have honored.
			gate.B = ""
			if err := plan.HandoffAdmission(opt); err != nil {
				label := opt.Phase
				if label == "" {
					label = fmt.Sprintf("phase %d", i+1)
				}
				errs = append(errs, label+": "+err.Error())
			}
		}
		label := opt.Phase
		if label == "" {
			label = fmt.Sprintf("phase %d", i+1)
		}
		if err := plan.PreflightMemoryWorkloads(gate); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
		}
		if msg := externalToolMissing(ext, gate.A, gate.B); msg != "" {
			errs = append(errs, label+": "+msg)
		}
	}
	return errs
}

// externalToolMissing reports an external workload whose registered
// command is not readable on disk. Execution would die in the child's
// setup; every entry point - repeats above all - must say so BEFORE
// load: a moved, removed or renamed tool is a clear preflight refusal
// naming the path, never a silent substitution or a mid-run setup
// error after other phases already heated the machine (audit A04).
// Returns "" when every named workload is fine.
func externalToolMissing(ext map[string]workload.ExternalSpec, names ...string) string {
	for _, n := range names {
		spec, ok := ext[n]
		if !ok || n == "" {
			continue
		}
		if _, err := os.Stat(spec.Command); err != nil {
			return fmt.Sprintf("external tool %q is registered as %q but that command is not readable: %v", n, spec.Command, err)
		}
	}
	return ""
}

// scenarioRequirements lists the refusals execution would make: ram
// workloads without a budget (P06 preflight), a budget on an
// in-process phase (the engine is not applied there), and transition
// phases outside per-process mode.
func scenarioRequirements(cfg config.Config, opts []plan.Options) []string {
	var warns []string
	for i, opt := range opts {
		label := opt.Phase
		if label == "" {
			label = fmt.Sprintf("phase %d", i+1)
		}
		if err := plan.PreflightMemoryWorkloads(opt); err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", label, err))
		}
		if opt.Memory != "" && !opt.PerProcess {
			warns = append(warns, fmt.Sprintf("%s: memory budget configured but the phase runs in-process: the engine will NOT allocate in it", label))
		}
		if err := plan.ValidateTransition(opt); err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", label, err))
		}
	}
	// Q04 caps honesty: when any phase will observe parking, the plan
	// says WHICH source at WHICH cadence - and probes the source now,
	// so a machine where it cannot answer is named in the dry-run
	// instead of letting the fact surface as a journal surprise (try)
	// or a mid-refusal (required). The probe answers once, here; the
	// run still re-asks honestly and records whatever it gets.
	pol := ""
	for _, opt := range opts {
		if opt.Parking != "" {
			pol = opt.Parking
			break
		}
	}
	if pol != "" {
		if _, err := sysinfo.CpuSetStates(); err != nil {
			warns = append(warns, fmt.Sprintf("parking evidence (policy %q): source %s did not answer the probe made for this plan (%v): no parking evidence can come from it", pol, subprocess.ParkingSourceName, err))
		} else {
			warns = append(warns, fmt.Sprintf("parking evidence (policy %q): source %s answers on this machine, sampled every %s inside each session's measured idle windows", pol, subprocess.ParkingSourceName, subprocess.ParkingPollInterval))
		}
	}
	return warns
}

// runScenario executes the phases once and reports the scenario outcome.
// It is the single-cycle entry point of runCycles.
func runScenario(opts []plan.Options, out io.Writer, execute func(plan.Options) plan.Result, continueOnFault bool) int {
	return runCycles(opts, 1, -1, out, execute, continueOnFault, nil, nil, nil)
}

// runCycles runs the whole phase list `cycles` times as one session. The
// faulted-core blacklist is scenario-wide: it carries across cycles. With
// seed >= 0 the phases are shuffled before every cycle from that seed, so
// the entire multi-cycle order is reproducible — the CoreCycler habit of
// cycling tests in random order, but rerunnable. A cycle that stops
// (abort without faults, setup error, no usable cores left) ends the
// scenario; faults with continueOnFault only grow the blacklist.
func runCycles(opts []plan.Options, cycles int, seed int64, out io.Writer, execute func(plan.Options) plan.Result, continueOnFault bool, tr *attemptReceipt, st *sessionTracker, obs *[]wheaObs) int {
	faulted := map[int]bool{}
	var rng *rand.Rand
	if seed >= 0 {
		rng = rand.New(rand.NewSource(seed))
	}
	exit := 0
	var all []plan.Result
	for c := 1; c <= cycles; c++ {
		if cycles > 1 {
			fmt.Fprintf(out, "\n=== cycle %d/%d ===\n", c, cycles)
		}
		seq := opts
		if rng != nil {
			seq = make([]plan.Options, len(opts))
			copy(seq, opts)
			rng.Shuffle(len(seq), func(i, j int) { seq[i], seq[j] = seq[j], seq[i] })
		}
		results, ran, stopped, stopMsg := runCycle(seq, out, execute, continueOnFault, faulted, obs)
		for i, res := range results {
			if ran[i] {
				all = append(all, res)
			}
		}
		writeScenarioSummary(out, seq, results, ran, stopped, stopMsg, faulted)
		exit = cycleExit(results, stopped, faulted)
		if stopped >= 0 {
			break
		}
	}
	tr.finish(exit, all)
	st.finish(exit, all)
	// R19: the end-of-run evidence block. It arrives only for the
	// entry points that asked for observation capture (obs non-nil):
	// test harnesses orchestrating fake executors pass nil and stay
	// byte-stable. A phase's degraded window was captured at ITS
	// return boundary inside runCycle - a later clean phase cannot
	// mask it - and the renderer refuses to merge windows into one
	// verdict.
	if obs != nil {
		printRunEvidence(out, scenarioRuns(opts), *obs)
	}
	return exit
}

// newScenarioReceipt arms the receipt for a scenario attempt: the
// phase logs and plan hashes are the artifacts this attempt will
// produce, planned = phases x cycles.
func newScenarioReceipt(lr lastRun, opts []plan.Options, cycles int) *attemptReceipt {
	logs := make([]string, 0, len(opts))
	hashes := make([]string, 0, len(opts))
	for _, o := range opts {
		logs = append(logs, o.LogFile)
		hashes = append(hashes, o.PlanHash)
	}
	return newAttemptReceipt(lr.RecipeID, len(opts)*max(cycles, 1), logs, hashes)
}

// scenarioRecipe builds a scenario recipe from the entry point's
// request parts (C4): the resolved-config snapshot, the phase list,
// cycles, shuffle seed, continue policy and preset provenance - plus
// the catalog card when exactly one names that preset. The recipe id
// is stamped here, so every entry point (and the receipt it arms)
// shares one identity.
func scenarioRecipe(cfg config.Config, cycles int, seed *int64, continueOnFault bool, presetName string) lastRun {
	snap := recipeConfig(cfg)
	lr := lastRun{Kind: lastRunScenario, Config: &snap, Phases: cfg.Phases,
		Cycles: cycles, Seed: seed, ContinueOnFault: continueOnFault, Preset: presetName,
		RecipeID: newRecipeID()}
	if p := profileForPreset(presetName); p.ID != "" {
		lr.ProfileID, lr.ProfileVersion = p.ID, p.Version
	}
	return lr
}

// runScenarioRequest is the single scenario tail every entry point
// shares (C4): stamp the experiment identity into every phase, save
// the recipe (new requests only - a repeat replays the recipe already
// on disk), arm the receipt and run the cycles. The resource gate and
// the confirm screen stay with the caller: both must happen BEFORE
// the save could displace the last good recipe (audit A04).
func runScenarioRequest(lr lastRun, opts []plan.Options, cycles int, seed int64, continueOnFault bool, ui *screenUI, save bool) int {
	// B08: the whole scenario (every phase, every cycle) holds the
	// run lock: a per-phase lock would flicker between phases and
	// let another load slip into the gap. The refusal arrives
	// before the slot is displaced or any attempt is armed.
	relLock, lerr := acquireRunLockShared()
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "scenario refused: %v\n", lerr)
		return 2
	}
	defer relLock()
	// A11: every phase carries the identity of THIS request + its own
	// resolved plan; the engines journal it into run_meta, so a report
	// can tell two experiments apart even when config.json never moved.
	for i := range opts {
		opts[i].PlanHash = planHash(lr, opts[i])
	}
	// R21: before the receipt this scenario arms displaces the previous
	// unfinished attempt's evidence, state that evidence (and the OS's own
	// unexpected-shutdown records, when local ones say something never
	// ended). Silent, and System-log-free, on a clean start.
	printPreviousAttemptWitness(os.Stdout)
	if save {
		if err := saveLastRun(lr); err != nil {
			noteSaveFailure(err)
		}
	}
	// U02: one scenario attempt is one session - a repeat included -
	// with its own id and manifest. The id is stamped into every
	// phase so the engines journal it into run_meta: cycles share
	// the phase log files, and the session id is what attributes
	// those runs to THIS session afterwards.
	st, serr := newSessionTracker("scenario", &lr, scenarioRuns(opts))
	if serr != nil {
		fmt.Fprintf(os.Stderr, "scenario: %v\n", serr)
		return 2
	}
	for i := range opts {
		opts[i].SessionID = st.m.SessionID
	}
	tr := newScenarioReceipt(lr, opts, cycles)
	var hashes []string
	for _, o := range opts {
		hashes = append(hashes, o.PlanHash)
	}
	currentEvents.emit("resolved", "", resolvedPayload{Runs: len(opts) * max(cycles, 1), PlanHashes: hashes}, false)
	currentEvents.emit("started", "", startedPayload{Runs: len(opts) * max(cycles, 1), Cycles: max(cycles, 1)}, false)
	execute, out := menuExecution(ui)
	var obs []wheaObs
	return runCycles(opts, cycles, seed, out, execute, continueOnFault, tr, st, &obs)
}

// scenarioRuns lists the log and journal each phase writes. Cycles
// re-run the same phases through the same files: the journal
// accumulates the session's runs, and run_meta.session_id tells
// them apart from other sessions that once used the same names.
func scenarioRuns(opts []plan.Options) []manifestRun {
	out := make([]manifestRun, 0, len(opts))
	for _, o := range opts {
		out = append(out, manifestRun{Name: o.Phase, Log: o.LogFile, Journal: journal.Path(o.LogFile)})
	}
	return out
}

// cycleExit maps one cycle's outcome onto the scenario exit code.
func cycleExit(results []plan.Result, stopped int, faulted map[int]bool) int {
	if stopped >= 0 && results[stopped].Exit != 0 {
		return results[stopped].Exit
	}
	if stopped >= 0 || len(faulted) > 0 {
		return 1 // faults were found, even if no single phase "stopped" us
	}
	return 0
}

// runCycle executes one pass over the phases. faulted is the scenario-wide
// blacklist: it excludes cores from this pass and grows with new faults.
// execute is injected so tests can watch the orchestration without
// loading the machine. With continueOnFault, a phase that reports core
// faults does not stop the scenario: the faulted cores are blacklisted
// and excluded from every later phase — the CoreCycler behavior of
// "keep testing what still works". Aborts without faults (Ctrl+C, setup
// errors) always stop the scenario.
func runCycle(opts []plan.Options, out io.Writer, execute func(plan.Options) plan.Result, continueOnFault bool, faulted map[int]bool, obs *[]wheaObs) (results []plan.Result, ran []bool, stopped int, stopMsg string) {
	results = make([]plan.Result, len(opts))
	ran = make([]bool, len(opts))
	stopped = -1

	for i, opt := range opts {
		label := phaseLabel(opt.Phase, i)

		if len(faulted) > 0 {
			if opt.AllOrNone && anyFaulted(opt.Cores, faulted) {
				// A directed pair with one participant missing is not
				// that pair anymore: the survivor would silently change
				// role. A handoff partition behaves the same way - its
				// blocks divide across exactly these cores, so skipping
				// the phase whole beats re-dividing it into a different
				// (smaller, differently claimed) experiment.
				fmt.Fprintf(out, "\n=== phase %d/%d: %s — skipped (a participant core faulted earlier) ===\n",
					i+1, len(opts), label)
				continue
			}
			opt.Cores = excludeCores(opt.Cores, faulted)
			if len(opt.Cores) == 0 {
				stopped, stopMsg = i, "no usable cores left after faults"
				break
			}
		}

		if opt.HandoffBlocks > 0 {
			// The geometry, not the clock, defines this phase - the
			// header states it so a reader is never pointed at an
			// inherited duration this phase never uses.
			cores := "all"
			if len(opt.Cores) > 0 {
				cores = human.Ints(opt.Cores)
			}
			fmt.Fprintf(out, "\n=== phase %d/%d: %s — handoff: cores %s, %d block(s) x %s, %d round(s), seed %d (parent-arbitrated leases) ===\n",
				i+1, len(opts), label, cores, opt.HandoffBlocks, human.Bytes(opt.HandoffBlockBytes), opt.HandoffRounds, opt.HandoffSeed)
		} else if opt.Bursts > 0 {
			// The prepared schedule, not a duration, defines this phase:
			// say so in the header too, so a reader is never pointed at
			// an inherited duration this phase never uses.
			fmt.Fprintf(out, "\n=== phase %d/%d: %s — transitions: %d bursts a=%s gap=%s run=%s ===\n",
				i+1, len(opts), label, opt.Bursts, opt.A, opt.BurstGap, opt.BurstRun)
		} else if plan.SwitchActive(opt) {
			// Q05: the prepared turn schedule defines this phase, and
			// the groups (who loads together) belong in the header -
			// never an inherited duration this phase never uses.
			order := opt.SwitchOrder
			if order == "" {
				order = "fixed"
			}
			fmt.Fprintf(out, "\n=== phase %d/%d: %s — pair switch: cores %s, %d group(s) x %d round(s), gap=%s run=%s, order %s ===\n",
				i+1, len(opts), label, human.Ints(opt.Cores), len(opt.SwitchPairs), opt.SwitchRounds, opt.SwitchGap, opt.SwitchRun, order)
		} else {
			fmt.Fprintf(out, "\n=== phase %d/%d: %s — test=%s a=%s b=%s duration=%s ===\n",
				i+1, len(opts), label, opt.TestName, opt.A, opt.B, opt.Duration)
		}

		phaseID := label
		currentEvents.setPhase(phaseID)
		currentEvents.emit("phase_started", phaseID, scenarioPhaseStartedPayload{Test: opt.TestName, A: opt.A, B: opt.B}, false)
		res := execute(opt)
		// R19: this phase's WHEA observation window closes HERE, at
		// its own engine-return boundary. Capturing at the scenario
		// tail instead would let one clean LAST phase hide an earlier
		// degraded window (each phase runs under its own Guard), so
		// every phase's state is attributed while it is still the
		// truth about that phase.
		captureWheaWindow(label, opt.PlanHash, obs)
		results[i], ran[i] = res, true
		emitPhaseResult(phaseID, res)
		if res.Exit == 0 {
			continue
		}
		if res.Exit == 1 && continueOnFault && len(res.Faults) > 0 {
			for core := range res.Faults {
				faulted[core] = true
			}
			continue
		}
		stopped, stopMsg = i, res.Verdict().String()
		if !strings.Contains(stopMsg, "exit") {
			stopMsg = fmt.Sprintf("%s (exit %d)", stopMsg, res.Exit)
		}
		break
	}
	return
}

// countRan counts the phases that executed.
func countRan(ran []bool) int {
	n := 0
	for _, r := range ran {
		if r {
			n++
		}
	}
	return n
}

// findingLine renders one finding with its structured attribution:
// what it names, how firmly, and where it was observed - so a memory
// region finding or a run-level tool failure is stated exactly as
// fully as a per-core one (the old summary showed only the per-CPU
// Faults map and silently dropped everything that named no CPU).
func findingLine(f plan.Finding) string {
	att := "run-level"
	switch f.Attribution.Kind {
	case plan.AttrLogicalCPU:
		if f.Attribution.Index != nil {
			att = fmt.Sprintf("logical cpu %d (%s)", *f.Attribution.Index, f.Attribution.Confidence)
		}
	case plan.AttrPhysicalCore:
		if f.Attribution.Index != nil {
			att = fmt.Sprintf("physical core %d (%s)", *f.Attribution.Index, f.Attribution.Confidence)
		}
	case plan.AttrWorker:
		if f.Attribution.Index != nil {
			att = fmt.Sprintf("worker slot %d", *f.Attribution.Index)
		}
	case plan.AttrMemoryRegion:
		att = fmt.Sprintf("memory region %s", f.Attribution.Region)
	case plan.AttrUnknown:
		att = "unattributed"
	}
	if f.Attribution.ObservedOn != nil {
		att += fmt.Sprintf(", observed on cpu %d", *f.Attribution.ObservedOn)
	}
	return fmt.Sprintf("%s: %s", att, f.Message)
}

// anyFaulted reports whether any of the phase's cores is blacklisted.
func anyFaulted(cores []int, faulted map[int]bool) bool {
	for _, c := range cores {
		if faulted[c] {
			return true
		}
	}
	return false
}

// excludeCores removes blacklisted cores from a phase's core list,
// expanding "all cores" first when the phase did not name any.
func excludeCores(cores []int, faulted map[int]bool) []int {
	if len(faulted) == 0 {
		return cores
	}
	if len(cores) == 0 {
		cores = plan.SelectedLogicalCPUs(nil, runtime.NumCPU())
	}
	var out []int
	for _, c := range cores {
		if !faulted[c] {
			out = append(out, c)
		}
	}
	return out
}

// writeScenarioSummary reports every phase in one place: the journal
// keeps the machine-readable detail, this is the human recap of which
// phase ended how and which cores faulted. P13 vocabulary: the
// scenario gets ONE headline verdict word (PASS / FAIL / INCOMPLETE /
// UNVERIFIED) built from the structured per-phase results - never a
// bare "aborted (exit N)" - and every phase's findings print verbatim
// with their structured attribution, including the findings that name
// no logical CPU (a memory region, a run-level tool failure).
func writeScenarioSummary(out io.Writer, opts []plan.Options, results []plan.Result, ran []bool, stopped int, stopMsg string, faulted map[int]bool) {
	fmt.Fprintln(out, "\nScenario summary:")

	// Headline: worst phase verdict, and a scenario that stopped early
	// is INCOMPLETE even if every phase that ran was spotless - what
	// was planned did not finish.
	kinds := make([]plan.VerdictKind, 0, len(opts))
	reason := map[plan.VerdictKind]string{}
	for i := range opts {
		if !ran[i] {
			continue
		}
		v := results[i].Verdict()
		kinds = append(kinds, v.Kind)
		if _, seen := reason[v.Kind]; !seen && v.Reason != "" {
			reason[v.Kind] = v.Reason
		}
	}
	scen := plan.WorstVerdict(kinds...)
	if stopped >= 0 && plan.VerdictSeverity(scen) < plan.VerdictSeverity(plan.VerdictIncomplete) {
		scen = plan.VerdictIncomplete
	}
	head := plan.Verdict{Kind: scen, Reason: reason[scen]}
	if stopped >= 0 {
		head.Reason = fmt.Sprintf("scenario stopped after phase %d (%s): %s; %d of %d phase(s) never ran",
			stopped+1, phaseLabel(opts[stopped].Phase, stopped), stopMsg,
			len(opts)-countRan(ran), len(opts))
	}
	fmt.Fprintf(out, "RESULT: %s\n", head.String())

	for i := range opts {
		label := phaseLabel(opts[i].Phase, i)
		switch {
		case !ran[i]:
			if stopped >= 0 {
				fmt.Fprintf(out, "  %-12s not run (the scenario stopped before this phase)\n", label)
			} else {
				fmt.Fprintf(out, "  %-12s not run (skipped: a participant core faulted earlier)\n", label)
			}
		default:
			v := results[i].Verdict()
			fmt.Fprintf(out, "  %-12s %s\n", label, v.String())
			for _, f := range results[i].Findings {
				fmt.Fprintf(out, "    finding [%s · %s]: %s\n", f.Kind, f.Source, findingLine(f))
			}
		}
	}

	if len(faulted) > 0 {
		cores := make([]int, 0, len(faulted))
		for core := range faulted {
			cores = append(cores, core)
		}
		sort.Ints(cores)
		fmt.Fprintf(out, "scenario: faulted cores excluded from later phases: %v\n", cores)
	}

	switch {
	case stopped >= 0:
		fmt.Fprintf(out, "scenario: stopped after phase %d (%s), %s\n",
			stopped+1, phaseLabel(opts[stopped].Phase, stopped), stopMsg)
	case len(faulted) > 0:
		fmt.Fprintf(out, "scenario: all %d phase(s) ran, faults on %d core(s)\n", len(opts), len(faulted))
	default:
		// "completed OK" is only claimed when every ran phase also
		// confirmed its own verification; otherwise the count of
		// unconfirmed phases is said out loud.
		unverifiedPhases := 0
		for i := range opts {
			if ran[i] && results[i].Exit == 0 && !verificationConfirmed(results[i].Validation) {
				unverifiedPhases++
			}
		}
		if unverifiedPhases > 0 {
			fmt.Fprintf(out, "scenario: all %d phase(s) ran clean, %d without confirmed verification (see above)\n",
				len(opts), unverifiedPhases)
		} else {
			fmt.Fprintf(out, "scenario: all %d phase(s) completed OK\n", len(opts))
		}
	}

	// Stability conclusion: the scenario's own reach, stated plainly -
	// one scenario never "proves stability", and an unfinished or
	// unverified scenario says so instead of hedging.
	switch scen {
	case plan.VerdictPass:
		fmt.Fprintln(out, "stability conclusion: this scenario reported no faults and verified its load; a stability claim still needs long soak runs plus WHEA coverage (see 'report')")
	case plan.VerdictFail:
		fmt.Fprintln(out, "stability conclusion: a fault was reported - treat the cores/regions named by the findings as suspect and re-test after the reported cause is addressed")
	case plan.VerdictIncomplete:
		fmt.Fprintln(out, "stability conclusion: NOT DETERMINED - the scenario did not finish its plan (see the stop reason above)")
	case plan.VerdictUnverified:
		fmt.Fprintln(out, "stability conclusion: NOT DETERMINED - load finished but its correctness contract was not confirmed (see findings above)")
	}

	// Coverage: what the scenario actually exercised. Cores named by
	// the phases and whether transitions were commanded are stated;
	// an unpinned core set is said so rather than counted as zero.
	// Per-algorithm (kernel) coverage lives in the report, which reads
	// the journal's kernel_version records.
	coreSet := map[int]bool{}
	transitionPhases := 0
	for _, opt := range opts {
		for _, c := range opt.Cores {
			coreSet[c] = true
		}
		if opt.Bursts > 0 || plan.SwitchActive(opt) {
			// both prepared-schedule shapes command transitions
			// (bursts within one core, switches between groups)
			transitionPhases++
		}
	}
	if len(coreSet) > 0 {
		cores := make([]int, 0, len(coreSet))
		for c := range coreSet {
			cores = append(cores, c)
		}
		sort.Ints(cores)
		fmt.Fprintf(out, "coverage: %d distinct core(s) named across phases (%s); %d phase(s) command transitions\n",
			len(coreSet), human.Ints(cores), transitionPhases)
	} else {
		fmt.Fprintf(out, "coverage: phases target all cores (core set not pinned); %d phase(s) command transitions\n", transitionPhases)
	}

	// Reproduction: the saved plan replays this scenario through the
	// same pipeline (generators and a realized randomized walk
	// included). The per-phase parameters are printed by each run.
	ranPhases := 0
	for _, r := range ran {
		if r {
			ranPhases++
		}
	}
	fmt.Fprintf(out, "reproduce: cpu-stability repeat (saved plan: %d phase(s), %d ran)\n", len(opts), ranPhases)
}
