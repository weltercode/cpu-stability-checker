package workload

import (
	"context"
	"encoding/binary"
	"testing"
)

// Beat map for the 128K+64K pair (3 chunks per sweep, 4 sweeps per
// pass = 12 beats): fill 1-3, verify-fwd 4-6, verify-rev 7-9,
// verify-final 10-12.

func TestRamMovingPassAccounting(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 5}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 24 // exactly two full passes
	it, err := runRamMoving(ctx, rep)
	if err != nil {
		t.Fatalf("runRamMoving: %v", err)
	}
	if it != 2 {
		t.Errorf("passes = %d, want 2 (two full fill+fwd+rev+final cycles)", it)
	}
	if rep.beats != 24 {
		t.Errorf("beats = %d, want 24 (4 sweeps x 3 chunks x 2 passes)", rep.beats)
	}
	if rep.ramPasses != 2 || rep.ramUnique != 192<<10 || rep.ramTotal != 2*(192<<10) {
		t.Errorf("coverage accounting wrong: passes=%d unique=%d total=%d", rep.ramPasses, rep.ramUnique, rep.ramTotal)
	}
	if rep.ramFound != 0 {
		t.Errorf("clean run reported %d mismatches", rep.ramFound)
	}
	// After a completed pass every word must hold the BASE pattern
	// again (the two inversions cancel out) - the final sweep
	// already proved it, this re-proves it from the test side.
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		for i := 0; i+4 <= len(v); i += 4 {
			want := ramAddressWord(5, 2, base+uint64(i)/4)
			if got := binary.LittleEndian.Uint32(v[i:]); got != want {
				t.Fatalf("after pass 2 word (global %d) = %08x, want the restored base %08x", base+uint64(i)/4, got, want)
			}
		}
	}
}

// Corruption must be caught by the very next sweep that touches
// the word, with the PHASE named in the finding: flipping after
// fill is a forward-sweep catch, flipping after the forward sweep
// is a reverse-sweep catch, flipping after the reverse sweep is a
// final-sweep catch. A kernel that only checked once would miss
// two of these.
func TestRamMovingPhaseFlipDetection(t *testing.T) {
	tests := []struct {
		name   string
		flipAt uint64
		phase  string
	}{
		{"flip after fill caught by fwd", 1, "verify-fwd"},
		{"flip after fwd caught by rev", 4, "verify-rev"},
		{"flip after rev caught by final", 7, "verify-final"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := openTestEngine(t, 128<<10, 64<<10)
			rep := &regionReporter{regions: e.Regions(), seed: 5}
			v := rep.regions[0].Block.Bytes()
			rep.flipAt = tt.flipAt
			rep.flip = func() { v[4] ^= 0x01 } // LSB of region 1's second word
			it, err := runRamMoving(context.Background(), rep)
			if err == nil {
				t.Fatal("corruption went undetected")
			}
			if it != 0 {
				t.Errorf("passes = %d, want 0 (the pass aborted at the mismatch)", it)
			}
			if rep.ramFound == 0 {
				t.Fatal("no structured finding")
			}
			g := rep.ramDetails[0]
			if g.Phase != tt.phase {
				t.Errorf("finding phase = %q, want %q", g.Phase, tt.phase)
			}
			if g.Algorithm != "ram-moving-v1" || g.Pass != 1 || g.Offset != 4 || g.Seed != 5 {
				t.Errorf("finding identity wrong: %+v", g)
			}
		})
	}
}

// After a mismatch the evidence must survive: the rest of the
// chunk is READ but never written again, the later sweeps of the
// pass never run, and the next pass never starts. Every word in
// region 1 chunk 1 must sit exactly where the abort left it.
func TestRamMovingReadOnlyTailAndFrozenEvidence(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 5}
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 1 // flip right after fill wrote chunk 1
	rep.flip = func() { v[4] ^= 0x01 }

	_, err := runRamMoving(context.Background(), rep)
	if err == nil {
		t.Fatal("expected fault")
	}

	p0 := ramAddressWord(5, 1, 0) // base word at global idx 0
	p1 := ramAddressWord(5, 1, 1) // base word at global idx 1 (offset 4)
	p2 := ramAddressWord(5, 1, 2) // base word at global idx 2 (offset 8)
	// Word 0: fwd verified it clean and INVERTED it before the
	// mismatch - it must still hold the inversion (rev/final never
	// ran, next pass never started).
	if got := binary.LittleEndian.Uint32(v[0:]); got != ^p0 {
		t.Errorf("word 0 = %08x, want the frozen inversion %08x", got, ^p0)
	}
	// Word 1: the flipped value must be untouched - not re-inverted,
	// not overwritten by any later sweep.
	if got := binary.LittleEndian.Uint32(v[4:]); got != p1^0x01 {
		t.Errorf("flipped word = %08x, want the corrupted value %08x preserved as evidence", got, p1^0x01)
	}
	// Word 2: after the mismatch the sweep stopped writing: this
	// word was read (and reported) but NOT inverted.
	if got := binary.LittleEndian.Uint32(v[8:]); got != p2 {
		t.Errorf("word 2 = %08x, want %08x: the read-only tail must not write", got, p2)
	}
}

// Cancellation per phase: cancel at the end of each sweep. A
// cancel landing during the fill leaves the pass uncredited (its
// extent was not fully written); a cancel landing after the fill
// completed lets the inversion sweeps finish (bounded data
// movement that must not be left half-applied) and credits the
// completed pass. No cancel is ever a fault.
func TestRamMovingCancelPerPhase(t *testing.T) {
	tests := []struct {
		name     string
		cancelAt uint64
		wantIt   uint64
	}{
		{"cancel at end of fill", 3, 0},
		{"cancel at end of fwd", 6, 1},
		{"cancel at end of rev", 9, 1},
		{"cancel at end of final", 12, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := openTestEngine(t, 128<<10, 64<<10)
			rep := &regionReporter{regions: e.Regions(), seed: 5}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rep.cancel = cancel
			rep.cancelAt = tt.cancelAt
			it, err := runRamMoving(ctx, rep)
			if err != nil {
				t.Fatalf("cancel is not a fault: %v", err)
			}
			if it != tt.wantIt {
				t.Errorf("passes = %d, want %d", it, tt.wantIt)
			}
			if rep.ramPasses != it {
				t.Errorf("coverage credited for %d passes but %d completed", rep.ramPasses, it)
			}
		})
	}
}

// Upfront cancel: nothing ran, nothing is counted.
func TestRamMovingUpfrontCancel(t *testing.T) {
	e := openTestEngine(t, 128<<10, 64<<10)
	rep := &regionReporter{regions: e.Regions(), seed: 5}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamMoving(ctx, rep)
	if err != nil || it != 0 || rep.beats != 0 || rep.ramPasses != 0 {
		t.Errorf("cancelled run counted work: it=%d err=%v beats=%d passes=%d", it, err, rep.beats, rep.ramPasses)
	}
}
