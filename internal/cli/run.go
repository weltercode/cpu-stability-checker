package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/subprocess"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func runConfigured(askForConfirmation bool) int {
	return runConfiguredIO(askForConfirmation, os.Stdin, os.Stdout)
}

func runConfiguredIO(askForConfirmation bool, in io.Reader, out io.Writer) int {
	return runConfiguredIOWithUI(askForConfirmation, in, out, nil)
}

func runConfiguredIOWithUI(askForConfirmation bool, in io.Reader, out io.Writer, ui *screenUI) int {
	opt, topology, err := plan.LoadResolvedPlan(config.DefaultPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		fmt.Fprintln(os.Stderr, "Run 'cpu-stability config init' to create a default config.json.")
		return 2
	}

	plan.WriteRunPlan(out, config.DefaultPath, opt, topology)
	if !askForConfirmation {
		return 0
	}
	if !confirmRun(in, out) {
		fmt.Fprintln(out, "Run cancelled. No load was started.")
		return 0
	}
	// A04: the config-plan entry point leaves the same recipe too -
	// gate first, so a plan the machine cannot honor never displaces
	// the last good one.
	if perr := plan.PreflightMemoryWorkloads(opt); perr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", perr)
		return 2
	}
	cfg := config.Load(config.DefaultPath)
	if msg := externalToolMissing(cfg.External, opt.A, opt.B); msg != "" {
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		return 2
	}
	lr := buildRunRecipe(cfg, opt, cfg.ControlCore, 0, "", "", "", "", 0)
	opt.PlanHash = planHash(lr, opt)
	if err := saveLastRun(lr); err != nil {
		noteSaveFailure(err)
	}
	st, serr := newSessionTracker("plan", &lr, []manifestRun{{Log: opt.LogFile, Journal: journal.Path(opt.LogFile)}})
	if serr != nil {
		fmt.Fprintf(os.Stderr, "cannot start session: %v\n", serr)
		return 2
	}
	opt.SessionID = st.m.SessionID
	tr := newAttemptReceipt(lr.RecipeID, 1, []string{opt.LogFile}, []string{opt.PlanHash})
	res := executeOptWithPanel(opt, ui.monitor())
	tr.finish(res.Exit, []plan.Result{res})
	st.finish(res.Exit, []plan.Result{res})
	return res.Exit
}

func confirmRun(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Start this run? [y/N]: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func runCmd(args []string) int {
	cfg := config.Load(config.DefaultPath)
	workload.RegisterExternal(cfg.External)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	testName := fs.String("test", cfg.Test, "test name (see 'list')")
	a := fs.String("a", cfg.WorkloadA, "workload slot A")
	b := fs.String("b", cfg.WorkloadB, "workload slot B")
	cores := fs.String("cores", "", "comma list of cores, e.g. 0,1,2 (empty=config/all)")
	workers := fs.Int("workers", 0, "workers per core (0=config)")
	duration := fs.String("duration", "", "how long to run, e.g. 10s (empty=config)")
	logFile := fs.String("log", "", "log file (empty=config)")
	stopOnFault := fs.Bool("stop-on-fault", cfg.StopOnFault, "abort the run on the first fault")
	perProcess := fs.Bool("per-process", false, "one pinned subprocess per CPU instead of threads in one process (experimental)")
	rotate := fs.Bool("rotate", false, "with -per-process: freeze all cores but one, rotating exclusive windows")
	controlFlag := fs.Int("control", -2, "CPU for helper threads: -1 = off, N = that core (default: config / auto)")
	maxTemp := fs.Float64("max-temp", cfg.MaxTemp, "shared panel sensor limit in °C; stops if readings are lost (0 = off)")
	telemetry := fs.Bool("telemetry", cfg.Telemetry, "log a compact per-second clock/idle telemetry line during the run")
	beatFlag := fs.String("beat", cfg.BeatEvery, "core_beat journal cadence, e.g. 5s (empty = 1s; the live UI is unaffected)")
	ramSeed := fs.Uint64("ram-seed", cfg.RamSeed, "seed for the ram-* memory-test family: expected data = f(seed, region, pass, offset); 0 is a valid seed")
	// P08 transition mode (0 = off): instead of one fixed window per
	// core, N verified bursts per core on a PREPARED schedule, each
	// core served by one prepared worker process that allocates once
	// and stays alive across bursts.
	bursts := fs.Int("bursts", 0, "P08 transition mode: verified bursts per core on a prepared schedule (0 = off); requires -per-process")
	burstGap := fs.String("burst-gap", "200ms", "prepared idle gap before each burst")
	burstRun := fs.String("burst-run", "500ms", "each burst runs this long, then VERIFY closes the verified block")
	burstJitter := fs.String("burst-jitter", "0", "seeded jitter on the gap (+/-, clamped at zero)")
	burstSeed := fs.Uint64("burst-seed", 0, "jitter seed for the schedule (0 is a valid seed); the concrete windows are journaled, replay never needs this seed")
	burstStagger := fs.String("burst-stagger", "0", "shift each core's session start by this (waves; 0 = all cores jump together)")
	request := fs.String("request", "", "headless (G01): absolute path of a request file; the request holds every parameter and runs through the repeat pipeline")
	events := fs.String("events", "", "managed mode (G01): JSONL event stream on stdout ('jsonl'); requires -request")
	output := fs.String("output", "", "managed mode: directory for the durable result file (default: working directory)")
	fs.Parse(args)

	if (*events != "" || *output != "") && *request == "" {
		fmt.Fprintln(os.Stderr, "-events/-output belong to -request mode: a request-driven run is the only managed stream in this build")
		return 2
	}
	if *request != "" {
		// The request file is the whole question: mixing run flags
		// into it would silently answer a different one. Positional
		// arguments are refused too (menu plan section 9: an extra
		// positional is an error before load).
		if fs.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "run -request takes no positional arguments")
			return 2
		}
		stray := ""
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "request" && f.Name != "events" && f.Name != "output" && stray == "" {
				stray = f.Name
			}
		})
		if stray != "" {
			fmt.Fprintf(os.Stderr, "run -request takes no other flags (-%s was set): the request file holds every parameter\n", stray)
			return 2
		}
		return headlessRunRequest(*request, *events, *output)
	}

	if *testName == "" {
		fmt.Fprintln(os.Stderr, "-test is required")
		return 2
	}
	if _, ok := plan.GetTest(*testName); !ok {
		fmt.Fprintf(os.Stderr, "unknown test %q (see 'list')\n", *testName)
		return 2
	}
	// "idle" is a plan value, not a registered workload: config
	// accepts it for A/B (OptionsFromConfig), so the flags must
	// accept it too - rejecting it only from the command line would
	// be a silent inconsistency between the two config paths.
	if _, ok := workload.Get(*a); !ok && *a != workload.Idle {
		fmt.Fprintf(os.Stderr, "unknown workload A %q (see 'list')\n", *a)
		return 2
	}
	if _, ok := workload.Get(*b); !ok && *b != workload.Idle {
		fmt.Fprintf(os.Stderr, "unknown workload B %q (see 'list')\n", *b)
		return 2
	}

	var coresList []int
	if *cores != "" {
		var cerr error
		coresList, cerr = parseNumberSet(*cores, runtime.NumCPU())
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "-cores: %v\n", cerr)
			return 2
		}
	} else {
		coresList = cfg.Cores
	}

	w := *workers
	if w <= 0 {
		w = cfg.Workers
	}
	if w < 1 {
		w = 1
	}

	dur, err := time.ParseDuration(*duration)
	if err != nil || dur <= 0 {
		dur = cfg.ParsedDuration()
	}

	lf := *logFile
	if lf == "" {
		lf = cfg.LogFile
	}

	explicitControl := cfg.ControlCore
	if *controlFlag >= -1 {
		v := *controlFlag
		explicitControl = &v
	}
	control, effCores, removed, err := plan.ResolveControl(explicitControl, coresList, cfg.ReserveControl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control core: %v\n", err)
		return 2
	}
	if len(removed) > 0 {
		// The coverage loss must be loud BEFORE the load starts, in
		// the same breath as the plan: this run tests fewer cpus than
		// the machine shows, by explicit reserve_control policy.
		fmt.Printf("Note: reserve_control=logical: cpu %d removed from the worker set for the helper threads\n  (watchdog, journal, panel): coverage %d of %d logical cpus, workers pin to the remaining set.\n",
			removed[0], len(effCores), len(effCores)+len(removed))
	}
	if *maxTemp < 0 || math.IsNaN(*maxTemp) || math.IsInf(*maxTemp, 0) {
		fmt.Fprintln(os.Stderr, "-max-temp must be 0 (off) or a positive Celsius limit")
		return 2
	}
	var beatEvery time.Duration
	if s := strings.TrimSpace(*beatFlag); s != "" {
		var berr error
		beatEvery, berr = time.ParseDuration(s)
		if berr != nil || beatEvery <= 0 {
			fmt.Fprintln(os.Stderr, "-beat must be a positive duration such as 5s")
			return 2
		}
	}

	if err := plan.ValidateMemorySettings(cfg.Memory, cfg.MemoryLock); err != nil {
		fmt.Fprintf(os.Stderr, "memory engine: %v\n", err)
		return 2
	}

	// Q04: the global parking word resolves here, like the memory-lock
	// and execution vocabularies: an unknown word is a visible refusal
	// (exit 2) naming its vocabulary before any load, never a silent
	// fall back. What the word then APPLIES to is the shape rule below.
	parking, perr := transition.ParseParkingPolicy(cfg.ParkingPolicy)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "parking policy: %v\n", perr)
		return 2
	}

	opt := plan.Options{
		TestName:     *testName,
		Cores:        effCores,
		A:            *a,
		B:            *b,
		Workers:      w,
		Duration:     dur,
		StopOnFault:  *stopOnFault,
		Rotate:       *rotate,
		PerProcess:   *perProcess,
		ControlCore:  control,
		ReservedCPUs: removed,
		MaxTempC:     *maxTemp,
		Telemetry:    *telemetry,
		BeatEvery:    beatEvery,
		Memory:       strings.TrimSpace(cfg.Memory),
		MemLock:      strings.TrimSpace(cfg.MemoryLock),
		RamSeed:      *ramSeed,
		LogFile:      lf,
		ShowMetrics:  cfg.ShowMetrics,
		Parking:      string(parking),
	}

	// U01 auto-execution (menu plan section 4): without an explicit
	// -per-process/-rotate flag the policy resolver picks the worker
	// mode from the workloads and the burst schedule; a contradicting
	// settings override exits 2 here, naming the fix - before any
	// load, never as a quiet switch.
	if !opt.PerProcess {
		xdec, derr := plan.ResolveExecution(cfg.Execution, opt.A, opt.B, *bursts)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", derr)
			return 2
		}
		opt.PerProcess = xdec.PerProcess
		opt.Rotate = opt.Rotate || xdec.Rotate
	}

	// A moved or removed external tool is a clear refusal BEFORE any
	// load, naming the registered path (audit A04): the child would
	// otherwise die in setup, mid-plan. Transition mode judges slot A
	// only - slot B never loads there.
	extNames := []string{opt.A, opt.B}
	if *bursts > 0 {
		extNames = []string{opt.A}
	}
	if msg := externalToolMissing(cfg.External, extNames...); msg != "" {
		fmt.Fprintf(os.Stderr, "%s\n", msg)
		return 2
	}

	// Q04: parking evidence rides the measured idle windows of a
	// PREPARED session. An ordinary single run has none, so the global
	// parking word cannot shape this run: say so out loud and drop it
	// before the recipe/hash - keeping an inert word in the plan hash
	// would fork experiment identity over something that changes
	// nothing. (The phase path enforces the same split: inherited words
	// are cleared there too, and a phase that NAMED the word without
	// bursts is a compile error.)
	if *bursts == 0 && opt.Parking != "" {
		fmt.Printf("note: parking_policy %q applies to prepared transition runs (bursts); this is an ordinary run, so no parking observation will be made\n", opt.Parking)
		opt.Parking = ""
	}

	// P08 transition mode: prepared workers, prepared schedule,
	// commanded vs observed kept apart in the journal.
	if *bursts > 0 {
		if !opt.PerProcess {
			// Only reachable if a future path skips the resolver: the
			// auto policy already satisfies it, and a threaded
			// override was refused above with the fix named.
			fmt.Fprintln(os.Stderr, "-bursts needs per-process mode: transition mode drives prepared worker processes (execution=auto picks it)")
			return 2
		}
		if *rotate {
			fmt.Fprintln(os.Stderr, "-bursts and -rotate do not mix: the schedule commands the bursts, rotate freezes whole windows")
			return 2
		}
		gap, gerr := time.ParseDuration(*burstGap)
		runDur, rerr := time.ParseDuration(*burstRun)
		jit, jerr := time.ParseDuration(*burstJitter)
		stag, serr := time.ParseDuration(*burstStagger)
		if gerr != nil || gap < 0 || rerr != nil || runDur <= 0 || jerr != nil || jit < 0 || serr != nil || stag < 0 {
			fmt.Fprintln(os.Stderr, "-burst-gap/-burst-run/-burst-jitter/-burst-stagger: bad durations (burst-run must be positive, the others non-negative)")
			return 2
		}
		if strings.TrimSpace(*duration) != "" {
			fmt.Fprintln(os.Stderr, "note: -duration is ignored in transition mode: the prepared schedule defines the time")
		}
		// Session mode runs workload A only: slot B never runs, so
		// the memory preflight must judge A alone (judging a slot
		// that will never load would invent a refusal reason).
		sessOpt := opt
		sessOpt.B = ""
		if perr := plan.PreflightMemoryWorkloads(sessOpt); perr != nil {
			fmt.Fprintf(os.Stderr, "%v\n", perr)
			return 2
		}
		lr := buildRunRecipe(cfg, opt, explicitControl,
			*bursts, *burstGap, *burstRun, *burstJitter, *burstStagger, *burstSeed)
		// The identity covers the plan that ACTUALLY runs (slot A
		// only in transition mode) - and planHash normalizes slot B
		// away for burst phases, so a fresh run and its scenario
		// replay share one hash.
		sessOpt.PlanHash = planHash(lr, sessOpt)
		if err := saveLastRun(lr); err != nil {
			noteSaveFailure(err)
		}
		st, serr := newSessionTracker("run", &lr, []manifestRun{{Log: sessOpt.LogFile, Journal: journal.Path(sessOpt.LogFile)}})
		if serr != nil {
			fmt.Fprintf(os.Stderr, "cannot start session: %v\n", serr)
			return 2
		}
		sessOpt.SessionID = st.m.SessionID
		sched := transition.BuildSchedule(*bursts, gap, runDur, jit, *burstSeed)
		tr := newAttemptReceipt(lr.RecipeID, 1, []string{sessOpt.LogFile}, []string{sessOpt.PlanHash})
		exit := subprocess.ExecuteSession(sessOpt, sched, stag)
		// Transition mode's verdict lives in the journal: the CLI only
		// saw the exit code, so the receipt honestly leaves
		// execution/validation unknown rather than inferring them.
		tr.finish(exit, nil)
		// The session manifest says the same thing: state "unknown"
		// with the exit code recorded, no guessed outcome.
		st.finish(exit, nil)
		// The reproduction note: the parameters that define this run,
		// printed after the result so a failure can be re-created.
		fmt.Println(plan.ReproductionParameters(sessOpt))
		return exit
	}
	// A04: the plain CLI run leaves the same recipe every other entry
	// point leaves (see buildRunRecipe). The gate comes first: a run the
	// machine cannot honor must not displace the last good recipe on
	// its way to the executor's own visible refusal below.
	if perr := plan.PreflightMemoryWorkloads(opt); perr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", perr)
		return 2
	}
	lr := buildRunRecipe(cfg, opt, explicitControl, 0, "", "", "", "", 0)
	opt.PlanHash = planHash(lr, opt)
	if err := saveLastRun(lr); err != nil {
		noteSaveFailure(err)
	}
	st, serr := newSessionTracker("run", &lr, []manifestRun{{Log: opt.LogFile, Journal: journal.Path(opt.LogFile)}})
	if serr != nil {
		fmt.Fprintf(os.Stderr, "cannot start session: %v\n", serr)
		return 2
	}
	opt.SessionID = st.m.SessionID
	tr := newAttemptReceipt(lr.RecipeID, 1, []string{opt.LogFile}, []string{opt.PlanHash})
	res := executeOpt(opt)
	tr.finish(res.Exit, []plan.Result{res})
	st.finish(res.Exit, []plan.Result{res})
	fmt.Println(plan.ReproductionParameters(opt))
	return res.Exit
}
