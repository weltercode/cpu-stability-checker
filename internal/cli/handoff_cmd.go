package cli

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/subprocess"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// handoffCmd is the P07e standalone entry: block-ownership handoff
// between cores - and across last-level-cache domains when the OS
// cpu-set view can see more than one - over one shared section.
//
// Since S5 the same geometry also compiles as an ordinary scenario
// phase (config handoff_* fields, one dispatch); since S6 the
// standalone command is a PUBLIC run mode: the durable manifest
// (saved before the first load), the session manifest, the attempt
// receipt, the lastresult binding and the run_meta provenance in the
// journal are the same lifecycle every other entry point records, and
// the menu repeat, 'handoff -repeat' and a kind=handoff request file
// all settings-repeat that manifest through ONE tracked runner (the
// machine-drift gate refuses drift by name, never re-picks). The
// exit contract is the shared one (fault 1, setup 2, interrupted 130,
// clean 0). -helper keeps the legacy internal test-helper form whose
// journals stay byte-identical to the accepted P12-era records.
func handoffCmd(args []string) int {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	coresFlag := fs.String("cores", "auto", "logical CPUs: comma list, or 'auto' = one core per llc domain")
	blocks := fs.Int("blocks", 8, "blocks in the shared section (must divide evenly across the cores)")
	blockSize := fs.String("block-size", "64KiB", "uniform block size (a whole number of 4-byte words)")
	rounds := fs.Int("rounds", 1, "handoff rounds: every block is published and independently re-read this many times")
	seed := fs.Uint64("seed", 0, "seed for the schedule order and the ram-handoff image (printed, and every executed step is journaled)")
	lock := fs.String("lock", "off", "memory lock policy for the engine regions: off|try|required")
	jp := fs.String("journal", "", "journal file (default: the config's run log)")
	repeat := fs.Bool("repeat", false, "settings-repeat of the durable manifest: same geometry and seed, re-run as a NEW attempt under a fresh run_id (the menu repeat and a kind=handoff request file do the same); machine or topology drift is refused, never re-picked (P07e S3, public since S6)")
	replayJournal := fs.String("replay-journal", "", "verify one handoff generation of an existing journal against the arbiter contract, without touching hardware (P07e S3)")
	replayRun := fs.String("replay-run", "", "which handoff generation -replay-journal verifies (empty = only when the journal holds exactly one)")
	helper := fs.Bool("helper", false, "legacy internal test-helper form: the coordinator runs WITHOUT the session manifest, attempt receipt and run_meta provenance the public form records, so helper journals stay byte-comparable to the accepted P12-era records")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*repeat && *replayJournal != "") || (*replayRun != "" && *replayJournal == "") {
		fmt.Fprintln(os.Stderr, "handoff: -repeat reruns the recorded geometry; -replay-journal verifies an existing journal; -replay-run only selects a generation of -replay-journal. Pick one job")
		return 2
	}
	if *replayJournal != "" {
		return handoffReplay(*replayJournal, *replayRun)
	}
	if *repeat {
		if *helper {
			return handoffRepeat()
		}
		return handoffRepeatPublic()
	}
	cfg := config.Load(config.DefaultPath)
	logFile := *jp
	if logFile == "" {
		logFile = cfg.LogFile
	}
	size, err := memtest.ParseSize(*blockSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff: -block-size: %v\n", err)
		return 2
	}
	res, err := handoffCores(*coresFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff: -cores: %v\n", err)
		return 2
	}
	cores := res.Cores
	jpath := journal.Path(logFile)
	fmt.Printf("handoff: cores %v, %d blocks x %s, %d round(s), seed %d\n", cores, *blocks, *blockSize, *rounds, *seed)
	// S2 discipline, public and helper form alike (A04): the durable
	// manifest is saved before the first load; a failed save is said
	// out loud, it never silently degrades into a run whose repeat
	// promise is a fiction.
	lr := handoffRecipeOf(cfg, *coresFlag, res, *blocks, *blockSize, size, *rounds, *seed, *lock, jpath)
	if err := saveLastRun(lr); err != nil {
		noteSaveFailure(err)
	} else {
		fmt.Printf("handoff: durable manifest saved (kind=%s, recipe %s, journal %s); the menu repeat, 'handoff -repeat' and a kind=handoff request file settings-repeat it\n", lastRunHandoff, lr.RecipeID, jpath)
	}
	if *helper {
		// S6 kept this form as an explicitly labeled legacy seam: the
		// shared safety lifecycle IS active and the journal bytes are
		// exactly what the pre-S6 helper generations wrote (no
		// run_meta provenance), but the attempt carries none of the
		// tracking a public run owes - no session manifest, no
		// outcome receipt, no lastresult binding.
		fmt.Fprintln(os.Stderr, "handoff: INTERNAL TEST HELPER (legacy form): durable manifest recorded, lease steps join the -events jsonl stream when a run drives one (this form opens no stream itself), the shared safety lifecycle IS active, and the journal stays byte-identical to the pre-S6 helper records; it is NOT the tracked public mode - drop -helper for that")
		return subprocess.ExecuteHandoff(subprocess.HandoffSpec{
			Cores:       cores,
			BlockBytes:  size,
			Blocks:      *blocks,
			Rounds:      *rounds,
			Seed:        *seed,
			Lock:        *lock,
			JournalPath: jpath,
			Opt:         handoffSafetyOptions(cfg, cores),
			OnLease:     handoffLeaseEmitter(), // S4: lease steps reach the -events jsonl stream when one is live
		}, nil)
	}
	// S6: the public form IS a tracked run - session manifest before
	// load, attempt receipt, lastresult binding, run_meta provenance
	// in the journal, and (with a managed stream live, which this
	// command never opens by itself) the lease steps and phase
	// records on that stream.
	return runHandoffTracked(&lr, nil)
}

// handoffRunOptions resolves a durable manifest into the plan.Options
// of the SINGLE dispatch (S6): the geometry fields the executor
// reads, the safety request from the manifest's own config snapshot
// (never today's config.json), and the journal FILE itself as pure
// transport (opt.HandoffJournal - excluded from the plan identity,
// like LogFile is in planHash: where the evidence is written does
// not change what was tested). validate and the run call this same
// builder, which is what makes their plan hashes agree.
func handoffRunOptions(cfg config.Config, h *handoffRecipe) plan.Options {
	opt := handoffSafetyOptions(cfg, h.Cores)
	// The kernel the coordinator actually drives - the same name the
	// scenario phase carries. ValidateHandoff refuses handoff_* on
	// any other workload, and it is right: geometry without the
	// lease kernel would pose as a test this shape never runs.
	opt.A = plan.HandoffKernel
	opt.HandoffBlocks = h.Blocks
	opt.HandoffBlockBytes = h.BlockBytes
	opt.HandoffRounds = h.Rounds
	opt.HandoffSeed = h.Seed
	opt.MemLock = h.Lock
	// The block-to-core partition makes every participant
	// load-bearing; the flag states that even where a standalone
	// attempt has no sibling phase to blacklist against.
	opt.AllOrNone = true
	opt.HandoffJournal = h.Journal
	return opt
}

// handoffDriftGate is the S3 machine gate every replay of a recorded
// handoff experiment passes BEFORE anything loads (menu repeat,
// 'handoff -repeat' and a request-driven run all share it): every
// recorded cpu must exist today, and a run recorded with cpu-set/llc
// provenance must be reconfirmable against today's cpu-set view. It
// returns the refusal sentence when the recorded experiment cannot
// be reproduced as described - the manifest belongs to a different
// machine, and replay never quietly re-picks cores or downgrades a
// cross-domain pairing. "" means the machine still honors it.
func handoffDriftGate(h *handoffRecipe) string {
	for _, c := range h.Cores {
		if c < 0 || c >= runtime.NumCPU() {
			return fmt.Sprintf("cpu %d from the recorded manifest does not exist on this machine (logical cpus 0..%d): the manifest belongs to a different machine and replay never quietly re-picks cores", c, runtime.NumCPU()-1)
		}
	}
	if len(h.LLCByCore) > 0 {
		states, verr := sysinfo.CpuSetStates()
		if verr != nil {
			return fmt.Sprintf("the recorded run resolved its cores against the OS cpu-set view, which cannot be consulted now (%v): the cross-LLC pairing it claimed cannot be reconfirmed, and quietly downgrading it to core-only pairing is the substitution this tool refuses", verr)
		}
		today := make(map[int]int, len(states))
		for _, st := range states {
			today[st.LogicalCPU] = st.LastLevelCache
		}
		for _, c := range h.Cores {
			rec, recorded := h.LLCByCore[c]
			if !recorded {
				continue
			}
			now, ok := today[c]
			if ok && now == rec {
				continue
			}
			todayLabel := "the cpu-set view no longer reports it at all"
			if ok {
				todayLabel = fmt.Sprintf("it reports llc domain %d today", now)
			}
			return fmt.Sprintf("core %d sat in llc domain %d when the experiment was recorded, but %s: the recorded cross-domain pairing cannot be reproduced on this machine as described, so the repeat refuses", c, rec, todayLabel)
		}
	}
	return ""
}

// runHandoffTracked is the public handoff attempt (S6). Every entry
// - the menu repeat, 'handoff -repeat', run -request with a
// kind=handoff manifest, and the bare 'handoff' command's public
// form - runs the coordinator through the SAME single dispatch every
// scenario phase uses (executeOptWithPanel), wrapped in the
// lifecycle every other entry point records: the session manifest
// and pending receipt before load, the lastresult binding at the
// end, and - when a managed stream is live - resolved/started and
// phase_started records before the run, the journaled lease steps as
// handoff_lease records (emitted by the executor's own seam), and
// the phase's structured outcome as phase_finished plus findings.
// Nothing is re-resolved behind the manifest's back (the drift gate
// refuses first), the shared section is admitted against today's
// memory BEFORE identity (like the scenario dry gate), and every
// later return closes receipt and session with what was actually
// observed - fault 1, setup 2, interrupted 130, clean 0.
func runHandoffTracked(lr *lastRun, ui *screenUI) int {
	h := lr.Handoff
	if msg := handoffDriftGate(h); msg != "" {
		fmt.Fprintf(os.Stderr, "handoff: %s\n", msg)
		return 2
	}
	opt := handoffRunOptions(*lr.Config, h)
	if aerr := plan.HandoffAdmission(opt); aerr != nil {
		fmt.Fprintf(os.Stderr, "handoff: %v\n", aerr)
		return 2
	}
	opt.PlanHash = planHash(*lr, opt)
	st, serr := newSessionTracker("handoff", lr, []manifestRun{{Name: "handoff", Journal: h.Journal}})
	if serr != nil {
		fmt.Fprintf(os.Stderr, "cannot start session: %v\n", serr)
		return 2
	}
	opt.SessionID = st.m.SessionID
	tr := newAttemptReceipt(lr.RecipeID, 1, nil, []string{opt.PlanHash})
	currentEvents.emit("resolved", "", resolvedPayload{Runs: 1, PlanHashes: []string{opt.PlanHash}}, false)
	currentEvents.emit("started", "", startedPayload{Runs: 1, Cycles: 1}, false)
	currentEvents.emit("phase_started", "handoff", handoffPhaseStartedPayload{
		Cores:      append([]int(nil), h.Cores...),
		Blocks:     h.Blocks,
		BlockBytes: h.BlockBytes,
		Rounds:     h.Rounds,
		Seed:       h.Seed,
	}, false)
	res := executeOptWithPanel(opt, ui.monitor())
	emitPhaseResult("handoff", res)
	tr.finish(res.Exit, []plan.Result{res})
	st.finish(res.Exit, []plan.Result{res})
	return res.Exit
}

// describeHandoff is runLastRun's one summary line for the handoff
// kind (describeSession and describeScenario print their own): what
// the recorded experiment WAS, so "the same" is verifiable at a
// glance before the new attempt starts.
func describeHandoff(lr *lastRun) {
	h := lr.Handoff
	profile := ""
	if lr.ProfileID != "" {
		profile = fmt.Sprintf(" · profile %s %s", lr.ProfileID, lr.ProfileVersion)
	}
	fmt.Printf("Repeating last run:%s Handoff · cores %v · %d block(s) x %s · %d round(s) · seed %d · lock %s\n",
		profile, h.Cores, h.Blocks, human.Bytes(h.BlockBytes), h.Rounds, h.Seed, h.Lock)
}

// handoffRepeatPublic is the public settings-repeat (S6): the menu
// repeat's twin. The drift gate, the session identity, the receipt
// and the single dispatch all live in runHandoffTracked, so a CLI
// repeat cannot drift from a menu repeat or a request-driven run.
func handoffRepeatPublic() int {
	lr, err := loadLastRun()
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff repeat: no durable request to repeat: %v\n", err)
		return 2
	}
	if lr.Kind != lastRunHandoff {
		fmt.Fprintf(os.Stderr, "handoff repeat: the repeat slot holds kind %q: no handoff manifest to repeat\n", lr.Kind)
		return 2
	}
	describeHandoff(lr)
	describeLastOutcome(lr)
	return runHandoffTracked(lr, nil)
}

// handoffReplay is observed-order replay (S3): an offline arbiter-
// contract check over one journaled generation. The durable manifest
// in the repeat slot, when one is present, BOUNDS the check: grant
// ownership and the seed's schedule order are verified against it;
// without a manifest only the arbiter invariants are checked, and
// that limit is said out loud.
func handoffReplay(path, runID string) int {
	exp, note := replayExpectation(path)
	fmt.Printf("handoff replay: %v\n", note)
	rep, err := subprocess.VerifyHandoffJournal(path, runID, exp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff replay: %v\n", err)
		return 2
	}
	fmt.Printf("handoff replay: generation %s: %d lease-write(s) tried, %d full handoff(s) done, %d published-but-unverified\n", rep.RunID, rep.Tried, rep.Done, rep.Unfinished)
	checks := "unknown"
	if rep.ChecksKnown {
		checks = human.Count(rep.Checks)
	}
	fmt.Printf("handoff replay: %s words verified by READ_OK acks; the journaled run ended with exit %d\n", checks, rep.RecordedExit)
	if rep.Fault != "" {
		fmt.Printf("handoff replay: the generation journaled a lease failure on core %d: %s\n", rep.FaultCore, rep.Fault)
	}
	if len(rep.Violations) > 0 {
		fmt.Fprintln(os.Stderr, "handoff replay: the lease stream is NOT internally consistent:")
		for _, v := range rep.Violations {
			fmt.Fprintf(os.Stderr, "  - %s\n", v)
		}
		return 2
	}
	bound := " (arbiter invariants only: no manifest bound)"
	if rep.ManifestBound {
		bound = " (arbiter invariants + grant ownership + seed schedule order, bounded by the durable manifest)"
	}
	fmt.Println("handoff replay: consistent" + bound)
	// The replay's exit mirrors the recorded verdict: a journaled
	// fault (1) is never downgraded by a "successful replay", and a
	// journaled setup refusal (2) stays a 2 with its story told.
	return rep.RecordedExit
}

// replayExpectation binds the validator to the durable manifest when
// the repeat slot holds one - and only says so when it truly does.
func replayExpectation(path string) (*subprocess.HandoffExpectation, string) {
	lr, err := loadLastRun()
	if err != nil {
		return nil, fmt.Sprintf("handoff replay: no manifest bound (%v): verifying the arbiter invariants only", err)
	}
	if lr.Kind != lastRunHandoff {
		return nil, fmt.Sprintf("handoff replay: the repeat slot holds kind %q: verifying the arbiter invariants only", lr.Kind)
	}
	h := lr.Handoff
	note := fmt.Sprintf("handoff replay: manifest bound (kind=handoff, recipe %s): grant ownership and seed %d schedule order are verified against it", lr.RecipeID, h.Seed)
	if h.Journal != path {
		note += fmt.Sprintf(" · NOTE: the slot's journal is %s but the replayed file is %s - if they are different runs, expect binding violations", h.Journal, path)
	}
	return &subprocess.HandoffExpectation{Cores: h.Cores, Blocks: h.Blocks, Rounds: h.Rounds, Seed: h.Seed}, note
}

// handoffRepeat is settings-repeat (S3) in the legacy HELPER form:
// the recorded geometry and seed run AGAIN as a new attempt (fresh
// workers, fresh lease records under a fresh run_id, appended to the
// same journal as the next generation), journaled byte-identically to
// the accepted P12-era records - no session manifest, no receipt, no
// run_meta. It re-resolves NOTHING behind the manifest's back and
// re-verifies nothing about the old attempt - both facts are stated
// in the output, and machine or topology drift is a refusal, not a
// re-pick. The PUBLIC repeat (S6) is handoffRepeatPublic: same gate,
// same geometry, with the full tracking lifecycle.
func handoffRepeat() int {
	lr, err := loadLastRun()
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff repeat: no durable request to repeat: %v\n", err)
		return 2
	}
	if lr.Kind != lastRunHandoff {
		fmt.Fprintf(os.Stderr, "handoff repeat: the repeat slot holds kind %q: no handoff manifest to repeat\n", lr.Kind)
		return 2
	}
	h := lr.Handoff
	if msg := handoffDriftGate(h); msg != "" {
		fmt.Fprintf(os.Stderr, "handoff repeat: %s\n", msg)
		return 2
	}
	fmt.Printf("handoff: settings-repeat from the durable manifest (cores %v, %d block(s) x %d bytes, %d round(s), seed %d, journal %s): this is a NEW attempt with fresh evidence; the recorded attempt is not re-verified here, and its acks and results are never reused\n",
		h.Cores, h.Blocks, h.BlockBytes, h.Rounds, h.Seed, h.Journal)
	return subprocess.ExecuteHandoff(subprocess.HandoffSpec{
		Cores:       append([]int(nil), h.Cores...),
		BlockBytes:  h.BlockBytes,
		Blocks:      h.Blocks,
		Rounds:      h.Rounds,
		Seed:        h.Seed,
		Lock:        h.Lock,
		JournalPath: h.Journal,
		Opt:         handoffSafetyOptions(*lr.Config, h.Cores),
		OnLease:     handoffLeaseEmitter(), // S4: the repeat attempt streams its lease steps under the same contract
	}, nil)
}

// handoffSafetyOptions resolves the shared safety request for a
// handoff run from config (B09): the same limit, stop-on-fault
// policy and control-core vocabulary the scenario engines use.
func handoffSafetyOptions(cfg config.Config, cores []int) plan.Options {
	control := -1 // observers unpinned: handoff has no scenario worker set to dodge
	if cfg.ControlCore != nil {
		control = *cfg.ControlCore
	}
	return plan.Options{
		TestName:    "handoff",
		Cores:       cores,
		PerProcess:  true,
		ControlCore: control,
		MaxTempC:    cfg.MaxTemp,
		StopOnFault: cfg.StopOnFault,
	}
}

// handoffCoreResolution is the resolved core set plus what the OS
// cpu-set view showed while choosing it. Explicit lists carry zero
// provenance (the view was never consulted - honestly absent, never
// guessed); "auto" records the domains it saw and each selected
// core's domain, so the manifest can explain its own choice later.
type handoffCoreResolution struct {
	Cores       []int
	DomainsSeen int
	LLCByCore   map[int]int
}

// handoffCores resolves -cores. "auto" asks the OS's own cpu-set view
// for the last-level-cache domains and picks one core per domain
// (unparked preferred, lowest number wins): the point of P07e is
// data crossing an L3 boundary, and a self-picked pair that quietly
// shares one cache would weaken the claim without saying so. If the
// view is unavailable or shows fewer than two domains, auto refuses
// and says why - naming cores explicitly is always possible, and the
// run then honestly reports "pairing by core only".
func handoffCores(flag string) (handoffCoreResolution, error) {
	if flag != "auto" {
		var out []int
		for _, p := range strings.Split(flag, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 0 {
				return handoffCoreResolution{}, fmt.Errorf("bad core %q (want numbers or 'auto')", p)
			}
			out = append(out, n)
		}
		return handoffCoreResolution{Cores: out}, nil
	}
	states, err := sysinfo.CpuSetStates()
	if err != nil {
		return handoffCoreResolution{}, fmt.Errorf("auto needs the cpu-set view (%v); name the cores explicitly", err)
	}
	best := map[int]sysinfo.CpuSetState{} // llc domain -> chosen core
	for _, st := range states {
		cur, seen := best[st.LastLevelCache]
		if !seen || (!st.Parked && cur.Parked) || (st.Parked == cur.Parked && st.LogicalCPU < cur.LogicalCPU) {
			best[st.LastLevelCache] = st
		}
	}
	cores := make([]int, 0, len(best))
	for _, st := range best {
		cores = append(cores, st.LogicalCPU)
	}
	sort.Ints(cores)
	if len(cores) < 2 {
		return handoffCoreResolution{}, fmt.Errorf("the cpu-set view shows only %d llc domain(s) here; name cores explicitly if you still want a cross-core handoff", len(cores))
	}
	byCore := make(map[int]int, len(best))
	for _, st := range best {
		byCore[st.LogicalCPU] = st.LastLevelCache
	}
	return handoffCoreResolution{Cores: cores, DomainsSeen: len(best), LLCByCore: byCore}, nil
}
