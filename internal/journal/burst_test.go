package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Burst records must survive write -> read -> recover untouched: the
// identity (burst id, closing command's seq), the worker-local
// timestamps, the per-burst counts, and the complete flag that keeps
// an aborted burst from reading as a closed verified block. A slot
// with no burst records (one-shot run, older journal) must read back
// nil - unknown, never "zero bursts".
func TestBurstDoneRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-b")
	jw.RunStart("general", "", []int{0, 1}, 1, "", true)
	jw.CoreStart(0, "ram-constant")
	jw.CoreStart(1, "float")

	jw.BurstDone(0, BurstAccounting{
		Burst: "s1-b1", Seq: 2,
		TriggeredAt: "2026-10-16T12:00:00.000000000Z",
		StartedAt:   "2026-10-16T12:00:00.000100000Z",
		ClosedAt:    "2026-10-16T12:00:00.150000000Z",
		ExecMS:      149.9, Checks: 7, Bytes: 131072, Complete: true,
	})
	jw.BurstDone(0, BurstAccounting{
		Burst: "s1-b2", Seq: 5,
		TriggeredAt: "2026-10-16T12:00:01.000000000Z",
		StartedAt:   "2026-10-16T12:00:01.000200000Z",
		ClosedAt:    "2026-10-16T12:00:01.050000000Z",
		ExecMS:      49.8, Checks: 3, Bytes: 0, Complete: false,
	})
	jw.CoreDoneRam(0, OK, "", 0, nil, "", "ram-constant-v1", &RamEvidence{Total: 0})
	jw.CoreDoneChecks(1, OK, "", time.Second, nil, "", "float-repeated-v1")
	jw.RunEnd(0)
	jw.Close()

	records, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := RecoverRuns(records)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	byCore := map[int]*CoreOutcome{}
	for i := range runs[0].Cores {
		c := runs[0].Cores[i]
		byCore[c.Core] = &c
	}

	c0 := byCore[0]
	if c0 == nil || len(c0.Bursts) != 2 {
		t.Fatalf("core 0 bursts = %+v, want 2 records", c0)
	}
	b0, b1 := c0.Bursts[0], c0.Bursts[1]
	if b0.Burst != "s1-b1" || b0.Seq != 2 || !b0.Complete {
		t.Errorf("burst 0 identity drifted: %+v", b0)
	}
	if b0.TriggeredAt == "" || b0.StartedAt == "" || b0.ClosedAt == "" {
		t.Errorf("burst 0 lost its worker-local timestamps: %+v", b0)
	}
	if b0.ExecMS != 149.9 || b0.Checks != 7 || b0.Bytes != 131072 {
		t.Errorf("burst 0 counts drifted: %+v", b0)
	}
	if b1.Complete {
		t.Errorf("aborted burst reads as complete: %+v", b1)
	}
	if b1.Checks != 3 {
		t.Errorf("aborted burst lost its partial counts: %+v", b1)
	}

	// A slot that never ran bursts: nil, not an empty claim.
	if c1 := byCore[1]; c1 == nil || c1.Bursts != nil {
		t.Errorf("one-shot slot bursts = %+v, want nil (unknown, not zero)", c1)
	}
}

// The prepared schedule must survive write -> read -> recover with
// its concrete windows intact, and the seed distinction must hold:
// nil seed = fixed schedule, seed 0 = a real seeded schedule.
// Replay reads the windows, not the seed - that is the plan's
// "recovery must not depend on a future PRNG version".
func TestScheduleRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-sch")
	seed := uint64(0)
	jw.RunStart("general", "", []int{0}, 1, "", true)
	jw.SchedulePlan(SchedulePlan{
		BaseNS:   int64(100 * time.Millisecond),
		JitterNS: int64(30 * time.Millisecond),
		Seed:     &seed, // seed 0 is a real seed, not "unset"
		Windows: []ScheduleWindow{
			{GapNS: 85000000, RunForNS: 20000000},
			{GapNS: 112000000, RunForNS: 20000000},
		},
	})
	jw.CoreStart(0, "ram-constant")
	jw.CoreDoneChecks(0, OK, "", 0, nil, "", "ram-constant-v1")
	jw.RunEnd(0)
	jw.Close()

	records, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := RecoverRuns(records)
	if len(runs) != 1 || runs[0].Schedule == nil {
		t.Fatalf("schedule lost in recovery: %+v", runs)
	}
	s := runs[0].Schedule
	if s.Seed == nil || *s.Seed != 0 {
		t.Errorf("seed-0 schedule read back as %v: fixed and seeded must stay distinguishable", s.Seed)
	}
	if len(s.Windows) != 2 || s.Windows[0].GapNS != 85000000 || s.Windows[1].GapNS != 112000000 {
		t.Errorf("concrete windows drifted: %+v", s.Windows)
	}

	// A run with no schedule record: nil, never an empty plan.
	path2 := filepath.Join(t.TempDir(), "j2.jsonl")
	f2, _ := OpenFile(path2)
	jw2 := NewWriter(f2, "rid-plain")
	jw2.RunStart("general", "", []int{0}, 1, "", true)
	jw2.CoreStart(0, "float")
	jw2.CoreDoneChecks(0, OK, "", 0, nil, "", "float-repeated-v1")
	jw2.RunEnd(0)
	jw2.Close()
	runs2 := RecoverRuns(mustRead(t, path2))
	if len(runs2) != 1 || runs2[0].Schedule != nil {
		t.Errorf("plain run grew a schedule out of nothing: %+v", runs2[0].Schedule)
	}
}

func mustRead(t *testing.T, path string) []Record {
	t.Helper()
	recs, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read %s: %s", path, iss.ReadErr)
	}
	return recs
}

// The report must show the prepared timeline as what was COMMANDED
// (seeded jitter labeled as such, with the windows as the
// reproduction artifact), and never invent a schedule line for a
// run that had none.
func TestPrintReportScheduleLine(t *testing.T) {
	seed := uint64(777)
	rep := RunReport{
		RunID: "r1", Test: "general", Ended: true, Exit: 0, Schema: 1,
		Schedule: &SchedulePlan{
			BaseNS: int64(100 * time.Millisecond), JitterNS: int64(30 * time.Millisecond), Seed: &seed,
			Windows: []ScheduleWindow{{GapNS: 90000000, RunForNS: 20000000}, {GapNS: 110000000, RunForNS: 20000000}},
		},
		Cores: []CoreOutcome{{Core: 0, Workload: "ram-constant", Status: OK}},
	}
	var out bytes.Buffer
	PrintReport(&out, []RunReport{rep})
	line := out.String()
	for _, want := range []string{
		"prepared schedule: 2 windows, base 100ms ± 30ms",
		"seed 777",
		"reproduction artifact, not the seed",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("schedule line missing %q:\n%s", want, line)
		}
	}

	rep.Schedule = nil
	out.Reset()
	PrintReport(&out, []RunReport{rep})
	if strings.Contains(out.String(), "prepared schedule") {
		t.Errorf("plain run grew a schedule line out of nothing:\n%s", out.String())
	}
}

// The report must say how many bursts closed verified blocks and how
// many were aborted - an aborted burst is counted out loud, never
// folded into the clean total.
func TestPrintReportBurstSummary(t *testing.T) {
	rep := RunReport{
		RunID: "r1", Test: "general", Ended: true, Exit: 0, Schema: 1,
		Cores: []CoreOutcome{{
			Core: 0, Workload: "ram-constant", Status: OK,
			KernelVersion: "ram-constant-v1",
			Bursts: []BurstAccounting{
				{Burst: "s1-b1", Seq: 2, ExecMS: 10, Checks: 7, Bytes: 131072, Complete: true},
				{Burst: "s1-b2", Seq: 5, ExecMS: 5, Checks: 3, Complete: false},
				{Burst: "s1-b3", Seq: 8, ExecMS: 2, Checks: 1, Complete: false},
			},
		}},
	}
	var out bytes.Buffer
	PrintReport(&out, []RunReport{rep})
	line := out.String()
	want := "bursts: 3 (1 verified blocks closed, 2 aborted/incomplete, worker exec total 17.0ms)"
	if !strings.Contains(line, want) {
		t.Errorf("report missing the burst summary %q:\n%s", want, line)
	}

	// No bursts: no burst claim at all (unknown, not "0 bursts").
	rep.Cores[0].Bursts = nil
	out.Reset()
	PrintReport(&out, []RunReport{rep})
	if strings.Contains(out.String(), "bursts:") {
		t.Errorf("one-shot run grew a burst claim out of nothing:\n%s", out.String())
	}
}
