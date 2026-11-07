package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// lastresult.json is the receipt of the LATEST attempt made against the
// latest saved recipe (C3): a disposable one-screen summary, not replay
// input and not the history index - U02's session manifests will be
// the durable history. It answers the repeat screen's second question
// (menu plan section 1): besides "what will replay", "how did the last
// attempt of THIS recipe end".
//
// Two states, and the difference is the crash story: pending means the
// attempt was armed and started and never finalized - a machine that
// died mid-run leaves exactly this, and the menu says so instead of
// showing yesterday's outcome. final means the attempt ended and said
// how (exit code, execution, validation - each only as far as the
// entry point actually observed it; "" = unknown, e.g. transition
// runs whose verdict lives in the journal).
//
// The binding is recipe_id: a receipt written for a request that was
// later displaced by a new save is never shown as the current
// recipe's outcome - mismatch reads as unknown, never as a guess.

const (
	lastResultVersion = 1
	receiptPending    = "pending"
	receiptFinal      = "final"
)

// lastResultPath is a var so tests can point it at a scratch directory.
var lastResultPath = "lastresult.json"

type lastResult struct {
	Version    int    `json:"version"`
	State      string `json:"state"`
	RecipeID   string `json:"recipe_id,omitempty"` // the saved request this attempt replayed
	AttemptID  string `json:"attempt_id"`          // this attempt's own stamp (second + random)
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	// Exit is the attempt's process-level verdict; nil until final.
	Exit *int `json:"exit,omitempty"`
	// Execution/Validation carry plan's own vocabularies, aggregated
	// across the attempt's runs by the severity tables below - and
	// stay "" when the entry point never saw a plan.Result (transition
	// mode: the journal holds the story, the receipt says unknown).
	Execution  string `json:"execution,omitempty"`
	Validation string `json:"validation,omitempty"`
	// Verdict/VerdictReason carry the P13 result vocabulary (PASS /
	// FAIL / INCOMPLETE / UNVERIFIED) computed from the attempt's
	// structured outcomes when the receipt was finalized: a reader
	// never re-guesses, and a known non-zero exit can never render as
	// "unknown" while a reason exists. Older receipts lack the field
	// - the reader rebuilds the verdict from the fields they do have.
	Verdict       string   `json:"verdict,omitempty"`
	VerdictReason string   `json:"verdict_reason,omitempty"`
	RunsPlanned   int      `json:"runs_planned,omitempty"`  // slots the attempt intended to run
	RunsFinished  int      `json:"runs_finished,omitempty"` // slots that reached a terminal Result
	Findings      int      `json:"findings,omitempty"`      // observed findings across all runs
	AbortMsg      string   `json:"abort_msg,omitempty"`     // first non-empty stop reason
	Logs          []string `json:"logs,omitempty"`          // the attempt's actual log paths
	PlanHashes    []string `json:"plan_hashes,omitempty"`   // experiment identities this attempt ran
}

// Severity orders for aggregation: the attempt's label is the WORST
// terminal state any of its runs reached - an attempt that faulted is
// never labeled "completed" because its other runs finished. Unknown
// validation outranks partial, partial outranks not_performed, and
// only an all-confirmed attempt may claim verified.
var execSeverity = map[plan.Execution]int{
	plan.ExecCompleted:    0,
	plan.ExecSkipped:      1,
	plan.ExecCancelled:    2,
	plan.ExecInterrupted:  3,
	plan.ExecRuntimeError: 4,
	plan.ExecSetupError:   5,
}
var valSeverity = map[plan.Validation]int{
	plan.ValVerified:          0,
	plan.ValExternalConfirmed: 1,
	plan.ValNotPerformed:      2,
	plan.ValPartial:           3,
	plan.ValUnknown:           4,
}

// attemptReceipt is the per-attempt writer. A nil receiver means this
// caller does not keep a receipt (tests, standalone paths): every
// method no-ops, so execution paths stay single.
type attemptReceipt struct {
	r lastResult
}

// newAttemptReceipt stamps the attempt and writes the pending state
// immediately: from this moment on, a crash leaves a truthful
// "started, never finalized" behind.
func newAttemptReceipt(recipeID string, planned int, logs, hashes []string) *attemptReceipt {
	if recipeID == "" {
		return nil // unbound recipe (save failed or predates C3): no receipt to mislead with
	}
	tr := &attemptReceipt{r: lastResult{
		Version:     lastResultVersion,
		State:       receiptPending,
		RecipeID:    recipeID,
		AttemptID:   newRecipeID(),
		StartedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		RunsPlanned: planned,
		Logs:        logs,
		PlanHashes:  hashes,
	}}
	tr.save("started")
	return tr
}

// finish writes the final state from the attempt's collected results.
// results may be shorter than RunsPlanned - the difference is stated
// as runs that never reached a terminal state, never smoothed over.
func (tr *attemptReceipt) finish(exit int, results []plan.Result) {
	if tr == nil {
		return
	}
	// Only observed outcomes overwrite: finishSetup has already
	// stamped setup_error, and results that never revealed an
	// execution must not erase it ("" = never observed).
	exec, val, findings, abort := worstOutcome(results)
	if exec != "" {
		tr.r.Execution = exec
	}
	if val != "" {
		tr.r.Validation = val
	}
	tr.r.Findings += findings
	if tr.r.AbortMsg == "" && abort != "" {
		tr.r.AbortMsg = abort
	}
	tr.r.RunsFinished = len(results)
	// The P13 word: fold every observed run's structured verdict into
	// the attempt's word (FAIL > INCOMPLETE > UNVERIFIED > PASS), and
	// stamp it beside the exit code so no reader ever has to guess
	// between "the machine faulted" and "the run did not finish". An
	// attempt that never ran a load (finishSetup) still gets the word
	// from its exit.
	if vd := attemptVerdict(exit, results); vd.Kind != "" {
		tr.r.Verdict = string(vd.Kind)
		tr.r.VerdictReason = vd.Reason
	}
	tr.r.State = receiptFinal
	tr.r.Exit = &exit
	tr.r.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	tr.save("finalized")
}

// worstOutcome aggregates child-run outcomes the way the receipt
// does: the most severe execution and validation actually observed
// ("" = never observed, never assumed), the total findings and the
// first abort message. Shared with the U02 session manifest so the
// two history views can never drift apart in how they judge the
// same results.
func worstOutcome(results []plan.Result) (exec, val string, findings int, abort string) {
	worstExec, worstVal := -1, -1
	for _, res := range results {
		if s, ok := execSeverity[res.Execution]; ok && s > worstExec {
			worstExec, exec = s, string(res.Execution)
		}
		if s, ok := valSeverity[res.Validation]; ok && s > worstVal {
			worstVal, val = s, string(res.Validation)
		}
		findings += len(res.Findings)
		if abort == "" && res.AbortMsg != "" {
			abort = res.AbortMsg
		}
	}
	return
}

// attemptVerdict folds the attempt's observed run results into the
// P13 word (FAIL > INCOMPLETE > UNVERIFIED > PASS). With no run
// result at all, the exit code is the only structured fact left: a
// non-zero exit is INCOMPLETE (the attempt ended in a way that cannot
// be success - typically finishSetup or a refusal), and a clean exit
// that ran nothing is UNVERIFIED (nothing was verified).
func attemptVerdict(exit int, results []plan.Result) plan.Verdict {
	worst := plan.Verdict{}
	for i := range results {
		v := results[i].Verdict()
		if worst.Kind == "" || plan.VerdictSeverity(v.Kind) > plan.VerdictSeverity(worst.Kind) {
			worst = v
		}
	}
	if worst.Kind != "" {
		return worst
	}
	if exit == 0 {
		return plan.Verdict{Kind: plan.VerdictUnverified, Reason: "attempt produced no run result: nothing was verified"}
	}
	return plan.Verdict{Kind: plan.VerdictIncomplete, Reason: fmt.Sprintf("attempt ended with exit %d without reporting a run result", exit)}
}

// verdictOfReceipt reads the word the receipt stored; a receipt from
// before the field existed is rebuilt from the structured fields it
// DOES have (exit, execution, validation, counts). A final receipt
// with a readable exit code never reads UNKNOWN - that word belongs
// only to a missing, corrupt or foreign record.
func verdictOfReceipt(r *lastResult) plan.Verdict {
	if r == nil {
		return plan.Verdict{Kind: plan.VerdictUnknown, Reason: "no receipt"}
	}
	if r.Verdict != "" {
		return plan.Verdict{Kind: plan.VerdictKind(r.Verdict), Reason: r.VerdictReason}
	}
	res := plan.Result{
		Execution:  plan.Execution(r.Execution),
		Validation: plan.Validation(r.Validation),
		AbortMsg:   r.AbortMsg,
		Coverage:   plan.Coverage{Started: r.RunsFinished, Completed: r.RunsFinished, ChecksKnown: false},
	}
	if r.Exit != nil {
		res.Exit = *r.Exit
	}
	return res.Verdict()
}

// finishSetup closes an attempt that died before any run executed:
// setup refusals at the entry point. Execution says setup_error -
// the one label that never blames hardware for a broken setup.
func (tr *attemptReceipt) finishSetup(exit int) {
	if tr == nil {
		return
	}
	tr.r.Execution = string(plan.ExecSetupError)
	tr.finish(exit, nil)
}

func (tr *attemptReceipt) save(what string) {
	if tr == nil {
		return
	}
	if err := saveLastResult(tr.r); err != nil {
		fmt.Fprintf(os.Stderr, "warning: outcome receipt not %s (%v); the run result itself is unaffected\n", what, err)
	}
}

// saveLastResult writes the receipt atomically (same discipline as the
// recipe: unique temp, Sync, replace-rename - a crash never leaves a
// half-written receipt that would read as an outcome).
func saveLastResult(r lastResult) error {
	r.Version = lastResultVersion
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir, base := filepath.Split(lastResultPath)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, lastResultPath); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// loadLastResult reads and validates the receipt. Anything corrupt,
// newer or half-stated comes back as an error: the caller says
// "outcome unknown" with the reason instead of guessing.
func loadLastResult() (*lastResult, error) {
	b, err := os.ReadFile(lastResultPath)
	if err != nil {
		return nil, err
	}
	var r lastResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", lastResultPath, err)
	}
	if r.Version != lastResultVersion {
		if r.Version < lastResultVersion {
			return nil, fmt.Errorf("%s is a version-%d receipt", lastResultPath, r.Version)
		}
		return nil, fmt.Errorf("%s was written by a newer tool (version %d)", lastResultPath, r.Version)
	}
	switch r.State {
	case receiptPending, receiptFinal:
	default:
		return nil, fmt.Errorf("%s has unknown state %q", lastResultPath, r.State)
	}
	return &r, nil
}

// describeLastOutcome prints how the last attempt of THIS recipe
// ended - or honestly says why it cannot tell. It never runs
// anything and never changes what the repeat will replay.
func describeLastOutcome(lr *lastRun) {
	r, err := loadLastResult()
	if err != nil {
		fmt.Printf("Last outcome: unknown (%v).\n", err)
		return
	}
	if lr.RecipeID == "" || r.RecipeID != lr.RecipeID {
		fmt.Println("Last outcome: unknown for this recipe (the receipt belongs to a different saved request).")
		return
	}
	switch r.State {
	case receiptPending:
		fmt.Printf("Last outcome: attempt %s started %s and never finalized (crash, or still running?).\n",
			r.AttemptID, r.StartedAt)
		return
	}
	execution, validation := r.Execution, r.Validation
	if execution == "" {
		execution = "unknown"
	}
	if validation == "" {
		validation = "unknown"
	}
	abort := ""
	if r.AbortMsg != "" {
		abort = fmt.Sprintf(", stopped: %s", r.AbortMsg)
	}
	v := verdictOfReceipt(r)
	fmt.Printf("Last outcome: %s | %s / %s, exit %d, finished %s, runs %d/%d, findings %d%s.\n",
		v.String(), execution, validation, *r.Exit, r.FinishedAt, r.RunsFinished, r.RunsPlanned, r.Findings, abort)
}
