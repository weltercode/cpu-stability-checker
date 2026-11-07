package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Both dispatch branches must be wired: an invalid plan must come back
// from either engine as a setup error (exit 2) without loading the
// machine. This is the contract every command relies on now that the
// mode choice lives in one place.
func TestExecuteOptDispatch(t *testing.T) {
	cases := []struct {
		name       string
		perProcess bool
	}{
		{"in-process", false},
		{"per-process", true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			res := executeOpt(plan.Options{TestName: "no-such-test", PerProcess: tt.perProcess})
			if res.Exit != 2 {
				t.Errorf("executeOpt(%s): exit = %d, want 2 (setup error, no load started)", tt.name, res.Exit)
			}
		})
	}
}

// A single-instance workload (Prime95's GUI, y-cruncher) must never run
// per-process: every worker would start its own copy of a tool that
// already spans the whole machine — the user's crash was 16 Prime95
// instances. The dispatch point downgrades to in-process and says so.
func TestExecuteOptSingleInstanceDowngrade(t *testing.T) {
	workload.Register(&workload.Workload{
		Name:           "p95fake",
		Description:    "fake single-instance tool",
		Run:            nil,
		SingleInstance: true,
	})
	defer workload.Delete("p95fake")

	cases := []struct {
		name       string
		a          string
		perProcess bool
		want       []string // must appear in stdout
		never      []string // must not appear
	}{
		{
			name: "single-instance forces in-process", a: "p95fake", perProcess: true,
			want: []string{"single-instance", "in-process"},
		},
		{
			name: "ordinary workload stays per-process", a: "int", perProcess: true,
			never: []string{"single-instance"},
		},
		{
			name: "single-instance already in-process says nothing", a: "p95fake", perProcess: false,
			never: []string{"single-instance"},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			old := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w
			res := executeOpt(plan.Options{TestName: "no-such-test", A: tt.a, PerProcess: tt.perProcess})
			w.Close()
			os.Stdout = old
			gotBytes, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read captured output: %v", err)
			}
			got := string(gotBytes)
			if res.Exit != 2 {
				t.Errorf("executeOpt: exit = %d, want 2 (setup error, no load started)", res.Exit)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("executeOpt output %q, want to contain %q", got, want)
				}
			}
			for _, bad := range tt.never {
				if strings.Contains(got, bad) {
					t.Errorf("executeOpt output %q, must not contain %q", got, bad)
				}
			}
		})
	}
}

// The menu's session hands one panel to every run so hardware
// monitoring never pauses between runs or after the last one until the
// menu takes over. Engines paint into a caller-owned panel but must
// never Stop it - only the owner does, when the menu screen is ready.
func TestExecuteOptWithPanelOwnership(t *testing.T) {
	panel := dashboard.New(os.Stdout) // test stdout is a pipe: inactive panel
	res := executeOptWithPanel(plan.Options{TestName: "no-such-test", PerProcess: true}, panel)
	if res.Exit != 2 {
		t.Errorf("exit = %d, want 2 (setup error, no load started)", res.Exit)
	}
	if panel.Stopped() {
		t.Error("engine stopped the caller's panel; only the session owner may")
	}
	panel.Stop() // the owner's Stop must still work cleanly
}
