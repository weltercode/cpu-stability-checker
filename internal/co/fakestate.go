package co

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// StateFake is a STATEFUL fake machine: unlike Fake (which replays
// scripted answers), it maintains the current profile, so apply ->
// readback -> restore behave like a machine within the fake's bounds.
// It exists so the P11 tuning layer and its CLI can be exercised
// end-to-end without touching hardware: P11 automation is allowed
// only against a backend carrying the IsP11Fake marker, and this is
// that marker's production-side fake. It is NEVER wired to a real
// tool, driver or SMU.
type StateFake struct {
	mu    sync.Mutex
	hw    Snapshot
	Calls []string // op order, for honest call-sequence assertions
}

// NewStateFake builds a small four-core machine with a fixed,
// clearly-fake identity and a known starting profile.
func NewStateFake() *StateFake {
	return &StateFake{hw: Snapshot{
		Profile: Profile{Offsets: []Offset{
			{Core: "core0", Value: -10}, {Core: "core1", Value: -20},
			{Core: "core2", Value: -30}, {Core: "core3", Value: -40},
		}},
		Quality:    "exact",
		HardwareID: "fake-machine-1",
		ReadAt:     time.Now(),
		Notes:      []string{"stateful fake machine: no real hardware behind this"},
	}}
}

var _ Backend = (*StateFake)(nil)

// IsP11Fake marks StateFake for P11 automation (same marker as the
// scripted Fake; the real adapter deliberately lacks it).
func (*StateFake) IsP11Fake() bool { return true }

func (f *StateFake) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, op)
}

func (f *StateFake) Probe(ctx context.Context) (Capabilities, error) {
	f.record("probe")
	if err := ctx.Err(); err != nil {
		return Capabilities{}, canceled("probe")
	}
	return Capabilities{
		Tool: "fake-state", Version: "1", CPU: "FAKE MACHINE (not real)",
		Supported: Yes("stateful fake"), Read: Yes("stateful fake"),
		Readback: Yes("stateful fake"), Apply: Yes("stateful fake"),
		RangeKnown: true, RangeMin: -99, RangeMax: 99,
		Notes: []string{"stateful fake: range is scripted, not hardware-validated"},
	}, nil
}

func (f *StateFake) Topology(ctx context.Context) (Mapping, error) {
	f.record("topology")
	if err := ctx.Err(); err != nil {
		return Mapping{}, canceled("topology")
	}
	m := Mapping{Quality: "confirmed", Notes: []string{"stateful fake topology"}}
	for i, o := range f.current().Profile.Offsets {
		idx := i
		phys := i
		m.Slots = append(m.Slots, CoreSlot{ToolID: o.Core, SMUIndex: &idx, Physical: &phys})
	}
	return m, nil
}

func (f *StateFake) current() Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hw
}

func (f *StateFake) Read(ctx context.Context) (Snapshot, error) {
	f.record("read")
	if err := ctx.Err(); err != nil {
		return Snapshot{}, canceled("read")
	}
	s := f.current()
	s.ReadAt = time.Now()
	return s, nil
}

func (f *StateFake) Validate(p Profile) error {
	f.record("validate")
	known := map[string]bool{}
	for _, o := range f.current().Profile.Offsets {
		known[o.Core] = true
	}
	for _, o := range p.Offsets {
		if !known[o.Core] {
			return &Error{Kind: KindBadProfile, Op: "validate",
				Err: fmt.Errorf("fake: core %q not in the fake machine", o.Core)}
		}
		if o.Value < -99 || o.Value > 99 {
			return &Error{Kind: KindBadProfile, Op: "validate", Err: errors.New("fake: offset outside -99..99")}
		}
	}
	return nil
}

func (f *StateFake) Apply(ctx context.Context, p Profile) (WriteResult, error) {
	f.record("apply")
	if err := ctx.Err(); err != nil {
		return WriteResult{Mutated: MutatedUnknown}, canceled("apply")
	}
	f.mu.Lock()
	f.hw.Profile = Profile{Offsets: append([]Offset(nil), p.Offsets...)}
	f.mu.Unlock()
	return WriteResult{Mutated: MutatedCompleted, Applied: append([]Offset(nil), p.Offsets...),
		Notes: []string{"stateful fake write: no hardware involved"}}, nil
}

func (f *StateFake) Verify(ctx context.Context, p Profile) (Verification, error) {
	f.record("verify")
	if err := ctx.Err(); err != nil {
		return Verification{}, canceled("verify")
	}
	cur := f.current().Profile
	v := Verification{Match: Unknown("fake: comparison skipped")}
	if len(cur.Offsets) == len(p.Offsets) {
		want := map[string]int{}
		for _, o := range p.Offsets {
			want[o.Core] = o.Value
		}
		ok := true
		for _, o := range cur.Offsets {
			if want[o.Core] != o.Value {
				ok = false
				break
			}
		}
		if ok {
			v.Match = Yes("stateful fake readback equals the requested profile")
		} else {
			v.Match = No("stateful fake readback differs")
		}
	}
	return v, nil
}

func (f *StateFake) Restore(ctx context.Context, s Snapshot) (Verification, error) {
	f.record("restore")
	if err := ctx.Err(); err != nil {
		return Verification{Match: Unknown("restore canceled mid-write; state unknown")}, canceled("restore")
	}
	f.mu.Lock()
	f.hw.Profile = Profile{Offsets: append([]Offset(nil), s.Profile.Offsets...)}
	f.mu.Unlock()
	return Verification{Match: Yes("stateful fake restored to the saved snapshot")}, nil
}
