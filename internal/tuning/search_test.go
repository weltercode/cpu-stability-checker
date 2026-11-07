package tuning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// countingTester fails a candidate on a specific run number: the
// reproducibility probe (a value can pass once and fail the next).
type countingTester struct {
	runs      map[int]int
	failOnRun map[int]int // candidate -> fail on the Nth run
}

func (c *countingTester) RunTest(_ context.Context, _ string, cand int) Evidence {
	if c.runs == nil {
		c.runs = map[int]int{}
	}
	c.runs[cand]++
	if n, ok := c.failOnRun[cand]; ok && c.runs[cand] == n {
		ev := passEvidence()
		ev.Checks[0] = Check{Name: CheckSteady, Known: true, Yes: false, Why: fmt.Sprintf("steady compute failed on run %d", c.runs[cand])}
		return ev
	}
	return passEvidence()
}

func TestSearchRefusesBadOptions(t *testing.T) {
	h := newHarness(t)
	if _, err := h.runner.Search(context.Background(), SearchOptions{Step: 0, Direction: "down"}); !co.IsKind(err, co.KindBadProfile) {
		t.Fatalf("zero step not refused: %v", err)
	}
	if _, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "sideways"}); !co.IsKind(err, co.KindBadProfile) {
		t.Fatalf("bad direction not refused: %v", err)
	}
}

func TestSearchStepsDownUntilRangeExhausted(t *testing.T) {
	h := newHarness(t)
	p := loadPlanT(t, h.store)
	p.RangeMin = -30
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 5, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.StopReason != StopRangeExhausted {
		t.Errorf("stop = %s, want %s", rep.StopReason, StopRangeExhausted)
	}
	want := []int{-25, -30}
	if len(rep.Steps) != 2 || rep.Steps[0].Candidate != want[0] || rep.Steps[1].Candidate != want[1] {
		t.Fatalf("steps wrong: %+v", rep.Steps)
	}
	for _, s := range rep.Steps {
		if s.Outcome != OutcomeAccepted {
			t.Errorf("step %+v not accepted", s)
		}
	}
	if got := h.fake.hwProfile(t); !profileEqual(got, baseProf()) {
		t.Errorf("hardware not at baseline after sweep: %+v", got)
	}
}

func TestSearchStopsAtBudget(t *testing.T) {
	h := newHarness(t)
	p := loadPlanT(t, h.store)
	p.AttemptLimit = 3
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.StopReason != StopBudgetExhausted {
		t.Errorf("stop = %s, want %s", rep.StopReason, StopBudgetExhausted)
	}
	if len(rep.Steps) != 3 {
		t.Fatalf("want 3 steps within budget, got %+v", rep.Steps)
	}
	p = loadPlanT(t, h.store)
	if p.AttemptsUsed != 3 {
		t.Errorf("attempts not counted: %d", p.AttemptsUsed)
	}
}

// The plan forbids blind binary search and assumes nothing about
// monotonicity: a rejection must not prune later candidates, and the
// sweep must record every outcome in order.
func TestSearchNonMonotonicDoesNotPrune(t *testing.T) {
	h := newHarness(t)
	kt := &keyedTester{byCandidate: map[int]Evidence{
		-22: {Checks: []Check{
			{Name: CheckSteady, Known: true, Yes: false, Why: "failed"},
			checkYes(CheckTransitions, "ok"), checkYes(CheckExtended, "ok")},
			WheaGrowth: checkNo("whea", "none"), Temperature: checkYes("temp", "ok")},
		-26: {Checks: []Check{
			checkYes(CheckSteady, "ok"),
			{Name: CheckTransitions, Known: true, Yes: false, Why: "failed"},
			checkYes(CheckExtended, "ok")},
			WheaGrowth: checkNo("whea", "none"), Temperature: checkYes("temp", "ok")},
	}}
	h.runner.Tester = kt
	p := loadPlanT(t, h.store)
	p.RangeMin = -26
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Steps) != 3 {
		t.Fatalf("sweep pruned after the first rejection: %+v", rep.Steps)
	}
	wantOutcome := []string{OutcomeRejected, OutcomeAccepted, OutcomeRejected}
	for i, s := range rep.Steps {
		if s.Outcome != wantOutcome[i] {
			t.Errorf("step %d outcome %s, want %s", i, s.Outcome, wantOutcome[i])
		}
	}
	if len(rep.Accepted) != 1 || rep.Accepted[0] != -24 {
		t.Errorf("accepted set wrong: %v", rep.Accepted)
	}
}

func TestSearchStopsOnInterrupted(t *testing.T) {
	h := newHarness(t)
	deaths := 0
	h.engine.SetAbortAfter(func(step string) error {
		if step == "apply_result" {
			deaths++
			if deaths == 2 {
				return errors.New("simulated death on the second candidate")
			}
		}
		return nil
	})
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.StopReason != StopInterrupted {
		t.Errorf("stop = %s, want %s", rep.StopReason, StopInterrupted)
	}
	if len(rep.Steps) != 2 || rep.Steps[1].Outcome != OutcomeInterruptedUnknown {
		t.Fatalf("steps wrong: %+v", rep.Steps)
	}
	p := loadPlanT(t, h.store)
	if p.Status != StatusRecoveryRequired {
		t.Errorf("plan must demand reconciliation after an interrupted sweep step: %+v", p)
	}
	// A second sweep must be refused until reconciliation.
	if _, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down"}); err == nil {
		t.Errorf("second sweep ran over an unreconciled interruption")
	}
}

func TestSearchRetestReproduced(t *testing.T) {
	h := newHarness(t)
	p := loadPlanT(t, h.store)
	p.RangeMin = -22
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down", RetestLastAccepted: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Retest == nil || rep.Retest.Outcome != OutcomeAccepted {
		t.Fatalf("retest missing or wrong: %+v", rep.Retest)
	}
	if !strings.Contains(rep.RetestNote, "reproduced") {
		t.Errorf("retest note wrong: %q", rep.RetestNote)
	}
}

func TestSearchRetestNotReproduced(t *testing.T) {
	h := newHarness(t)
	ct := &countingTester{failOnRun: map[int]int{-22: 2}} // pass once, fail on retest
	h.runner.Tester = ct
	p := loadPlanT(t, h.store)
	p.RangeMin = -22
	if err := h.store.Save(*p, "edit"); err != nil {
		t.Fatal(err)
	}
	rep, err := h.runner.Search(context.Background(), SearchOptions{Step: 2, Direction: "down", RetestLastAccepted: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Retest == nil || rep.Retest.Outcome != OutcomeRejected {
		t.Fatalf("retest should have failed: %+v", rep.Retest)
	}
	if !strings.Contains(rep.RetestNote, "NOT reproduced") {
		t.Errorf("retest note must say NOT reproduced: %q", rep.RetestNote)
	}
	// The accepted list still records what passed once, but the note
	// keeps it honest: one pass is not stability.
	if len(rep.Accepted) != 1 || rep.Accepted[0] != -22 {
		t.Errorf("accepted set wrong: %v", rep.Accepted)
	}
}
