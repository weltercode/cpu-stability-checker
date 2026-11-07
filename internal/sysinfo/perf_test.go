//go:build windows

package sysinfo

import (
	"math"
	"testing"
	"time"
)

func TestPerfInstanceCPU(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"0,0", 0, true},
		{"0,7", 7, true},
		{"0,31", 31, true},
		{"7", 7, true},
		{"_Total", 0, false}, // aggregate, not a CPU
		{"0,_Total", 0, false},
		{"", 0, false},
		{"0,a", 0, false},
		{"Core 0", 0, false},
		{"-1", 0, false},
	}
	for _, tt := range cases {
		got, ok := perfInstanceCPU(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("perfInstanceCPU(%q) = %d,%v want %d,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// The rated base must come from firmware and be plausible; if this
// machine's SMBIOS is uncooperative the source must refuse, not guess.
func TestRatedBaseMHzLive(t *testing.T) {
	base, err := ratedBaseMHz()
	if err != nil {
		t.Skipf("SMBIOS rated base unavailable: %v", err)
	}
	if base < 500 || base > 10000 || math.IsNaN(base) {
		t.Fatalf("implausible rated base %v MHz", base)
	}
	t.Logf("SMBIOS rated base: %.0f MHz", base)
}

// End-to-end against the real counter: every logical CPU should get a
// plausible MHz, and boost above the rated base must survive
// (NOCAP100) - the whole point of the source.
func TestPerfClockLive(t *testing.T) {
	p, err := OpenPerfClock()
	if err != nil {
		t.Skipf("no PDH performance clocks: %v", err)
	}
	defer p.Close()
	time.Sleep(1200 * time.Millisecond) // let the rate counter accumulate
	clocks := p.Read()
	if len(clocks) < 2 {
		t.Fatalf("expected per-CPU clocks, got %v", clocks)
	}
	for cpu, mhz := range clocks {
		if mhz < 50 || mhz > p.Base()*4 {
			t.Errorf("cpu %d: implausible %v MHz (base %.0f)", cpu, mhz, p.Base())
		}
	}
	t.Logf("base %.0f MHz, %d CPUs, sample c0=%.0f", p.Base(), len(clocks), clocks[0])
}
