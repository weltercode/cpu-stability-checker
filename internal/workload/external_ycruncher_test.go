package workload

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// ycSpec builds an external spec that re-runs this test binary as a
// fake y-cruncher speaking the v0.8 stress protocol. The startup
// options sit AFTER the test flags on purpose: the test binary parses
// its flags first, and the adapter's launchArgs must then prepend
// nothing (the options are already present).
func ycSpec(env map[string]string) ExternalSpec {
	if env == nil {
		env = map[string]string{}
	}
	env["GO_WANT_HELPER_PROCESS"] = "1"
	env["FAKE_YC"] = "1"
	return ExternalSpec{
		Command: os.Args[0],
		Adapter: "ycruncher-v0.8",
		Args:    []string{"-test.run=TestHelperProcess", "skip-warnings", "pause:-2", "colors:0", "status:none"},
		Env:     env,
	}
}

// The adapter's reducer is pure line parsing: the whole protocol state
// machine - start, progress, completion, fault, exception, version
// cross-check - is testable without spawning anything.
func TestYCruncherAdapterReducer(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		waitErr   error
		ourStop   bool
		wantErr   string // substring of the returned error; "" = want nil
		wantSetup bool
		wantEv    string
	}{
		{
			name: "full clean protocol confirms completion",
			lines: []string{
				"y-cruncher v0.8.7 Build 9547-vsc",
				"Component Stress Tester",
				"Allocating Memory...",
				"Iteration: 0  Total Elapsed Time: 0.002 seconds  ( 0.000 minutes )",
				"Running BKT: Passed  Test Speed:  2.31 * 10^10  bits / sec",
				"Test Finished. Waiting for threads to terminate...",
			},
			wantEv: EvConfirmed,
		},
		{
			name: "self-stop on error is a fault even with exit 0",
			lines: []string{
				"y-cruncher v0.8.7 Build 9547-vsc",
				"Allocating Memory...",
				"Running BKT: Failed  Test Speed:  2.3 * 10^10  bits / sec",
				"Error(s) encountered on logical core 5",
				"Errors encountered. Stopping test...",
			},
			wantErr: "reported a fault",
			wantEv:  EvFailed,
		},
		{
			name: "version banner mismatch downgrades the claim",
			lines: []string{
				"y-cruncher v0.7.9 Build 1",
				"Allocating Memory...",
				"Running BKT: Passed",
				"Test Finished. Waiting for threads to terminate...",
			},
			wantEv: EvPartial,
		},
		{
			name: "exception is a tool error, not a hardware verdict",
			lines: []string{
				"y-cruncher v0.8.7 Build 9547-vsc",
				"Allocating Memory...",
				"Exception Encountered: access violation reading location 0x0",
			},
			wantSetup: true,
			wantEv:    "",
		},
		{
			name: "our deadline leaves a started run partial",
			lines: []string{
				"y-cruncher v0.8.7 Build 9547-vsc",
				"Allocating Memory...",
				"Running BKT: Passed",
			},
			waitErr: errors.New("signal: killed"),
			ourStop: true,
			wantEv:  EvPartial,
		},
		{
			name:   "fake tool exits 0 without ever starting stress: unknown",
			lines:  []string{"y-cruncher v0.8.7 Build 9547-vsc"},
			wantEv: "",
		},
		{
			name: "completion banner but crashed before a clean exit: not confirmed",
			lines: []string{
				"y-cruncher v0.8.7 Build 9547-vsc",
				"Allocating Memory...",
				"Test Finished. Waiting for threads to terminate...",
			},
			waitErr:   errors.New("signal: killed"),
			wantSetup: true,
			wantEv:    EvPartial,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newYCruncherAdapter(ExternalSpec{Command: "y-cruncher.exe"})
			if err != nil {
				t.Fatalf("newYCruncherAdapter() = %v", err)
			}
			ya := a.(*yCruncherAdapter)
			for _, l := range tt.lines {
				act := ya.line(l)
				if act.fault != "" && tt.wantErr == "" {
					t.Errorf("unexpected fault action on %q: %s", l, act.fault)
				}
			}
			gotErr := ya.exit(tt.waitErr, tt.ourStop)
			switch {
			case tt.wantErr != "":
				if gotErr == nil || !strings.Contains(gotErr.Error(), tt.wantErr) {
					t.Errorf("exit() error = %v, want it to contain %q", gotErr, tt.wantErr)
				}
			case tt.wantSetup:
				if !IsSetupError(gotErr) {
					t.Errorf("exit() error = %v, want a setup error", gotErr)
				}
			default:
				if gotErr != nil {
					t.Errorf("exit() error = %v, want nil", gotErr)
				}
			}
			if ev := ya.evidence(); ev != tt.wantEv {
				t.Errorf("evidence = %q, want %q", ev, tt.wantEv)
			}
		})
	}
}

// The startup options the tool needs must be added before the action,
// and never conflict with what the config already chose.
func TestYCruncherLaunchArgs(t *testing.T) {
	a, err := newYCruncherAdapter(ExternalSpec{Command: "y-cruncher.exe", Args: []string{"config", "stress.json"}})
	if err != nil {
		t.Fatalf("newYCruncherAdapter() = %v", err)
	}
	got := strings.Join(a.launchArgs(), " ")
	want := "skip-warnings pause:-2 colors:0 status:none config stress.json"
	if got != want {
		t.Errorf("launchArgs() = %q, want %q", got, want)
	}

	// A config that set pause:-1 itself keeps it: we do not send the
	// tool two conflicting pause values.
	a2, err := newYCruncherAdapter(ExternalSpec{Command: "x", Args: []string{"skip-warnings", "pause:-1", "config", "c.json"}})
	if err != nil {
		t.Fatalf("newYCruncherAdapter() = %v", err)
	}
	got2 := strings.Join(a2.launchArgs(), " ")
	if strings.Contains(got2, "pause:-2") {
		t.Errorf("launchArgs() = %q, must not override the configured pause:-1", got2)
	}
	if n := strings.Count(got2, "skip-warnings"); n != 1 {
		t.Errorf("launchArgs() = %q, skip-warnings duplicated", got2)
	}
}

// End to end through the real plumbing: a fake tool speaking the
// measured protocol gets an honest confirmed-complete evidence.
func TestYCruncherRunConfirmed(t *testing.T) {
	rep := &recordingReporter{}
	if _, err := ycSpec(nil).run(context.Background(), rep); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvConfirmed {
		t.Errorf("evidence = %v, want [%s]", ev, EvConfirmed)
	}
	if adds, _ := rep.snapshot(); adds != 0 {
		t.Errorf("adds = %d, protocol lines are never iterations", adds)
	}
}

// y-cruncher stops itself on a hardware error and still exits 0: the
// adapter must fail the run on the fault lines, not the exit code.
func TestYCruncherRunFaultSelfStop(t *testing.T) {
	rep := &recordingReporter{}
	_, err := ycSpec(map[string]string{"FAKE_YC_FAIL": "1"}).run(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "y-cruncher reported a fault") {
		t.Fatalf("run() error = %v, want the fault from the protocol lines", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvFailed {
		t.Errorf("evidence = %v, want [%s]", ev, EvFailed)
	}
}

// A tool whose banner contradicts v0.8 may not be claimed complete on
// this protocol: the claim drops to partial even with a full run.
func TestYCruncherRunVersionMismatch(t *testing.T) {
	rep := &recordingReporter{}
	if _, err := ycSpec(map[string]string{"FAKE_YC_BANNER": "y-cruncher v0.7.9 Build 1"}).run(context.Background(), rep); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvPartial {
		t.Errorf("evidence = %v, want [%s] for an unverified protocol version", ev, EvPartial)
	}
}

// Our deadline hitting mid-test is not the tool's fault: the run ends
// clean with partial evidence (real start, no confirmed completion).
func TestYCruncherRunTimeoutPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	rep := &recordingReporter{}
	if _, err := ycSpec(map[string]string{"FAKE_YC_HANG": "1"}).run(ctx, rep); err != nil {
		t.Fatalf("run() error = %v, want nil: our deadline is not the tool's fault", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvPartial {
		t.Errorf("evidence = %v, want [%s]", ev, EvPartial)
	}
}

// An exception inside y-cruncher is a tool error (setup), never a
// hardware verdict and never a confirmed run.
func TestYCruncherRunExceptionIsSetupError(t *testing.T) {
	_, err := ycSpec(map[string]string{"FAKE_YC_EXC": "1"}).run(context.Background(), &recordingReporter{})
	if !IsSetupError(err) {
		t.Fatalf("run() error = %v, want a setup error", err)
	}
}

// The completion banner is the one protocol line that also arms the
// plumbing's teardown grace; no other line may. (The defect this ties
// to: measured 2026-10-24, the mode binaries linger in their own
// "Waiting for threads to terminate..." for minutes after the banner.)
func TestYCruncherCompletionBannerArmsTeardown(t *testing.T) {
	a, err := newYCruncherAdapter(ExternalSpec{Command: "y-cruncher.exe"})
	if err != nil {
		t.Fatalf("newYCruncherAdapter() = %v", err)
	}
	ya := a.(*yCruncherAdapter)
	for _, l := range []string{
		"y-cruncher v0.8.7 Build 9547-vsc",
		"Component Stress Tester",
		"Iteration: 0  Total Elapsed Time: 0.002 seconds  ( 0.000 minutes )",
		"Running BKT: Passed  Test Speed:  2.31 * 10^10  bits / sec",
	} {
		if act := ya.line(l); act.done {
			t.Errorf("line %q must not arm the teardown grace", l)
		}
	}
	if act := ya.line("Test Finished. Waiting for threads to terminate..."); !act.done {
		t.Errorf("the completion banner must arm the teardown grace, action = %+v", act)
	}
}

// The defect fix, end to end: a tool that declared its protocol
// complete but then stalls in its own teardown must not hold the run
// hostage. The plumbing ends the tree after the grace, our terminate
// is not counted against the tool (same rule as our own deadline),
// and the earned completion claim stands. The 10 s fake stall vs the
// 300 ms grace is the whole point: the run returns at the grace, not
// at the stall.
func TestYCruncherStalledTeardownEndsAtGrace(t *testing.T) {
	old := externalTeardownGrace
	externalTeardownGrace = 300 * time.Millisecond
	defer func() { externalTeardownGrace = old }()

	start := time.Now()
	rep := &recordingReporter{}
	_, err := ycSpec(map[string]string{"FAKE_YC_STALL_MS": "10000"}).run(context.Background(), rep)
	el := time.Since(start)
	if err != nil {
		t.Fatalf("run() error = %v, want nil: our post-banner terminate says nothing about the tool", err)
	}
	if el > 5*time.Second {
		t.Errorf("run() took %v, want the %v grace to end the stalled teardown, not the fake's 10 s sleep", el, externalTeardownGrace)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvConfirmed {
		t.Errorf("evidence = %v, want [%s]: the claim was protocol history before we ended the teardown", ev, EvConfirmed)
	}
}

// The grace only exists after a self-declared completion: a tool that
// hangs mid-test (no banner) is still ended by our deadline, and the
// evidence honestly stays partial.
func TestYCruncherHangBeforeBannerIgnoresGrace(t *testing.T) {
	old := externalTeardownGrace
	externalTeardownGrace = 300 * time.Millisecond
	defer func() { externalTeardownGrace = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	rep := &recordingReporter{}
	if _, err := ycSpec(map[string]string{"FAKE_YC_HANG": "1"}).run(ctx, rep); err != nil {
		t.Fatalf("run() error = %v, want nil: our deadline is not the tool's fault", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvPartial {
		t.Errorf("evidence = %v, want [%s]: no completion was ever declared", ev, EvPartial)
	}
}
