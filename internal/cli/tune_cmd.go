package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
	"github.com/weltercode/cpu-stability-checker/internal/tuning"
)

// tuneCmd is the P11 CO auto-tuning surface. Until P12 proves the
// real backend's diagnostics, automation is FAKE-ONLY: every
// mutating subcommand requires -fake, which wires the marked
// stateful fake machine, and the real CO backend is never connected
// to tuning. The plan document, the trial history and the durable
// transaction engine are the real machinery being exercised; only
// the machine and the tester are scripted fakes, and their output
// says FAKE on every observation.
//
// Exit codes match the co contract: 2 = refused before any hardware
// (or fake-machine) change; 1 = a trial/sweep/reconcile did not
// complete cleanly and reconciliation is required; 0 = completed
// (whatever the honest outcome was - a REJECTED candidate is a
// completed trial, not a tool failure).
func tuneCmd(args []string) int {
	if len(args) == 0 {
		printTuneUsage()
		return 2
	}
	sub := args[0]
	switch sub {
	case "plan-create", "plan-show", "trial", "sweep", "reconcile":
	default:
		fmt.Fprintf(os.Stderr, "tune: unknown subcommand %q\n", sub)
		printTuneUsage()
		return 2
	}
	fs := flag.NewFlagSet("tune "+sub, flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "machine-readable output (never ANSI)")
	fake := fs.Bool("fake", false, "REQUIRED for mutating ops: run against the marked stateful fake machine (P11 automation is fake-only until P12)")
	store := fs.String("store", "", "tuning plan document path (default: machine-wide location)")
	tuneLock := fs.String("lock", "", "tuning session lock path")
	txLock := fs.String("tx-lock", "", "CO transaction lock path")
	txStore := fs.String("tx-store", "", "CO transaction document path")
	core := fs.String("core", "core1", "target core (exactly one core is tuned)")
	minV := fs.Int("min", -50, "user search range low bound")
	maxV := fs.Int("max", 0, "user search range high bound")
	attempts := fs.Int("attempts", 10, "total attempt budget for the plan")
	temp := fs.Int("temp", 85, "user temperature limit in C for THIS CPU (0 = unset: acceptance stays incomplete)")
	seed := fs.Int64("seed", 0, "seed recorded in the plan")
	candidate := fs.Int("candidate", 0, "candidate value for tune trial")
	step := fs.Int("step", 1, "sweep stride (positive)")
	direction := fs.String("direction", "down", "sweep direction: down (toward -min) or up")
	retest := fs.Bool("retest", false, "sweep: re-run the last accepted candidate once to test reproducibility")
	timeout := fs.Duration("timeout", 120*time.Second, "operation bound")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	// plan-show is read-only: it needs no fake wiring and never
	// mutates the plan document.
	if sub == "plan-show" {
		st := &tuning.Store{Path: *store}
		return coTuneShow(st, *jsonOut)
	}

	if !*fake {
		fmt.Fprintln(os.Stderr, "tune: P11 automation is fake-only until P12 proves the real diagnostics: pass -fake to run against the marked stateful fake machine; the real CO backend is never wired into tuning")
		return 2
	}

	f := co.NewStateFake()
	st := &tuning.Store{Path: *store}
	e := &cotx.Engine{Backend: f, LockPath: *txLock, StorePath: *txStore}
	r := &tuning.Runner{Backend: f, Engine: e, Store: st, LockPath: *tuneLock, Tester: fakeTuner{}}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch sub {
	case "plan-create":
		p, err := tuning.CreatePlan(ctx, f, tuning.Options{
			TargetCore: *core, RangeMin: *minV, RangeMax: *maxV,
			TempLimitC: *temp, AttemptLimit: *attempts, Seed: *seed,
		})
		if err != nil {
			return tuneFail("plan-create", err)
		}
		if err := st.Save(*p, "create"); err != nil {
			return tuneFail("plan-create", err)
		}
		if *jsonOut {
			return coJSON(p)
		}
		fmt.Printf("tune plan %s created (FAKE machine %s, target %s, range %d..%d, attempts %d, temp limit %dC)\n",
			p.PlanID, p.HardwareID, p.TargetCore, p.RangeMin, p.RangeMax, p.AttemptLimit, p.TempLimitC)
		fmt.Println("  WARNING: this plan is bound to the stateful FAKE machine; it proves the machinery, not hardware stability")
		return 0

	case "trial":
		tr, err := r.RunTrial(ctx, *candidate)
		if err != nil {
			return tuneFail("trial", err)
		}
		if *jsonOut {
			return coJSON(tr)
		}
		fmt.Printf("tune trial %s: %s %d -> %d: %s\n", tr.TrialID, tr.Core, tr.FromValue, tr.Candidate, tr.Outcome)
		fmt.Printf("  cotx tx %s status %s, restore confirmed %v\n", orUnknown(tr.CotxTxID), orUnknown(tr.CotxStatus), tr.RestoreConfirmed)
		fmt.Printf("  why: %s\n", tr.Why)
		if tr.Outcome == tuning.OutcomeInterruptedUnknown {
			return 1
		}
		return 0

	case "sweep":
		rep, err := r.Search(ctx, tuning.SearchOptions{Step: *step, Direction: *direction, RetestLastAccepted: *retest})
		if err != nil {
			return tuneFail("sweep", err)
		}
		if *jsonOut {
			return coJSON(rep)
		}
		for _, s := range rep.Steps {
			fmt.Printf("  candidate %d: %s (%s)\n", s.Candidate, s.Outcome, s.Why)
		}
		fmt.Printf("tune sweep: stop=%s accepted=%v\n", rep.StopReason, rep.Accepted)
		if rep.RetestNote != "" {
			fmt.Printf("  retest: %s\n", rep.RetestNote)
		}
		fmt.Println("  these are FAKE-machine results under scripted observations: they show the machinery works, not that any CO value is stable")
		if rep.StopReason == tuning.StopInterrupted {
			return 1
		}
		return 0

	case "reconcile":
		tr, err := r.Reconcile(ctx)
		if err != nil {
			if co.IsKind(err, co.KindRecoveryRequired) || co.IsKind(err, co.KindExternalChange) || co.IsKind(err, co.KindFingerprint) {
				fmt.Fprintf(os.Stderr, "tune reconcile: %v\n", err)
				return 1
			}
			return tuneFail("reconcile", err)
		}
		if tr == nil {
			fmt.Println("tune reconcile: nothing undecided")
			return 0
		}
		if *jsonOut {
			return coJSON(tr)
		}
		fmt.Printf("tune reconcile: trial %s -> %s\n", tr.TrialID, tr.Outcome)
		fmt.Printf("  why: %s\n", tr.Why)
		return 0
	}
	return 2
}

// fakeTuner is the scripted tester for the fake-only CLI path. Every
// observation says FAKE: it demonstrates the durable pipeline, it is
// not evidence about any real CPU.
type fakeTuner struct{}

func (fakeTuner) RunTest(_ context.Context, core string, candidate int) tuning.Evidence {
	return tuning.Evidence{
		Checks: []tuning.Check{
			{Name: tuning.CheckSteady, Known: true, Yes: true, Why: fmt.Sprintf("FAKE scripted steady compute on %s=%d (no real load ran)", core, candidate)},
			{Name: tuning.CheckTransitions, Known: true, Yes: true, Why: "FAKE scripted idle/load transitions"},
			{Name: tuning.CheckExtended, Known: true, Yes: true, Why: "FAKE scripted extended confirmation"},
		},
		WheaGrowth:  tuning.Check{Name: "whea_corrected", Known: true, Yes: false, Why: "FAKE: no WHEA source on the fake machine"},
		Temperature: tuning.Check{Name: "cpu_temp", Known: true, Yes: true, Why: "FAKE: scripted within the configured limit"},
		Notes:       []string{"all observations are scripted fake data: they exercise the pipeline, they do not measure hardware"},
	}
}

func coTuneShow(st *tuning.Store, jsonOut bool) int {
	p, err := st.Load()
	if err != nil {
		return tuneFail("plan-show", err)
	}
	if p == nil {
		fmt.Println("tune: no tuning plan on disk")
		return 0
	}
	if jsonOut {
		return coJSON(p)
	}
	fmt.Printf("tune plan %s: status %s, target %s, range %d..%d, attempts %d/%d, temp limit %dC\n",
		p.PlanID, p.Status, p.TargetCore, p.RangeMin, p.RangeMax, p.AttemptsUsed, p.AttemptLimit, p.TempLimitC)
	fmt.Printf("  bound to: fake=%v host %s hw %s tool %s %s\n", p.Fake, p.Host, p.HardwareID, orUnknown(p.Tool), orUnknown(p.ToolVersion))
	for _, t := range p.Trials {
		fmt.Printf("  trial %s attempt %d: %s %d->%d state %s outcome %s\n",
			t.TrialID, t.Attempt, t.Core, t.FromValue, t.Candidate, t.State, orUnknown(t.Outcome))
		if t.Why != "" {
			fmt.Printf("    why: %s\n", t.Why)
		}
	}
	return 0
}

func tuneFail(op string, err error) int {
	if k := co.KindOf(err); k != "" {
		fmt.Fprintf(os.Stderr, "tune %s: %s: %v\n", op, k, err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "tune %s: %v\n", op, err)
	return 2
}

func printTuneUsage() {
	fmt.Fprintln(os.Stderr, "usage: tune plan-create|plan-show|trial|sweep|reconcile [-json] [-timeout DUR]")
	fmt.Fprintln(os.Stderr, "  tune plan-create -fake -core coreN -min M -max X -attempts N [-temp C]  create a plan on the FAKE machine")
	fmt.Fprintln(os.Stderr, "  tune plan-show                             show the plan and its trial history (read-only)")
	fmt.Fprintln(os.Stderr, "  tune trial -fake -candidate V              one explicit candidate trial")
	fmt.Fprintln(os.Stderr, "  tune sweep -fake -step S -direction down|up [-retest]  bounded stepwise search (never binary)")
	fmt.Fprintln(os.Stderr, "  tune reconcile -fake                       reconcile an interrupted trial (fresh read, no auto-resume)")
	fmt.Fprintln(os.Stderr, "P11 automation is fake-only until P12 proves the real diagnostics; the real CO backend is never wired in.")
}
