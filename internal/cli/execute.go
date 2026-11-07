package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runner"
	"github.com/weltercode/cpu-stability-checker/internal/subprocess"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// executeOpt is the single dispatch point between the two execution
// engines: per-process (one pinned child per logical CPU) or in-process
// (worker threads in this process). Every command that runs a plan goes
// through here, so the mode contract lives in exactly one place — a
// change to how modes are chosen or wired is one edit, not six.
//
// Single-instance workloads (Prime95's GUI, y-cruncher) force the
// in-process engine: per-process mode would give every worker its own
// copy of a tool that already spans the whole machine - the user's
// crash was exactly 16 Prime95 instances fighting over one desktop.
// The in-process engine shares one tool process across all slots.
func executeOpt(opt plan.Options) plan.Result {
	return executeOptWithPanel(opt, nil)
}

// Menu commands share both the panel and its text writer, including
// summaries between phases. Standalone commands retain their own panels.
func menuExecution(ui *screenUI) (func(plan.Options) plan.Result, io.Writer) {
	panel := ui.monitor()
	if panel == nil {
		return executeOpt, os.Stdout
	}
	return func(opt plan.Options) plan.Result { return executeOptWithPanel(opt, panel) }, panel.Writer()
}

// executeOptWithPanel is executeOpt with an optional caller-owned
// panel: the menu's session hands one Console to every run so hardware
// monitoring never pauses between runs or after the last one until the
// menu screen takes over.
func executeOptWithPanel(opt plan.Options, panel *dashboard.Console) plan.Result {
	// B08: every load path in the tool goes through this choke
	// point, so every load carries the run lock - menu, run,
	// scenario, session, headless - from any directory. The lock is
	// re-entrant per operation (an outer holder, e.g. the managed
	// entry or a session loop, is never contended by its own inner
	// calls) and released when the outermost join leaves.
	relLock, lerr := acquireRunLockShared()
	if lerr != nil {
		note := fmt.Sprintf("error: refusing to start: %v\n", lerr)
		if panel != nil {
			panel.Print(note)
		} else {
			fmt.Fprint(os.Stderr, note)
		}
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: lerr.Error()})
	}
	defer relLock()
	if opt.PerProcess {
		var single []string
		for _, name := range []string{opt.A, opt.B} {
			if workload.IsSingleInstance(name) && !contains(single, name) {
				single = append(single, name)
			}
		}
		if len(single) > 0 {
			note := fmt.Sprintf("Note: %v is single-instance (one tool process for the whole machine):\n"+
				"  running in-process, not per-process; every slot follows the one tool.\n", single)
			if panel != nil {
				panel.Print(note)
			} else {
				fmt.Print(note)
			}
			opt.PerProcess = false
			opt.Rotate = false
		}
	}
	// Workloads that verify data in memory-engine regions (the P06
	// ram-* family) are refused HERE, before anything starts, when
	// the engine cannot run: no budget, or in-process mode (checked
	// AFTER the single-instance downgrade above, so a downgrade that
	// strips the engine away also gets refused instead of quietly
	// running the workload memory-less).
	if err := plan.PreflightMemoryWorkloads(opt); err != nil {
		note := fmt.Sprintf("error: %v\n", err)
		if panel != nil {
			panel.Print(note)
		} else {
			fmt.Fprint(os.Stderr, note)
		}
		return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError,
			Reason: err.Error()})
	}
	// The memory engine is wired into per-process mode first (P05).
	// If a run asks for it but ends up in-process - including via the
	// single-instance downgrade above - say so visibly: silently
	// skipping the allocation the user configured is exactly the
	// kind of quiet reduction this project promises never to do.
	// (ram-* workloads never reach this note: the preflight above
	// already refused them.)
	if opt.Memory != "" && !opt.PerProcess {
		note := "Note: the memory engine is applied in per-process mode only;\n" +
			"  this run does NOT allocate or first-touch engine memory.\n"
		if panel != nil {
			panel.Print(note)
		} else {
			fmt.Print(note)
		}
	}
	// P08 transition mode: prepared per-core sessions driven by a
	// prepared burst schedule. It lives in the per-process engine only
	// (a prepared session IS a pinned process), and the single-instance
	// downgrade above would strip that away - so ValidateTransition
	// refuses such a phase visibly instead of running it degraded.
	if opt.Bursts > 0 {
		if err := plan.ValidateTransition(opt); err != nil {
			note := fmt.Sprintf("error: %v\n", err)
			if panel != nil {
				panel.Print(note)
			} else {
				fmt.Fprint(os.Stderr, note)
			}
			return plan.Finalize(plan.FinalInput{Execution: plan.ExecSetupError, Reason: err.Error()})
		}
		return subprocess.ExecuteSessionResult(opt, panel)
	}
	// P07e handoff phase (S5): the SAME dispatch carries it - the
	// coordinator is per-process by nature (pinned workers sharing one
	// inherited section), the scenario compile already validated the
	// shape and the resource gate admitted the geometry; the executor
	// revalidates (defense in depth) and reports through plan.Result,
	// so fault blacklisting, receipts and the stream treat the phase
	// exactly like every other engine's phase.
	if opt.HandoffBlocks > 0 {
		return subprocess.ExecuteHandoffResult(opt, panel, handoffLeaseEmitter())
	}
	// Q05 prepared pair-switch phase: the SAME dispatch carries it -
	// the scenario compile validated the shape (and OptionsFromPhase
	// built the geometry), the executor revalidates (defense in depth)
	// and reports through plan.Result, so fault blacklisting, receipts
	// and the stream treat the switch exactly like every other engine's
	// phase. One execution pipeline, no second replay path.
	if plan.SwitchActive(opt) {
		return subprocess.ExecuteSwitchResult(opt, panel)
	}
	if opt.PerProcess {
		return subprocess.ExecuteSubprocessWithPanel(opt, panel)
	}
	return runner.ExecuteWithPanel(opt, panel)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// verificationConfirmed reports whether a run's structured claim says
// the declared checking actually ran and confirmed itself. Exit 0 alone
// never says this: a run can end clean while verifying nothing (an
// external tool we cannot confirm) or while one required slot confirmed
// nothing. The brief summaries use this to never print "passed" on the
// exit code alone.
func verificationConfirmed(v plan.Validation) bool {
	return v == plan.ValVerified || v == plan.ValExternalConfirmed
}

// verificationLabel renders the claim for human output; an empty claim
// (a result built without Finalize, or an older code path) reads as
// unknown, never as a pass.
func verificationLabel(v plan.Validation) string {
	if v == "" {
		return "unknown"
	}
	return string(v)
}
