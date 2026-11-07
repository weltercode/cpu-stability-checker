package sysinfo

import (
	"runtime"
	"testing"
)

// TestCpuMetricsRuns is a smoke test that the platform metrics path works
// without panicking (exercises the real Win32/procfs calls) and that unknown
// values render as "n/a". Real values are environment-dependent, so we only
// check they are plausible.
func TestCpuMetricsRuns(t *testing.T) {
	// Prime the load baseline (the first sample has no delta yet), then read.
	_ = CPUStats()
	m := CPUStats()

	line := MetricsLine(m)
	if line == "" {
		t.Fatalf("MetricsLine should not be empty")
	}
	t.Logf("metrics line: %q", line)
	t.Logf("load=%.1f freqMin=%.1f freqMax=%.1f temp=%.1f",
		m.LoadPct, m.FreqMinMHz, m.FreqMaxMHz, m.TempC)

	if m.FreqKnown && m.FreqMaxMHz < 1 {
		t.Fatalf("plausible frequency should be >= 1 MHz, got %.1f", m.FreqMaxMHz)
	}
	if m.LoadKnown && (m.LoadPct < 0 || m.LoadPct > 100) {
		t.Fatalf("load out of range: %.1f", m.LoadPct)
	}

	unknown := Metrics{}
	if got := unknown.loadLabel(); got != "load n/a" {
		t.Fatalf("loadLabel() unknown = %q, want load n/a", got)
	}
	if got := unknown.freqLabel(); got != "freq n/a" {
		t.Fatalf("freqLabel() unknown = %q, want freq n/a", got)
	}
	if got := unknown.tempLabel(); got != "temp n/a" {
		t.Fatalf("tempLabel() unknown = %q, want temp n/a", got)
	}
}

// TestMinMaxMHz pins the aggregation shared by the machine-wide frequency
// metric: empty means unknown, bounds must be right.
func TestMinMaxMHz(t *testing.T) {
	cases := []struct {
		name  string
		freqs []float64
		min   float64
		max   float64
		ok    bool
	}{
		{"empty is unknown", nil, 0, 0, false},
		{"single core is both bounds", []float64{3600}, 3600, 3600, true},
		{"bounds across cores", []float64{4100, 800, 4200, 3600}, 800, 4200, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			min, max, ok := minMaxMHz(tt.freqs)
			if ok != tt.ok || min != tt.min || max != tt.max {
				t.Errorf("minMaxMHz(%v) = (%.0f, %.0f, %v), want (%.0f, %.0f, %v)",
					tt.freqs, min, max, ok, tt.min, tt.max, tt.ok)
			}
		})
	}
}

// TestCoreFrequenciesRuns exercises the real platform query: when it
// reports anything it must cover every logical CPU with plausible values.
func TestCoreFrequenciesRuns(t *testing.T) {
	freqs, ok := CoreFrequencies()
	if !ok {
		t.Skip("platform does not report per-CPU frequency")
	}
	if len(freqs) < runtime.NumCPU() {
		t.Errorf("got %d entries, want at least NumCPU=%d", len(freqs), runtime.NumCPU())
	}
	for i, f := range freqs {
		if f < 1 || f > 20000 {
			t.Errorf("cpu %d frequency %.0f MHz is not plausible", i, f)
		}
	}
}
