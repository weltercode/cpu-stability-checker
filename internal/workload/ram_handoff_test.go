package workload

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

// Golden vectors pin the ram-handoff-v1 word spec: the mixing IS the
// reproduction contract, so a change here means bumping the kernel
// version, never silently redefining what old evidence reproduces to.
func TestHandoffWordGolden(t *testing.T) {
	cases := []struct {
		seed, gen, idx uint64
		want           uint32
	}{
		{0x123, 1, 0, 0x2eb4a51d},
		{0x123, 1, 1, 0x32f5419a},
		{0x123, 1, 2, 0x71807a31},
		{0, 7, 16384, 0xde0bcfb1},
		{0xDEADBEEFCAFEBABE, 3, 5, 0x705cdfb1},
		{0, 1, 0, 0x39d7a6e4},
	}
	for _, c := range cases {
		if got := HandoffWord(c.seed, c.gen, c.idx); got != c.want {
			t.Errorf("HandoffWord(%#x, %d, %d) = %08x, want %08x", c.seed, c.gen, c.idx, got, c.want)
		}
	}
}

// The write/verify round trip: every word recomputed independently,
// checks counting every word examined.
func TestHandoffWriteVerifyRoundTrip(t *testing.T) {
	const size = 1024
	const wordBase = 4096 // region parked at global byte offset 16384
	view := make([]byte, size)
	if err := HandoffWrite(view, 0xABCD, 1, wordBase); err != nil {
		t.Fatalf("HandoffWrite: %v", err)
	}
	for i := 0; i < size; i += 4 {
		want := HandoffWord(0xABCD, 1, wordBase+uint64(i>>2))
		if got := binary.LittleEndian.Uint32(view[i:]); got != want {
			t.Fatalf("word at %d = %08x, want %08x", i, got, want)
		}
	}
	checks, mm, err := HandoffVerify(view, 0xABCD, 1, wordBase, "worker=0 region=0 role=dest offset=16384 len=1024")
	if err != nil {
		t.Fatalf("HandoffVerify: %v", err)
	}
	if mm != nil {
		t.Fatalf("clean image reported mismatch: %+v", mm)
	}
	if checks != size/4 {
		t.Errorf("checks = %d, want %d", checks, size/4)
	}
}

// One flipped word: the verify stops at the FIRST mismatch, names
// offset/expected/actual, and the evidence carries the kernel
// identity, the seed and the generation (the pass role).
func TestHandoffVerifySingleCorruption(t *testing.T) {
	const size = 256
	view := make([]byte, size)
	if err := HandoffWrite(view, 42, 2, 7); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(view[16:], binary.LittleEndian.Uint32(view[16:])^0x80000000)
	checks, mm, err := HandoffVerify(view, 42, 2, 7, "rid")
	if err != nil {
		t.Fatalf("HandoffVerify: %v", err)
	}
	if mm == nil {
		t.Fatal("corrupted word not detected")
	}
	if mm.Offset != 16 || checks != 5 {
		t.Errorf("mismatch offset = %d checks = %d, want 16 and 5 (stop at first)", mm.Offset, checks)
	}
	want := HandoffWord(42, 2, 7+4)
	if mm.Expected != uint64(want) || mm.Actual != uint64(want^0x80000000) {
		t.Errorf("evidence values = %08x/%08x, want %08x/%08x", mm.Expected, mm.Actual, want, want^0x80000000)
	}
	if mm.Algorithm != HandoffKernelVersion || mm.Seed != 42 || mm.Pass != 2 || mm.Region != "rid" || mm.Phase != "lease-read" {
		t.Errorf("evidence identity = %+v", mm)
	}
}

// The content-identity contract (the ram-copy lesson carried over):
// verifying at a different generation reads the older values and
// faults at the very first word. A stale block can never pass by
// luck because the data IS the identity claim.
func TestHandoffStaleGenerationFaults(t *testing.T) {
	view := make([]byte, 64)
	if err := HandoffWrite(view, 9, 1, 0); err != nil {
		t.Fatal(err)
	}
	checks, mm, err := HandoffVerify(view, 9, 2, 0, "rid")
	if err != nil {
		t.Fatalf("HandoffVerify: %v", err)
	}
	if mm == nil {
		t.Fatal("stale generation (gen 1 content read as gen 2) passed")
	}
	if mm.Offset != 0 || checks != 1 {
		t.Errorf("stale detection at offset %d checks %d, want 0 and 1", mm.Offset, checks)
	}
	if mm.Pass != 2 {
		t.Errorf("evidence Pass = %d, want the generation that was requested (2)", mm.Pass)
	}
}

// Malformed views are plumbing bugs: refused as setup errors on both
// sides, never a vacuous pass and never a fabricated mismatch.
func TestHandoffMalformedViews(t *testing.T) {
	if err := HandoffWrite([]byte{1, 2, 3}, 1, 1, 0); err == nil || !IsSetupError(err) {
		t.Errorf("HandoffWrite(3 bytes) = %v, want a setup refusal", err)
	}
	if err := HandoffWrite(nil, 1, 1, 0); err == nil || !IsSetupError(err) {
		t.Errorf("HandoffWrite(empty) = %v, want a setup refusal", err)
	}
	checks, mm, err := HandoffVerify([]byte{1, 2, 3}, 1, 1, 0, "rid")
	if err == nil || !IsSetupError(err) {
		t.Errorf("HandoffVerify(3 bytes) = %v, want a setup refusal", err)
	}
	if mm != nil || checks != 0 {
		t.Errorf("malformed verify invented results: checks=%d mismatch=%+v", checks, mm)
	}
}

// The registry entry exists (version, memory need, fault class) but
// the phase mode refuses: a standalone Run would claim handoff work
// no coordinator ordered.
func TestHandoffPhaseModeRefused(t *testing.T) {
	wl, ok := Get("ram-handoff")
	if !ok {
		t.Fatal("ram-handoff not registered")
	}
	if !wl.RequiresMemory || wl.FaultClass != FaultMemory || wl.KernelVersion != HandoffKernelVersion {
		t.Errorf("registry data = %+v", wl)
	}
	if _, err := wl.Run(context.Background(), nopReporter{}); err == nil || !IsSetupError(err) {
		t.Errorf("phase Run = %v, want a setup refusal", err)
	}
}

// The ram-* family contract carried into handoff: expected content
// depends on (seed, generation, GLOBAL position) and never on how
// many workers the section was split between. The same block at the
// same offset must mean the same bytes in a 2-worker and a 4-worker
// layout - repartitioning may move blocks between processes, it may
// never silently rewrite what a position holds. And a neighbour
// block at the same size must differ: position feeds the image, so
// a swapped block is detectable by content alone.
func TestHandoffContentIndependentOfWorkerCount(t *testing.T) {
	const size = 4096
	const block = 2
	wordBase := uint64(block) * size / 4

	a := make([]byte, size)
	b := make([]byte, size)
	if err := HandoffWrite(a, 7, 3, wordBase); err != nil {
		t.Fatal(err)
	}
	if err := HandoffWrite(b, 7, 3, wordBase); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same block/seed/generation produced different bytes")
	}
	if checks, mm, err := HandoffVerify(a, 7, 3, wordBase, "layout-4worker"); err != nil || mm != nil {
		t.Errorf("verify across the second layout: checks=%d mismatch=%+v err=%v", checks, mm, err)
	}

	c := make([]byte, size)
	if err := HandoffWrite(c, 7, 3, uint64(block+1)*size/4); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Error("neighbour block identical: the global position does not feed the image")
	}
}
