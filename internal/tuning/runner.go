package tuning

import (
	"context"
	"errors"
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
	"github.com/weltercode/cpu-stability-checker/internal/lockfile"
)

// Tester runs one candidate's observation suite and returns the
// evidence. It never decides: the runner classifies. A tester that
// cannot observe something reports it Unknown - never a silent pass.
// It is told which core and candidate value are under test so its
// observations can be attributed honestly.
type Tester interface {
	RunTest(ctx context.Context, core string, candidate int) Evidence
}

// Runner drives one explicit trial at a time against a marked fake
// backend, through the proven cotx engine, with the plan document as
// the durable trial record. Engine.Backend MUST be the same backend
// instance as Backend: the trial's hardware and the trial's plan
// cannot be allowed to talk to different machines.
type Runner struct {
	Backend  co.Backend
	Engine   *cotx.Engine
	Store    *Store
	LockPath string // "" -> DefaultTuneLockPath()
	Tester   Tester
}

func (r *Runner) lockPath() string {
	if r.LockPath != "" {
		return r.LockPath
	}
	return DefaultTuneLockPath()
}

// tuneErr is the package's error shape (op-tagged co.Error keeps the
// CLI's kind mapping uniform).
func tuneErr(kind, op string, err error) *co.Error {
	return &co.Error{Kind: kind, Op: "tune-" + op, Err: err}
}

func lockErr(err error) *co.Error {
	var he *lockfile.HeldError
	if errors.As(err, &he) {
		return tuneErr(co.KindLockHeld, "lock", err)
	}
	return tuneErr(co.KindLockHeld, "lock", fmt.Errorf("cannot establish tuning session lock state: %w", err))
}

// RunTrial runs exactly one explicitly requested candidate trial.
// It refuses: a non-fake backend, a missing plan, an undecided trial
// or recovery_required status (reconcile first), an exhausted attempt
// budget, a candidate outside the plan range or equal to the current
// value (a trial that rewrites the same value proves nothing), and
// any identity/baseline drift since the plan was created. The
// attempt is reserved when the trial is prepared and never refunded.
func (r *Runner) RunTrial(ctx context.Context, candidate int) (*Trial, error) {
	if _, ok := r.Backend.(FakeBackend); !ok {
		return nil, notFakeErr("trial")
	}
	if r.Engine == nil || r.Tester == nil {
		return nil, tuneErr(co.KindNotValidated, "trial", fmt.Errorf("runner needs an Engine and a Tester"))
	}
	if r.Engine.Backend != r.Backend {
		return nil, tuneErr(co.KindNotValidated, "trial",
			fmt.Errorf("Engine.Backend and Runner.Backend are different instances: the trial and its plan must observe the same machine"))
	}
	if _, ok := r.Engine.Backend.(FakeBackend); !ok {
		return nil, notFakeErr("trial")
	}

	lk, err := lockfile.Acquire(r.lockPath(), "co-tune")
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Release()

	plan, err := r.Store.Load()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, tuneErr(co.KindBadProfile, "trial", fmt.Errorf("no tuning plan on disk: create one first"))
	}
	if plan.Status == StatusRecoveryRequired {
		return nil, tuneErr(co.KindRecoveryRequired, "trial",
			fmt.Errorf("plan %s needs reconciliation before new trials", plan.PlanID))
	}
	for _, t := range plan.Trials {
		if t.State != TrialDecided {
			return nil, tuneErr(co.KindRecoveryRequired, "trial",
				fmt.Errorf("trial %s is undecided (%s): reconcile it before starting another", t.TrialID, t.State))
		}
	}
	if plan.AttemptsUsed >= plan.AttemptLimit {
		return nil, tuneErr(co.KindBadProfile, "trial",
			fmt.Errorf("attempt limit %d reached: no budget left for another trial", plan.AttemptLimit))
	}

	baseVal, ok := profileValue(plan.Baseline, plan.TargetCore)
	if !ok {
		return nil, tuneErr(co.KindBadProfile, "trial",
			fmt.Errorf("%s is not in the plan baseline: refusing to substitute a value silently", plan.TargetCore))
	}
	if candidate < plan.RangeMin || candidate > plan.RangeMax {
		return nil, tuneErr(co.KindBadProfile, "trial",
			fmt.Errorf("candidate %d is outside the plan range %d..%d", candidate, plan.RangeMin, plan.RangeMax))
	}
	if candidate == baseVal {
		return nil, tuneErr(co.KindBadProfile, "trial",
			fmt.Errorf("candidate %d equals the current %s value: a trial that rewrites the same value proves nothing", candidate, plan.TargetCore))
	}

	// Re-establish and bind: the plan is only valid for the exact
	// machine, topology and baseline profile it was created against.
	caps, mapping, cur, err := r.establish(ctx)
	if err != nil {
		return nil, err
	}
	if mappingFP(mapping) != plan.MappingFP {
		return nil, tuneErr(co.KindFingerprint, "trial",
			fmt.Errorf("topology mapping changed since the plan was created: refusing to run a plan bound to a different topology"))
	}
	if cur.HardwareID != plan.HardwareID {
		return nil, tuneErr(co.KindFingerprint, "trial",
			fmt.Errorf("hardware fingerprint changed since the plan was created: refusing to run a plan bound to a different machine"))
	}
	if !profileEqual(cur.Profile, plan.Baseline) {
		return nil, tuneErr(co.KindExternalChange, "trial",
			fmt.Errorf("current profile is not the plan baseline: the machine moved outside this plan; re-create the plan against the new starting point"))
	}
	_ = caps

	candProfile := co.Profile{Offsets: append([]co.Offset(nil), plan.Baseline.Offsets...)}
	for i := range candProfile.Offsets {
		if candProfile.Offsets[i].Core == plan.TargetCore {
			candProfile.Offsets[i].Value = candidate
		}
	}

	// Reserve the attempt and record the trial BEFORE any hardware
	// call. A storage failure here means zero hardware calls.
	trial := Trial{
		TrialID:   newID("trial"),
		Attempt:   plan.AttemptsUsed + 1,
		Core:      plan.TargetCore,
		FromValue: baseVal,
		Candidate: candidate,
		State:     TrialPrepared,
		StartedAt: now(),
		UpdatedAt: now(),
	}
	plan.AttemptsUsed++
	plan.Trials = append(plan.Trials, trial)
	if err := r.Store.Save(*plan, TrialPrepared); err != nil {
		return nil, tuneErr(co.KindStorage, TrialPrepared,
			fmt.Errorf("trial intent not durable, no hardware call made: %w", err))
	}

	// The phase runs inside the cotx transaction AFTER the apply was
	// verified and BEFORE the guaranteed restore. test_running and
	// test_finished are durable around the tester; a storage failure
	// here is a phase error - cotx still restores the baseline.
	var phaseFailure error
	phase := func(ctx context.Context) error {
		if _, err := r.updateTrial(plan.PlanID, trial.TrialID, func(t *Trial) {
			t.State = TestRunning
		}, TestRunning); err != nil {
			phaseFailure = fmt.Errorf("test_running not durable: %w", err)
			return phaseFailure
		}
		ev := r.Tester.RunTest(ctx, plan.TargetCore, candidate)
		if _, err := r.updateTrial(plan.PlanID, trial.TrialID, func(t *Trial) {
			t.Evidence = &ev
			t.State = TestFinished
		}, TestFinished); err != nil {
			phaseFailure = fmt.Errorf("test_finished/evidence not durable: %w", err)
			return phaseFailure
		}
		return nil
	}

	res, runErr := r.Engine.Run(ctx, candProfile, phase)

	// Associate the cotx transaction and decide. The plan document
	// on disk may have moved (the phase wrote through it), so reload.
	plan, err = r.Store.Load()
	if err != nil {
		return nil, tuneErr(co.KindRecoveryRequired, "decide",
			fmt.Errorf("plan unreadable after the trial: %w (hardware state needs reconciliation)", err))
	}
	var outcome, why string
	needRecovery := false
	restoreConfirmed := false
	if res != nil {
		restoreConfirmed = res.Status == cotx.StatusRestored || res.Status == cotx.StatusClosedNoChange
	}
	if res == nil {
		// The transaction never reached its durable intent: no
		// hardware call happened. A pending foreign transaction or a
		// corrupt cotx document still demands reconciliation.
		if co.IsKind(runErr, co.KindRecoveryRequired) || co.IsKind(runErr, co.KindStateCorrupt) {
			outcome, why, needRecovery = OutcomeInterruptedUnknown,
				fmt.Sprintf("cotx gate refused the transaction (%v): this trial made no hardware call, but an unrecovered transaction exists", runErr), true
		} else {
			outcome, why = OutcomeIncomplete,
				fmt.Sprintf("transaction could not start (%v): no hardware call was made and no test ran", runErr)
		}
	} else {
		if !terminalCotx(res.Status) {
			// The transaction is non-terminal on disk: the hardware
			// may still hold the candidate. Reconciliation required;
			// the trial is interrupted/unknown, not proof of anything.
			outcome, why, needRecovery = OutcomeInterruptedUnknown,
				fmt.Sprintf("cotx transaction %s ended %s (not terminal): hardware state must be reconciled; interruption cause is unknown (OS, power, or another core), not proof the candidate is bad", res.TxID, res.Status), true
		} else if phaseFailure != nil {
			// The runner knows exactly why there is no evidence
			// (storage failed inside the phase) and the hardware was
			// restored: the candidate is untested, not rejected.
			outcome, why = OutcomeIncomplete,
				fmt.Sprintf("test phase failed before durable evidence (%v): the transaction restored the hardware; the candidate is untested, not rejected", phaseFailure)
		} else {
			outcome, why = classify(r.loadTrial(plan, trial.TrialID).Evidence, restoreConfirmed, plan)
		}
	}

	plan, err = r.updateTrial(plan.PlanID, trial.TrialID, func(t *Trial) {
		if res != nil {
			t.CotxTxID = res.TxID
			t.CotxStatus = res.Status
			t.RestoreConfirmed = restoreConfirmed
		}
		t.Outcome = outcome
		t.Why = why
		t.State = TrialDecided
	}, "decided")
	if err != nil {
		// The decision is not durable: the trial stays undecided on
		// disk and Reconcile will classify it interrupted/unknown.
		// Acceptance NEVER rides on an in-memory decision.
		return nil, tuneErr(co.KindStorage, "decided",
			fmt.Errorf("decision not durable (outcome would have been %s): reconcile required: %w", outcome, err))
	}
	if needRecovery {
		plan.Status = StatusRecoveryRequired
		if err := r.Store.Save(*plan, "plan_status"); err != nil {
			return nil, tuneErr(co.KindStorage, "plan_status", fmt.Errorf("recovery status not durable: %w", err))
		}
	}
	tr := r.loadTrial(plan, trial.TrialID)
	return &tr, nil
}

// establish probes, confirms read/readback, maps topology and takes
// the exact fingerprinted current snapshot.
func (r *Runner) establish(ctx context.Context) (co.Capabilities, co.Mapping, co.Snapshot, error) {
	caps, err := r.Backend.Probe(ctx)
	if err != nil {
		return caps, co.Mapping{}, co.Snapshot{}, err
	}
	if !caps.Supported.Known || !caps.Supported.Yes {
		return caps, co.Mapping{}, co.Snapshot{}, tuneErr(co.KindUnsupportedCPU, "probe", fmt.Errorf("tool/CPU support not confirmed"))
	}
	if !caps.Read.Known || !caps.Read.Yes || !caps.Readback.Known || !caps.Readback.Yes {
		return caps, co.Mapping{}, co.Snapshot{}, tuneErr(co.KindNotValidated, "probe",
			fmt.Errorf("read/readback not confirmed: trials need a trustworthy readback"))
	}
	mapping, err := r.Backend.Topology(ctx)
	if err != nil {
		return caps, mapping, co.Snapshot{}, err
	}
	snap, err := r.Backend.Read(ctx)
	if err != nil {
		return caps, mapping, snap, err
	}
	if snap.Quality != "exact" || snap.HardwareID == "" {
		return caps, mapping, snap, tuneErr(co.KindNotValidated, "read",
			fmt.Errorf("current read not exact/fingerprinted (quality %q): cannot bind a trial to an unverified profile", snap.Quality))
	}
	return caps, mapping, snap, nil
}

// updateTrial reloads the plan, mutates the named trial, and durably
// saves it. The reloaded plan is returned so callers never carry
// stale history in memory.
func (r *Runner) updateTrial(planID, trialID string, fn func(*Trial), step string) (*Plan, error) {
	plan, err := r.Store.Load()
	if err != nil {
		return nil, err
	}
	if plan == nil || plan.PlanID != planID {
		return nil, fmt.Errorf("plan disappeared or changed identity mid-trial")
	}
	found := false
	for i := range plan.Trials {
		if plan.Trials[i].TrialID == trialID {
			fn(&plan.Trials[i])
			plan.Trials[i].UpdatedAt = now()
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("trial %s is not in the plan on disk", trialID)
	}
	if err := r.Store.Save(*plan, step); err != nil {
		return nil, err
	}
	return plan, nil
}

func (r *Runner) loadTrial(p *Plan, id string) Trial {
	for _, t := range p.Trials {
		if t.TrialID == id {
			return t
		}
	}
	return Trial{}
}

// classify turns durable evidence plus a confirmed restore into an
// outcome. Acceptance requires ALL of: the hardware confirmed back at
// the baseline, every required observation known-pass, corrected WHEA
// known-not-grown, and the fresh CPU temperature known-within-limit.
// Any unavailable required observation is incomplete, never a pass.
func classify(ev *Evidence, restoreConfirmed bool, plan *Plan) (string, string) {
	if !restoreConfirmed {
		return OutcomeInterruptedUnknown,
			"cotx restore was not confirmed by its terminal read: the hardware is not proven back at the baseline"
	}
	if ev == nil {
		return OutcomeInterruptedUnknown, "no durable evidence: the test never finished"
	}
	if ev.WheaGrowth.Known && ev.WheaGrowth.Yes {
		return OutcomeRejected, "corrected WHEA grew during the trial: acceptance forbidden" + why(ev.WheaGrowth.Why)
	}
	if !ev.WheaGrowth.Known {
		return OutcomeIncomplete, "corrected WHEA observation unavailable: required observation missing" + why(ev.WheaGrowth.Why)
	}
	if plan.TempLimitC <= 0 {
		return OutcomeIncomplete, "no user temperature limit configured: the fresh CPU sensor check cannot be evaluated (never a universal default)"
	}
	if ev.Temperature.Known && !ev.Temperature.Yes {
		return OutcomeRejected, "CPU temperature exceeded the configured limit" + why(ev.Temperature.Why)
	}
	if !ev.Temperature.Known {
		return OutcomeIncomplete, "fresh CPU temperature observation unavailable: required observation missing" + why(ev.Temperature.Why)
	}
	for _, name := range []string{CheckSteady, CheckTransitions, CheckExtended} {
		c, found := findCheck(ev.Checks, name)
		if !found {
			return OutcomeIncomplete, "required observation " + name + " is missing from the evidence"
		}
		if !c.Known {
			return OutcomeIncomplete, "required observation " + name + " is unavailable" + why(c.Why)
		}
		if !c.Yes {
			return OutcomeRejected, name + " failed" + why(c.Why)
		}
	}
	return OutcomeAccepted, "all required observations passed, corrected WHEA did not grow, temperature stayed within the configured limit, and the cotx transaction confirmed the restore"
}

func findCheck(cs []Check, name string) (Check, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func why(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func terminalCotx(s string) bool {
	// Exactly cotx's terminal set: restored_failed is NOT terminal
	// (recovery may still close it by observation), so it demands
	// reconciliation here too.
	return s == cotx.StatusRestored || s == cotx.StatusClosedNoChange
}

// Reconcile is the restart path for an undecided trial: it re-reads
// the hardware fresh, lets cotx reconcile its own transaction
// observationally, and classifies the trial interrupted/unknown. It
// NEVER re-runs the tester and NEVER resumes load from the mere
// existence of a state file: an interrupted trial is repeated or
// rejected by user policy, not automatically.
func (r *Runner) Reconcile(ctx context.Context) (*Trial, error) {
	if _, ok := r.Backend.(FakeBackend); !ok {
		return nil, notFakeErr("reconcile")
	}
	if r.Engine == nil {
		return nil, tuneErr(co.KindNotValidated, "reconcile", fmt.Errorf("runner needs an Engine"))
	}
	if r.Engine.Backend != r.Backend {
		return nil, tuneErr(co.KindNotValidated, "reconcile", fmt.Errorf("Engine.Backend and Runner.Backend are different instances"))
	}

	lk, err := lockfile.Acquire(r.lockPath(), "co-tune-recover")
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Release()

	plan, err := r.Store.Load()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, tuneErr(co.KindBadProfile, "reconcile", fmt.Errorf("no tuning plan on disk"))
	}
	var open *Trial
	for i := range plan.Trials {
		if plan.Trials[i].State != TrialDecided {
			open = &plan.Trials[i]
			break
		}
	}
	// Reconciliation is demanded by EITHER an undecided trial OR the
	// plan's recovery_required status (a trial may already be decided
	// interrupted_unknown while its cotx transaction is still open on
	// disk with the hardware holding the candidate).
	if plan.Status != StatusRecoveryRequired && open == nil {
		return nil, nil
	}

	// Fresh observation first: reconcile never trusts the plan's or
	// cotx's last words about the hardware.
	_, mapping, cur, err := r.establish(ctx)
	if err != nil {
		return nil, err
	}
	if mappingFP(mapping) != plan.MappingFP || cur.HardwareID != plan.HardwareID {
		return nil, tuneErr(co.KindFingerprint, "reconcile",
			fmt.Errorf("identity/topology changed since the plan: refusing automatic reconciliation; resolve the machine state manually and re-create the plan"))
	}

	// Let cotx close its own transaction observationally (restore if
	// the hardware still holds the candidate, close by observation if
	// the restore already landed, refuse across external change).
	res, recErr := r.Engine.Recover(ctx)

	atBaseline := profileEqual(cur.Profile, plan.Baseline)

	if !atBaseline {
		// cotx may have just restored it; re-read after Recover.
		after, rerr := r.Backend.Read(ctx)
		if rerr != nil || after.Quality != "exact" || after.HardwareID != plan.HardwareID {
			plan.Status = StatusRecoveryRequired
			if err := r.Store.Save(*plan, "plan_status"); err != nil {
				return nil, tuneErr(co.KindStorage, "plan_status", err)
			}
			return nil, tuneErr(co.KindRecoveryRequired, "reconcile",
				fmt.Errorf("hardware is not at the baseline and a trustworthy re-read after recovery is impossible (err=%v quality %q): fail closed", rerr, after.Quality))
		}
		if !profileEqual(after.Profile, plan.Baseline) {
			plan.Status = StatusRecoveryRequired
			if err := r.Store.Save(*plan, "plan_status"); err != nil {
				return nil, tuneErr(co.KindStorage, "plan_status", err)
			}
			return nil, tuneErr(co.KindRecoveryRequired, "reconcile",
				fmt.Errorf("hardware still not at the plan baseline after recovery (recover err=%v): fail closed, no automatic writes", recErr))
		}
	}

	if open != nil {
		whyStr := fmt.Sprintf("trial %s (attempt %d, %s %d->%d) never reached a durable decision (disk state %s, cotx tx %s); the hardware is confirmed back at the baseline; the interruption cause is unknown (OS, power, or another core): not proof the candidate is bad and not proof the CPU is broken; repeat or reject by user policy, never auto-resumed",
			open.TrialID, open.Attempt, open.Core, open.FromValue, open.Candidate, open.State, cotxTxOf(res))
		trialID := open.TrialID
		plan, err = r.updateTrial(plan.PlanID, trialID, func(t *Trial) {
			if res != nil {
				t.CotxTxID = res.TxID
				t.CotxStatus = res.Status
				t.RestoreConfirmed = res.Status == cotx.StatusRestored || res.Status == cotx.StatusClosedNoChange
			}
			t.Outcome = OutcomeInterruptedUnknown
			t.Why = whyStr
			t.State = TrialDecided
		}, "decided")
		if err != nil {
			return nil, tuneErr(co.KindStorage, "decided", fmt.Errorf("reconcile decision not durable: %w", err))
		}
		tr := r.loadTrial(plan, trialID)
		if noUndecided(plan) && plan.Status != StatusOpen {
			plan.Status = StatusOpen
			if err := r.Store.Save(*plan, "plan_status"); err != nil {
				return nil, tuneErr(co.KindStorage, "plan_status", err)
			}
		}
		return &tr, nil
	}
	// The trial was already decided (interrupted_unknown recorded by
	// the runner that saw the transaction stop): reconciliation here
	// was about the hardware, and it is now proven back at baseline.
	if plan.Status != StatusOpen {
		plan.Status = StatusOpen
		if err := r.Store.Save(*plan, "plan_status"); err != nil {
			return nil, tuneErr(co.KindStorage, "plan_status", err)
		}
	}
	return nil, nil
}

func cotxTxOf(res *cotx.Result) string {
	if res == nil {
		return "none"
	}
	if res.TxID == "" {
		return "none"
	}
	return res.TxID
}

func noUndecided(p *Plan) bool {
	for _, t := range p.Trials {
		if t.State != TrialDecided {
			return false
		}
	}
	return true
}

// profileValue returns the value of core in the profile.
func profileValue(p co.Profile, core string) (int, bool) {
	for _, o := range p.Offsets {
		if o.Core == core {
			return o.Value, true
		}
	}
	return 0, false
}

// profileEqual compares profiles as sets of core->value (same rule
// cotx uses): order is irrelevant, duplicates are never equal.
func profileEqual(a, b co.Profile) bool {
	if len(a.Offsets) != len(b.Offsets) {
		return false
	}
	am := map[string]int{}
	for _, o := range a.Offsets {
		if _, dup := am[o.Core]; dup {
			return false
		}
		am[o.Core] = o.Value
	}
	for _, o := range b.Offsets {
		if v, ok := am[o.Core]; !ok || v != o.Value {
			return false
		}
	}
	return true
}
