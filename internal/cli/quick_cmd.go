package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// quickCmd (R17): the one-command quick check the owner asked for -
// and it is the SAME pipeline the "Quick check" catalog card drives
// through the menu, not a second execution path: lookupPreset ->
// cfg.Phases -> compileScenario -> resource refusals (audit A04,
// before any confirm or save) -> confirm -> scenarioRecipe ->
// runScenarioRequest. Everything that makes a preset run reproducible
// (plan hash, session manifest, attempt receipt, lastresult) happens
// here exactly as it does in the menu.
//
// The closing block stays honest in both directions: the verdict word
// is read from the attempt receipt through runFinishNote - the same
// mechanism menuRunOutcome uses, never a paraphrase of the exit code -
// and the scope line repeats what the preset itself promises: this
// verdict covers the quick-check window (all-core int, all-core
// float, three transition bursts) and NOTHING else. No RAM engine,
// no per-core walk, no soak: a clean word is a sanity signal, not a
// stability verdict, and saying less would sell the tool's own claim
// short.
func quickCmd(args []string) int {
	fs := flag.NewFlagSet("quick", flag.ExitOnError)
	assumeYes := fs.Bool("yes", false, "start without asking (refusals and preflight stay in force)")
	dryRun := fs.Bool("dry-run", false, "print the plan this machine would run, then stop before any load or save")
	fs.Parse(args)

	cfg, err := config.LoadOrDefault(config.DefaultPath)
	if err != nil {
		// R15 rule, kept by the new entry point on its first day:
		// a present-but-broken config is this machine's unread plan.
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		fmt.Fprintln(os.Stderr, "Fix the file, or remove it to run on defaults.")
		return 2
	}
	// R15 disclosure: an absent config is legitimate defaults, and
	// the screen only means "defaults are in effect" if it SAYS so.
	if _, statErr := os.Stat(config.DefaultPath); os.IsNotExist(statErr) {
		fmt.Println("config.json: not found - this quick check runs on built-in defaults ('config init' writes one).")
	}
	workload.RegisterExternal(cfg.External)
	p, ok := lookupPreset("quick-check")
	if !ok {
		// Unreachable in a healthy build (the preset is part of it);
		// stating it anyway beats dereferencing a guess.
		fmt.Fprintln(os.Stderr, "quick check: the quick-check preset is missing from this build - that is a broken build, report it")
		return 2
	}
	cfg.Phases = p.Phases(cfg)
	opts, notices, err := compileScenario(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "quick check: %v\n", err)
		return 2
	}
	for _, n := range notices {
		fmt.Println(n)
	}
	// A04: resource refusals land before the confirm could ask for a
	// run this machine cannot honor, and before anything is saved.
	// quick-check maps no RAM-engine regions, so today this is a
	// formality kept on purpose: if the geometry ever gains memory,
	// the gate must already be here.
	if errs := scenarioMemoryRefusals(cfg.External, opts); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "quick check:", e)
		}
		return 2
	}
	printScenarioPlan(os.Stdout, cfg, opts, 1)
	if *dryRun {
		fmt.Println("Dry run: nothing was started, nothing was saved. Drop -dry-run to actually run the check.")
		return 0
	}
	if !*assumeYes {
		if !confirmRun(os.Stdin, os.Stdout) {
			fmt.Println("Run cancelled. No load was started.")
			return 0
		}
	}
	before := lastAttemptID()
	lr := scenarioRecipe(cfg, 1, nil, false, p.Name)
	ui := newScreenUI(os.Stdout)
	code := runScenarioRequest(lr, opts, 1, 0, false, ui, true)
	ui.finish() // release the live panel before the closing block (the menu defers the same call)

	// The verdict word comes from the attempt receipt, the same
	// source menuRunOutcome reads. No attempt was armed (a refusal
	// or a lock clash after the confirm): no verdict word exists to
	// state, and inventing one from the exit code is exactly the
	// drift this pin family keeps out.
	if note, _ := runFinishNote(before); note != "" {
		fmt.Println()
		fmt.Println(note)
	}

	// R19 dedup: the journal paths, the durable last result and the
	// report/bundle sharing how-to now arrive from the ONE shared
	// end-of-run evidence block (printed by the scenario pipeline for
	// every entry, menu and headless repeat included). What stays here
	// is what only quick can say: the receipt verdict word, the scope
	// of this preset's claim, and the next step - each exactly ONCE,
	// so no line appears twice because two renderers both thought it
	// was helpful.
	fmt.Println("\nScope of this verdict: the quick-check window only - all-core int, all-core float, three idle->load transition bursts.")
	fmt.Println("It is NOT a stability verdict: no RAM-engine verification, no per-core walk, no soak.")
	fmt.Println("Next step - after a fault or an INCOMPLETE/UNVERIFIED word: open the journal paths above, then run 'cpu-stability scenarios' and pick a longer profile (cpu-baseline, core-cycle, transitions).")
	fmt.Println("After a clean word: deeper evidence needs those same longer profiles; 'quick' only proves this window verified clean.")
	return code
}
