package tuning

import (
	"context"
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// SearchOptions configures the bounded stepwise sweep (plan P11):
// candidates are visited deterministically one stride at a time
// inside the USER-chosen range, under the attempt budget. There is
// deliberately NO binary search: instability detection is
// probabilistic and not necessarily monotonic in CO, so a rejection
// never prunes the rest of the range and an acceptance never implies
// the neighbours behave alike.
type SearchOptions struct {
	Step      int    // positive stride between candidates
	Direction string // "down" (toward RangeMin) or "up" (toward RangeMax)
	// RetestLastAccepted re-runs the last accepted candidate once
	// after the sweep. A margin or an acceptance is a heuristic until
	// it reproduces; the retest says honestly whether it did.
	RetestLastAccepted bool
}

// StepResult is one candidate's durable outcome.
type StepResult struct {
	Candidate int    `json:"candidate"`
	TrialID   string `json:"trial_id"`
	Outcome   string `json:"outcome"`
	Why       string `json:"why,omitempty"`
}

// Stop reasons for a sweep. Each is honest about why stepping ended;
// none of them claims more than the trials proved.
const (
	StopRangeExhausted  = "range_exhausted"
	StopBudgetExhausted = "budget_exhausted"
	StopInterrupted     = "interrupted"
	StopRefused         = "refused"
)

// SearchReport is the structured result of one sweep: every step,
// why stepping stopped, the accepted candidates, and the
// reproducibility retest when requested. It supports the report
// wording the plan demands ("passed profile X under conditions Y"),
// never "absolutely stable CO".
type SearchReport struct {
	Steps             []StepResult `json:"steps"`
	StopReason        string       `json:"stop_reason"`
	Accepted          []int        `json:"accepted"`
	LastAccepted      int          `json:"last_accepted"`
	LastAcceptedFound bool         `json:"last_accepted_found"`
	Retest            *StepResult  `json:"retest,omitempty"`
	RetestNote        string       `json:"retest_note,omitempty"`
}

// Search runs the bounded sweep. It refuses a non-positive step or
// an unknown direction up front; every other stop is a reported
// reason, not a silent truncation. An interrupted trial ends the
// sweep immediately (the plan demands reconciliation before any
// further hardware work), and the attempt budget is honored by the
// trial layer, never bypassed here.
func (r *Runner) Search(ctx context.Context, opts SearchOptions) (*SearchReport, error) {
	if opts.Step <= 0 {
		return nil, tuneErr(co.KindBadProfile, "search", fmt.Errorf("step must be positive, got %d", opts.Step))
	}
	if opts.Direction != "down" && opts.Direction != "up" {
		return nil, tuneErr(co.KindBadProfile, "search", fmt.Errorf(`direction must be "down" or "up", got %q`, opts.Direction))
	}
	plan, err := r.Store.Load()
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, tuneErr(co.KindBadProfile, "search", fmt.Errorf("no tuning plan on disk: create one first"))
	}
	// A sweep is refused up front while the plan demands
	// reconciliation or holds an undecided trial: no candidate is
	// even attempted until the machine state is proven clean again.
	if plan.Status == StatusRecoveryRequired {
		return nil, tuneErr(co.KindRecoveryRequired, "search",
			fmt.Errorf("plan %s needs reconciliation before a sweep", plan.PlanID))
	}
	for _, t := range plan.Trials {
		if t.State != TrialDecided {
			return nil, tuneErr(co.KindRecoveryRequired, "search",
				fmt.Errorf("trial %s is undecided (%s): reconcile it before sweeping", t.TrialID, t.State))
		}
	}
	baseVal, ok := profileValue(plan.Baseline, plan.TargetCore)
	if !ok {
		return nil, tuneErr(co.KindBadProfile, "search",
			fmt.Errorf("%s is not in the plan baseline", plan.TargetCore))
	}
	delta := opts.Step
	if opts.Direction == "down" {
		delta = -opts.Step
	}

	rep := &SearchReport{}
	for k := 1; ; k++ {
		cand := baseVal + k*delta
		if cand < plan.RangeMin || cand > plan.RangeMax {
			rep.StopReason = StopRangeExhausted
			break
		}
		if plan.AttemptsUsed >= plan.AttemptLimit {
			rep.StopReason = StopBudgetExhausted
			break
		}
		tr, err := r.RunTrial(ctx, cand)
		if err != nil {
			// A gate or storage refusal ends the sweep honestly; the
			// reason is reported, never hidden.
			rep.StopReason = StopRefused
			rep.Steps = append(rep.Steps, StepResult{Candidate: cand, Outcome: "refused", Why: err.Error()})
			break
		}
		st := StepResult{Candidate: cand, TrialID: tr.TrialID, Outcome: tr.Outcome, Why: tr.Why}
		rep.Steps = append(rep.Steps, st)
		switch tr.Outcome {
		case OutcomeAccepted:
			rep.Accepted = append(rep.Accepted, cand)
			rep.LastAccepted = cand
			rep.LastAcceptedFound = true
		case OutcomeInterruptedUnknown:
			rep.StopReason = StopInterrupted
			r.retest(ctx, rep, opts)
			return rep, nil
		}
		// Reload for the fresh budget count (the trial wrote through
		// the store).
		plan, err = r.Store.Load()
		if err != nil {
			rep.StopReason = StopRefused
			rep.Steps = append(rep.Steps, StepResult{Candidate: cand, Outcome: "unreadable", Why: err.Error()})
			break
		}
	}
	r.retest(ctx, rep, opts)
	return rep, nil
}

// retest re-runs the last accepted candidate once. Reproduction is
// evidence of reproducibility under the same conditions, nothing
// more; a non-reproduction is reported as such (probabilistic
// instability) and never converted into a silent acceptance.
func (r *Runner) retest(ctx context.Context, rep *SearchReport, opts SearchOptions) {
	if !opts.RetestLastAccepted || !rep.LastAcceptedFound {
		return
	}
	plan, err := r.Store.Load()
	if err != nil {
		rep.RetestNote = fmt.Sprintf("retest not run: plan unreadable: %v", err)
		return
	}
	if plan.AttemptsUsed >= plan.AttemptLimit {
		rep.RetestNote = "retest not run: attempt budget exhausted"
		return
	}
	tr, err := r.RunTrial(ctx, rep.LastAccepted)
	if err != nil {
		rep.RetestNote = fmt.Sprintf("retest not run: %v", err)
		return
	}
	rep.Retest = &StepResult{Candidate: rep.LastAccepted, TrialID: tr.TrialID, Outcome: tr.Outcome, Why: tr.Why}
	if tr.Outcome == OutcomeAccepted {
		rep.RetestNote = "reproduced: the accepted candidate passed again under the same conditions (still only 'passed under these conditions', never 'absolutely stable')"
	} else {
		rep.RetestNote = fmt.Sprintf("NOT reproduced: the same candidate came out %s on retest; instability is probabilistic, treat any margin as a heuristic and retest", tr.Outcome)
	}
}
