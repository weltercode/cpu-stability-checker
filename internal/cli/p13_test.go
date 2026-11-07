package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// Owner mandatory shape 1: two completed phases + one aborted phase +
// six phases that never ran must read INCOMPLETE with the count of
// what did not run - never "completed OK", never "unknown".
func TestScenarioVerdictStopsEarlyIsIncomplete(t *testing.T) {
	opts := make([]plan.Options, 9)
	for i := range opts {
		opts[i] = plan.Options{Phase: fmt.Sprintf("p%d", i+1), A: "int"}
	}
	results := make([]plan.Result, 9)
	results[0] = plan.Result{Exit: 0, Execution: plan.ExecCompleted, Validation: plan.ValVerified,
		Coverage: plan.Coverage{Started: 2, Completed: 2, Verified: 913, ChecksKnown: true}}
	results[1] = plan.Result{Exit: 0, Execution: plan.ExecCompleted, Validation: plan.ValVerified,
		Coverage: plan.Coverage{Started: 2, Completed: 2, Verified: 640, ChecksKnown: true}}
	results[2] = incidentPhaseResult()
	ran := []bool{true, true, true, false, false, false, false, false, false}

	var out bytes.Buffer
	writeScenarioSummary(&out, opts, results, ran, 2, "UNVERIFIED: load finished but 0 verified checks (20 of 20 units completed, exit 1)", nil)
	text := out.String()
	for _, want := range []string{
		"RESULT: INCOMPLETE",
		"6 of 9 phase(s) never ran",
		"not run (the scenario stopped before this phase)",
		"UNVERIFIED",
		"0 verified checks",
		"stability conclusion: NOT DETERMINED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "completed OK") || strings.Contains(text, "RESULT: PASS") {
		t.Errorf("an unfinished scenario must never claim a clean pass:\n%s", text)
	}
}

// incidentPhaseResult is the exact shape of the 2026-10-24 mix-trans
// phase: 20 units all completed their windows, every one confirmed
// zero checks, and the tool rejected the contract with exit 1.
func incidentPhaseResult() plan.Result {
	return plan.Result{Exit: 1, Execution: plan.ExecCompleted, Validation: plan.ValNotPerformed,
		Coverage: plan.Coverage{Started: 20, Completed: 20, Verified: 0, ChecksKnown: true}}
}

// Owner mandatory shape 2: when a run stops for an infrastructure
// reason (a crashing worker, a memory-region finding that names no
// CPU), the ORIGINAL structured reason must appear in the summary -
// the defect was a summary that replaced a known reason with "aborted
// (exit 1)" and showed only per-CPU faults.
func TestScenarioSummaryShowsNonCPUBoundFindingsVerbatim(t *testing.T) {
	opts := []plan.Options{{Phase: "mix-trans", A: "ram-constant"}}
	region := plan.Finding{
		Kind:    plan.FindDataMismatch,
		Source:  "workload",
		Message: "verified region mismatch: pattern block differs after fill",
		Attribution: plan.Attribution{
			Kind:       plan.AttrMemoryRegion,
			Region:     "worker0+64MiB",
			Confidence: plan.ConfConfirmed,
			ObservedOn: plan.IntPtr(3),
		},
	}
	crash := plan.Finding{
		Kind:        plan.FindToolFailure,
		Source:      "runner",
		Message:     "core 7 child exited with code 1067",
		Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed},
	}
	res := plan.Result{Exit: 1, Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Findings: []plan.Finding{region, crash}}

	var out bytes.Buffer
	writeScenarioSummary(&out, opts, []plan.Result{res}, []bool{true}, 0, res.Verdict().String(), nil)
	text := out.String()
	for _, want := range []string{
		"RESULT: FAIL", // a CONFIRMED region mismatch is hardware evidence...
		"memory region worker0+64MiB, observed on cpu 3",
		"verified region mismatch: pattern block differs after fill",
		"core 7 child exited with code 1067",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lost the finding evidence %q:\n%s", want, text)
		}
	}
}

// ...but the same phase with a tool crash and NO confirmed hardware
// evidence reads INCOMPLETE, not FAIL: the incident rule (a reported
// fault without confirmed hardware cause is run-control, never a
// hardware claim).
func TestScenarioReportedFaultWithoutHardwareStaysIncomplete(t *testing.T) {
	opts := []plan.Options{{Phase: "burst", A: "int"}, {Phase: "next", A: "int"}}
	crash := plan.Finding{Kind: plan.FindToolFailure, Source: "runner", Message: "worker process crashed: exit 1067",
		Attribution: plan.Attribution{Kind: plan.AttrWorker, Index: plan.IntPtr(7), Confidence: plan.ConfConfirmed}}
	res := plan.Result{Exit: 1, Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Findings: []plan.Finding{crash}}
	var out bytes.Buffer
	writeScenarioSummary(&out, opts, []plan.Result{res, {}}, []bool{true, false}, 0, res.Verdict().String(), nil)
	text := out.String()
	if !strings.Contains(text, "RESULT: INCOMPLETE") || !strings.Contains(text, "worker process crashed: exit 1067") {
		t.Errorf("reported fault must read INCOMPLETE with its reason:\n%s", text)
	}
	if strings.Contains(text, "RESULT: FAIL") {
		t.Errorf("no hardware evidence was confirmed - FAIL would blame the machine:\n%s", text)
	}
}

// Owner mandatory shape 7 at phase level: a workload mismatch phase
// renders FAIL (not INCOMPLETE), and the scenario headline follows.
func TestScenarioMismatchPhaseIsFail(t *testing.T) {
	opts := []plan.Options{{Phase: "cpu", A: "int"}, {Phase: "walk", A: "int"}}
	mism := plan.Finding{Kind: plan.FindDataMismatch, Source: "workload", Message: "int-chain reference differs on core 5",
		Attribution: plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(5), Confidence: plan.ConfConfirmed}}
	res := plan.Result{Exit: 1, Execution: plan.ExecRuntimeError, Validation: plan.ValPartial,
		Findings: []plan.Finding{mism}, Faults: map[int]string{5: "int-chain reference differs on core 5"}}
	var out bytes.Buffer
	writeScenarioSummary(&out, opts, []plan.Result{{Exit: 0, Validation: plan.ValVerified}, res}, []bool{true, true}, 1, res.Verdict().String(), map[int]bool{5: true})
	text := out.String()
	if !strings.Contains(text, "RESULT: FAIL") || !strings.Contains(text, "FAIL: 1 confirmed data mismatch") {
		t.Errorf("mismatch phase must read FAIL:\n%s", text)
	}
}

// runCycle's stop string (the input to the summary) carries the
// verdict word and its structured reason, not a bare "exit 1".
func TestRunCycleStopReasonCarriesVerdict(t *testing.T) {
	opts := []plan.Options{{Phase: "mix-trans", A: "ram-constant"}}
	execute := func(plan.Options) plan.Result { return incidentPhaseResult() }
	results, ran, stopped, stopMsg := runCycle(opts, &bytes.Buffer{}, execute, false, map[int]bool{})
	if stopped != 0 || !ran[0] || results[0].Exit != 1 {
		t.Fatalf("stopped=%d ran=%v results=%+v", stopped, ran, results)
	}
	if !strings.Contains(stopMsg, "UNVERIFIED") || !strings.Contains(stopMsg, "0 verified checks") {
		t.Errorf("stop reason must name the contract failure, got %q", stopMsg)
	}
}

// Owner mandatory shape 6: UNKNOWN belongs ONLY to a missing, corrupt
// or foreign record. A pre-P13 receipt (no verdict field) with a known
// exit must still render a dictionary word, never "Result: unknown".
func TestLegacyReceiptNeverRendersUnknown(t *testing.T) {
	useScratchReceipt(t)
	legacy := map[string]any{
		"version": 1, "state": "final", "recipe_id": "r-1", "attempt_id": "a-1",
		"started_at": "2026-10-24T12:00:00Z", "finished_at": "2026-10-24T12:01:00Z",
		"exit": 1, "validation": "not_performed", "runs_finished": 20, "findings": 0,
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastResultPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	lr := &lastRun{RecipeID: "r-1"}
	got, _ := outcomeSummary(lr)
	if strings.Contains(got, "UNKNOWN") || strings.Contains(got, "unknown") {
		t.Errorf("a legacy receipt with exit 1 must not read unknown: %q", got)
	}
	if !strings.Contains(got, "UNVERIFIED") {
		t.Errorf("exit 1 without fault findings is a contract failure (UNVERIFIED/INCOMPLETE): %q", got)
	}
	// Corrupt and foreign records DO read UNKNOWN.
	if err := os.WriteFile(lastResultPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := outcomeSummary(lr); !strings.Contains(got, "UNKNOWN") {
		t.Errorf("corrupt receipt must read UNKNOWN: %q", got)
	}
}

// The finalized receipt stores the word: a reader (menu, history)
// never has to re-derive it, and a mismatch attempt reads FAIL.
func TestReceiptStoresVerdict(t *testing.T) {
	useScratchReceipt(t)
	tr := newAttemptReceipt("r-1", 1, nil, nil)
	tr.finish(1, []plan.Result{{Exit: 1, Execution: plan.ExecRuntimeError, Validation: plan.ValPartial,
		Findings: []plan.Finding{{Kind: plan.FindDataMismatch, Source: "workload", Message: "mismatch",
			Attribution: plan.Attribution{Kind: plan.AttrLogicalCPU, Index: plan.IntPtr(4), Confidence: plan.ConfConfirmed}}}}})
	r, err := loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != string(plan.VerdictFail) || !strings.Contains(r.VerdictReason, "data mismatch") {
		t.Errorf("receipt verdict = %q %q, want FAIL with reason", r.Verdict, r.VerdictReason)
	}
	// The stored word is additive: an old reader still parses the file.
	var raw map[string]any
	if err := json.Unmarshal(mustRead(t), &raw); err != nil {
		t.Fatalf("receipt not parseable as generic JSON: %v", err)
	}
	if raw["verdict"] != "FAIL" {
		t.Errorf("raw receipt verdict = %v", raw["verdict"])
	}
}

func mustRead(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(lastResultPath)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
