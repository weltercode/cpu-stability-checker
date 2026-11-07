package co

import (
	"errors"
	"fmt"
)

// Error kinds pin the failure branches the P10 plan requires tests
// for. They are stable strings (journal/JSON-safe); the wrapped cause
// carries the detail. Callers branch on Kind, never on message text.
const (
	KindUnsupportedCPU   = "unsupported_cpu"     // this backend does not know this CPU
	KindMissingTool      = "missing_tool"        // executable not present/not executable
	KindMissingDriver    = "missing_driver"      // required component absent
	KindNoPrivilege      = "no_privilege"        // elevation/driver access refused
	KindMalformedOutput  = "malformed_output"    // answer not in a known format - never guessed
	KindOffsetCount      = "offset_count"        // wrong number of offsets in a read/answer
	KindBadProfile       = "bad_profile"         // unaddressable core, bad format, out of probed range
	KindTimeout          = "timeout"             // bounded runtime exceeded
	KindReadbackMismatch = "readback_mismatch"   // readback disagrees with what was applied
	KindPartialApply     = "partial_apply"       // some offsets applied, others did not
	KindCancelled        = "cancelled"           // ctx canceled mid-operation
	KindRestoreRefused   = "restore_refused"     // restore would overwrite foreign changes
	KindFingerprint      = "fingerprint_changed" // hardware identity moved under the snapshot
	KindExternalChange   = "external_change"     // profile differs from what we last verified applied
	KindLockHeld         = "lock_held"           // another CO operation owns the lock
	KindStateCorrupt     = "state_corrupt"       // transaction history damaged/incomplete - writes blocked
	KindRecoveryRequired = "recovery_required"   // an unfinished CO transaction must be recovered before new writes
	KindStorage          = "storage"             // durable state write/read failed - hardware calls gated accordingly
	KindUnsupportedOp    = "unsupported_op"      // backend cannot do this operation at all
	KindNotValidated     = "not_validated"       // write prerequisites (validated range, mapping, probe) not established
)

// Error is the typed CO failure: kind + operation + cause.
type Error struct {
	Kind string
	Op   string // probe | topology | read | validate | apply | verify | restore
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("co %s: %s: %v", e.Op, e.Kind, e.Err)
	}
	return fmt.Sprintf("co %s: %s", e.Op, e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// IsKind reports whether err carries the given CO kind.
func IsKind(err error, kind string) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}

// KindOf extracts the CO kind of an error ("" when it is not a CO
// error) - for honest reporting without string matching.
func KindOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
