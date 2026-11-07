package co

import (
	"context"
	"errors"
)

// Sentinel causes inside Fake's built-in validation; the Kind carries
// the machine-readable classification.
var (
	errNoScript     = errors.New("fake: no scripted read left")
	errUnknownRange = errors.New("fake: probed CO range unknown - cannot confirm profile")
	errCoreUnknown  = errors.New("fake: core id not in confirmed mapping")
	errRangeBad     = errors.New("fake: offset outside probed range")
)

// Fake is a scripted Backend: every method returns exactly what the
// scenario put into it. It is the P10 test surface required by the
// plan - every failure branch (unsupported CPU, missing tool/driver/
// privilege, malformed output, wrong offset count, timeout, readback
// mismatch, partial apply, cancellation, restore refusal, fingerprint
// change, external change) is pinned against the contract here,
// before any real hardware is touched. P11 automation is only allowed
// against this backend until P10+P12 prove the real one.
type Fake struct {
	Caps Capabilities
	Map  Mapping

	// Read answers come out in order; the last one repeats forever
	// (a fingerprint-change scenario scripts two different reads).
	Reads    []Snapshot
	ReadErrs []error // index-aligned with Reads; nil entry = no error

	ApplyResult WriteResult
	ApplyErr    error
	VerifyRes   Verification
	VerifyErr   error
	RestoreRes  Verification
	RestoreErr  error

	ErrProbe    error
	ErrTopology error
	ErrValidate error

	// Calls records "<op> <detail>" in order so tests can pin the
	// sequence the coordinator performed (apply before verify,
	// restore after failed apply, and so on).
	Calls []string

	readIdx int
}

var _ Backend = (*Fake)(nil)

// IsP11Fake marks Fake as the scripted backend P11 automation is
// allowed against until P10+P12 prove the real one (plan P11: "До
// этого автомат допускается только с fake backend"). The tuning layer
// refuses any backend that does not carry this marker, so a real
// adapter can never be wired into auto-tuning by accident.
func (*Fake) IsP11Fake() bool { return true }

func (f *Fake) record(op string) {
	f.Calls = append(f.Calls, op)
}

// canceled is the honest ctx outcome every method must produce.
func canceled(op string) error {
	return &Error{Kind: KindCancelled, Op: op}
}

func (f *Fake) Probe(ctx context.Context) (Capabilities, error) {
	f.record("probe")
	if err := ctx.Err(); err != nil {
		return Capabilities{}, canceled("probe")
	}
	if f.ErrProbe != nil {
		return Capabilities{}, f.ErrProbe
	}
	return f.Caps, nil
}

func (f *Fake) Topology(ctx context.Context) (Mapping, error) {
	f.record("topology")
	if err := ctx.Err(); err != nil {
		return Mapping{}, canceled("topology")
	}
	if f.ErrTopology != nil {
		return Mapping{}, f.ErrTopology
	}
	return f.Map, nil
}

func (f *Fake) Read(ctx context.Context) (Snapshot, error) {
	f.record("read")
	if err := ctx.Err(); err != nil {
		return Snapshot{}, canceled("read")
	}
	if len(f.Reads) == 0 && (len(f.ReadErrs) == 0 || f.ReadErrs[0] == nil) {
		return Snapshot{}, &Error{Kind: KindMalformedOutput, Op: "read",
			Err: errNoScript}
	}
	i := f.readIdx
	if i >= len(f.Reads) {
		i = len(f.Reads) - 1 // the last scripted read repeats forever
	} else {
		f.readIdx++
	}
	if i < 0 {
		i = 0 // errors-only script with no snapshots
	}
	if i < len(f.ReadErrs) && f.ReadErrs[i] != nil {
		return Snapshot{}, f.ReadErrs[i]
	}
	return f.Reads[i], nil
}

// Validate checks the profile against the probed range in Caps -
// exactly the contract: the range comes from the backend's own
// support, never from a constant. Unknown range refuses honestly
// (nothing can be confirmed), as do unaddressable cores.
func (f *Fake) Validate(p Profile) error {
	f.record("validate")
	if f.ErrValidate != nil {
		return f.ErrValidate
	}
	if !f.Caps.RangeKnown {
		return &Error{Kind: KindBadProfile, Op: "validate",
			Err: errUnknownRange}
	}
	known := map[string]bool{}
	for _, s := range f.Map.Slots {
		known[s.ToolID] = true
	}
	for _, o := range p.Offsets {
		if !known[o.Core] {
			return &Error{Kind: KindBadProfile, Op: "validate",
				Err: errCoreUnknown}
		}
		if o.Value < f.Caps.RangeMin || o.Value > f.Caps.RangeMax {
			return &Error{Kind: KindBadProfile, Op: "validate",
				Err: errRangeBad}
		}
	}
	return nil
}

func (f *Fake) Apply(ctx context.Context, p Profile) (WriteResult, error) {
	f.record("apply")
	if err := ctx.Err(); err != nil {
		// The fake reports what a real backend must report: an
		// unknown mutation outcome once a write may have started.
		return WriteResult{Mutated: MutatedUnknown}, canceled("apply")
	}
	if f.ApplyErr != nil {
		return f.ApplyResult, f.ApplyErr
	}
	return f.ApplyResult, nil
}

func (f *Fake) Verify(ctx context.Context, p Profile) (Verification, error) {
	f.record("verify")
	if err := ctx.Err(); err != nil {
		return Verification{}, canceled("verify")
	}
	if f.VerifyErr != nil {
		return Verification{}, f.VerifyErr
	}
	return f.VerifyRes, nil
}

func (f *Fake) Restore(ctx context.Context, s Snapshot) (Verification, error) {
	f.record("restore")
	if err := ctx.Err(); err != nil {
		return Verification{Match: Unknown("restore canceled mid-write; state unknown")}, canceled("restore")
	}
	if f.RestoreErr != nil {
		return Verification{}, f.RestoreErr
	}
	return f.RestoreRes, nil
}
