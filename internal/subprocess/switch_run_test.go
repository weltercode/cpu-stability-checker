package subprocess

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// Q05 driven pair-switch (hermetic, self-exec workers): every
// participant is PREPAREd before the first switch, turns are granted
// ONLY by the ledger, and the journal keeps three separate views -
// the prepared turn timeline (commanded), the parent's switch-turn
// observations (upper bounds incl IPC), and each member's own
// session_burst. Cancel mid-window and an injected fault mid-schedule
// must leave the honest partial story, and no process may survive
// either end.

func switchArgv(jp, runID string, core int, workload string) []string {
	return []string{"--core", strconv.Itoa(core), "--journal", jp, "--run-id", runID, "--session", "--workload", workload}
}

func openSwitchJournal(t *testing.T, jp, runID string, cores []int) *journal.Writer {
	t.Helper()
	f, err := journal.OpenFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	jw := journal.NewWriter(f, runID)
	jw.RunStart("general", "", cores, 1, "", true)
	return jw
}

func switchFixture(t *testing.T) (string, []int) {
	t.Helper()
	requireFixtureCPUs(t, 4) // participants are fixed cores 0..3; the worker bounds -core by its own NumCPU
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	cores := []int{0, 1, 2, 3}
	return jp, cores
}

func fourFloatParts() []SwitchParticipant {
	return []SwitchParticipant{{Core: 0, Group: 0}, {Core: 1, Group: 0}, {Core: 2, Group: 1}, {Core: 3, Group: 1}}
}

// The readiness bar for Q05b: two prepared groups, one full A->B->A->B
// cycle, no spawn inside a turn, and the report can tell the
// COMMANDED timeline from the OBSERVED turns and members.
func TestSwitchSessionFullCycle(t *testing.T) {
	jp, cores := switchFixture(t)
	runID := "rid-sw1"
	jw := openSwitchJournal(t, jp, runID, cores)

	sched, err := transition.BuildSwitchSchedule(2, 2, 60*time.Millisecond, 120*time.Millisecond, 0, transition.SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts := fourFloatParts()
	argvs := make([][]string, len(parts))
	sids := make([]string, len(parts))
	for i, p := range parts {
		argvs[i] = switchArgv(jp, runID, p.Core, "float")
		sids[i] = fmt.Sprintf("%s-p%d", runID, i)
	}

	start := time.Now()
	out := RunSwitchSession(context.Background(), argvs, sids, parts, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr != "" || out.Fault != "" || out.Interrupted {
		t.Fatalf("clean switch run came back dirty: %+v", out)
	}
	if out.TurnsTried != 4 || out.TurnsDone != 4 {
		t.Fatalf("turn counts: tried=%d done=%d, want 4/4", out.TurnsTried, out.TurnsDone)
	}
	if out.Exit != 0 {
		t.Fatalf("worker exits = %d, want 0", out.Exit)
	}
	if out.Checks == 0 || !out.ChecksKnown {
		t.Errorf("verified checks = %d (known=%v), want a nonzero fully-counted total", out.Checks, out.ChecksKnown)
	}

	recs, iss := journal.ReadAll(jp) // merges worker fragments automatically
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	rep := runs[0]
	if !rep.Ended {
		t.Error("run NOT ENDED")
	}

	// Commanded: the prepared timeline was journaled and reads back.
	if rep.SwitchSchedule == nil || len(rep.SwitchSchedule.Turns) != 4 || rep.SwitchSchedule.Seed != nil {
		t.Fatalf("switch_schedule record wrong: %+v", rep.SwitchSchedule)
	}
	// Observed turns: four complete parent records; the fixed order
	// must alternate groups exactly as prepared (0,1,0,1) - the
	// realized order IS the planned one, with no overlap possible
	// (each record closed before the next was even reserved).
	if len(rep.Switches) != 4 {
		t.Fatalf("session_switch records = %d, want 4", len(rep.Switches))
	}
	wantGroup := []int{0, 1, 0, 1}
	wantRound := []int{0, 0, 1, 1}
	for i, o := range rep.Switches {
		if o.Turn != i || o.Group != wantGroup[i] || o.Round != wantRound[i] {
			t.Errorf("turn %d identity wrong: %+v (want group %d round %d)", i, o, wantGroup[i], wantRound[i])
		}
		if !o.Complete {
			t.Errorf("turn %d not complete: %+v", i, o)
		}
		if o.SwitchToStartMS < 0 || o.TurnCloseMS < o.SwitchToStartMS {
			t.Errorf("turn %d intervals wrong (to-start %.3fms, close %.3fms): the barrier cannot close before the group started", i, o.SwitchToStartMS, o.TurnCloseMS)
		}
	}
	if !rep.Switches[0].Complete || rep.Switches[0].Cores == nil {
		t.Errorf("turn 0 must name its members: %+v", rep.Switches[0])
	}

	// Member views: each participant was TRIGGERed once per round of
	// its group - two complete session_burst records each - with the
	// same upper-bound discipline as P08 bursts.
	if len(rep.Cores) != 4 {
		t.Fatalf("recovered cores = %d, want 4", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if len(c.SessionObs) != 2 {
			t.Errorf("core %d session_burst records = %d, want 2", c.Core, len(c.SessionObs))
			continue
		}
		for j, o := range c.SessionObs {
			if !o.Complete || o.Checks == nil || *o.Checks == 0 {
				t.Errorf("core %d member view %d not complete/counted: %+v", c.Core, j, o)
			}
			if o.TriggerToStartMS < 0 || o.TriggerToVerifiedMS < o.TriggerToStartMS {
				t.Errorf("core %d member view %d intervals wrong: start %.3f verified %.3f", c.Core, j, o.TriggerToStartMS, o.TriggerToVerifiedMS)
			}
		}
		if c.Status != journal.OK {
			t.Errorf("core %d worker status = %s, want ok", c.Core, c.Status)
		}
	}
	if len(rep.Missing) > 0 {
		t.Errorf("missing fragments: %v", rep.Missing)
	}

	var sb strings.Builder
	journal.PrintReport(&sb, runs)
	s := sb.String()
	if !strings.Contains(s, "prepared switch: 4 turns, 2 group(s), order fixed, base 60ms") ||
		!strings.Contains(s, "switch turns observed: 4 (complete 4)") {
		t.Errorf("report must tell the switch story, got:\n%s", s)
	}
	if strings.Contains(s, "NOT ENDED") || strings.Contains(s, "never closed") {
		t.Errorf("a clean switch run must not read as broken:\n%s", s)
	}
	t.Logf("switch cycle wall time: %v", time.Since(start).Round(10*time.Millisecond))
}

// Cancel mid-window: the turn aborts at the honest boundary (IDLE
// answers), the journal says the turn never closed and why, the
// member views exist as incomplete, and the run reads interrupted -
// never a success.
func TestSwitchSessionCancelMidWindow(t *testing.T) {
	jp, cores := switchFixture(t)
	runID := "rid-sw2"
	jw := openSwitchJournal(t, jp, runID, cores)

	// One round, two turns: turn 0 (group 0) gets a long load window
	// and the cancel lands inside it (spawn+prepare fit far inside
	// 1.5s). The loop breaks at the aborted turn, so turn 1 is never
	// tried.
	sched, err := transition.BuildSwitchSchedule(2, 1, 50*time.Millisecond, 3000*time.Millisecond, 0, transition.SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts := fourFloatParts()
	argvs := make([][]string, len(parts))
	sids := make([]string, len(parts))
	for i, p := range parts {
		argvs[i] = switchArgv(jp, runID, p.Core, "float")
		sids[i] = fmt.Sprintf("%s-p%d", runID, i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		timer := time.NewTimer(1500 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		cancel()
	}()
	out := RunSwitchSession(ctx, argvs, sids, parts, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(130)
	jw.Close()
	cancel()

	if !out.Interrupted {
		t.Fatalf("outcome: %+v, want interrupted", out)
	}
	if out.Fault != "" {
		t.Errorf("cancel invented a fault: %q", out.Fault)
	}
	if out.SetupErr != "" {
		t.Errorf("cancel reported as setup error: %q", out.SetupErr)
	}
	if out.TurnsTried != 1 || out.TurnsDone != 0 {
		t.Errorf("turn counts: tried=%d done=%d, want 1/0 (the cancelled turn is not done, and no later turn was tried)", out.TurnsTried, out.TurnsDone)
	}

	recs, iss := journal.ReadAll(jp)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	rep := runs[0]
	if len(rep.Switches) != 1 {
		t.Fatalf("session_switch records = %d, want 1 (the cancelled turn says so itself)", len(rep.Switches))
	}
	o := rep.Switches[0]
	if o.Complete || !strings.Contains(o.Error, "cancelled mid-turn") {
		t.Errorf("cancelled turn record dishonest: %+v, want incomplete + named cancel", o)
	}
	// Both group-0 members keep their incomplete views: the story
	// names what never came instead of going silent.
	for _, c := range rep.Cores {
		if c.Core > 1 {
			continue // group 1 was never TRIGGERed
		}
		if len(c.SessionObs) != 1 || c.SessionObs[0].Complete {
			t.Errorf("core %d member view after cancel: %+v, want one incomplete view", c.Core, c.SessionObs)
		}
	}
	for _, c := range rep.Cores {
		if c.Core > 1 && len(c.SessionObs) != 0 {
			t.Errorf("core %d has a burst record although group 1 never got a turn: %+v", c.Core, c.SessionObs)
		}
	}

	var sb strings.Builder
	journal.PrintReport(&sb, runs)
	if s := sb.String(); !strings.Contains(s, "never closed: cancelled mid-turn") {
		t.Errorf("report must name the unclosed turn, got:\n%s", s)
	}
}

// An injected fault mid-schedule: the FIRST group's turn completed
// honestly, the second group's turn never closed and names the
// fault, the faulted member's worker story says fault, and the
// outcome carries the fault with its core - the vocabulary the entry
// point folds into exit 1.
func TestSwitchSessionFaultMidSchedule(t *testing.T) {
	jp, cores := switchFixture(t)
	runID := "rid-sw3"
	jw := openSwitchJournal(t, jp, runID, cores)

	sched, err := transition.BuildSwitchSchedule(2, 2, 50*time.Millisecond, 150*time.Millisecond, 0, transition.SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts := fourFloatParts()
	argvs := make([][]string, len(parts))
	sids := make([]string, len(parts))
	for i, p := range parts {
		wl := "float"
		if p.Core == 2 {
			wl = "fault" // the injected-fault probe kernel: ERROR ack mid-window
		}
		argvs[i] = switchArgv(jp, runID, p.Core, wl)
		sids[i] = fmt.Sprintf("%s-p%d", runID, i)
	}

	out := RunSwitchSession(context.Background(), argvs, sids, parts, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.Fault == "" || !strings.Contains(out.Fault, "injected fault") {
		t.Fatalf("fault outcome: %+v, want the injected fault named", out)
	}
	if out.FaultCore != 2 {
		t.Errorf("fault core = %d, want 2 (the member running the fault probe)", out.FaultCore)
	}
	if out.SetupErr != "" {
		t.Errorf("a fault must not read as setup error: %q", out.SetupErr)
	}
	if out.Interrupted {
		t.Error("a fault is not an interruption")
	}
	if out.TurnsDone != 1 || out.TurnsTried != 2 {
		t.Errorf("turn counts: tried=%d done=%d, want 2/1 (turn 0 closed, turn 1 faulted, nothing after it was tried)", out.TurnsTried, out.TurnsDone)
	}
	if out.Exit != 1 {
		t.Errorf("faulted worker exit = %d, want 1", out.Exit)
	}

	recs, iss := journal.ReadAll(jp)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	rep := runs[0]
	if len(rep.Switches) != 2 {
		t.Fatalf("session_switch records = %d, want 2 (the closed turn + the faulted one)", len(rep.Switches))
	}
	if !rep.Switches[0].Complete {
		t.Errorf("turn 0 should have closed honestly before the fault: %+v", rep.Switches[0])
	}
	if rep.Switches[1].Complete || !strings.Contains(rep.Switches[1].Error, "injected fault") {
		t.Errorf("faulted turn record dishonest: %+v", rep.Switches[1])
	}
	for _, c := range rep.Cores {
		switch c.Core {
		case 2:
			if c.Status != journal.Fault {
				t.Errorf("faulted member worker status = %s, want fault", c.Status)
			}
			for _, o := range c.SessionObs {
				if o.Complete {
					t.Errorf("faulted member has a COMPLETE view: %+v", o)
				}
			}
		case 3:
			// The healthy sibling of the faulted member: its view is
			// journaled as incomplete - it was TRIGGERed but its turn
			// never closed.
			for _, o := range c.SessionObs {
				if o.Complete {
					t.Errorf("sibling of a faulted turn reads as verified: %+v", o)
				}
			}
		}
	}

	var sb strings.Builder
	journal.PrintReport(&sb, runs)
	if s := sb.String(); !strings.Contains(s, "switch turns observed: 2 (complete 1)") || !strings.Contains(s, "turn 1 (group 1 round 0) never closed") {
		t.Errorf("report must count the closed turn and name the faulted one, got:\n%s", s)
	}
}

// A participant that dies on arrival (the worker rejects its args
// and exits before any PREPARE answers): the prepare-all barrier
// names the setup failure with its core, NO turn is ever commanded
// for a half-prepared group, and the deferred reap releases every
// already-spawned sibling - the run returns bounded, it never hangs
// waiting on a dead pipe and leaves no process behind. (A dead PIPE
// with no ERROR ack is a setup class exactly like P08's: a fault is
// only what a worker REPORTED.)
func TestSwitchSessionPrepareDeath(t *testing.T) {
	jp, cores := switchFixture(t)
	runID := "rid-sw4"
	jw := openSwitchJournal(t, jp, runID, cores)

	sched, err := transition.BuildSwitchSchedule(2, 1, 50*time.Millisecond, 100*time.Millisecond, 0, transition.SwitchOrderFixed, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts := fourFloatParts()
	argvs := make([][]string, len(parts))
	sids := make([]string, len(parts))
	for i, p := range parts {
		workerCore := p.Core
		if p.Core == 3 {
			workerCore = 99999 // out of range: the child exits at parse time, after spawn
		}
		argvs[i] = switchArgv(jp, runID, workerCore, "float")
		sids[i] = fmt.Sprintf("%s-p%d", runID, i)
	}

	start := time.Now()
	out := RunSwitchSession(context.Background(), argvs, sids, parts, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr == "" || !strings.Contains(out.SetupErr, "core 3") {
		t.Fatalf("outcome: %+v, want a setup error naming core 3 (the participant that died)", out)
	}
	if out.Fault != "" {
		t.Errorf("a dead pipe with no ERROR ack must not read as a fault: %q", out.Fault)
	}
	if out.TurnsTried != 0 || out.TurnsDone != 0 {
		t.Errorf("turns tried/done = %d/%d, want 0/0: a half-prepared group never gets a turn", out.TurnsTried, out.TurnsDone)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("reaping already-spawned siblings took %v: the guarantee is bounded", elapsed)
	}

	recs, iss := journal.ReadAll(jp)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	rep := runs[0]
	if rep.SwitchSchedule == nil || len(rep.SwitchSchedule.Turns) != 2 {
		t.Errorf("the prepared timeline must be journaled before any spawn, even when nothing ran: %+v", rep.SwitchSchedule)
	}
	if len(rep.Switches) != 0 {
		t.Errorf("switch turn records = %d, want none (no turn was ever commanded)", len(rep.Switches))
	}
}
