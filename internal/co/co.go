// Package co is the swappable Curve Optimizer layer (P10): a backend
// contract, a scripted fake that pins every failure branch, and the
// ryzen-smu-cli adapter. CO offsets change per-core voltage curves -
// real hardware state - so the whole contract is built around honest
// observation: capabilities are tri-state (known-yes / known-no /
// unknown with a reason), every read carries its quality and a
// hardware fingerprint, every write reports what actually mutated
// (none / completed / partial / unknown), and nothing is ever
// silently substituted, assumed, or guessed. The backend exposes CO
// operations only: no core parking, no scalar voltage, no arbitrary
// SMU messages, and never any driver or BIOS installation.
package co

import (
	"context"
	"fmt"
	"time"
)

// TriState is the honesty shape for any capability or comparison:
// unknown is NOT false, and a known answer carries why it is what it
// is. JSON renders null when unknown - a consumer can never mistake
// "we could not look" for "no".
type TriState struct {
	Known bool
	Yes   bool
	Why   string
}

func Unknown(why string) TriState { return TriState{Why: why} }
func Yes(why string) TriState     { return TriState{Known: true, Yes: true, Why: why} }
func No(why string) TriState      { return TriState{Known: true, Yes: false, Why: why} }

func (t TriState) MarshalJSON() ([]byte, error) {
	if !t.Known {
		return []byte("null"), nil
	}
	if t.Yes {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

// UnmarshalJSON reads the PUBLISHED shape back: exactly true, false
// or null (B11). null means unknown and resets the receiver - a stale
// known answer must not survive a null. Anything else (numbers,
// strings, objects) is a decode error, never a silent false. The
// published shape carries no reason: reasons survive only through
// the versioned recovery DTO (State/TriStateV1), never here.
func (t *TriState) UnmarshalJSON(b []byte) error {
	switch string(b) {
	case "null":
		*t = TriState{}
		return nil
	case "true":
		*t = TriState{Known: true, Yes: true}
		return nil
	case "false":
		*t = TriState{Known: true, Yes: false}
		return nil
	}
	return fmt.Errorf("co: TriState must be true, false or null, got %s", string(b))
}

// Capabilities is what Probe establishes about the tool and the CPU
// before anything is read or written. Version and hash are the pinned
// identity the parsers were written against; empty means unknown,
// never "assumed current". Range is deliberately per-backend: the CO
// span is probed support, not a constant copied from one generation.
type Capabilities struct {
	Tool      string   `json:"tool"` // backend name: "ryzen-smu-cli", "fake"
	Version   string   `json:"version"`
	SHA256    string   `json:"sha256,omitempty"`
	CPU       string   `json:"cpu,omitempty"` // tool-reported CPU identity
	Supported TriState `json:"supported"`
	Read      TriState `json:"read"`
	Apply     TriState `json:"apply"`
	Readback  TriState `json:"readback"`
	// Range* is the HARDWARE-VALIDATED span: known only from a
	// verified read-only smoke on the installed build against this
	// CPU (P10d). The tool's argument regex bound is NOT evidence of
	// accepted values, so Probe leaves this unknown (B12) and writes
	// stay gated on it (B04).
	RangeKnown bool `json:"range_known"`
	RangeMin   int  `json:"range_min,omitempty"`
	RangeMax   int  `json:"range_max,omitempty"`
	// Syntax* is the backend's own argument limit (e.g. ryzen-smu-
	// cli's -?\d{1,2} regex: -99..99). It bounds what the tool will
	// PARSE, never what the hardware accepts; kept separate from the
	// validated range above on purpose.
	SyntaxKnown bool `json:"syntax_known,omitempty"`
	SyntaxMin   int  `json:"syntax_min,omitempty"`
	SyntaxMax   int  `json:"syntax_max,omitempty"`
	// PayloadSHA256 is the MEASURED digest of the managed payload that
	// sits beside the launcher (audit C02): for a .NET apphost the exe
	// is only the launcher, the executable logic lives in the paired
	// DLL, so build trust must cover the payload, not just the launcher.
	// Empty means never measured - and an unmeasured payload can never
	// produce BuildValidated.
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
	// BuildValidated is true only when Probe confirmed all four
	// identity axes - reported version, the SourceLink commit that
	// build embeds, the measured exe digest and the measured managed
	// payload digest - against the pinned provenance. The minimal
	// bootstrap write requires it: a single-core delta is minimal-risk
	// against the CURRENT state, but a different or tampered binary
	// must never reach the SMU at any delta size.
	BuildValidated bool     `json:"build_validated,omitempty"`
	Notes          []string `json:"notes,omitempty"` // component findings (driver, elevation)
}

// Mapping ties the backend's own core identifiers to our topology.
// OS logical IDs, SMT siblings and SMU indices are NOT interchangeable;
// a nil pointer is an honest unknown, never a guessed zero. Quality
// says how much of the mapping was actually confirmed.
type Mapping struct {
	Quality string     `json:"quality"` // confirmed | partial | unknown
	Slots   []CoreSlot `json:"slots"`
	Notes   []string   `json:"notes,omitempty"`
}

type CoreSlot struct {
	ToolID     string   `json:"tool_id"`
	SMUIndex   *int     `json:"smu_index,omitempty"`
	OSLogical  *int     `json:"os_logical,omitempty"` // confirmed match only
	Physical   *int     `json:"physical,omitempty"`
	SmtSibling *int     `json:"smt_sibling,omitempty"`
	Notes      []string `json:"notes,omitempty"` // slot-level honest findings
}

// Offset is one core's CO value in the backend's own numbering and
// sign convention; interpretation belongs to the backend, storage
// keeps the raw observation.
type Offset struct {
	Core  string `json:"core"`
	Value int    `json:"value"`
}

// Profile is a set of CO offsets addressed by backend core id.
type Profile struct {
	Offsets []Offset `json:"offsets"`
}

// Snapshot is one Read result: the profile plus read quality and the
// hardware fingerprint that lets a later restore detect that the
// machine (or another tool) changed underneath it.
type Snapshot struct {
	Profile    Profile   `json:"profile"`
	Quality    string    `json:"quality"` // exact | partial | unknown
	HardwareID string    `json:"hardware_id,omitempty"`
	ReadAt     time.Time `json:"read_at"`
	Notes      []string  `json:"notes,omitempty"`
}

// Mutation vocabulary: what a write call actually changed. Partial
// and unknown are honest outcomes, not successes with fine print -
// exit 0 from the external tool does NOT mean the profile matches.
const (
	MutatedNone      = "none"
	MutatedCompleted = "completed"
	MutatedPartial   = "partial"
	MutatedUnknown   = "unknown"
)

// WriteResult preserves the observation alongside any error: a
// partial apply with an error still says which offsets are confirmed
// applied, because the restore path needs exactly that.
type WriteResult struct {
	Mutated string   `json:"mutated"`
	Applied []Offset `json:"applied,omitempty"`
	Failed  []Offset `json:"failed,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// Verification is a readback comparison. Match unknown is a real
// answer (the backend cannot read back), not a failure and not a
// pass.
type Verification struct {
	Match TriState `json:"match"`
	Diff  []Offset `json:"diff,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// Backend is the swappable CO contract. Every method must honor ctx
// cancellation; every returned struct must keep the honesty rules
// above even on the error paths.
type Backend interface {
	Probe(ctx context.Context) (Capabilities, error)
	Topology(ctx context.Context) (Mapping, error)
	Read(ctx context.Context) (Snapshot, error)
	Validate(p Profile) error
	Apply(ctx context.Context, p Profile) (WriteResult, error)
	Verify(ctx context.Context, p Profile) (Verification, error)
	Restore(ctx context.Context, s Snapshot) (Verification, error)
}
