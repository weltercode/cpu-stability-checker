//go:build windows

package subprocess

// S5: the scenario-phase entry point ExecuteHandoffResult. Same real
// two-process proof as the helper-form tests above, but the contract
// under test is the PHASE contract: the outcome reports through
// plan.Result (the pipeline classifies it through the same Finalize
// as every other engine), the phase journal carries run_meta
// provenance (the session id and plan hash the pipeline stamped),
// and the S3 replay still binds to the phase's generation.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func handoffPhaseOptions(t *testing.T) (plan.Options, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "phase.log")
	opt := plan.Options{
		TestName: "s5-phase", A: "ram-handoff", Cores: []int{0, 1},
		PerProcess: true, AllOrNone: true, StopOnFault: true, ControlCore: -1,
		HandoffBlocks: 4, HandoffBlockBytes: 64 * 1024, HandoffRounds: 1, HandoffSeed: 42,
		MemLock: "off", LogFile: log,
		SessionID: "sess-phase", PlanHash: "hash-phase",
	}
	return opt, journal.Path(log)
}

func TestHandoffPhaseResultClean(t *testing.T) {
	requireMemoryAdmission(t, 1<<20) // small handoff geometry; documented upper bound
	handoffSeams(t)
	opt, jpath := handoffPhaseOptions(t)
	var stream []leaseEvent
	res := ExecuteHandoffResult(opt, nil, func(core int, obs journal.LeaseObservation) {
		stream = append(stream, leaseEvent{core, obs})
	})
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (%+v)", res.Exit, res)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("validation = %q, want verified (every block re-read independently)", res.Validation)
	}
	if len(stream) != 8 {
		t.Errorf("OnLease deliveries = %d, want 8 (the phase path wires the S4 stream too)", len(stream))
	}

	reports, _ := recoverPhaseJournal(t, jpath)
	if len(reports) != 1 {
		t.Fatalf("runs = %d, want 1", len(reports))
	}
	rep := reports[0]
	if rep.Test != "handoff" {
		t.Errorf("journaled test name = %q, want handoff (the phase rides the pipeline's files)", rep.Test)
	}
	if rep.Validation != "verified" {
		t.Errorf("journaled validation = %q, want verified", rep.Validation)
	}
	if rep.Exit != 0 || !rep.Ended {
		t.Errorf("journaled exit = %d (ended=%v), want 0/true", rep.Exit, rep.Ended)
	}
	var leases, acked, okCores int
	for _, c := range rep.Cores {
		leases += len(c.LeaseObs)
		for _, o := range c.LeaseObs {
			if o.Acked {
				acked++
			}
		}
		if c.Status == journal.OK {
			okCores++
		}
	}
	if leases != 8 || acked != 8 {
		t.Errorf("recovered leases = %d (acked %d), want 8/8", leases, acked)
	}
	if okCores != 2 {
		t.Errorf("worker core_done OK = %d, want 2", okCores)
	}
	if rep.Meta == nil {
		t.Fatal("phase journal lacks run_meta: a scenario run must stay attributable to its session")
	}
	if rep.Meta.SessionID != "sess-phase" || rep.Meta.PlanHash != "hash-phase" {
		t.Errorf("run_meta provenance = session %q plan %q, want the pipeline's sess-phase/hash-phase",
			rep.Meta.SessionID, rep.Meta.PlanHash)
	}

	// S3 regression, phase side: the generation the pipeline wrote
	// replays against the geometry the phase promised.
	replay, err := VerifyHandoffJournal(jpath, "", &HandoffExpectation{
		Cores: []int{0, 1}, Blocks: 4, Rounds: 1, Seed: 42,
	})
	if err != nil {
		t.Fatalf("phase generation replay: %v", err)
	}
	if len(replay.Violations) != 0 {
		t.Errorf("replay violations: %v", replay.Violations)
	}
	if replay.Done != 4 || replay.Tried != 4 || replay.RecordedExit != 0 {
		t.Errorf("replay = tried %d done %d exit %d, want 4/4/0", replay.Tried, replay.Done, replay.RecordedExit)
	}
}

// A setup refusal keeps the vocabulary (exit 2) and the honest claim:
// nothing ran, nothing may be called verified, and the journal still
// records the whole (short) story including the run_end.
func TestHandoffPhaseResultSetupRefusal(t *testing.T) {
	handoffSeams(t)
	opt, jpath := handoffPhaseOptions(t)
	opt.Cores = []int{0, 999} // a worker could never pin to it
	res := ExecuteHandoffResult(opt, nil, nil)
	if res.Exit != 2 {
		t.Fatalf("exit = %d, want 2 (%+v)", res.Exit, res)
	}
	if res.Validation == plan.ValVerified {
		t.Error("setup refusal claims verification it never performed")
	}
	if s := fmt.Sprintf("%+v", res); !strings.Contains(s, "999") {
		t.Errorf("refusal does not name the bad core: %s", s)
	}
	reports, _ := recoverPhaseJournal(t, jpath)
	if len(reports) != 1 {
		t.Fatalf("runs = %d, want 1 (the journal still tells the whole short story)", len(reports))
	}
	rep := reports[0]
	if !rep.Ended || rep.Exit != 2 {
		t.Errorf("journaled run_end: ended=%v exit=%d, want true/2", rep.Ended, rep.Exit)
	}
	if rep.Validation == "verified" {
		t.Error("journal claims verification for a run that refused before spawning")
	}
}

func recoverPhaseJournal(t *testing.T, path string) ([]journal.RunReport, string) {
	t.Helper()
	recs, iss := journal.ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("journal read: %s", iss.ReadErr)
	}
	return journal.RecoverRuns(recs), iss.ReadErr
}
