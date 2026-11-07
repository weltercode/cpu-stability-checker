package sysinfo

import (
	"math"
	"testing"
)

func perfBytes(entries ...[3]uint64) []byte {
	buf := make([]byte, 0, len(entries)*48)
	for _, e := range entries {
		entry := make([]byte, 48)
		for i, v := range e {
			for b := 0; b < 8; b++ {
				entry[i*8+b] = byte(v >> (8 * b))
			}
		}
		buf = append(buf, entry...)
	}
	return buf
}

func TestParseProcessorPerformance(t *testing.T) {
	got := parseProcessorPerformance(perfBytes(
		[3]uint64{100, 1000, 50},
		[3]uint64{0, 0, 0},
		[3]uint64{math.MaxUint64, 20, 5},
	))
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3", len(got))
	}
	// 100ns units become nanoseconds.
	if got[0] != (CoreTime{IdleNS: 10000, KernelNS: 100000, UserNS: 5000}) {
		t.Errorf("entry 0 = %+v, want times scaled x100", got[0])
	}
	if got[1] != (CoreTime{}) {
		t.Errorf("entry 1 = %+v, want zeros", got[1])
	}
	maxRaw := uint64(math.MaxUint64)
	if got[2].IdleNS != maxRaw*100 {
		t.Errorf("entry 2 idle = %d, want wrapped-scaled MaxUint64*100", got[2].IdleNS)
	}

	// Trailing partial entries are ignored, not guessed at.
	part := append(perfBytes([3]uint64{1, 1, 1}), 1, 2)
	if n := len(parseProcessorPerformance(part)); n != 1 {
		t.Fatalf("parsed %d entries from 50 bytes, want 1", n)
	}
	if n := len(parseProcessorPerformance(nil)); n != 0 {
		t.Fatalf("parsed %d entries from empty buffer, want 0", n)
	}
}

// /proc/stat per-cpu lines must map onto the same idle-inclusive-kernel
// contract the Windows source produces.
func TestParseProcStatCPUs(t *testing.T) {
	const stat = "cpu  100 20 30 400 5 1 2 0 0 0\n" +
		"cpu0 10 1 2 30 0 1 1 0 0 0\n" +
		"cpu1 5 0 5 50 0 0 0 3 0 0\n" +
		"intr 1234 5\n" +
		"cpu2 1 2 3\n" + // too few fields: ignored, not guessed
		"cpu3 bogus\n"
	got := parseProcStatCPUs(stat)
	if len(got) != 2 {
		t.Fatalf("parsed %d cpu lines, want 2 (cpu0, cpu1): %+v", len(got), got)
	}
	// cpu0: idle 30 ticks; user 10+1; kernel 2+1(irq)+1(softirq)+30(idle) = 34.
	if got[0] != (CoreTime{IdleNS: 30 * 10_000_000, UserNS: 11 * 10_000_000, KernelNS: 34 * 10_000_000}) {
		t.Errorf("cpu0 = %+v, want idle 30/user 11/kernel 34 ticks (x10ms)", got[0])
	}
	// cpu1: steal counts against the user's view.
	if got[1] != (CoreTime{IdleNS: 50 * 10_000_000, UserNS: 8 * 10_000_000, KernelNS: 55 * 10_000_000}) {
		t.Errorf("cpu1 = %+v, want idle 50/user 8/kernel 55 ticks (x10ms)", got[1])
	}
	// Δidle 6 over Δkernel 8 + Δuser 4 = exactly 50%.
	if p, ok := got[0].IdlePct(CoreTime{IdleNS: 36 * 10_000_000, UserNS: 15 * 10_000_000, KernelNS: 42 * 10_000_000}); !ok || p != 50 {
		t.Errorf("IdlePct over /proc/stat deltas = %.1f (ok=%v), want 50", p, ok)
	}
}

func TestCoreTimeIdlePct(t *testing.T) {
	cases := []struct {
		name   string
		prev   CoreTime
		now    CoreTime
		want   float64
		wantOK bool
	}{
		{"kernel includes idle: 25% idle", CoreTime{}, CoreTime{IdleNS: 25, KernelNS: 100, UserNS: 0}, 25, true},
		{"fully busy", CoreTime{}, CoreTime{IdleNS: 0, KernelNS: 60, UserNS: 40}, 0, true},
		{"fully idle", CoreTime{}, CoreTime{IdleNS: 100, KernelNS: 100, UserNS: 0}, 100, true},
		{"empty window is unknown", CoreTime{}, CoreTime{}, 0, false},
		{"negative deltas are unknown", CoreTime{IdleNS: 50, KernelNS: 50}, CoreTime{IdleNS: 10, KernelNS: 10}, 0, false},
		{"idle above kernel+user clamps to 100", CoreTime{}, CoreTime{IdleNS: 150, KernelNS: 100, UserNS: 0}, 100, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.prev.IdlePct(tt.now)
			if ok != tt.wantOK {
				t.Fatalf("IdlePct() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > 0.001 {
				t.Fatalf("IdlePct() = %.3f, want %.3f", got, tt.want)
			}
		})
	}
}
