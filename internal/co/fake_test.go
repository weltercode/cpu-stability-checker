package co

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func intp(v int) *int { return &v }

func capsKnown() Capabilities {
	return Capabilities{
		Tool: "fake", Version: "test",
		Supported: Yes("test cpu"), Read: Yes("test"), Apply: Yes("test"),
		Readback:   Yes("test"),
		RangeKnown: true, RangeMin: -30, RangeMax: 30,
	}
}

func mapTwo() Mapping {
	return Mapping{Quality: "confirmed", Slots: []CoreSlot{
		{ToolID: "core0", SMUIndex: intp(0), OSLogical: intp(0), Physical: intp(0)},
		{ToolID: "core1", SMUIndex: intp(1), OSLogical: intp(2), Physical: intp(1)},
	}}
}

// The plan's failure-branch list, pinned at the contract level: every
// kind must surface typed (callers branch on Kind, never on text).
func TestFailureKindsSurface(t *testing.T) {
	probeKinds := []string{
		KindUnsupportedCPU, KindMissingTool, KindMissingDriver,
		KindNoPrivilege, KindTimeout,
	}
	for _, kind := range probeKinds {
		f := &Fake{ErrProbe: &Error{Kind: kind, Op: "probe", Err: errors.New("detail")}}
		_, err := f.Probe(context.Background())
		if !IsKind(err, kind) {
			t.Errorf("probe kind %q did not surface: %v", kind, err)
		}
		if KindOf(err) != kind {
			t.Errorf("KindOf(%v) = %q, want %q", err, KindOf(err), kind)
		}
	}

	f := &Fake{Caps: capsKnown(), Map: mapTwo()}
	f.ErrTopology = &Error{Kind: KindMalformedOutput, Op: "topology"}
	if _, err := f.Topology(context.Background()); !IsKind(err, KindMalformedOutput) {
		t.Errorf("topology malformed did not surface: %v", err)
	}
	f.ReadErrs = []error{&Error{Kind: KindOffsetCount, Op: "read"}}
	if _, err := f.Read(context.Background()); !IsKind(err, KindOffsetCount) {
		t.Errorf("read offset_count did not surface: %v", err)
	}
	f.VerifyErr = &Error{Kind: KindReadbackMismatch, Op: "verify"}
	if _, err := f.Verify(context.Background(), Profile{}); !IsKind(err, KindReadbackMismatch) {
		t.Errorf("verify mismatch did not surface: %v", err)
	}
	f.RestoreErr = &Error{Kind: KindRestoreRefused, Op: "restore"}
	if _, err := f.Restore(context.Background(), Snapshot{}); !IsKind(err, KindRestoreRefused) {
		t.Errorf("restore refusal did not surface: %v", err)
	}
}

// A fingerprint-change scenario is two reads with different hardware
// identity; the fake replays the last read forever, so exactly two
// distinct snapshots must be scripted.
func TestReadSequenceFingerprint(t *testing.T) {
	f := &Fake{Reads: []Snapshot{
		{Quality: "exact", HardwareID: "hw-A"},
		{Quality: "exact", HardwareID: "hw-B"},
	}}
	ctx := context.Background()
	s1, _ := f.Read(ctx)
	s2, _ := f.Read(ctx)
	s3, _ := f.Read(ctx)
	if s1.HardwareID != "hw-A" || s2.HardwareID != "hw-B" || s3.HardwareID != "hw-B" {
		t.Fatalf("read sequence = %q %q %q, want A B B", s1.HardwareID, s2.HardwareID, s3.HardwareID)
	}
}

// Validate must refuse on unknown probed range (nothing confirmable),
// unaddressable cores, and out-of-probed-range values - and accept
// inside the range. Never a hardcoded generation constant.
func TestValidateAgainstProbedRange(t *testing.T) {
	f := &Fake{Caps: capsKnown(), Map: mapTwo()}
	ok := Profile{Offsets: []Offset{{Core: "core0", Value: -30}, {Core: "core1", Value: 30}}}
	if err := f.Validate(ok); err != nil {
		t.Fatalf("in-range profile rejected: %v", err)
	}
	if err := f.Validate(Profile{Offsets: []Offset{{Core: "core9", Value: 1}}}); !IsKind(err, KindBadProfile) {
		t.Errorf("unknown core accepted: %v", err)
	}
	if err := f.Validate(Profile{Offsets: []Offset{{Core: "core0", Value: 31}}}); !IsKind(err, KindBadProfile) {
		t.Errorf("out-of-range accepted: %v", err)
	}
	f2 := &Fake{Caps: Capabilities{Tool: "fake"}, Map: mapTwo()} // RangeKnown false
	if err := f2.Validate(ok); !IsKind(err, KindBadProfile) {
		t.Errorf("unknown range accepted: %v", err)
	}
}

// A partial apply with an error still reports the confirmed subset -
// the restore path needs exactly that observation.
func TestPartialApplyKeepsObservation(t *testing.T) {
	f := &Fake{
		ApplyResult: WriteResult{
			Mutated: MutatedPartial,
			Applied: []Offset{{Core: "core0", Value: -10}},
			Failed:  []Offset{{Core: "core1", Value: -20}},
		},
		ApplyErr: &Error{Kind: KindPartialApply, Op: "apply"},
	}
	res, err := f.Apply(context.Background(), Profile{})
	if !IsKind(err, KindPartialApply) {
		t.Fatalf("apply error kind lost: %v", err)
	}
	if res.Mutated != MutatedPartial || len(res.Applied) != 1 || len(res.Failed) != 1 {
		t.Fatalf("partial observation lost: %+v", res)
	}
}

// Cancellation mid-apply must report the mutation outcome as UNKNOWN
// (the write may have partially reached the SMU), never "none".
func TestCancelApplyIsUnknownMutation(t *testing.T) {
	f := &Fake{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := f.Apply(ctx, Profile{})
	if !IsKind(err, KindCancelled) {
		t.Fatalf("cancel kind lost: %v", err)
	}
	if res.Mutated != MutatedUnknown {
		t.Fatalf("Mutated = %q, want %q", res.Mutated, MutatedUnknown)
	}
}

// Readback that cannot be performed is Match-unknown, not a failure
// and not a pass.
func TestVerifyUnknownReadback(t *testing.T) {
	f := &Fake{VerifyRes: Verification{Match: Unknown("backend has no readback")}}
	v, err := f.Verify(context.Background(), Profile{})
	if err != nil {
		t.Fatalf("unknown readback must not error: %v", err)
	}
	if v.Match.Known {
		t.Fatalf("unknown readback reported known: %+v", v.Match)
	}
	b, _ := json.Marshal(v.Match)
	if string(b) != "null" {
		t.Fatalf("unknown TriState JSON = %s, want null", b)
	}
}

// Restore cancellation leaves an explicit unknown-state verification:
// a mid-write restore may have changed the hardware.
func TestRestoreCancelUnknownState(t *testing.T) {
	f := &Fake{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := f.Restore(ctx, Snapshot{})
	if !IsKind(err, KindCancelled) {
		t.Fatalf("cancel kind lost: %v", err)
	}
	if v.Match.Known {
		t.Fatalf("canceled restore claimed known state: %+v", v.Match)
	}
}

// The call log pins that the coordinator can later prove the ORDER
// of operations (apply before verify, restore after failed apply).
func TestCallOrderRecorded(t *testing.T) {
	f := &Fake{Caps: capsKnown(), Map: mapTwo(),
		ApplyResult: WriteResult{Mutated: MutatedCompleted}}
	ctx := context.Background()
	f.Probe(ctx)
	f.Read(ctx)
	f.Apply(ctx, Profile{})
	f.Verify(ctx, Profile{})
	want := []string{"probe", "read", "apply", "verify"}
	if len(f.Calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.Calls, want)
	}
	for i := range want {
		if f.Calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", f.Calls, want)
		}
	}
}

// TriState JSON honesty: unknown is null, never false.
func TestTriStateJSON(t *testing.T) {
	for _, tc := range []struct {
		ts   TriState
		want string
	}{
		{Unknown("no tool"), "null"},
		{Yes("probed"), "true"},
		{No("probed refused"), "false"},
	} {
		b, err := json.Marshal(tc.ts)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("TriState(%v) JSON = %s, want %s", tc.ts, b, tc.want)
		}
	}
}
