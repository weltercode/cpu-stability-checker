package co

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// Versioned recovery DTO (audit B11): the published live-JSON shapes
// (TriState as bool/null, Capabilities as printed by `co probe`) are
// NOT a durable recovery format - bool/null cannot carry the reason
// an answer is what it is. This DTO is the format the transaction
// state file uses: an explicit version, TriState as an object with
// its reason, and the snapshots/mutation details a restore needs.
// It reads back exactly what it wrote; a file it cannot understand
// is an error, never a silent default.
const StateVersion = 1

// TriStateV1 is the DTO rendering of TriState: the reason survives
// the round trip here, unlike the published bool/null form.
type TriStateV1 struct {
	Known bool   `json:"known"`
	Yes   bool   `json:"yes"`
	Why   string `json:"why,omitempty"`
}

func triToV1(t TriState) TriStateV1 { return TriStateV1{Known: t.Known, Yes: t.Yes, Why: t.Why} }
func (v TriStateV1) tri() TriState  { return TriState{Known: v.Known, Yes: v.Yes, Why: v.Why} }

// CapabilitiesV1 mirrors Capabilities with tri-fields as objects.
type CapabilitiesV1 struct {
	Tool        string     `json:"tool"`
	Version     string     `json:"version"`
	SHA256      string     `json:"sha256,omitempty"`
	CPU         string     `json:"cpu,omitempty"`
	Supported   TriStateV1 `json:"supported"`
	Read        TriStateV1 `json:"read"`
	Apply       TriStateV1 `json:"apply"`
	Readback    TriStateV1 `json:"readback"`
	RangeKnown  bool       `json:"range_known"`
	RangeMin    int        `json:"range_min,omitempty"`
	RangeMax    int        `json:"range_max,omitempty"`
	SyntaxKnown bool       `json:"syntax_known,omitempty"`
	SyntaxMin   int        `json:"syntax_min,omitempty"`
	SyntaxMax   int        `json:"syntax_max,omitempty"`
	// PayloadSHA256 is additive in V1 (absent = never measured, the
	// pre-C02 honest unknown), mirroring Capabilities.
	PayloadSHA256  string   `json:"payload_sha256,omitempty"`
	BuildValidated bool     `json:"build_validated,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

func capsToV1(c Capabilities) CapabilitiesV1 {
	return CapabilitiesV1{
		Tool: c.Tool, Version: c.Version, SHA256: c.SHA256, CPU: c.CPU,
		Supported: triToV1(c.Supported), Read: triToV1(c.Read),
		Apply: triToV1(c.Apply), Readback: triToV1(c.Readback),
		RangeKnown: c.RangeKnown, RangeMin: c.RangeMin, RangeMax: c.RangeMax,
		SyntaxKnown: c.SyntaxKnown, SyntaxMin: c.SyntaxMin, SyntaxMax: c.SyntaxMax,
		PayloadSHA256:  c.PayloadSHA256,
		BuildValidated: c.BuildValidated,
		Notes:          c.Notes,
	}
}

func (v CapabilitiesV1) caps() Capabilities {
	return Capabilities{
		Tool: v.Tool, Version: v.Version, SHA256: v.SHA256, CPU: v.CPU,
		Supported: v.Supported.tri(), Read: v.Read.tri(),
		Apply: v.Apply.tri(), Readback: v.Readback.tri(),
		RangeKnown: v.RangeKnown, RangeMin: v.RangeMin, RangeMax: v.RangeMax,
		SyntaxKnown: v.SyntaxKnown, SyntaxMin: v.SyntaxMin, SyntaxMax: v.SyntaxMax,
		PayloadSHA256:  v.PayloadSHA256,
		BuildValidated: v.BuildValidated,
		Notes:          v.Notes,
	}
}

// VerificationV1 mirrors Verification (its Match is a TriState).
type VerificationV1 struct {
	Match TriStateV1 `json:"match"`
	Diff  []Offset   `json:"diff,omitempty"`
	Notes []string   `json:"notes,omitempty"`
}

// State is one versioned CO transaction record: what the machine
// looked like before (snapshot + fingerprint), what was asked for,
// what the write call reported, and what the readback said. P10c's
// state file is exactly this, and ParseState refuses anything it was
// not built to read.
type State struct {
	Version    int            `json:"version"`
	SavedAt    time.Time      `json:"saved_at"`
	HardwareID string         `json:"hardware_id,omitempty"`
	Caps       CapabilitiesV1 `json:"caps"`
	Before     Snapshot       `json:"before"`
	Desired    Profile        `json:"desired"`
	Result     WriteResult    `json:"result"`
	Verify     VerificationV1 `json:"verify"`
}

// NewState stamps the current version and converts the live shapes.
func NewState(caps Capabilities, before Snapshot, desired Profile, res WriteResult, ver Verification) State {
	return State{
		Version:    StateVersion,
		SavedAt:    time.Now(),
		HardwareID: before.HardwareID,
		Caps:       capsToV1(caps),
		Before:     before,
		Desired:    desired,
		Result:     res,
		Verify:     VerificationV1{Match: triToV1(ver.Match), Diff: ver.Diff, Notes: ver.Notes},
	}
}

// Capabilities decodes the stored capabilities back to live shape.
func (s State) Capabilities() Capabilities { return s.Caps.caps() }

// Verification decodes the stored readback verdict back to live shape.
func (s State) Verification() Verification {
	return Verification{Match: s.Verify.Match.tri(), Diff: s.Verify.Diff, Notes: s.Verify.Notes}
}

// MarshalJSON stamps the current version on every write: a
// hand-built State can never produce an unstamped file. The local
// type conversion drops the method to avoid marshal recursion.
func (s State) MarshalJSON() ([]byte, error) {
	s.Version = StateVersion
	type stateNoMethod State
	return json.Marshal(stateNoMethod(s))
}

// ParseState reads one state document. A missing version is not
// "version 1": today no older format exists, and guessing would
// silently misread a future or hand-edited file. An unknown version
// is refused, never partially decoded.
func ParseState(data []byte) (State, error) {
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return State{}, fmt.Errorf("co state: not a JSON object: %w", err)
	}
	if probe.Version == nil {
		return State{}, fmt.Errorf("co state: missing version field")
	}
	if *probe.Version != StateVersion {
		return State{}, fmt.Errorf("co state: unsupported version %d (this build reads %d)", *probe.Version, StateVersion)
	}
	var st State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return State{}, fmt.Errorf("co state: version %d document unreadable: %w", StateVersion, err)
	}
	if st.Version != StateVersion {
		return State{}, fmt.Errorf("co state: version %d document unreadable: version drifted mid-decode", StateVersion)
	}
	return st, nil
}

// RestorePermission gates a restore BEFORE any write (audit B03: an
// incomplete snapshot must never gain restore permission; unknown
// identity or unvalidated support blocks too). It returns "" when a
// restore may be attempted, otherwise the first honest reason it may
// not. The caller still re-reads and compares: this gate proves the
// saved record is trustworthy, not that the hardware is unchanged.
func (s State) RestorePermission() (bool, string) {
	switch {
	case s.Version != StateVersion:
		return false, fmt.Sprintf("state version %d is not readable by this build", s.Version)
	case s.Before.Quality != "exact":
		return false, fmt.Sprintf("saved snapshot quality is %q: only an exact snapshot may be restored", s.Before.Quality)
	case s.Before.HardwareID == "":
		return false, "saved snapshot carries no hardware fingerprint: identity was never confirmed"
	case len(s.Before.Profile.Offsets) == 0:
		return false, "saved snapshot holds no offsets: there is nothing to restore"
	case s.Caps.Read.Known && !s.Caps.Read.Yes:
		return false, "backend read was known-unavailable when saved: readback cannot confirm a restore"
	case s.Caps.Supported.Known && !s.Caps.Supported.Yes:
		return false, "backend support was known-unavailable when saved"
	case !s.Caps.RangeKnown:
		return false, "hardware-validated CO range was never established: writes stay refused"
	default:
		return true, ""
	}
}
