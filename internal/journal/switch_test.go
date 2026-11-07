package journal

import (
	"strings"
	"testing"
	"time"
)

func nsecs(d time.Duration) int64 { return int64(d) } // *NS fields are integer NANOSECONDS on the wire

// The Q05 additive switch records ride the same reader as everything
// else: the prepared turn timeline is captured run-level, the turn
// observations ride run-level too (they are GROUP level - no single
// core owns them), and the members' own session_burst records stay
// separate per-core evidence. Round trip through the real wire format.
func TestSwitchRecordsRoundTrip(t *testing.T) {
	var buf strings.Builder
	j := NewWriter(&buf, "run-sw")
	j.RunStart("general", "", []int{0, 1, 2, 3}, 1, "4s", true)
	j.SwitchPlan(SwitchPlan{
		BaseNS: nsecs(600 * time.Millisecond), JitterNS: 0, Order: "",
		Turns: []SwitchPlanTurn{
			{Group: 0, Round: 0, GapNS: nsecs(600 * time.Millisecond), RunForNS: nsecs(200 * time.Millisecond)},
			{Group: 1, Round: 0, GapNS: nsecs(600 * time.Millisecond), RunForNS: nsecs(200 * time.Millisecond)},
		},
	})
	j.SessionBurst(0, BurstObservation{Burst: "t0", Seq: 2, SchedDevMS: 0.4, TriggerToStartMS: 1.1, TriggerToVerifiedMS: 9.5, IdleBeforeMS: 600.0, Complete: true})
	j.SessionBurst(1, BurstObservation{Burst: "t0", Seq: 2, SchedDevMS: 0.4, TriggerToStartMS: 1.2, TriggerToVerifiedMS: 9.7, IdleBeforeMS: 600.0, Complete: true})
	j.SessionSwitch(SwitchObservation{Turn: 0, Group: 0, Round: 0, Cores: []int{0, 1}, PrimarySeq: 2, SchedDevMS: 0.4, SwitchToStartMS: 1.2, TurnCloseMS: 10.3, Complete: true})
	j.SessionSwitch(SwitchObservation{Turn: 1, Group: 1, Round: 0, Cores: []int{2, 3}, PrimarySeq: 3, SchedDevMS: 0.2, SwitchToStartMS: 0.9, TurnCloseMS: 0, Error: "worker vanished", Complete: false})
	j.RunEndValidation(1, "partial")
	j.Close()

	rep := RecoverRuns(Parse(strings.NewReader(buf.String())))
	if len(rep) != 1 {
		t.Fatalf("runs = %d, want 1", len(rep))
	}
	r := rep[0]
	if r.SwitchSchedule == nil || len(r.SwitchSchedule.Turns) != 2 {
		t.Fatalf("SwitchSchedule = %+v, want the 2 prepared turns back", r.SwitchSchedule)
	}
	if r.SwitchSchedule.Turns[1].Group != 1 || r.SwitchSchedule.Turns[0].GapNS != nsecs(600*time.Millisecond) {
		t.Errorf("turn fidelity lost: %+v", r.SwitchSchedule.Turns)
	}
	if len(r.Switches) != 2 || !r.Switches[0].Complete || r.Switches[1].Error != "worker vanished" {
		t.Fatalf("Switches = %+v, want one complete and one carrying its error", r.Switches)
	}
	// Group-level records must not invent core states: the only cores
	// recovery may know are the two with session_burst evidence.
	if len(r.Cores) != 2 {
		t.Errorf("recovered cores = %v, want exactly the 2 burst-evidenced members", r.Cores)
	}
	if r.Ended && r.Exit == 1 && r.Validation == "partial" {
		// sanity on the classic fields around the new ones
	} else {
		t.Errorf("classic recovery fields broke beside the new kinds: ended=%v exit=%d validation=%q", r.Ended, r.Exit, r.Validation)
	}

	var out strings.Builder
	PrintReport(&out, rep)
	s := out.String()
	if !strings.Contains(s, "prepared switch: 2 turns, 2 group(s), order fixed, base 600ms") {
		t.Errorf("report must state the prepared switch plan, got:\n%s", s)
	}
	if !strings.Contains(s, "switch turns observed: 2 (complete 1)") || !strings.Contains(s, "turn 1 (group 1 round 0) never closed: worker vanished") {
		t.Errorf("report must state observed turns and name the unclosed one, got:\n%s", s)
	}
	if strings.Contains(s, "NOT ENDED") || strings.Contains(s, "MISSING WORKER") {
		t.Errorf("switch records must not read as an unfinished run:\n%s", s)
	}
}

// Older readers ignore unknown record lines; today's reader must keep
// reading pre-Q05 journals exactly as before (no phantom runs, no
// phantom switches, switch story UNKNOWN - never "no switches").
func TestSwitchRecordsAreAdditiveForOldAndNew(t *testing.T) {
	// A pre-Q05 session run: no switch records at all.
	var old strings.Builder
	j := NewWriter(&old, "run-old")
	j.RunStart("general", "", []int{4}, 1, "2s", true)
	j.SchedulePlan(SchedulePlan{BaseNS: nsecs(200 * time.Millisecond), Windows: []ScheduleWindow{{GapNS: nsecs(200 * time.Millisecond), RunForNS: nsecs(500 * time.Millisecond)}}})
	j.SessionBurst(4, BurstObservation{Burst: "b1", Seq: 2, Complete: true})
	j.RunEndValidation(0, "verified")
	j.Close()
	rep := RecoverRuns(Parse(strings.NewReader(old.String())))
	if rep[0].SwitchSchedule != nil || rep[0].Switches != nil {
		t.Errorf("pre-Q05 run must read as switch-unknown, got %v / %v", rep[0].SwitchSchedule, rep[0].Switches)
	}
	var out strings.Builder
	PrintReport(&out, rep)
	if strings.Contains(out.String(), "switch") {
		t.Errorf("report of a pre-Q05 run must not mention switches:\n%s", out.String())
	}

	// A foreign (future-kind) line inside a run must not break parsing
	// nor invent a run/core state.
	lines := old.String() + `{"schema":1,"type":"quantum_check","run_id":"run-old","verdict":"fifty"}` + "\n"
	rep2 := RecoverRuns(Parse(strings.NewReader(lines)))
	if len(rep2) != 1 || rep2[0].SwitchSchedule != nil {
		t.Fatalf("unknown future kind invented state: %d runs, switch=%+v", len(rep2), rep2[0].SwitchSchedule)
	}
}
