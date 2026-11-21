package memtest

import (
	"math"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"512MiB", 512 << 20, false},
		{"2GiB", 2 << 30, false},
		{"4 KiB", 4096, false},
		{"0", 0, false},
		{"", 0, true},
		{"MiB", 0, true},
		{"-5GiB", 0, true},
		{"5 GiB extra", 0, true},
		{"5mib", 0, true}, // wrong case is a typo, not a unit we accept quietly
		{"99999999999999999999", 0, true},
		{"18446744073709551615", math.MaxUint64, false},
		{"184467440737095516TiB", 0, true}, // overflows 64-bit bytes
		{"16EiB", 0, true},                 // unit not accepted at all
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseSize(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseRequest(t *testing.T) {
	if r, err := ParseRequest(""); err != nil || !r.Auto {
		t.Errorf(`ParseRequest("") = %+v, %v; want auto, nil`, r, err)
	}
	if r, err := ParseRequest("auto"); err != nil || !r.Auto {
		t.Errorf(`ParseRequest("auto") = %+v, %v; want auto, nil`, r, err)
	}
	// R28: "max" is a distinct mode - it must not be confused with
	// auto (which keeps the percent cap) or with a fixed size.
	if r, err := ParseRequest("max"); err != nil || !r.Max || r.Auto {
		t.Errorf(`ParseRequest("max") = %+v, %v; want max, nil`, r, err)
	}
	if r, err := ParseRequest(" MAX "); err != nil || !r.Max {
		t.Errorf(`ParseRequest(" MAX ") = %+v, %v; want max, nil`, r, err)
	}
	if r, err := ParseRequest("512MiB"); err != nil || r.Auto || r.Max || r.Bytes != 512<<20 {
		t.Errorf(`ParseRequest("512MiB") = %+v, %v; want fixed 512MiB`, r, err)
	}
	if _, err := ParseRequest("0"); err == nil {
		t.Error("fixed request of 0 must be an error, not a silent no-op")
	}
}

// snap builds a snapshot with plausible GiB-scale numbers.
func snap(totalPhys, availPhys, commitTotal, commitFree uint64) sysinfo.MemorySnapshot {
	return sysinfo.MemorySnapshot{
		Known: true, CommitKnown: true,
		TotalPhys: totalPhys << 30, AvailPhys: availPhys << 30,
		TotalPageFile: commitTotal << 30, AvailPageFile: commitFree << 30,
	}
}

func TestComputeBudgetAuto(t *testing.T) {
	p := DefaultTrialParams() // 70%, min 4GiB reserve, 2GiB commit reserve
	cases := []struct {
		name   string
		snap   sysinfo.MemorySnapshot
		want   uint64 // expected budget bytes
		wantOK bool
	}{
		// 94 GiB installed -> reserve = max(4 GiB, ceil(94GiB/10))
		// = 9.4 GiB (rounded up to the byte). avail 40: 70% = 28;
		// 40-9.4 = 30.6; commit 30-2 = 28 -> min 28 (frac/commit tie).
		{"fraction binds", snap(94, 40, 40, 30), 28 << 30, true},
		// avail 20: 70% = 14; 20 - ceil(94GiB/10)=20GiB-10093173146
		// (9.4 GiB rounded up to the byte) = 11381663334; commit
		// 30-2 = 28 -> min is the physical reserve bound.
		{"physical reserve binds", snap(94, 20, 40, 30), 11381663334, true},
		// commit free 5: 5-2 = 3 -> min 3.
		{"commit reserve binds", snap(94, 40, 40, 5), 3 << 30, true},
		// avail below the OS reserve: refused, not zero.
		{"avail below reserve", snap(94, 9, 40, 30), 0, false},
		// free commit below the commit reserve: refused.
		{"commit below reserve", snap(94, 40, 40, 1), 0, false},
		// small machine: 8 GiB installed -> reserve max(4, 1) = 4;
		// avail 6: 70% = 4.2; 6-4 = 2; commit 10-2 = 8 -> min 2.
		{"small machine", snap(8, 6, 10, 10), 2 << 30, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := ComputeBudget(Request{Auto: true}, c.snap, p)
			if (err == nil) != c.wantOK {
				t.Fatalf("err = %v, wantOK %v", err, c.wantOK)
			}
			if err == nil && b.Bytes != c.want {
				t.Errorf("budget = %d, want %d (rule: %s)", b.Bytes, c.want, b.Rule)
			}
			if err != nil && strings.Contains(err.Error(), "0 B") && c.wantOK {
				t.Errorf("refusal should not masquerade as a zero budget: %v", err)
			}
		})
	}
}

// A missing observation forbids auto: the rule cannot be applied to
// numbers nobody measured, and guessing zero would silently disable
// the memory engine.
func TestComputeBudgetAutoNeedsObservations(t *testing.T) {
	physOnly := sysinfo.MemorySnapshot{Known: true, TotalPhys: 64 << 30, AvailPhys: 30 << 30}
	if _, err := ComputeBudget(Request{Auto: true}, physOnly, DefaultTrialParams()); err == nil {
		t.Error("auto budget accepted without commit observations")
	}
	if _, err := ComputeBudget(Request{Auto: true}, sysinfo.MemorySnapshot{}, DefaultTrialParams()); err == nil {
		t.Error("auto budget accepted with no observations at all")
	}
}

// R28 ("max" mode, owner complaint 2026-11-12): max removes ONLY the
// percentage cap. The OS reserve, the commit reserve and every honest
// refusal of the auto rule stay exactly as they are - a machine that
// auto refuses, max refuses too, with the same named constraint.
func TestComputeBudgetMax(t *testing.T) {
	p := DefaultTrialParams() // min 4GiB OS reserve, 2GiB commit reserve
	cases := []struct {
		name   string
		snap   sysinfo.MemorySnapshot
		want   uint64
		wantOK bool
	}{
		// avail 40 of 94 installed: 70% would cap at 28, but max
		// takes the floor of availPhys-reserve instead:
		// 40GiB - ceil(94GiB/10) = 32856499814 (9.4 GiB reserve
		// rounded up to the byte).
		{"percent cap removed", snap(94, 40, 40, 40), 32856499814, true},
		// the physical reserve still binds: avail 20 - 9.4 GiB
		// reserve = 11381663334 (the identical number auto computes
		// when the reserve is what binds).
		{"physical reserve binds", snap(94, 20, 40, 40), 11381663334, true},
		// the commit reserve still binds even when physical has room:
		// free commit 5 - 2 GiB reserve = 3.
		{"commit reserve binds", snap(94, 40, 40, 5), 3 << 30, true},
		// avail below the OS reserve: refused, not zero.
		{"avail below reserve", snap(94, 9, 40, 30), 0, false},
		// free commit below the commit reserve: refused.
		{"commit below reserve", snap(94, 40, 40, 1), 0, false},
		// small machine: 8 GiB installed -> reserve max(4, 1) = 4;
		// avail 6 - 4 = 2 (same as auto here: the pct cap never
		// bound, so max changes nothing).
		{"small machine unchanged", snap(8, 6, 10, 10), 2 << 30, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := ComputeBudget(Request{Max: true}, c.snap, p)
			if (err == nil) != c.wantOK {
				t.Fatalf("err = %v, wantOK %v", err, c.wantOK)
			}
			if err == nil && b.Bytes != c.want {
				t.Errorf("budget = %d, want %d (rule: %s)", b.Bytes, c.want, b.Rule)
			}
			if err != nil && strings.Contains(err.Error(), "0 B") && c.wantOK {
				t.Errorf("refusal should not masquerade as a zero budget: %v", err)
			}
		})
	}
}

// max must never be larger than auto's floors allow, must state its
// own rule (not masquerade as auto), and must name the constraint
// that actually binds the number it shows.
func TestComputeBudgetMaxStaysHonest(t *testing.T) {
	p := DefaultTrialParams()
	s := snap(94, 40, 40, 30) // free commit 30 makes commit bind
	auto, err := ComputeBudget(Request{Auto: true}, s, p)
	if err != nil {
		t.Fatal(err)
	}
	maxB, err := ComputeBudget(Request{Max: true}, s, p)
	if err != nil {
		t.Fatal(err)
	}
	// commit binds both: max here must EQUAL auto, never exceed the
	// floors, and it must not inherit auto's percent-cap number.
	if maxB.Bytes != auto.Bytes {
		t.Errorf("max = %d, want %d (commit binds both)", maxB.Bytes, auto.Bytes)
	}
	if !strings.HasPrefix(maxB.Rule, "max:") {
		t.Errorf("max rule masquerades as something else: %q", maxB.Rule)
	}
	if !strings.Contains(maxB.Rule, "binding=free-commit-minus-commit-reserve") {
		t.Errorf("max rule does not name the binding commit constraint: %q", maxB.Rule)
	}
	// And when the percent cap is what binds auto, max must exceed
	// auto but stay under the physical floor.
	s2 := snap(94, 40, 40, 40)
	auto2, err := ComputeBudget(Request{Auto: true}, s2, p)
	if err != nil {
		t.Fatal(err)
	}
	max2, err := ComputeBudget(Request{Max: true}, s2, p)
	if err != nil {
		t.Fatal(err)
	}
	if max2.Bytes <= auto2.Bytes {
		t.Errorf("max %d did not exceed the percent-capped auto %d", max2.Bytes, auto2.Bytes)
	}
	if !strings.Contains(max2.Rule, "binding=available-physical-minus-OS-reserve") {
		t.Errorf("max rule does not name the binding physical constraint: %q", max2.Rule)
	}
}

// max on an unobserved machine is refused exactly like auto: there is
// no number to take the maximum of, and pretending otherwise would
// invent a budget nobody measured.
func TestComputeBudgetMaxNeedsObservations(t *testing.T) {
	physOnly := sysinfo.MemorySnapshot{Known: true, TotalPhys: 64 << 30, AvailPhys: 30 << 30}
	if _, err := ComputeBudget(Request{Max: true}, physOnly, DefaultTrialParams()); err == nil {
		t.Error("max budget accepted without commit observations")
	}
	if _, err := ComputeBudget(Request{Max: true}, sysinfo.MemorySnapshot{}, DefaultTrialParams()); err == nil {
		t.Error("max budget accepted with no observations at all")
	}
}

// Fixed requests are honored exactly or refused with the blocking
// constraint named - never quietly reduced.
func TestComputeBudgetFixed(t *testing.T) {
	p := DefaultTrialParams()
	s := snap(94, 40, 40, 30) // avail 40, reserve 9.4 -> phys fits <=30.6; commit free 30, reserve 2 -> fits <=28

	// 28 GiB fits both constraints exactly at the boundary: honored.
	b, err := ComputeBudget(Request{Bytes: 28 << 30}, s, p)
	if err != nil || b.Bytes != 28<<30 {
		t.Errorf("fixed 28GiB: got %d (%v), want honored exactly", b.Bytes, err)
	}
	// 29 GiB + 2 GiB commit reserve > 30 GiB free commit: refused, not shrunk.
	if _, err := ComputeBudget(Request{Bytes: 29 << 30}, s, p); err == nil {
		t.Error("fixed request beyond free commit was accepted")
	}
	// 31 GiB + 9.4 GiB OS reserve > 40 GiB available physical: refused.
	if _, err := ComputeBudget(Request{Bytes: 31 << 30}, s, p); err == nil {
		t.Error("fixed request beyond available physical was accepted")
	}
	// unobservable machine: the budget layer defers to the allocator,
	// which will honestly fail later - it must not invent a machine.
	unk := sysinfo.MemorySnapshot{}
	b, err = ComputeBudget(Request{Bytes: 1 << 60}, unk, p)
	if err != nil || b.Bytes != 1<<60 {
		t.Errorf("fixed request with unknown machine: got %d (%v), want deferred to allocator", b.Bytes, err)
	}
}

// The budget is one pool for the phase; partitioning (C3) divides it.
// This pins that ComputeBudget does not multiply anything by worker
// count - the same budget comes out regardless of how many workers
// will later share it.
func TestBudgetIsPerPhaseNotPerWorker(t *testing.T) {
	s := snap(94, 40, 40, 30)
	b1, err := ComputeBudget(Request{Auto: true}, s, DefaultTrialParams())
	if err != nil {
		t.Fatal(err)
	}
	if b1.Bytes != 28<<30 {
		t.Fatalf("budget = %d, want 28 GiB", b1.Bytes)
	}
}

func TestCheckedArithmetic(t *testing.T) {
	if got := mulDiv(math.MaxUint64, 100, 100); got != math.MaxUint64 {
		t.Errorf("mulDiv(MaxUint64,100,100) = %d, want MaxUint64 (no overflow loss)", got)
	}
	if got := mulDiv(40<<30, 70, 100); got != 28<<30 {
		t.Errorf("mulDiv(40GiB,70,100) = %d, want 28 GiB", got)
	}
	if _, ok := subChecked(5, 6); ok {
		t.Error("subChecked(5,6) must report underflow")
	}
	if _, ok := addChecked(math.MaxUint64, 1); ok {
		t.Error("addChecked(MaxUint64,1) must report overflow")
	}
	if got := ceilDiv(94, 10); got != 10 {
		t.Errorf("ceilDiv(94,10) = %d, want 10", got)
	}
	if got := ceilDiv(90, 10); got != 9 {
		t.Errorf("ceilDiv(90,10) = %d, want 9", got)
	}
}

// Bad parameters are a setup error, not a silently weird budget.
func TestComputeBudgetRejectsBadParams(t *testing.T) {
	p := DefaultTrialParams()
	p.MaxPct = 0
	if _, err := ComputeBudget(Request{Auto: true}, snap(94, 40, 40, 30), p); err == nil {
		t.Error("MaxPct=0 accepted")
	}
	p.MaxPct = 101
	if _, err := ComputeBudget(Request{Auto: true}, snap(94, 40, 40, 30), p); err == nil {
		t.Error("MaxPct=101 accepted")
	}
}
