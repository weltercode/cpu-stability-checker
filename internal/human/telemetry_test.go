package human

import "testing"

// All three live-telemetry sites render through this accumulator, so its
// exact wording is the log format — pinned here once. The two clock
// segments name different quantities: the observed cycle rate never
// borrows the core clock's identity, and the core clock names its
// producer source.
func TestTelemetryStatsLine(t *testing.T) {
	pct := func(v float64) *float64 { return &v }

	cases := []struct {
		name  string
		build func(*TelemetryStats)
		want  string
	}{
		{"nothing observed renders empty", func(*TelemetryStats) {}, ""},
		{
			"observed cycle rate min avg max",
			func(s *TelemetryStats) { s.AddClock(4100); s.AddClock(4200); s.AddClock(4000) },
			"telemetry: observed cycle rate min 4000 avg 4100 max 4200 MHz",
		},
		{
			"zero observed rate is a real reading (frozen process)",
			func(s *TelemetryStats) { s.AddClock(0) },
			"telemetry: observed cycle rate min 0 avg 0 max 0 MHz",
		},
		{
			"negative observed rate is impossible and dropped",
			func(s *TelemetryStats) { s.AddClock(-5) },
			"",
		},
		{
			"core clock names its source",
			func(s *TelemetryStats) { s.AddCoreClock(5600, "hwinfo"); s.AddCoreClock(5400, "hwinfo") },
			"telemetry: core clock min 5400 avg 5500 max 5600 MHz (hwinfo)",
		},
		{
			"a producer that did not name the core says nothing",
			func(s *TelemetryStats) { s.AddCoreClock(0, "hwinfo"); s.AddCoreClock(5000, "") },
			"",
		},
		{
			"both clock segments stay separate",
			func(s *TelemetryStats) {
				s.AddClock(4285)
				s.AddCoreClock(5500, "hwinfo")
			},
			"telemetry: observed cycle rate min 4285 avg 4285 max 4285 MHz, core clock min 5500 avg 5500 max 5500 MHz (hwinfo)",
		},
		{
			"idle only keeps the plain shape",
			func(s *TelemetryStats) { s.AddIdle(pct(25)) },
			"telemetry: idle avg 25.0%",
		},
		{
			"nil idle is unknown, not zero",
			func(s *TelemetryStats) { s.AddIdle(nil) },
			"",
		},
		{
			"all parts",
			func(s *TelemetryStats) {
				s.AddClock(3000)
				s.AddCoreClock(5500, "wmi")
				s.AddIdle(pct(10))
				s.AddIdle(pct(20))
			},
			"telemetry: observed cycle rate min 3000 avg 3000 max 3000 MHz, core clock min 5500 avg 5500 max 5500 MHz (wmi), idle avg 15.0%",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var s TelemetryStats
			tt.build(&s)
			if got := s.Line(); got != tt.want {
				t.Errorf("Line() = %q, want %q", got, tt.want)
			}
		})
	}
}
