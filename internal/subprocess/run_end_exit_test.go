package subprocess

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// P13 journal consistency: run_end must carry the FINAL verdict exit,
// the same one the Result carries - not the preliminary per-child
// mapping. The incident shape: every burst completed its window but
// none closed a verified pass (the incident phase's regions of
// hundreds of MiB never finished one fill+verify pass inside their
// 500ms bursts; here a 256 MiB region stands in for them inside an
// intentionally tiny 5 ms burst), so the children left no fault
// (preliminary exit 0) while the tool's contract verdict is
// exit 1. The journal used to say exit 0 and the menu then read a
// result the process never had.
//
// The stand-in region used to be 1 GiB. That is not what the incident
// had ("hundreds of MiB") and it made the fixture a memory admission
// hostage: 1 GiB + the 2 GiB commit reserve needs ~3 GiB of free
// commit at test time, and this machine's resident desktop baseline
// (local model stack, IDE, WSL) leaves ~2.5 GiB - a deterministic
// setup_error refusal, not the tested shape. 256 MiB preserves the
// incident exactly: one verified pass is fill + read-back + compare
// (>= 768 MiB of traffic) inside 5 ms, i.e. >= 150 GB/s single-core,
// physically impossible on any DDR5 desktop, while the admission only
// needs 256 MiB + reserve. Verified==0 for every burst stays a law of
// physics, not a race.
func TestRunEndJournalCarriesFinalExit(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	logPath := filepath.Join(dir, "run.log")

	res := ExecuteSessionResult(plan.Options{
		TestName:   "general",
		A:          "ram-constant",
		Cores:      []int{0},
		Workers:    1,
		PerProcess: true,
		Memory:     "256MiB", // the incident shape needs a region no 5ms window can pass over
		MemLock:    "off",
		RamSeed:    99,
		BeatEvery:  50 * time.Millisecond,
		LogFile:    logPath,
		Bursts:     3,
		BurstGap:   100 * time.Millisecond,
		BurstRun:   5 * time.Millisecond, // far too short for one verified pass over hundreds of MiB
	}, nil)

	if res.Exit != 1 {
		t.Fatalf("Result exit = %d, want 1 (a required slot verified nothing): %+v", res.Exit, res)
	}
	if res.Coverage.Started == 0 || res.Coverage.Completed != res.Coverage.Started {
		t.Fatalf("expected every burst unit to COMPLETE: %+v", res.Coverage)
	}
	if !res.Coverage.ChecksKnown || res.Coverage.Verified != 0 {
		t.Fatalf("expected 0 known verified checks (the incident shape): %+v", res.Coverage)
	}
	v := res.Verdict()
	if v.Kind != plan.VerdictUnverified {
		t.Errorf("incident phase verdict = %s (%s), want UNVERIFIED", v.Kind, v.Reason)
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
	if end.Validation != string(res.Validation) {
		t.Errorf("journal validation = %q, Result validation = %q", end.Validation, res.Validation)
	}
}
