// Package cotx is the durable CO transaction engine (P10c): exclusive
// OS-backed ownership (internal/lockfile), a durable transaction
// document written with confirmed flush BEFORE any hardware call, and
// recovery that always re-derives the truth from a fresh read instead
// of trusting its own last words.
//
// Durability contract, stated exactly (Sol checkpoint S1):
//   - Process crash: durable. The document is written to a unique
//     temp file, Synced, then renamed over the target; a reader sees
//     either the complete old document or the complete new one, and
//     a failed replacement preserves the prior document.
//   - Power loss / OS crash: NOT guaranteed. Rename metadata can be
//     lost with no directory-flush primitive on Windows (and POSIX
//     would need one), so after power loss the document may be
//     missing or stale even though a write reached the SMU. Recovery
//     therefore never concludes "never applied" from a missing or
//     stale document: it compares the live hardware state and says
//     what it can prove. There is no hardware compare-and-swap:
//     foreign tuning tools ignore this lock, so the engine rechecks
//     identity and profile immediately before every restore and
//     stops rather than racing.
//
// The document is NOT the best-effort run journal: it is a strict,
// versioned, single-document store (ParseDoc refuses unknown schema
// versions, unknown fields and malformed shapes), and every state
// transition is one atomic replacement.
package cotx

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// DocVersion is the transaction-document schema version. It is
// separate from co.StateVersion: the envelope (status machine, ops,
// fingerprints) and the payload DTO evolve independently, and both
// gates are checked on read.
const DocVersion = 1

// Status values on disk. The machine is:
//
//	apply_pending -> applied_verified -> restore_pending -> restored
//	                                  \-> restored_failed (recovery may retry)
//	apply_pending (outcome fields carry partial/unknown results -
//	              a separate status would add nothing recovery does
//	              not re-derive from a fresh read anyway)
//	closed_no_change (terminal, recovery-observed: current profile
//	                  equals the saved original; whether Apply ever
//	                  landed is NOT claimed either way)
//
// "prepared" exists only in memory: the first durable state is
// apply_pending, always written and Synced before backend.Apply.
const (
	StatusApplyPending    = "apply_pending"
	StatusAppliedVerified = "applied_verified"
	StatusRestorePending  = "restore_pending"
	StatusRestored        = "restored"
	StatusRestoredFailed  = "restored_failed"
	StatusClosedNoChange  = "closed_no_change"
)

func knownStatus(s string) bool {
	switch s {
	case StatusApplyPending, StatusAppliedVerified, StatusRestorePending,
		StatusRestored, StatusRestoredFailed, StatusClosedNoChange:
		return true
	}
	return false
}

// terminal statuses are never re-executed by recovery: the
// transaction is closed and a new Run may start over the document.
func terminal(s string) bool { return s == StatusRestored || s == StatusClosedNoChange }

// OpRec records one backend operation: what ran, when, and the
// machine-readable failure kind if it failed ("" = succeeded).
type OpRec struct {
	Op     string    `json:"op"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// TxDoc is the durable transaction document: envelope (id, version,
// status), the co.State payload (capabilities with reasons, before
// snapshot with fingerprint, desired profile, write result, readback
// verdict), the full observed snapshot after the apply, and the
// per-operation ledger.
type TxDoc struct {
	DocVersion int          `json:"doc_version"`
	TxID       string       `json:"tx_id"`
	Status     string       `json:"status"`
	UpdatedAt  time.Time    `json:"updated_at"`
	MappingFP  string       `json:"mapping_fp"`
	State      co.State     `json:"state"`
	Observed   *co.Snapshot `json:"observed,omitempty"`
	Ops        []OpRec      `json:"ops,omitempty"`
}

// docWire avoids MarshalJSON recursion via a local type conversion.
func (d TxDoc) MarshalJSON() ([]byte, error) {
	d.DocVersion = DocVersion
	type docNoMethod TxDoc
	return json.Marshal(docNoMethod(d))
}

// ParseDoc reads one document strictly: missing or unknown
// DocVersion, unknown fields, unknown status, empty tx id or a
// state payload this build cannot read are all errors. A damaged
// document is never partially trusted and never auto-repaired.
func ParseDoc(data []byte) (TxDoc, error) {
	var probe struct {
		DocVersion *int `json:"doc_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return TxDoc{}, fmt.Errorf("co tx: not a JSON object: %w", err)
	}
	if probe.DocVersion == nil {
		return TxDoc{}, fmt.Errorf("co tx: missing doc_version field")
	}
	if *probe.DocVersion != DocVersion {
		return TxDoc{}, fmt.Errorf("co tx: unsupported doc_version %d (this build writes %d)", *probe.DocVersion, DocVersion)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var d TxDoc
	if err := dec.Decode(&d); err != nil {
		return TxDoc{}, fmt.Errorf("co tx: version %d document unreadable: %w", DocVersion, err)
	}
	if d.DocVersion != DocVersion {
		return TxDoc{}, fmt.Errorf("co tx: doc_version drifted mid-decode")
	}
	if d.TxID == "" {
		return TxDoc{}, fmt.Errorf("co tx: empty tx_id")
	}
	if !knownStatus(d.Status) {
		return TxDoc{}, fmt.Errorf("co tx: unknown status %q", d.Status)
	}
	if d.State.Version != co.StateVersion {
		return TxDoc{}, fmt.Errorf("co tx: state payload version %d unreadable by this build", d.State.Version)
	}
	return d, nil
}

// profileEqual compares profiles as sets of core->value, ignoring
// order: the tool and the fake both address cores by id, and a
// reordered list is the same state.
func profileEqual(a, b co.Profile) bool {
	if len(a.Offsets) != len(b.Offsets) {
		return false
	}
	am := map[string]int{}
	for _, o := range a.Offsets {
		if _, dup := am[o.Core]; dup {
			return false // duplicate cores are not a state we compare
		}
		am[o.Core] = o.Value
	}
	for _, o := range b.Offsets {
		v, ok := am[o.Core]
		if !ok || v != o.Value {
			return false
		}
	}
	return true
}

// mappingFP fingerprints the established mapping (tool ids, SMU
// indices, quality): a changed topology or tool enumeration changes
// it, and recovery refuses to act across such drift.
func mappingFP(m co.Mapping) string {
	parts := make([]string, 0, len(m.Slots)+1)
	parts = append(parts, "q:"+m.Quality)
	for _, s := range m.Slots {
		smu := "?"
		if s.SMUIndex != nil {
			smu = fmt.Sprintf("%d", *s.SMUIndex)
		}
		parts = append(parts, s.ToolID+"="+smu)
	}
	sort.Strings(parts[1:])
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// newTxID is a random correlation id for one transaction.
func newTxID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A random source failure is not fatal to honesty: the id
		// correlates records, it never gates a decision. Fall back
		// to the clock and say so through the value's shape.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
