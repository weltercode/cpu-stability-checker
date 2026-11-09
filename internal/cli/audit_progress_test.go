package cli

// Audit C03 probes, kept as permanent regressions (R04b): ONE
// transaction outcome must yield ONE exit code regardless of output
// format. The JSON branch used to return only the encoder's result,
// so restored_failed/recovery_required exited 1 in text but 0 in
// JSON - a script or future GUI trusting exit codes would have seen
// success while the hardware was left modified. The table covers the
// audit's full list: every terminal and non-terminal status, a
// trailing error after a clean restore, a missing result, and an
// unavailable JSON output writer.

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
)

func TestAuditCOJSONFailureExit(t *testing.T) {
	cases := []struct {
		name   string
		status string
		err    error
		want   int
	}{
		// terminal-clean, nothing else wrong: 0 in BOTH formats.
		{"restored", cotx.StatusRestored, nil, 0},
		{"closed_no_change", cotx.StatusClosedNoChange, nil, 0},
		// clean durable status but a trailing step errored: never hidden.
		{"restored with trailing error", cotx.StatusRestored, &co.Error{Kind: co.KindNotValidated, Op: "verify"}, 1},
		{"closed_no_change with trailing error", cotx.StatusClosedNoChange, &co.Error{Kind: co.KindTimeout, Op: "readback"}, 1},
		// every other status means the hardware may be left modified.
		{"restored_failed", cotx.StatusRestoredFailed, &co.Error{Kind: co.KindRecoveryRequired}, 1},
		{"applied_verified", cotx.StatusAppliedVerified, nil, 1},
		{"apply_pending", cotx.StatusApplyPending, nil, 1},
		{"restore_pending", cotx.StatusRestorePending, nil, 1},
		{"unknown status is never assumed clean", "made_up", nil, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mk := func() *cotx.Result {
				return &cotx.Result{TxID: "audit-fake", Status: tc.status}
			}
			human := coTxResult("recover", mk(), tc.err, false)
			machine, body := captureAnswer(t, func() int {
				return coTxResult("recover", mk(), tc.err, true)
			})
			if human != tc.want || machine != tc.want {
				t.Fatalf("same outcome, different verdicts: text exit=%d, JSON exit=%d, want %d (body: %s)", human, machine, tc.want, body)
			}
			// The JSON body must parse and carry the SAME status,
			// plus the trailing error fact when present.
			var rep struct {
				Status string `json:"status"`
				Kind   string `json:"kind,omitempty"`
				Error  string `json:"error,omitempty"`
			}
			if err := json.Unmarshal([]byte(body), &rep); err != nil {
				t.Fatalf("JSON report unparseable: %v (%q)", err, body)
			}
			if rep.Status != tc.status {
				t.Errorf("JSON status %q != outcome status %q", rep.Status, tc.status)
			}
			if tc.err != nil && rep.Error == "" {
				t.Errorf("trailing error missing from the JSON report: %v", rep)
			}
		})
	}

	// Missing result: nothing reached the hardware; exit 2 in both
	// formats, and the JSON form carries kind+error as an object.
	t.Run("no result is a setup failure in both formats", func(t *testing.T) {
		human := coTxResult("recover", nil, &co.Error{Kind: co.KindNoPrivilege, Op: "probe"}, false)
		machine, body := captureAnswer(t, func() int {
			return coTxResult("recover", nil, &co.Error{Kind: co.KindNoPrivilege, Op: "probe"}, true)
		})
		if human != 2 || machine != 2 {
			t.Fatalf("no-result exit: text=%d JSON=%d, want 2/2 (body %q)", human, machine, body)
		}
		var rep struct {
			Op    string `json:"op"`
			Kind  string `json:"kind"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &rep); err != nil {
			t.Fatalf("no-result JSON unparseable: %v (%q)", err, body)
		}
		if rep.Op != "recover" || rep.Kind != co.KindNoPrivilege || rep.Error == "" {
			t.Errorf("no-result JSON verdict wrong: %+v", rep)
		}
	})

	// An output writer that fails mid-report is never success, even
	// for a clean status the consumer never got to see.
	t.Run("unavailable JSON writer is never success", func(t *testing.T) {
		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		_ = w.Close() // every write now fails
		_ = r.Close()
		code := coTxResult("recover", &cotx.Result{TxID: "x", Status: cotx.StatusRestored}, nil, true)
		os.Stdout = old
		if code == 0 {
			t.Fatal("a JSON report the consumer never received exited 0")
		}
	})
}

// captureAnswer runs fn with os.Stdout redirected, returning its exit
// code and everything it printed. The info-style commands (catalog,
// history, caps, version) answer on stdout in both shapes, so this is
// how their tests read an answer.
func captureAnswer(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := fn()
	_ = w.Close()
	os.Stdout = old
	body := <-done
	if err := r.Close(); err != nil && !strings.Contains(err.Error(), "use of closed") {
		t.Logf("pipe close: %v", err)
	}
	return code, body
}
