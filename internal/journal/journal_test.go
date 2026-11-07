package journal

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func TestWakeRecords(t *testing.T) {
	var buf strings.Builder
	j := NewWriter(&buf, "run-w")
	j.RunStart("general", "rot", []int{5, 6}, 1, "30s", true)
	j.CoreStart(5, "int")
	j.Wake(5, 12.5)
	j.Wake(5, 40.0)
	j.CoreDone(5, OK, "")
	j.CoreStart(6, "int")
	j.CoreDone(6, OK, "")
	j.RunEnd(0)

	rep := RecoverRuns(Parse(strings.NewReader(buf.String())))
	if len(rep) != 1 || len(rep[0].Cores) != 2 {
		t.Fatalf("recovered %+v, want 1 run with 2 cores", rep)
	}

	var c5, c6 CoreSummary
	for _, cs := range SummarizeCores(rep).Cores {
		switch cs.Core {
		case 5:
			c5 = cs
		case 6:
			c6 = cs
		}
	}
	if c5.WakeN != 2 || c5.WakeMinMS != 12.5 || c5.WakeMaxMS != 40.0 || c5.WakeAvgMS != 26.25 {
		t.Fatalf("core 5 wake stats = %+v, want n=2 min=12.5 max=40 avg=26.25", c5)
	}
	if c6.WakeN != 0 {
		t.Errorf("core 6 wake stats = %+v, want no wake records", c6)
	}
}

// run_start carries the configured duration; recovery parses it so the
// report can show per-core exposure (unparsable = 0, never a guess).
func TestRecoverRunsParsesRunDuration(t *testing.T) {
	c := 0
	recs := []Record{
		{Type: RunStart, RunID: "r1", Test: "general", Duration: "30s", Cores: []int{0}},
		{Type: CoreStart, RunID: "r1", Core: &c, Workload: "int"},
		{Type: CoreDone, RunID: "r1", Core: &c, Result: OK},
		{Type: RunEnd, RunID: "r1", Exit: 0},
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("got %d runs, want 1", len(reports))
	}
	if reports[0].Duration != 30*time.Second {
		t.Errorf("Duration = %v, want 30s", reports[0].Duration)
	}
}

func TestSummarizeCores(t *testing.T) {
	rep := func(runID, phase, test string, selected []int, perProcess bool, ended bool, exit int, cores ...CoreOutcome) RunReport {
		return RunReport{RunID: runID, Test: test, Phase: phase, Selected: selected,
			PerProcess: perProcess, Ended: ended, Exit: exit, Cores: cores}
	}

	t.Run("in-process slots map onto the selected logical CPUs", func(t *testing.T) {
		reports := []RunReport{
			rep("r1", "heavy", "general", []int{5, 6}, false, true, 1,
				CoreOutcome{Core: 0, Workload: "int", Status: Fault, Msg: "mismatch at iter 42"},
				CoreOutcome{Core: 1, Workload: "int", Status: OK}),
			rep("r2", "", "isolate", []int{5}, false, true, 0,
				CoreOutcome{Core: 0, Workload: "sha256", Status: OK}),
		}
		got := SummarizeCores(reports)
		want := Summary{Runs: 2, Cores: []CoreSummary{
			{Core: 5, Runs: 2, Faults: 1, LastFault: "heavy general/int: mismatch at iter 42"},
			{Core: 6, Runs: 1, Interrupted: 1},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SummarizeCores() = %+v, want %+v", got, want)
		}
	})

	t.Run("per-process core numbers are already logical", func(t *testing.T) {
		reports := []RunReport{
			rep("r1", "pp", "isolate", []int{5, 6}, true, true, 1,
				CoreOutcome{Core: 5, Workload: "prime95", Status: Fault, Msg: "hardware failure"},
				CoreOutcome{Core: 6, Workload: "idle", Status: OK}),
		}
		got := SummarizeCores(reports)
		want := Summary{Runs: 1, Cores: []CoreSummary{
			{Core: 5, Runs: 1, Faults: 1, LastFault: "pp isolate/prime95: hardware failure"},
			{Core: 6, Runs: 1, Skipped: 1},
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("SummarizeCores() = %+v, want %+v", got, want)
		}
	})

	t.Run("exposure accumulates the configured duration per core", func(t *testing.T) {
		r1 := rep("r1", "p", "general", []int{0, 1}, true, true, 0,
			CoreOutcome{Core: 0, Workload: "int", Status: OK},
			CoreOutcome{Core: 1, Workload: "int", Status: OK})
		r1.Duration = 30 * time.Second
		r2 := rep("r2", "p", "general", []int{0}, true, true, 0,
			CoreOutcome{Core: 0, Workload: "int", Status: OK})
		r2.Duration = 5 * time.Second
		got := SummarizeCores([]RunReport{r1, r2})
		if len(got.Cores) != 2 {
			t.Fatalf("cores = %+v, want 2", got.Cores)
		}
		if got.Cores[0].Exposure != 35*time.Second {
			t.Errorf("core 0 exposure = %v, want 35s", got.Cores[0].Exposure)
		}
		if got.Cores[1].Exposure != 30*time.Second {
			t.Errorf("core 1 exposure = %v, want 30s", got.Cores[1].Exposure)
		}
	})

	t.Run("crashes and unended runs are counted", func(t *testing.T) {
		reports := []RunReport{
			rep("r1", "", "general", []int{0, 1}, false, false, 0,
				CoreOutcome{Core: 0, Workload: "float", Status: OK},
				CoreOutcome{Core: 1, Workload: "float", Status: Crashed}),
		}
		got := SummarizeCores(reports)
		if got.Unended != 1 {
			t.Errorf("Summary.Unended = %d, want 1", got.Unended)
		}
		if len(got.Cores) != 2 || got.Cores[1].Core != 1 || got.Cores[1].Crashed != 1 {
			t.Fatalf("SummarizeCores() = %+v, want core 1 counted as crashed", got.Cores)
		}
	})

	t.Run("cores are sorted and unnamed selections pass through", func(t *testing.T) {
		reports := []RunReport{
			rep("r1", "", "general", nil, false, true, 0,
				CoreOutcome{Core: 7, Workload: "int", Status: OK},
				CoreOutcome{Core: 3, Workload: "int", Status: OK}),
		}
		got := SummarizeCores(reports)
		if len(got.Cores) != 2 || got.Cores[0].Core != 3 || got.Cores[1].Core != 7 {
			t.Fatalf("SummarizeCores() = %+v, want cores [3 7] sorted", got.Cores)
		}
	})

	t.Run("the summary renders per-core lines", func(t *testing.T) {
		var buf strings.Builder
		sum := Summary{Runs: 3, Unended: 1, Cores: []CoreSummary{
			{Core: 5, Runs: 2, Faults: 1, LastFault: "heavy general/int: mismatch"},
			{Core: 6, Runs: 1},
		}}
		PrintSummary(&buf, sum)
		out := buf.String()
		for _, want := range []string{"core 5: 2 run(s), 1 FAULT", "last fault: heavy general/int: mismatch",
			"core 6: 1 run(s), OK", "3 run(s) total, 1 did not end cleanly"} {
			if !strings.Contains(out, want) {
				t.Errorf("PrintSummary() = %q, want it to contain %q", out, want)
			}
		}
	})
}

func TestWriterRoundTrip(t *testing.T) {
	var buf strings.Builder
	j := NewWriter(&buf, "run-1")

	j.RunStart("general", "warmup", []int{0, 1}, 2, "30s", false)
	j.CoreStart(0, "float")
	j.CoreBeat(0, 12345)
	j.CoreDone(0, OK, "")
	j.CoreStart(1, "mem")
	j.CoreDone(1, Fault, "mismatch at iter 42")
	j.RunEnd(1)

	records := Parse(strings.NewReader(buf.String()))
	if len(records) != 7 {
		t.Fatalf("Parse() read %d records, want 7:\n%s", len(records), buf.String())
	}

	wantTypes := []string{
		RunStart, CoreStart, CoreBeat, CoreDone,
		CoreStart, CoreDone, RunEnd,
	}
	for i, rec := range records {
		if rec.Type != wantTypes[i] {
			t.Errorf("record %d type = %q, want %q", i, rec.Type, wantTypes[i])
		}
		if rec.RunID != "run-1" {
			t.Errorf("record %d run_id = %q, want %q", i, rec.RunID, "run-1")
		}
		if _, err := time.Parse(time.RFC3339Nano, rec.TS); err != nil {
			t.Errorf("record %d ts = %q, want a valid RFC3339 timestamp", i, rec.TS)
		}
	}

	start := records[0]
	if start.Test != "general" || start.Phase != "warmup" || !reflect.DeepEqual(start.Cores, []int{0, 1}) || start.Workers != 2 || start.Duration != "30s" {
		t.Errorf("run_start = %+v, want test=general phase=warmup cores=[0 1] workers=2 duration=30s", start)
	}
	if rec := records[1]; rec.Core == nil || *rec.Core != 0 || rec.Workload != "float" || rec.PID == 0 {
		t.Errorf("core_start = %+v, want core=0 workload=float and a pid", rec)
	}
	if rec := records[2]; rec.Core == nil || *rec.Core != 0 || rec.Iters != 12345 {
		t.Errorf("core_beat = %+v, want core=0 iters=12345", rec)
	}
	if rec := records[5]; rec.Core == nil || *rec.Core != 1 || rec.Result != Fault || rec.Msg != "mismatch at iter 42" {
		t.Errorf("core_done = %+v, want core=1 result=fault with the message", rec)
	}
	if records[6].Exit != 1 {
		t.Errorf("run_end exit = %d, want 1", records[6].Exit)
	}
}

func TestRunStartPerProcessRoundTrip(t *testing.T) {
	var buf strings.Builder
	j := NewWriter(&buf, "run-pp")
	j.RunStart("isolate", "pp", []int{5, 6}, 1, "30s", true)
	j.CoreStart(5, "prime95")
	j.CoreDone(5, Fault, "hardware failure")
	j.RunEnd(1)

	reports := RecoverRuns(Parse(strings.NewReader(buf.String())))
	if len(reports) != 1 {
		t.Fatalf("recovered %d runs, want 1", len(reports))
	}
	rep := reports[0]
	if !rep.PerProcess {
		t.Errorf("RunReport.PerProcess = false, want true (core numbers are logical)")
	}
	if !reflect.DeepEqual(rep.Selected, []int{5, 6}) {
		t.Errorf("RunReport.Selected = %v, want [5 6]", rep.Selected)
	}
}

func TestParseTolerance(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"run_start","run_id":"r1","test":"general","ts":"2026-10-06T12:00:00Z"}`,
		``,
		`   `,
		`not json at all`,
		`{"type":"core_start","run_id":"r1","core":0,"workload":"float","ts":"2026-10-06T12:00:01Z"}`,
		`{"type":"core_beat","run_id":"r1","core":0,"iters":100,"ts":"2026-10-06T12:00:0`, // truncated by a power loss
		`{"type":"core_done","core":0,"result":"ok","ts":"2026-10-06T12:00:02Z"}`,         // missing run_id
		`{"run_id":"r1","ts":"2026-10-06T12:00:03Z"}`,                                     // missing type
		`{"type":"core_done","run_id":"r1","core":0,"result":"ok","ts":"2026-10-06T12:00:04Z"}`,
	}, "\n")

	got := Parse(strings.NewReader(input))
	if len(got) != 3 {
		t.Fatalf("Parse() kept %d records, want 3: %+v", len(got), got)
	}
	wantTypes := []string{RunStart, CoreStart, CoreDone}
	for i, rec := range got {
		if rec.Type != wantTypes[i] {
			t.Errorf("record %d type = %q, want %q", i, rec.Type, wantTypes[i])
		}
	}
}

func TestRecoverRuns(t *testing.T) {
	const tsBeat = "2026-10-06T12:00:30Z"
	beatTime := time.Date(2026, 10, 6, 12, 0, 30, 0, time.UTC)

	rec := func(typ, runID string, core *int, extra func(*Record)) Record {
		r := Record{Type: typ, RunID: runID, Core: core, TS: tsBeat}
		if extra != nil {
			extra(&r)
		}
		return r
	}

	tests := []struct {
		name    string
		records []Record
		want    []RunReport
	}{
		{
			name:    "no records yields no runs",
			records: nil,
			want:    nil,
		},
		{
			name: "clean run has no crashed cores",
			records: []Record{
				rec(RunStart, "r1", nil, func(r *Record) { r.Test = "general" }),
				rec(CoreStart, "r1", intPtr(0), func(r *Record) { r.Workload = "float" }),
				rec(CoreStart, "r1", intPtr(1), func(r *Record) { r.Workload = "mem" }),
				rec(CoreBeat, "r1", intPtr(0), func(r *Record) { r.Iters = 999 }),
				rec(CoreDone, "r1", intPtr(0), func(r *Record) { r.Result = OK }),
				rec(CoreDone, "r1", intPtr(1), func(r *Record) { r.Result = OK }),
				rec(RunEnd, "r1", nil, func(r *Record) { r.Exit = 0 }),
			},
			want: []RunReport{{
				RunID: "r1", Test: "general", Ended: true, Exit: 0,
				Cores: []CoreOutcome{
					{Core: 0, Workload: "float", Status: OK, LastBeat: beatTime, LastIters: 999},
					{Core: 1, Workload: "mem", Status: OK},
				},
			}},
		},
		{
			name: "started core without completion is crashed",
			records: []Record{
				rec(RunStart, "r1", nil, func(r *Record) { r.Test = "abab" }),
				rec(CoreStart, "r1", intPtr(0), func(r *Record) { r.Workload = "float" }),
				rec(CoreStart, "r1", intPtr(1), func(r *Record) { r.Workload = "float" }),
				rec(CoreBeat, "r1", intPtr(1), func(r *Record) { r.Iters = 4242 }),
				rec(CoreDone, "r1", intPtr(0), func(r *Record) { r.Result = OK }),
			},
			want: []RunReport{{
				RunID: "r1", Test: "abab",
				Cores: []CoreOutcome{
					{Core: 0, Workload: "float", Status: OK},
					{Core: 1, Workload: "float", Status: Crashed, LastBeat: beatTime, LastIters: 4242},
				},
			}},
		},
		{
			name: "fault keeps its message",
			records: []Record{
				rec(CoreStart, "r1", intPtr(2), func(r *Record) { r.Workload = "sha256" }),
				rec(CoreDone, "r1", intPtr(2), func(r *Record) { r.Result = Fault; r.Msg = "hash mismatch" }),
			},
			want: []RunReport{{
				RunID: "r1",
				Cores: []CoreOutcome{
					{Core: 2, Workload: "sha256", Status: Fault, Msg: "hash mismatch"},
				},
			}},
		},
		{
			name: "multiple runs are kept separate and in order",
			records: []Record{
				rec(RunStart, "r2", nil, nil),
				rec(CoreStart, "r2", intPtr(0), func(r *Record) { r.Workload = "int" }),
				rec(CoreDone, "r2", intPtr(0), func(r *Record) { r.Result = OK }),
				rec(RunStart, "r1", nil, nil),
				rec(CoreStart, "r1", intPtr(0), func(r *Record) { r.Workload = "int" }),
			},
			want: []RunReport{
				{RunID: "r2", Cores: []CoreOutcome{{Core: 0, Workload: "int", Status: OK}}},
				{RunID: "r1", Cores: []CoreOutcome{{Core: 0, Workload: "int", Status: Crashed}}},
			},
		},
		{
			name: "cores are sorted by number",
			records: []Record{
				rec(CoreStart, "r1", intPtr(3), nil),
				rec(CoreStart, "r1", intPtr(1), nil),
				rec(CoreDone, "r1", intPtr(3), func(r *Record) { r.Result = OK }),
				rec(CoreDone, "r1", intPtr(1), func(r *Record) { r.Result = OK }),
			},
			want: []RunReport{{
				RunID: "r1",
				Cores: []CoreOutcome{
					{Core: 1, Status: OK},
					{Core: 3, Status: OK},
				},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RecoverRuns(tt.records)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("RecoverRuns() =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestEndToEndCrashRecovery(t *testing.T) {
	// Simulate: a run writes records, then the process "dies" before core 1
	// finishes. Recovery must name core 1 as crashed with its last beat.
	var buf strings.Builder
	j := NewWriter(&buf, "run-crash")
	j.RunStart("general", "heavy", []int{0, 1}, 1, "30s", false)
	j.CoreStart(0, "float")
	j.CoreStart(1, "float")
	j.CoreBeat(1, 777)
	j.CoreDone(0, OK, "")
	// no core_done for core 1, no run_end — the process died here

	records := Parse(strings.NewReader(buf.String()))
	reports := RecoverRuns(records)
	if len(reports) != 1 {
		t.Fatalf("RecoverRuns() found %d runs, want 1", len(reports))
	}
	rep := reports[0]
	if rep.Phase != "heavy" {
		t.Errorf("report Phase = %q, want %q (scenario phases must survive recovery)", rep.Phase, "heavy")
	}
	if rep.Ended {
		t.Errorf("report Ended = true, want false (no run_end was written)")
	}
	if len(rep.Cores) != 2 {
		t.Fatalf("report has %d cores, want 2", len(rep.Cores))
	}
	crashed := rep.Cores[1]
	if crashed.Core != 1 || crashed.Status != Crashed {
		t.Fatalf("core 1 outcome = %+v, want status %q", crashed, Crashed)
	}
	if crashed.LastIters != 777 || crashed.LastBeat.IsZero() {
		t.Fatalf("crashed core last-seen = iters %d beat %v, want 777 and a real timestamp",
			crashed.LastIters, crashed.LastBeat)
	}
	if ok := rep.Cores[0]; ok.Core != 0 || ok.Status != OK {
		t.Fatalf("core 0 outcome = %+v, want status %q", ok, OK)
	}
}

// Telemetry records carry the load window's effective clock and idle
// share. A real 0% idle must survive the round trip; unknown values stay
// absent (nil pointer), never a fake zero.
func TestTelemetryRecords(t *testing.T) {
	var buf strings.Builder
	j := NewWriter(&buf, "run-tel")
	j.RunStart("general", "", []int{4, 5}, 1, "30s", false)
	j.CoreStart(0, "int")
	j.CoreStart(1, "int")
	zero := 0.0
	j.Telemetry(0, 3850.5, &zero, 3000, []CoreClockGroup{
		{Source: "hwinfo", MinMHz: 5400, AvgMHz: 5500, MaxMHz: 5600, N: 3, FirstMS: 1000, LastMS: 3000, AgeBasis: "producer", MaxAgeMS: 500},
	}) // core 4: fully busy, real clocks sampled by the parent
	j.Telemetry(1, 0, nil, 3000, nil) // core 5: clock unknown, idle unknown, no producer
	j.CoreDone(0, OK, "")
	j.CoreDone(1, OK, "")
	j.RunEnd(0)

	reports := RecoverRuns(Parse(strings.NewReader(buf.String())))
	if len(reports) != 1 || len(reports[0].Cores) != 2 {
		t.Fatalf("recovered %+v, want one run with two cores", reports)
	}
	tel0 := reports[0].Cores[0].Telemetry
	if len(tel0) != 1 {
		t.Fatalf("core 0 telemetry = %+v, want one sample", tel0)
	}
	if tel0[0].ClockMHz != 3850.5 || tel0[0].IdlePct == nil || *tel0[0].IdlePct != 0 || tel0[0].WindowMS != 3000 {
		t.Errorf("core 0 sample = %+v, want clock 3850.5, idle 0 (present), window 3000", tel0[0])
	}
	if len(tel0[0].CoreClocks) != 1 || tel0[0].CoreClocks[0].Source != "hwinfo" ||
		tel0[0].CoreClocks[0].N != 3 || tel0[0].CoreClocks[0].AgeBasis != "producer" {
		t.Errorf("core 0 core_clocks = %+v, want one hwinfo/producer group with n=3", tel0[0].CoreClocks)
	}
	tel1 := reports[0].Cores[1].Telemetry
	if len(tel1) != 1 || tel1[0].ClockMHz != 0 || tel1[0].IdlePct != nil {
		t.Errorf("core 1 sample = %+v, want clock 0 and idle absent", tel1)
	}
	if tel1[0].CoreClocks != nil {
		t.Errorf("core 1 core_clocks = %v, want absent: no producer means unknown, never zeros", tel1[0].CoreClocks)
	}

	// Aggregation: idle weighted by window length, clock min/avg/max.
	sum := SummarizeCores(reports)
	byCore := map[int]CoreSummary{}
	for _, cs := range sum.Cores {
		byCore[cs.Core] = cs
	}
	c4 := byCore[4]
	if c4.ClockN != 1 || c4.ClockMinMHz != 3850.5 || c4.ClockMaxMHz != 3850.5 {
		t.Errorf("core 4 clock stats = %+v, want one sample at 3850.5", c4)
	}
	if c4.IdleN != 1 || c4.IdleAvgPct != 0 {
		t.Errorf("core 4 idle stats = %+v, want avg 0", c4)
	}
	if c4.CoreClockN != 3 || c4.CoreClockMinMHz != 5400 || c4.CoreClockMaxMHz != 5600 ||
		c4.CoreClockAvgMHz != 5500 {
		t.Errorf("core 4 core-clock stats = %+v, want n=3 min=5400 max=5600 avg=5500", c4)
	}
	if len(c4.CoreClockSources) != 1 || c4.CoreClockSources[0] != "hwinfo" {
		t.Errorf("core 4 core-clock sources = %v, want [hwinfo]", c4.CoreClockSources)
	}
	c5 := byCore[5]
	if c5.ClockN != 0 || c5.IdleN != 0 {
		t.Errorf("core 5 stats = %+v, want no clock/idle counts", c5)
	}
	if c5.CoreClockN != 0 || c5.CoreClockSources != nil {
		t.Errorf("core 5 core-clock stats = %+v, want no counts and no sources", c5)
	}
}

// Weighted idle across windows of different lengths must follow the
// windows, not the sample count.
func TestSummarizeCoresIdleWeighting(t *testing.T) {
	mk := func(core int, idle, window int64) RunReport {
		c := core
		idleF := float64(idle)
		return RunReport{RunID: "r", Ended: true, Selected: []int{core}, Cores: []CoreOutcome{{
			Core: c, Status: OK,
			Telemetry: []TelemetrySample{{IdlePct: &idleF, WindowMS: window}},
		}}}
	}
	sum := SummarizeCores([]RunReport{mk(7, 10, 1000), mk(7, 40, 3000)})
	if len(sum.Cores) != 1 {
		t.Fatalf("summary cores = %+v, want one entry for core 7", sum.Cores)
	}
	cs := sum.Cores[0]
	want := (10.0*1000 + 40.0*3000) / 4000.0
	if cs.IdleN != 2 || math.Abs(cs.IdleAvgPct-want) > 0.001 || cs.IdleMaxPct != 40 {
		t.Fatalf("core 7 idle = avg %.3f (n=%d) max %.1f, want weighted %.3f and max 40",
			cs.IdleAvgPct, cs.IdleN, cs.IdleMaxPct, want)
	}
}

// Averages must divide the accumulated sums exactly once: three runs with
// one wake sample each must average over all three, not re-average the
// partial result run by run.
func TestSummarizeCoresAveragesAcrossRuns(t *testing.T) {
	wake := func(id string, ms float64) RunReport {
		return RunReport{RunID: id, Ended: true, Selected: []int{3}, Cores: []CoreOutcome{{
			Core: 0, Status: OK, WakeMS: []float64{ms},
			Telemetry: []TelemetrySample{{ClockMHz: ms * 100}},
		}}}
	}
	sum := SummarizeCores([]RunReport{wake("a", 10), wake("b", 20), wake("c", 30)})
	if len(sum.Cores) != 1 {
		t.Fatalf("summary cores = %+v, want one entry", sum.Cores)
	}
	cs := sum.Cores[0]
	if cs.WakeN != 3 || cs.WakeAvgMS != 20 || cs.WakeMinMS != 10 || cs.WakeMaxMS != 30 {
		t.Errorf("wake stats = n %d avg %.1f min %.1f max %.1f, want n 3 avg 20 min 10 max 30",
			cs.WakeN, cs.WakeAvgMS, cs.WakeMinMS, cs.WakeMaxMS)
	}
	if cs.ClockN != 3 || cs.ClockAvgMHz != 2000 || cs.ClockMinMHz != 1000 || cs.ClockMaxMHz != 3000 {
		t.Errorf("clock stats = n %d avg %.1f min %.1f max %.1f, want n 3 avg 2000 min 1000 max 3000",
			cs.ClockN, cs.ClockAvgMHz, cs.ClockMinMHz, cs.ClockMaxMHz)
	}
}
