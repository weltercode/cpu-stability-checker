package subprocess

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// The live panel during a transition run must carry the run context
// (label, prepared span, commanded burst position), not just bare
// hardware bars: with cores toggling idle/load every few hundred ms,
// bars without a footer read as unrelated noise (the user's report
// on the menu's transitions card). And the final summary + journal
// report must reach the panel as ONE write - every Print repaints
// the whole frame, so the old line-by-line report was a repaint
// storm the user saw as constant flickering.

// countingWriter counts Write CALLS (not bytes): the panel routes
// each call to one full repaint, so the count is the flicker budget.
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

func TestCommandedBurst(t *testing.T) {
	sched := transition.Schedule{Windows: []transition.Window{
		{Gap: 100 * time.Millisecond, RunFor: 100 * time.Millisecond},
		{Gap: 100 * time.Millisecond, RunFor: 100 * time.Millisecond},
		{Gap: 100 * time.Millisecond, RunFor: 100 * time.Millisecond},
	}} // windows end at 200ms, 400ms, 600ms
	for _, tc := range []struct {
		at   time.Duration
		want int
	}{{0, 0}, {150 * time.Millisecond, 0}, {200 * time.Millisecond, 1},
		{399 * time.Millisecond, 1}, {400 * time.Millisecond, 2},
		{600 * time.Millisecond, 3}, {999 * time.Millisecond, 3}} {
		if got := commandedBurst(sched, tc.at); got != tc.want {
			t.Errorf("commandedBurst(%v) = %d, want %d", tc.at, got, tc.want)
		}
	}
}

func TestTransitionHeaderIsOneWrite(t *testing.T) {
	sched := transition.Schedule{Windows: []transition.Window{{Gap: time.Second, RunFor: time.Second}}}
	var cw countingWriter
	writeTransitionHeader(&cw, nil, 16, sched, 11*time.Second, transition.ParkingOff)
	if cw.writes != 1 {
		t.Errorf("header took %d writes, want 1 (each panel Print is a full repaint)", cw.writes)
	}
	if !strings.Contains(cw.buf.String(), "transition mode: 16 core(s), 1 burst(s) per core") {
		t.Errorf("header content wrong:\n%s", cw.buf.String())
	}

	if strings.Contains(cw.buf.String(), "parking") {
		t.Errorf("a policy-off run must not promise observation it will not make: %s", cw.buf.String())
	}

	cw = countingWriter{}
	writeTransitionHeader(&cw, nil, 4, sched, 11*time.Second, transition.ParkingTry)
	if cw.writes != 1 {
		t.Errorf("active-policy header took %d writes, want 1", cw.writes)
	}
	if !strings.Contains(cw.buf.String(), "parking observation: policy") || !strings.Contains(cw.buf.String(), "source cpuset") {
		t.Errorf("an active policy must be stated in the header before the first sample exists: %s", cw.buf.String())
	}

	cw = countingWriter{}
	writeTransitionHeader(&cw, &memPlanSpec{rule: "cap 1GiB", skipped: 3}, 16, sched, 11*time.Second, transition.ParkingOff)
	if cw.writes != 1 {
		t.Errorf("header with memory plan took %d writes, want 1", cw.writes)
	}
	out := cw.buf.String()
	if !strings.Contains(out, "cap 1GiB") || !strings.Contains(out, "3 of 16 core(s) received no regions") {
		t.Errorf("memory plan lines lost from the batched header:\n%s", out)
	}
}

func TestSessionReportIsOneWrite(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "j.jsonl")
	jf, err := journal.OpenFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	jw := journal.NewWriter(jf, "run-fixed")
	jw.RunStart("t", "", []int{0}, 1, "1s", true)
	jw.RunEndValidation(0, "verified")
	jw.Close()

	var cw countingWriter
	writeSessionReport(&cw, jp, []int{0}, []SessionOutcome{{BurstsDone: 2, BurstsTried: 2, Exit: 0}}, "")
	if cw.writes != 1 {
		t.Errorf("summary+report took %d writes, want 1 (the old line-by-line report was the flicker storm)", cw.writes)
	}
	out := cw.buf.String()
	for _, want := range []string{"Session summary:", "core 0: OK · bursts 2/2 verified", "run run-fixed", "validation=verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lost %q:\n%s", want, out)
		}
	}
}

// End to end on a real (pipe) panel: during the run the panel must
// hold a pushed run snapshot; after the run it must be cleared. The
// pipe console never paints, but Push/ClearRun record the same state
// the painted frames use.
func TestSessionPanelRunContextDuringRun(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	opt := plan.Options{
		TestName: "panelctx", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		// span = 3*(400+400) = 2.4s: the 1s and 2s pushes land while
		// the session is still running.
		Bursts: 3, BurstGap: 400 * time.Millisecond, BurstRun: 400 * time.Millisecond,
	}
	panel := dashboard.New(os.Stdout)

	var mu sync.Mutex
	var sawRun bool
	var sample dashboard.Snapshot
	pollStop := make(chan struct{})
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-pollStop:
				return
			case <-tick.C:
				if s := panel.Current(); s.HasRun {
					mu.Lock()
					if !sawRun {
						sawRun, sample = true, s
					}
					mu.Unlock()
				}
			}
		}
	}()

	res := ExecuteSessionResult(opt, panel)
	close(pollStop)

	if res.Exit != 0 {
		t.Fatalf("clean session: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawRun {
		t.Fatal("the panel never received run context during the transition run")
	}
	if sample.Label != "panelctx" || !strings.Contains(sample.Run, "transition: 1 core(s), 3 burst(s)") {
		t.Errorf("run context identity wrong: %+v", sample)
	}
	if !strings.Contains(sample.Status, "commanded burst ") || !strings.Contains(sample.Status, "/3") {
		t.Errorf("commanded-burst status missing: %+v", sample)
	}
	if want := 2400 * time.Millisecond; sample.Duration != want {
		t.Errorf("prepared span on the panel = %v, want %v", sample.Duration, want)
	}
	// The deferred pushDone wait makes the ClearRun deterministic:
	// executeSession does not return before the push goroutine has
	// handed the panel back.
	if s := panel.Current(); s.HasRun {
		t.Errorf("stale run footer left on the panel after the run: %+v", s)
	}
	fmt.Fprintln(os.Stderr, "panel run context observed:", sample.Run, "|", sample.Status)
}
