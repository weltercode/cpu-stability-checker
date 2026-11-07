package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The menu's stop note must name what the attempt receipt actually
// recorded - the old "fault or user stop" wording made the user
// guess whether they had pressed Esc or the machine reported
// something, and on a setup-error exit the tab closed with the
// report unread so the whole story was lost ("the program just
// fell over").

func writeFinalReceipt(t *testing.T, attempt, exec, abort string, findings int) {
	t.Helper()
	exit := 1
	r := lastResult{
		Version: lastResultVersion, State: receiptFinal,
		AttemptID: attempt, Execution: exec, AbortMsg: abort,
		Findings: findings, Exit: &exit,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastResultPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopNoteNamesTheActualEnding(t *testing.T) {
	chdirTemp(t)
	path := useScratchReceipt(t)

	if runStopNote(0) != "" {
		t.Error("a clean run must get no stop note")
	}
	if n := runStopNote(1); !strings.Contains(n, "unreadable") {
		t.Errorf("missing receipt note = %q, must say the receipt is unreadable", n)
	}

	for _, tc := range []struct {
		name, exec, abort string
		findings          int
		want              []string
	}{
		{"cancel with reason", "cancelled", "user interrupt", 0, []string{"INCOMPLETE", "operator", "user interrupt"}},
		{"cancel without reason", "interrupted", "", 0, []string{"INCOMPLETE", "collateral"}},
		{"setup failure", "setup_error", "", 30, []string{"INCOMPLETE", "setup/tool problem", "30 finding(s) recorded"}},
		{"setup with reason", "setup_error", "unknown workload", 1, []string{"INCOMPLETE", "setup/tool problem", "unknown workload"}},
		{"run error", "runtime_error", "thermal limit reached", 0, []string{"INCOMPLETE", "thermal limit reached"}},
		{"fault findings", "completed", "", 3, []string{"INCOMPLETE", "3 finding(s) recorded"}},
		{"unverified finish", "completed", "", 0, []string{"INCOMPLETE"}},
	} {
		writeFinalReceipt(t, "att-"+tc.name, tc.exec, tc.abort, tc.findings)
		n := runStopNote(1)
		for _, w := range tc.want {
			if !strings.Contains(n, w) {
				t.Errorf("%s: note %q lacks %q", tc.name, n, w)
			}
		}
		if strings.Contains(n, "fault or user stop") {
			t.Errorf("%s: note still guesses: %q", tc.name, n)
		}
	}

	// A pending receipt (the run never finalized its attempt) must
	// say so, not dress up as a final ending.
	p := lastResult{Version: lastResultVersion, State: receiptPending, AttemptID: "att-pending"}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := runStopNote(1); !strings.Contains(n, "never reported an ending") {
		t.Errorf("pending receipt note = %q", n)
	}
}

// A stop exit must state the reason AND hold the screen: exiting
// straight away closed the terminal tab with the report unread.
// EOF input stands in for the scripted case (no Enter needed); the
// point proven here is the note on stderr and the code passthrough.
func TestMenuStopExitStatesReasonAndWaits(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	writeFinalReceipt(t, "att-1", "setup_error", "", 30)

	var got int
	out := captureStderr(t, func() {
		got = menuStopExit(bufio.NewReader(strings.NewReader("")), nil, 2, "")
	})
	if got != 2 {
		t.Errorf("menuStopExit code = %d, want 2", got)
	}
	if !strings.Contains(out, "setup/tool problem") || !strings.Contains(out, "30 finding(s) recorded") {
		t.Errorf("stop note missing from the exit output:\n%s", out)
	}

	// A refusal that never armed an attempt must not borrow the
	// previous run's receipt: the attempt id captured before the
	// run is still the one on disk.
	before := lastAttemptID()
	out = captureStderr(t, func() {
		got = menuStopExit(bufio.NewReader(strings.NewReader("")), nil, 2, before)
	})
	if !strings.Contains(out, "before an attempt was armed") {
		t.Errorf("borrowed receipt not called out:\n%s", out)
	}
	if strings.Contains(out, "30 finding(s)") {
		t.Errorf("the old run's story was reused:\n%s", out)
	}
}

// writeVerdictReceipt writes a final, exit-0 receipt carrying the P13
// verdict vocabulary directly - the shape a clean run leaves on disk.
func writeVerdictReceipt(t *testing.T, attempt, verdict, reason string, finished, planned, findings int) {
	t.Helper()
	exit := 0
	r := lastResult{
		Version: lastResultVersion, State: receiptFinal, AttemptID: attempt,
		Verdict: verdict, VerdictReason: reason, Exit: &exit,
		RunsFinished: finished, RunsPlanned: planned, Findings: findings,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastResultPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A clean (exit 0) ending must not bounce back to the menu with no
// closing word: the owner watched a finished preset profile redraw the
// menu instantly and asked whether the run had failed - the panel's
// RESULT lines were buried by the next frame. The note states only
// what the receipt recorded, and the screen is held for reading.
func TestRunFinishNoteNamesTheCleanEnding(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)

	// No readable receipt: nothing is provable - silence (a cancelled
	// dialog also exits 0, and inventing an ending here would guess).
	if n, _ := runFinishNote("att-none"); n != "" {
		t.Errorf("unreadable receipt note = %q, want silence", n)
	}

	// Receipt provably still the pre-run one: this exit 0 armed no
	// attempt (cancel at the confirm), stay silent.
	writeVerdictReceipt(t, "att-old", "PASS", "all required load completed and verified: 42 confirmed checks across 2 unit(s)", 2, 2, 0)
	if n, _ := runFinishNote("att-old"); n != "" {
		t.Errorf("unchanged-attempt note = %q, want silence for a cancelled dialog", n)
	}

	n, color := runFinishNote("att-before")
	if !strings.Contains(n, "RESULT: PASS") || !strings.Contains(n, "42 confirmed checks") || !strings.Contains(n, "2/2 run(s)") {
		t.Errorf("PASS note = %q", n)
	}
	if color != "" {
		t.Errorf("PASS color = %q, want neutral", color)
	}

	writeVerdictReceipt(t, "att-unver", "UNVERIFIED", "external tool without confirmed protocol evidence", 1, 1, 0)
	n, color = runFinishNote("att-before")
	if !strings.Contains(n, "RESULT: UNVERIFIED") || !strings.Contains(n, "did not confirm themselves") {
		t.Errorf("UNVERIFIED note = %q", n)
	}
	if color == "" {
		t.Error("UNVERIFIED must carry a warning color")
	}

	// A receipt that records FAIL under a clean exit must never read
	// as a success: the receipt wins over the exit code.
	writeVerdictReceipt(t, "att-fail", "FAIL", "1 confirmed data mismatch on core 3", 1, 1, 1)
	n, color = runFinishNote("att-before")
	if !strings.Contains(n, "RESULT: FAIL") || !strings.Contains(n, "1 finding(s)") {
		t.Errorf("FAIL note = %q", n)
	}
	if color == "" {
		t.Error("FAIL must carry the fault color")
	}

	// A pending receipt that this run armed: say it never ended.
	p := lastResult{Version: lastResultVersion, State: receiptPending, AttemptID: "att-pending"}
	b, _ := json.Marshal(p)
	if err := os.WriteFile(lastResultPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := runFinishNote("att-before"); !strings.Contains(n, "never reported an ending") {
		t.Errorf("pending receipt note = %q", n)
	}
}

// The wiring: menuRunOutcome must print the note AND hold the screen
// for a clean run, and must not touch the screen when the exit 0 came
// from a cancelled dialog.
func TestMenuRunOutcomeHoldsScreenForCleanEnding(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	writeVerdictReceipt(t, "att-new", "PASS", "all required load completed and verified: 42 confirmed checks across 2 unit(s)", 2, 2, 0)

	worst := 0
	out := captureStdout(t, func() {
		stop, exit := menuRunOutcome(bufio.NewReader(strings.NewReader("\n")), nil, &worst, 0, "att-before")
		if stop || exit != 0 {
			t.Errorf("menuRunOutcome(clean) = stop %v exit %d, want stay-in-menu", stop, exit)
		}
	})
	if !strings.Contains(out, "RESULT: PASS") || !strings.Contains(out, "Press Enter") {
		t.Errorf("clean ending was not stated or not held:\n%s", out)
	}
	if worst != 0 {
		t.Errorf("worst = %d, a clean run must not count as a stop", worst)
	}

	// Cancelled dialog (receipt unchanged): no note, no pause.
	out = captureStdout(t, func() {
		stop, exit := menuRunOutcome(bufio.NewReader(strings.NewReader("")), nil, &worst, 0, "att-new")
		if stop || exit != 0 {
			t.Errorf("menuRunOutcome(cancel) = stop %v exit %d, want stay-in-menu", stop, exit)
		}
	})
	if out != "" {
		t.Errorf("cancelled dialog printed %q, want silence", out)
	}
}
