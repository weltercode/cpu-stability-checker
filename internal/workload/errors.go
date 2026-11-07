package workload

import "errors"

// SetupError means that the workload could not be executed reliably. It
// is not evidence of a calculation fault on the CPU running the workload.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

func NewSetupError(err error) error {
	if err == nil || IsSetupError(err) {
		return err
	}
	return &SetupError{Err: err}
}

func IsSetupError(err error) bool {
	var setup *SetupError
	return errors.As(err, &setup)
}

// External tools must explicitly identify a fault (exit code or output
// match); every other external failure is an execution/setup error.
type externalFault struct{ error }

// IsExternalFault reports whether the external tool itself identified a
// hardware fault (a configured fault_exit code or a fault_pattern
// match). Anything else from a tool is a setup/runtime problem, never
// evidence about a CPU.
func IsExternalFault(err error) bool {
	var f externalFault
	return errors.As(err, &f)
}

// SharedRunError wraps the outcome of the one process a SingleInstance
// workload owns so every follower slot reports the SAME run-level fact
// instead of sixteen per-CPU faults: one process cannot prove sixteen
// separate culprits. ID names the shared process for deduplication;
// Err keeps the underlying classification (external fault vs setup)
// reachable through Unwrap.
type SharedRunError struct {
	ID  string
	Err error
}

func (e *SharedRunError) Error() string { return e.Err.Error() }
func (e *SharedRunError) Unwrap() error { return e.Err }

// SharedRunOf extracts the shared-process outcome when err came from one.
func SharedRunOf(err error) (*SharedRunError, bool) {
	var sh *SharedRunError
	if errors.As(err, &sh) {
		return sh, true
	}
	return nil, false
}
