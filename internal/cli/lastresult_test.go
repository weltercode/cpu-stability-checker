package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// useScratchReceipt points the receipt file at a private temp dir for
// one test and restores the package default afterwards.
func useScratchReceipt(t *testing.T) string {
	t.Helper()
	prev := lastResultPath
	lastResultPath = filepath.Join(t.TempDir(), "lastresult.json")
	t.Cleanup(func() { lastResultPath = prev })
	return lastResultPath
}

// captureStdout runs fn with os.Stdout redirected and returns what it
// printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The receipt aggregation must never promote an attempt above the
// worst run it actually contained, and never count a run that never
// reached a terminal state.
func TestReceiptAggregation(t *testing.T) {
	ok := plan.Result{Execution: plan.ExecCompleted, Validation: plan.ValVerified, Exit: 0}
	extOK := plan.Result{Execution: plan.ExecCompleted, Validation: plan.ValExternalConfirmed, Exit: 0}
	fault := plan.Result{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Exit: 1,
		AbortMsg: "mismatch on CPU 3", Findings: []plan.Finding{{}, {}}}
	cancelled := plan.Result{Execution: plan.ExecCancelled, Validation: plan.ValUnknown, Exit: 1,
		AbortMsg: "user cancel"}

	for _, tc := range []struct {
		name     string
		results  []plan.Result
		exit     int
		exec     string
		val      string
		finished int
		findings int
		abort    string
	}{
		{name: "all clean", results: []plan.Result{ok, ok}, exit: 0, exec: "completed", val: "verified", finished: 2, findings: 0},
		{name: "external confirmed stays below verified", results: []plan.Result{ok, extOK}, exit: 0, exec: "completed", val: "external_confirmed", finished: 2},
		{name: "one fault defines the attempt", results: []plan.Result{ok, fault}, exit: 1, exec: "runtime_error", val: "partial", finished: 2, findings: 2, abort: "mismatch on CPU 3"},
		{name: "unknown validation outranks partial", results: []plan.Result{fault, cancelled}, exit: 1, exec: "runtime_error", val: "unknown", finished: 2, findings: 2, abort: "mismatch on CPU 3"},
		{name: "no results stays unknown", results: nil, exit: 2, exec: "", val: "", finished: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := useScratchReceipt(t)
			tr := newAttemptReceipt("recipe-x", 5, nil, nil)
			if tr == nil {
				t.Fatal("bound recipe produced no receipt")
			}
			tr.finish(tc.exit, tc.results)
			r, err := loadLastResult()
			if err != nil {
				t.Fatal(err)
			}
			if r.State != receiptFinal || r.Execution != tc.exec || r.Validation != tc.val {
				t.Errorf("state/exec/val = %s/%s/%s, want final/%s/%s", r.State, r.Execution, r.Validation, tc.exec, tc.val)
			}
			if r.Exit == nil || *r.Exit != tc.exit {
				t.Errorf("exit = %v, want %d", r.Exit, tc.exit)
			}
			if r.RunsFinished != tc.finished || r.RunsPlanned != 5 {
				t.Errorf("runs = %d/%d, want %d/5", r.RunsFinished, r.RunsPlanned, tc.finished)
			}
			if r.Findings != tc.findings || r.AbortMsg != tc.abort {
				t.Errorf("findings=%d abort=%q, want %d/%q", r.Findings, r.AbortMsg, tc.findings, tc.abort)
			}
			if r.FinishedAt == "" || r.AttemptID == "" || r.StartedAt == "" {
				t.Errorf("receipt lost its stamps: %+v", r)
			}
			_ = path
		})
	}
}

// The pending state is written the moment the attempt is armed: a
// crash from here on leaves a truthful "started, never finalized".
func TestReceiptPendingThenFinal(t *testing.T) {
	useScratchReceipt(t)
	tr := newAttemptReceipt("recipe-p", 2, []string{"a.log"}, []string{"hash-a"})
	r, err := loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != receiptPending || r.Exit != nil || r.FinishedAt != "" {
		t.Fatalf("pending receipt looks final: %+v", r)
	}
	if r.RecipeID != "recipe-p" || len(r.Logs) != 1 || len(r.PlanHashes) != 1 {
		t.Fatalf("pending receipt lost its binding/artifacts: %+v", r)
	}
	tr.finish(0, []plan.Result{{Execution: plan.ExecCompleted, Validation: plan.ValVerified}})
	r, err = loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != receiptFinal || r.Exit == nil || *r.Exit != 0 {
		t.Fatalf("finish did not finalize: %+v", r)
	}
}

// An unbound recipe (save failed or predates C3) gets no receipt at
// all - a receipt that cannot say which request it belongs to would
// only mislead the repeat screen.
func TestReceiptSkippedForUnboundRecipe(t *testing.T) {
	path := useScratchReceipt(t)
	if tr := newAttemptReceipt("", 1, nil, nil); tr != nil {
		tr.finish(0, nil)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unbound recipe wrote a receipt: %v", err)
	}
}

// finishSetup is the honest label for an attempt that died before any
// run: setup_error, never a hardware verdict.
func TestReceiptFinishSetup(t *testing.T) {
	useScratchReceipt(t)
	tr := newAttemptReceipt("recipe-s", 0, nil, nil)
	tr.finishSetup(2)
	r, err := loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != receiptFinal || r.Execution != string(plan.ExecSetupError) || r.Exit == nil || *r.Exit != 2 {
		t.Fatalf("setup refusal receipt: %+v", r)
	}
}

func TestReceiptCorruptOrForeignReadsAsUnknown(t *testing.T) {
	path := useScratchReceipt(t)
	for _, tc := range []struct{ name, body string }{
		{"not json", "this is not json"},
		{"newer version", `{"version":99,"state":"final"}`},
		{"unknown state", `{"version":1,"state":"teleported"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLastResult(); err == nil {
				t.Fatal("unreadable receipt accepted")
			}
		})
	}
}

// The repeat screen's outcome line: every unknown path says unknown
// with its reason; a final receipt states what the attempt was.
func TestDescribeLastOutcomeStatesWhatItKnows(t *testing.T) {
	path := useScratchReceipt(t)
	lr := &lastRun{RecipeID: "recipe-d"}

	out := captureStdout(t, func() { describeLastOutcome(lr) })
	if !strings.Contains(out, "unknown") {
		t.Errorf("missing receipt printed %q, want unknown", out)
	}

	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() { describeLastOutcome(lr) })
	if !strings.Contains(out, "unknown") {
		t.Errorf("corrupt receipt printed %q, want unknown", out)
	}

	tr := newAttemptReceipt("other-recipe", 1, nil, nil)
	tr.finish(0, []plan.Result{{Execution: plan.ExecCompleted, Validation: plan.ValVerified}})
	out = captureStdout(t, func() { describeLastOutcome(lr) })
	if !strings.Contains(out, "different saved request") {
		t.Errorf("foreign receipt shown as this recipe's: %q", out)
	}

	tr = newAttemptReceipt("recipe-d", 3, nil, nil)
	out = captureStdout(t, func() { describeLastOutcome(lr) })
	if !strings.Contains(out, "never finalized") {
		t.Errorf("pending receipt printed %q, want never finalized", out)
	}

	tr.finish(1, []plan.Result{
		{Execution: plan.ExecCompleted, Validation: plan.ValVerified},
		{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Exit: 1, AbortMsg: "sensor lost"},
	})
	out = captureStdout(t, func() { describeLastOutcome(lr) })
	if !strings.Contains(out, "runtime_error / partial") || !strings.Contains(out, "exit 1") ||
		!strings.Contains(out, "runs 2/3") || !strings.Contains(out, "sensor lost") {
		t.Errorf("final receipt printed %q", out)
	}
}

// A real short run through runCmd: the recipe and the receipt bind by
// id, the receipt is final, and it names the run's actual artifacts.
func TestRunCmdWritesFinalBoundReceipt(t *testing.T) {
	chdirTemp(t)
	args := []string{"-test", "general", "-a", "int", "-duration", "100ms",
		"-cores", "0", "-control", "-1", "-log", "ok.log"}
	if code := runCmd(args); code != 0 {
		t.Fatalf("runCmd exit = %d, want 0", code)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.RecipeID == "" {
		t.Fatal("recipe lost its id")
	}
	r, err := loadLastResult()
	if err != nil {
		t.Fatalf("no readable receipt: %v", err)
	}
	if r.State != receiptFinal || r.RecipeID != lr.RecipeID {
		t.Errorf("receipt = %s bound to %q, want final bound to %q", r.State, r.RecipeID, lr.RecipeID)
	}
	if r.Exit == nil || *r.Exit != 0 || r.RunsFinished != 1 || r.RunsPlanned != 1 {
		t.Errorf("receipt outcome wrong: %+v", r)
	}
	if len(r.Logs) != 1 || r.Logs[0] != "ok.log" {
		t.Errorf("receipt logs = %v, want the run's own log", r.Logs)
	}
	if len(r.PlanHashes) != 1 || r.PlanHashes[0] == "" {
		t.Errorf("receipt lost the plan hash: %v", r.PlanHashes)
	}
}

// A receipt write failure is a loud warning, never a changed verdict:
// the run result and its exit codes stay exactly what they were.
func TestReceiptFailureNeverChangesTheVerdict(t *testing.T) {
	chdirTemp(t)
	prev := lastResultPath
	lastResultPath = filepath.Join(t.TempDir(), "no-such-dir", "lastresult.json")
	t.Cleanup(func() { lastResultPath = prev })

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
		"-cores", "0", "-control", "-1", "-log", "ok.log"})
	w.Close()
	os.Stderr = old
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("run exit = %d, want 0 despite the receipt failure", code)
	}
	if !strings.Contains(string(data), "outcome receipt not") {
		t.Errorf("receipt failure went silent:\n%s", data)
	}
}

// The scenario orchestration feeds the receipt every phase that ran
// (worst-state aggregation) and counts phases that never ran as
// unfinished, not as skipped silently.
func TestRunCyclesReceiptAggregatesPhases(t *testing.T) {
	useScratchReceipt(t)
	phase := func(name string) plan.Options {
		return plan.Options{TestName: "general", Phase: name, A: "int", LogFile: name + ".log", PlanHash: "h-" + name}
	}
	opts := []plan.Options{phase("a"), phase("b")}
	lr := lastRun{Kind: lastRunScenario, RecipeID: "recipe-cy"}
	tr := newScenarioReceipt(lr, opts, 2)

	execute := func(opt plan.Options) plan.Result {
		if opt.Phase == "b" {
			return plan.Result{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial,
				Exit: 1, AbortMsg: "phase b stopped", Findings: []plan.Finding{{}}}
		}
		return plan.Result{Execution: plan.ExecCompleted, Validation: plan.ValVerified}
	}
	if code := runCycles(opts, 2, -1, io.Discard, execute, false, tr, nil); code != 1 {
		t.Fatalf("runCycles exit = %d, want 1", code)
	}
	r, err := loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != receiptFinal || r.Execution != "runtime_error" || r.Validation != "partial" {
		t.Errorf("aggregated receipt: %+v", r)
	}
	if r.RunsPlanned != 4 || r.RunsFinished != 2 {
		t.Errorf("runs = %d/%d, want finished 2 of planned 4 (the fault stopped the scenario before cycle 2)", r.RunsFinished, r.RunsPlanned)
	}
	if r.Findings != 1 || r.AbortMsg != "phase b stopped" {
		t.Errorf("findings/abort = %d/%q", r.Findings, r.AbortMsg)
	}
	if len(r.Logs) != 2 || len(r.PlanHashes) != 2 {
		t.Errorf("receipt artifacts = logs %v hashes %v", r.Logs, r.PlanHashes)
	}
}

// A session attempt finalizes its receipt with the per-run logs and
// hashes it actually ran (fake quick workload, no real load).
func TestRunSessionFinalizesReceipt(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})

	workload.Register(&workload.Workload{Name: "receipt-session-fake",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("receipt-session-fake")

	control := -1
	session := SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
		Workload: "receipt-session-fake", ThreadsPerCore: 1, Duration: 50 * time.Millisecond,
		Passes: 1, ControlCore: &control, LogFile: filepath.Join(t.TempDir(), "s.log")}
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}},
	}}
	if code := runSessionWithPanel(session, topology, nil, nil, "recipe-sess"); code != 0 {
		t.Fatalf("session exit = %d", code)
	}
	r, err := loadLastResult()
	if err != nil {
		t.Fatal(err)
	}
	if r.State != receiptFinal || r.RecipeID != "recipe-sess" || r.RunsFinished != 1 {
		t.Errorf("session receipt: %+v", r)
	}
	if len(r.Logs) != 1 || !strings.Contains(r.Logs[0], "s-") {
		t.Errorf("session receipt logs = %v", r.Logs)
	}
}

// A corrupt or half-written receipt file must never parse as an
// outcome: the atomic discipline is what makes "pending" trustworthy.
func TestReceiptAtomicReplaceLeavesNoTemp(t *testing.T) {
	path := useScratchReceipt(t)
	tr := newAttemptReceipt("recipe-a", 1, nil, nil)
	tr.finish(0, nil)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp receipt left behind: %s", e.Name())
		}
	}
	var raw lastResult
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &raw); err != nil || raw.State != receiptFinal {
		t.Errorf("receipt file not final JSON: %v %+v", err, raw)
	}
}
