package journal

import "testing"

func tele(clock float64, idle *float64) TelemetrySample {
	return TelemetrySample{ClockMHz: clock, IdlePct: idle, WindowMS: 1000}
}

func pct(v float64) *float64 { return &v }

// The anomaly detector is a diagnostic: it must catch a real clock cliff
// against the phase's own median, and it must never fire on the shapes
// that look like a cliff but are not (idle windows, small groups,
// different phases mixed together).
func TestDetectClockAnomalies(t *testing.T) {
	run := func(runID, phase string, cores []int, perProcess bool, tel map[int][]TelemetrySample) RunReport {
		rep := RunReport{RunID: runID, Test: "general", Phase: phase, Selected: cores,
			PerProcess: perProcess, Ended: true}
		for c, samples := range tel {
			rep.Cores = append(rep.Cores, CoreOutcome{Core: c, Status: OK, Telemetry: samples})
		}
		return rep
	}

	t.Run("a cliff against the phase median is flagged", func(t *testing.T) {
		var reports []RunReport
		clocks := []float64{4100, 4150, 4200, 4050, 2000}
		for i, c := range clocks {
			reports = append(reports, run("r1", "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, pct(0))}}))
			_ = i
		}
		got := DetectClockAnomalies(reports)
		if len(got[5]) != 1 {
			t.Fatalf("dips for core 5 = %+v, want exactly one", got[5])
		}
		if got[5][0].ClockMHz != 2000 || got[5][0].MedianMHz != 4100 {
			t.Errorf("dip = %+v, want 2000 against median 4100", got[5][0])
		}
	})

	t.Run("idle windows never count as dips", func(t *testing.T) {
		var reports []RunReport
		clocks := []float64{4100, 4150, 4200, 4050}
		for _, c := range clocks {
			reports = append(reports, run("r1", "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, pct(0))}}))
		}
		// settle/rotation shapes: low clock but clearly idle — excluded,
		// the group stays at four and the median stays honest.
		reports = append(reports, run("r2", "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(100, pct(60))}}))
		if got := DetectClockAnomalies(reports); len(got) != 0 {
			t.Fatalf("dips = %+v, want none (idle window excluded)", got)
		}
	})

	t.Run("groups smaller than the warmup floor cannot verdict", func(t *testing.T) {
		var reports []RunReport
		for _, c := range []float64{4100, 4200, 1000} {
			reports = append(reports, run("r1", "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, pct(0))}}))
		}
		if got := DetectClockAnomalies(reports); len(got) != 0 {
			t.Fatalf("dips = %+v, want none for a group of 3", got)
		}
	})

	t.Run("phases are compared only against themselves", func(t *testing.T) {
		var reports []RunReport
		// light phase runs at ~1000 MHz legitimately; a heavy dip is
		// judged against heavy, not against the light median.
		for _, c := range []float64{1000, 1010, 990, 1005} {
			reports = append(reports, run("l"+string(rune('a'+int(c))), "light", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, pct(0))}}))
		}
		for _, c := range []float64{4100, 4150, 4200, 2000} {
			reports = append(reports, run("h"+string(rune('a'+int(c))), "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, pct(0))}}))
		}
		got := DetectClockAnomalies(reports)
		if len(got[5]) != 1 || got[5][0].MedianMHz != 4150 {
			t.Fatalf("dips = %+v, want exactly the heavy-phase cliff against 4150", got[5])
		}
	})

	t.Run("in-process slots map onto logical CPUs", func(t *testing.T) {
		var reports []RunReport
		for _, c := range []float64{4100, 4150, 4200, 4050, 1500} {
			reports = append(reports, run("r1", "heavy", []int{8, 9}, false, map[int][]TelemetrySample{0: {tele(c, pct(0))}}))
		}
		got := DetectClockAnomalies(reports)
		if len(got[8]) != 1 {
			t.Fatalf("dips = %+v, want the dip on logical CPU 8 (slot 0)", got)
		}
	})

	t.Run("unknown idle is not gated out", func(t *testing.T) {
		var reports []RunReport
		for _, c := range []float64{4100, 4150, 4200, 4050, 1800} {
			reports = append(reports, run("r1", "heavy", []int{5}, true, map[int][]TelemetrySample{5: {tele(c, nil)}}))
		}
		if got := DetectClockAnomalies(reports); len(got[5]) != 1 {
			t.Fatalf("dips = %+v, want the 1800 dip even without idle data", got)
		}
	})
}
