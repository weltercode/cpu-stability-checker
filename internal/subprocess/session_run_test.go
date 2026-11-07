package subprocess

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// P08 driven sessions: the parent journals the PREPARED schedule
// before anything spawns, commands every burst on the prepared
// timeline, and records its own view (session_burst) beside the
// worker's burst_done - two views of the same burst, never merged.
// Abnormal ends (cancel mid-burst, spontaneous fault, refused
// allocation) must leave the honest partial story, not a success.

func sessionRunArgv(jp, runID string, extra ...string) []string {
	base := []string{"--core", "0", "--journal", jp, "--run-id", runID, "--session"}
	return append(base, extra...)
}

func openParentJournal(t *testing.T, jp, runID string) *journal.Writer {
	t.Helper()
	f, err := journal.OpenFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	jw := journal.NewWriter(f, runID)
	jw.RunStart("general", "", []int{0}, 1, "", true)
	return jw
}

const ramGrants = "131072"

func ramGrantArgs() []string {
	return []string{
		"--mem-budget", ramGrants, "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:destination:65536:65536",
	}
}

// The readiness bar for P08: one prepared worker, many verified
// cycles on a prepared schedule, and the report can tell the
// COMMANDED transition (schedule record + parent session_burst)
// from the OBSERVATIONS (worker burst_done + ack arrivals).
func TestRunSessionBurstsRamConstant(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-r1")

	sched := transition.BuildSchedule(3, 60*time.Millisecond, 150*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-r1", append([]string{"--workload", "ram-constant"}, ramGrantArgs()...)...)
	out := RunSessionBursts(context.Background(), argv, "r1", 0, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr != "" || out.Fault != "" || out.Interrupted {
		t.Fatalf("clean session came back dirty: %+v", out)
	}
	if out.BurstsTried != 3 || out.BurstsDone != 3 || out.BurstsAborted != 0 {
		t.Fatalf("burst counts: %+v, want 3 tried / 3 done / 0 aborted", out)
	}
	if out.Exit != 0 {
		t.Fatalf("worker exit = %d, want 0", out.Exit)
	}

	recs, iss := journal.ReadAll(jp) // merges the worker fragments automatically
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	rep := runs[0]
	// The prepared schedule was journaled (fixed: no seed claim).
	if rep.Schedule == nil || len(rep.Schedule.Windows) != 3 || rep.Schedule.Seed != nil {
		t.Fatalf("schedule record wrong: %+v", rep.Schedule)
	}
	if len(rep.Cores) != 1 {
		t.Fatalf("cores = %d", len(rep.Cores))
	}
	c := rep.Cores[0]
	// Parent view: three complete observations with counts and
	// intervals; the trigger->first-verified interval is an upper
	// bound (it includes the IPC round trip) and must be >= the
	// trigger->STARTED interval.
	if len(c.SessionObs) != 3 {
		t.Fatalf("session_burst records = %d, want 3", len(c.SessionObs))
	}
	for i, o := range c.SessionObs {
		if !o.Complete || o.Checks == nil || *o.Checks == 0 {
			t.Errorf("observation %d not complete/counted: %+v", i, o)
		}
		// trigger->STARTED can honestly measure 0: this machine's
		// monotonic clock advances in ~0.5ms steps, and the ack
		// round trip fits inside one step. What must hold: no
		// negative intervals, and the verified interval (which
		// spans the whole window) dominates the start interval.
		if o.TriggerToStartMS < 0 || o.TriggerToVerifiedMS < o.TriggerToStartMS {
			t.Errorf("observation %d intervals wrong (start %.3fms, verified %.3fms): the acks cannot arrive before the command", i, o.TriggerToStartMS, o.TriggerToVerifiedMS)
		}
		if o.TriggerToVerifiedMS <= 0 {
			t.Errorf("observation %d: a 150ms window measured 0 total - the driver did not wait it out", i)
		}
		if o.SchedDevMS < 0 {
			t.Errorf("observation %d started %.3fms EARLY: the driver cut a prepared gap short", i, o.SchedDevMS)
		}
	}
	// Worker view: three complete burst_done records, separate from
	// the parent's, and the session totals still add up.
	if len(c.Bursts) != 3 {
		t.Fatalf("worker burst_done records = %d, want 3", len(c.Bursts))
	}
	for i, b := range c.Bursts {
		if !b.Complete || b.Checks == 0 {
			t.Errorf("worker burst %d wrong: %+v", i, b)
		}
	}
	if c.Mem == nil || c.Mem.Requested != 131072 {
		t.Errorf("session mem_done wrong: %+v", c.Mem)
	}
	if c.Ram == nil || c.Ram.Total != 0 {
		t.Errorf("clean session evidence wrong: %+v", c.Ram)
	}

	// The report distinguishes commanded from observed and labels
	// the upper bound out loud.
	var buf bytes.Buffer
	journal.PrintReport(&buf, []journal.RunReport{rep})
	line := buf.String()
	for _, want := range []string{
		"prepared schedule: 3 windows, base 60ms",
		"transitions: 3 commanded (3 verified acks)",
		"upper bound, incl IPC round trip",
		"bursts: 3 (3 verified blocks closed",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("report missing %q:\n%s", want, line)
		}
	}
}

// Cancel mid-burst: the burst aborts at the worker's honest
// boundary, the observation says complete=false, the session is
// interrupted (never "ok"), and the worker still released its
// memory on the STOP path.
func TestRunSessionBurstsCancelMidBurst(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-r2")

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(250 * time.Millisecond); cancel() }()
	// Long windows so the cancel lands mid-burst for sure.
	sched := transition.BuildSchedule(4, 0, 2*time.Second, 0, 0)
	argv := sessionRunArgv(jp, "rid-r2", append([]string{"--workload", "ram-constant"}, ramGrantArgs()...)...)
	out := RunSessionBursts(ctx, argv, "r2", 0, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if !out.Interrupted || out.Fault != "" || out.SetupErr != "" {
		t.Fatalf("cancel outcome: %+v, want interrupted, no fault, no setup error", out)
	}
	if out.BurstsTried != 1 || out.BurstsDone != 0 || out.BurstsAborted != 1 {
		t.Fatalf("burst counts: %+v, want 1 tried / 0 done / 1 aborted", out)
	}
	if out.Exit != 0 {
		t.Errorf("worker exit = %d, want 0 (the worker ended cleanly on STOP; the SESSION is what was interrupted)", out.Exit)
	}

	recs, _ := journal.ReadAll(jp)
	runs := journal.RecoverRuns(recs)
	c := runs[0].Cores[0]
	if len(c.SessionObs) != 1 || c.SessionObs[0].Complete {
		t.Errorf("parent observation for the aborted burst wrong: %+v", c.SessionObs)
	}
	if len(c.Bursts) != 1 || c.Bursts[0].Complete {
		t.Errorf("worker record for the aborted burst wrong: %+v", c.Bursts)
	}
	// The aborted burst's partial verified work is still recorded
	// (the graceful boundary completed AND verified what it wrote).
	if c.Bursts[0].Checks == 0 {
		t.Errorf("aborted burst lost its partial counts: %+v", c.Bursts[0])
	}
}

// A spontaneous fault (the fault probe fails with no VERIFY
// pending) must surface through the listening window wait: the
// parent records the incomplete burst, names the fault, and the
// worker's exit 1 rides the outcome.
func TestRunSessionBurstsSpontaneousFault(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-r3")

	sched := transition.BuildSchedule(2, 0, 500*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-r3", "--workload", "fault")
	out := RunSessionBursts(context.Background(), argv, "r3", 0, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.Fault == "" || !strings.Contains(out.Fault, "injected fault") {
		t.Fatalf("fault outcome: %+v, want the injected fault named", out)
	}
	if out.BurstsTried != 1 || out.BurstsDone != 0 || out.BurstsAborted != 1 {
		t.Errorf("burst counts: %+v, want 1 tried / 0 done / 1 aborted", out)
	}
	if out.Exit != 1 {
		t.Errorf("worker exit = %d, want 1 (a real fault)", out.Exit)
	}
	recs, _ := journal.ReadAll(jp)
	runs := journal.RecoverRuns(recs)
	c := runs[0].Cores[0]
	if len(c.SessionObs) != 1 || c.SessionObs[0].Complete {
		t.Errorf("parent observation for the faulted burst wrong: %+v", c.SessionObs)
	}
	if c.Status != journal.Fault {
		t.Errorf("worker core status = %s, want fault", c.Status)
	}
}

// A refused allocation: the PREPARE failure is a setup condition
// (exit 2, never a fault), and the journaled schedule still tells
// the reader exactly what WAS commanded even though nothing ran.
func TestRunSessionBurstsPrepareRefused(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-r4")

	sched := transition.BuildSchedule(2, 50*time.Millisecond, 50*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-r4",
		"--workload", "ram-constant",
		"--mem-budget", ramGrants, "--mem-allowance", "0", "--mem-lock", "off",
		// overlapping grants: refused before any allocation
		"--mem-grant", "0:0:source:0:65536", "--mem-grant", "0:1:source:0:65536")
	out := RunSessionBursts(context.Background(), argv, "r4", 0, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr == "" || out.Fault != "" {
		t.Fatalf("refused prepare: %+v, want setup error and no fault", out)
	}
	if out.Exit != 2 {
		t.Errorf("worker exit = %d, want 2", out.Exit)
	}
	if out.BurstsTried != 0 {
		t.Errorf("bursts tried = %d, want 0 (PREPARE never succeeded)", out.BurstsTried)
	}
	recs, _ := journal.ReadAll(jp)
	runs := journal.RecoverRuns(recs)
	if runs[0].Schedule == nil || len(runs[0].Schedule.Windows) != 2 {
		t.Errorf("the commanded schedule must be journaled even when nothing ran: %+v", runs[0].Schedule)
	}
	if len(runs[0].Cores[0].SessionObs) != 0 {
		t.Errorf("no burst was commanded, yet observations exist: %+v", runs[0].Cores[0].SessionObs)
	}
}
