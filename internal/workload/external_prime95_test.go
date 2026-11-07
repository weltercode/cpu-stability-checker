package workload

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// p95Spec builds an external spec that re-runs this test binary as a
// fake Prime95 writing the measured results.txt protocol into its own
// working directory. "-t" sits after the test flag so the flags still
// parse; the adapter must then leave the args alone.
func p95Spec(t *testing.T, env map[string]string) ExternalSpec {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	env["GO_WANT_HELPER_PROCESS"] = "1"
	env["FAKE_P95"] = "1"
	return ExternalSpec{
		Command: os.Args[0],
		Adapter: "prime95-v30.19",
		// "--" stops the test binary's flag parser before "-t": the
		// fake tool must receive the torture flag without the test
		// harness rejecting it as an unknown flag of its own.
		Args:      []string{"-test.run=TestHelperProcess", "--", "-t"},
		Dir:       t.TempDir(),
		FaultFile: "results.txt",
		Env:       env,
	}
}

// The reducer is pure line parsing over the results-file protocol:
// start, progress, hardware failure, tool error, and the honest
// ceiling (no completion format exists for the torture test).
func TestP95AdapterReducer(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		waitErr   error
		ourStop   bool
		wantFault bool   // a fileLine must raise the fault action
		wantErr   string // substring of the exit error; "" = want nil
		wantSetup bool
		wantEv    string
	}{
		{
			name: "progress lines mean a real started run",
			lines: []string{
				"[Fri Oct 16 03:43:21 2026]",
				"Self-test 8K (thread 1 of 2) passed!",
				"Self-test 8K (thread 2 of 2) passed!",
			},
			waitErr: errors.New("signal: killed"),
			ourStop: true,
			wantEv:  EvPartial,
		},
		{
			name: "hardware failure line fails the run even with exit 0",
			lines: []string{
				"[Fri Oct 16 03:43:21 2026]",
				"Self-test 8K (thread 1 of 2) passed!",
				"Hardware failure detected running 8K FFT size, consult stress.txt file.",
			},
			wantFault: true,
			waitErr:   nil,
			wantErr:   "hardware failure",
			wantEv:    EvFailed,
		},
		{
			name:      "self-test failure is a fault too",
			lines:     []string{"Self-test 10752K (thread 1 of 2) failed!"},
			wantFault: true,
			wantErr:   "hardware failure",
			wantEv:    EvFailed,
		},
		{
			name:      "tool error is a setup error, not a hardware verdict",
			lines:     []string{"Error: unable to open scratch file"},
			waitErr:   nil,
			wantSetup: true,
			wantEv:    "",
		},
		{
			name:    "clean early exit after progress: partial, never confirmed",
			lines:   []string{"Self-test 8K (thread 1 of 2) passed!"},
			waitErr: nil,
			wantEv:  EvPartial,
		},
		{
			name:    "fake tool exits 0 without ever writing findings: unknown",
			lines:   nil,
			waitErr: nil,
			wantEv:  "",
		},
		{
			name:      "crash mid-run is a setup error, evidence stays partial",
			lines:     []string{"Self-test 8K (thread 1 of 2) passed!"},
			waitErr:   errors.New("signal: aborted"),
			wantSetup: true,
			wantEv:    EvPartial,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newP95Adapter(ExternalSpec{Command: "prime95.exe", FaultFile: "results.txt"})
			if err != nil {
				t.Fatalf("newP95Adapter() = %v", err)
			}
			pa := a.(*p95Adapter)
			for _, l := range tt.lines {
				act := pa.fileLine(l)
				if act.fault != "" && !tt.wantFault {
					t.Errorf("unexpected fault action on %q: %s", l, act.fault)
				}
			}
			gotErr := pa.exit(tt.waitErr, tt.ourStop)
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
			if ev := pa.evidence(); ev != tt.wantEv {
				t.Errorf("evidence = %q, want %q", ev, tt.wantEv)
			}
		})
	}
}

// The torture-test flag must be present: prime95.exe without -t just
// opens a window, and that must not be mistaken for a running test.
func TestP95LaunchArgs(t *testing.T) {
	a, err := newP95Adapter(ExternalSpec{Command: "prime95.exe", Args: []string{"-m"}})
	if err != nil {
		t.Fatalf("newP95Adapter() = %v", err)
	}
	got := strings.Join(a.launchArgs(), " ")
	if !strings.HasPrefix(got, "-t ") {
		t.Errorf("launchArgs() = %q, want -t added first", got)
	}
	a2, err := newP95Adapter(ExternalSpec{Command: "x", Args: []string{"-t"}})
	if err != nil {
		t.Fatalf("newP95Adapter() = %v", err)
	}
	if got2 := strings.Join(a2.launchArgs(), " "); got2 != "-t" {
		t.Errorf("launchArgs() = %q, want -t not duplicated", got2)
	}
}

// End to end: a fake tool writing the measured protocol gets partial
// evidence at best - the honest ceiling for a torture test that never
// self-completes - even when it exits cleanly.
func TestP95RunProgressPartialNeverConfirmed(t *testing.T) {
	rep := &recordingReporter{}
	if _, err := p95Spec(t, nil).run(context.Background(), rep); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvPartial {
		t.Errorf("evidence = %v, want [%s]: a torture run that never self-completes cannot claim more", ev, EvPartial)
	}
	if adds, _ := rep.snapshot(); adds != 0 {
		t.Errorf("adds = %d, protocol lines are never iterations", adds)
	}
}

// The hardware-failure line fails the run even though Prime95 keeps
// its process alive with a dialog: the file line is the truth.
func TestP95RunHardwareFailure(t *testing.T) {
	rep := &recordingReporter{}
	_, err := p95Spec(t, map[string]string{"FAKE_P95_FAULT": "1"}).run(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "hardware failure") {
		t.Fatalf("run() error = %v, want the hardware failure from the results file", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvFailed {
		t.Errorf("evidence = %v, want [%s]", ev, EvFailed)
	}
}

// A tool-level error in the findings file is a setup error: the run
// says nothing about the hardware.
func TestP95RunToolErrorIsSetupError(t *testing.T) {
	_, err := p95Spec(t, map[string]string{"FAKE_P95_TOOLERR": "1"}).run(context.Background(), &recordingReporter{})
	if !IsSetupError(err) {
		t.Fatalf("run() error = %v, want a setup error", err)
	}
}

// Our deadline hitting mid-test is not the tool's fault: clean end,
// partial evidence (real self-tests, no completion format exists).
func TestP95RunHangOurDeadlinePartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	rep := &recordingReporter{}
	if _, err := p95Spec(t, map[string]string{"FAKE_P95_HANG": "1"}).run(ctx, rep); err != nil {
		t.Fatalf("run() error = %v, want nil: our deadline is not the tool's fault", err)
	}
	ev := rep.evidenceSnapshot()
	if len(ev) != 1 || ev[0] != EvPartial {
		t.Errorf("evidence = %v, want [%s]", ev, EvPartial)
	}
}

// A tool that never writes its findings file never started its
// protocol: evidence stays unknown, no matter the clean exit.
func TestP95RunSilentToolUnknown(t *testing.T) {
	spec := p95Spec(t, nil)
	delete(spec.Env, "FAKE_P95")
	spec.Env["FAKE_SLEEP_MS"] = "5000" // alive, but writes no results.txt
	rep := &recordingReporter{}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := spec.run(ctx, rep); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if ev := rep.evidenceSnapshot(); len(ev) != 0 {
		t.Errorf("evidence = %v, want none: the protocol never started", ev)
	}
}
