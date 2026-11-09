package subprocess

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
)

// P13 journal consistency: run_end must carry the FINAL verdict exit,
// the same one the Result carries - not the preliminary per-child
// mapping. The incident shape: every burst completed its window but
// none closed a verified pass (the incident phase's regions of
// hundreds of MiB never finished one fill+verify pass inside their
// 500ms bursts), so the children left no fault (preliminary process
// exit 0) while the tool's contract verdict is exit 1. The journal
// used to say exit 0 and the menu then read a result the process
// never had.
//
// R15b rewrote HOW this fixture produces "preliminary 0 vs final 1".
// The old shape bet on physics: a 256 MiB region could not close one
// fill+read-back+compare pass inside a 5 ms burst (>= 768 MiB of
// traffic in 5 ms). This machine disproved that bet: the 2026-11-09
// RED gate observed `verified=192.0 MiB, verified_total=576.0 MiB,
// bursts 3/3 verified, exit 0 at the expected 1` - the 9950X closed
// whole passes in windows the fixture called impossible. A timer
// race against machine speed is not determinism, so the shape is now
// built by CANCELLATION instead: the session runs small real bursts,
// the test requests a stop (runctl.RequestCancel, the same path the
// first Ctrl+C takes) as soon as the journal records the first
// burst_done, and the remaining schedule closes as cancelled. The
// pin was then PROVEN SENSITIVE by a negative control, not assumed:
// a disposable build that fed the preliminary `sessionRunExit`
// instead of the final exit into run_end made this test fail with
// `journal run_end exit = 130, final Result exit = 1` - on the
// cancellation path the preliminary per-child mapping lands on the
// interrupted-130 vocabulary while the cancelled session's contract
// verdict is exit 1. Same bug class as the incident (run_end
// carrying a preliminary value), demonstrated by control rather
// than by assumption. Nothing about memory bandwidth is assumed
// anywhere below.
func TestRunEndJournalCarriesFinalExit(t *testing.T) {
	requireMemoryAdmission(t, 128<<20) // the engine's minimum verifying budget (allowance is budget/2); never an admission hostage
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	logPath := filepath.Join(dir, "run.log")

	// No ghost may outlive this test: a pending cancel this process
	// records but no Guard takes would strike the NEXT run in the
	// test binary (audit A01's rule). Clearing after execution is
	// therefore part of the fixture, not cleanup theater.
	defer runctl.ClearPendingCancel()

	// Deterministic trigger: cancel the moment the session has
	// proven it is live by recording its first burst_done. With the
	// 100ms gap the stop lands while the worker idles between
	// bursts, which is exactly the polite-STOP window; if even that
	// window cannot be reached (a starved machine), the bounded
	// fallback cancels whenever the Guard appears, and the
	// speed-dependent assertions below stay switched off.
	sawFirstBurst := false
	machineMoment := false // set when NO burst_done was observable and no live Guard existed to cancel - the moment gets a named skip, not a red grade
	var once sync.Once
	stopNow := func() {
		once.Do(func() {
			runctl.RequestCancel(plan.ExecCancelled, "R15b: test-requested stop after the first recorded burst")
		})
	}
	defer stopNow() // never leave a live session for the next test
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			recs, _ := journal.ReadAll(journal.Path(logPath))
			for i := range recs {
				if recs[i].Type == journal.BurstDone {
					sawFirstBurst = true
					stopNow()
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if runctl.Current() == nil {
			// No live Guard to cancel and no burst ever recorded: the
			// session ended while this watcher starved, or had not
			// reached the Guard yet. Record the moment AND still
			// request the stop (a Guard created afterwards takes the
			// pending request; a finished session leaves a ghost this
			// test's deferred ClearPendingCancel removes).
			machineMoment = true
		}
		stopNow()
	}()

	res := ExecuteSessionResult(plan.Options{
		TestName:   "general",
		A:          "ram-constant",
		Cores:      []int{0},
		Workers:    1,
		PerProcess: true,
		Memory:     "128MiB", // the engine's smallest verifying geometry (64MiB of data): a 200ms window can close real passes at ANY speed this box runs at
		MemLock:    "off",
		RamSeed:    99,
		BeatEvery:  50 * time.Millisecond,
		LogFile:    logPath,
		Bursts:     3,
		BurstGap:   100 * time.Millisecond,
		BurstRun:   200 * time.Millisecond,
	}, nil)
	stopNow() // the session is over; whatever the watcher was waiting for, stop it
	<-done

	if res.Execution != plan.ExecCancelled {
		if machineMoment {
			t.Skipf("R15b named skip: no burst_done was journal-visible and no live Guard existed within 45 s (observed gate mechanism: pinned workers deschedule to 0 iterations); the session ended uncancelled while this test's own watcher starved - the final-exit pin was never exercised. Machine moment, not a product fault.")
		}
		t.Fatalf("execution = %s, want cancelled (the stop was requested by this test): %+v", res.Execution, res)
	}
	if !strings.Contains(res.AbortMsg, "R15b") {
		t.Errorf("abort reason = %q, want it to name this test's requested stop", res.AbortMsg)
	}
	if res.Exit != 1 {
		t.Fatalf("Result exit = %d, want 1 (a run cancelled early is not a completed contract): %+v", res.Exit, res)
	}
	v := res.Verdict()
	if v.Kind != plan.VerdictIncomplete {
		t.Errorf("cancelled run verdict = %s (%s), want INCOMPLETE", v.Kind, v.Reason)
	}
	if res.Faults != nil || len(res.Findings) != 0 {
		t.Errorf("a test-requested stop must confirm no hardware fault: findings=%v faults=%v", res.Findings, res.Faults)
	}

	// Only shape-independent facts may depend on speed: if the first
	// burst really ran (that is what armed the cancel), the verified
	// machinery must have had its honest say.
	if sawFirstBurst && res.Coverage.Started == 0 {
		t.Errorf("first burst_done was recorded yet Coverage.Started = 0: %+v", res.Coverage)
	}

	recs, iss := journal.ReadAll(journal.Path(logPath))
	if iss.ReadErr != "" {
		t.Fatalf("journal unreadable: %s", iss.ReadErr)
	}
	var end *journal.Record
	for i := range recs {
		if recs[i].Type == journal.RunEnd {
			end = &recs[i]
		}
	}
	if end == nil {
		t.Fatal("journal has no run_end record")
	}
	if end.Exit != res.Exit {
		t.Errorf("journal run_end exit = %d, final Result exit = %d: they must be the same verdict", end.Exit, res.Exit)
	}
	if end.Exit != 1 {
		t.Errorf("journal run_end exit = %d, want 1: run_end must carry the FINAL verdict; the preliminary per-child mapping disagrees here (interrupted children land 130; the incident's clean children landed 0) and carrying THAT is exactly the P13 incident bug class - the negative control proved this test sees it", end.Exit)
	}
	if end.Validation != string(res.Validation) {
		t.Errorf("journal validation = %q, Result validation = %q", end.Validation, res.Validation)
	}
}
