package plan

import (
	"fmt"
	"sort"
	"strings"
)

// VerdictKind is the P13 result vocabulary: every finished run reads as
// exactly one word, built ONLY from the structured fields of a Result -
// Execution, Validation, Coverage, Findings, Exit - never by parsing
// human-readable text.
type VerdictKind string

const (
	// VerdictPass: all required load completed and the declared
	// verification contract was fulfilled.
	VerdictPass VerdictKind = "PASS"
	// VerdictFail: a hardware or diagnostic fault was CONFIRMED by a
	// finding: a data mismatch, a WHEA hardware error record, or the
	// synthetic probe's injected fault.
	VerdictFail VerdictKind = "FAIL"
	// VerdictIncomplete: the run ended early for a run-control reason
	// - monitor cut-off, tool/worker failure, sensor loss, setup
	// problem, cancellation - and no hardware fault was confirmed.
	VerdictIncomplete VerdictKind = "INCOMPLETE"
	// VerdictUnverified: the load ran to its end but the correctness
	// contract was not confirmed: zero verified checks, partial or
	// unknowable validation, unadjudicated WHEA evidence.
	VerdictUnverified VerdictKind = "UNVERIFIED"
	// VerdictUnknown is reserved for a result RECORD that is missing,
	// corrupt or foreign. A run that reports an exit code, findings or
	// finished phases is never unknown.
	VerdictUnknown VerdictKind = "UNKNOWN"
)

// Verdict pairs the vocabulary word with the structured evidence that
// produced it, so every report line can show the reason too.
type Verdict struct {
	Kind   VerdictKind
	Reason string
}

// String renders "WORD - reason" (or just the word when there is no
// structured reason beyond the word itself).
func (v Verdict) String() string {
	if v.Reason == "" {
		return string(v.Kind)
	}
	return string(v.Kind) + ": " + v.Reason
}

// VerdictSeverity orders the words for aggregation. Hardware evidence
// outranks run-control problems, which outrank unverified loads.
func VerdictSeverity(k VerdictKind) int {
	switch k {
	case VerdictFail:
		return 4
	case VerdictIncomplete:
		return 3
	case VerdictUnverified:
		return 2
	case VerdictPass:
		return 1
	default:
		return 0
	}
}

// WorstVerdict folds verdicts from several phases into the scenario
// word: FAIL > INCOMPLETE > UNVERIFIED > PASS. UNKNOWN never wins by
// itself - a missing record is a reporting problem, not a run outcome.
func WorstVerdict(kinds ...VerdictKind) VerdictKind {
	best := VerdictPass
	for _, k := range kinds {
		if VerdictSeverity(k) > VerdictSeverity(best) {
			best = k
		}
	}
	return best
}

// Verdict classifies the run. The order of the rules below is the
// contract: confirmed findings first, then how the run ended, then the
// verification contract - so a mismatch inside a cancelled run still
// reads FAIL, and a stopped run that verified everything still reads
// INCOMPLETE, never PASS.
func (r *Result) Verdict() Verdict {
	if r == nil {
		return Verdict{Kind: VerdictUnknown, Reason: "no result record"}
	}
	var mismatch, wheaRec, wheaUnknown, synthetic, tool, sensor, thermal int
	for _, f := range r.Findings {
		switch f.Kind {
		case FindDataMismatch:
			mismatch++
		case FindWHEA:
			wheaRec++
		case FindWHEAUnknown:
			wheaUnknown++
		case FindSyntheticProbe:
			synthetic++
		case FindToolFailure:
			tool++
		case FindSensorLoss:
			sensor++
		case FindThermalStop:
			thermal++
		}
	}

	// 1. Confirmed hardware/diagnostic evidence decides FAIL. A
	// synthetic probe is the pipeline's own injected fault: FAIL with
	// the reason stated so no report ever mistakes it for hardware.
	switch {
	case mismatch > 0:
		return Verdict{VerdictFail, fmt.Sprintf("%d confirmed data mismatch(es)", mismatch)}
	case wheaRec > 0:
		return Verdict{VerdictFail, fmt.Sprintf("%d WHEA hardware error record(s) observed during the run", wheaRec)}
	case synthetic > 0:
		return Verdict{VerdictFail, fmt.Sprintf("%d synthetic probe finding(s): an injected fault proves the detection pipeline, not a hardware defect", synthetic)}
	}

	// Compatibility shape: a Result carrying the derived Faults map
	// but no Findings list (older engines and hand-built fixtures).
	// Finalize fills Faults only from confirmed per-CPU fault
	// findings, so treat its presence as confirmed fault evidence.
	if len(r.Faults) > 0 && len(r.Findings) == 0 {
		core, msg := "", ""
		cores := make([]int, 0, len(r.Faults))
		for c := range r.Faults {
			cores = append(cores, c)
		}
		sort.Ints(cores)
		core, msg = fmt.Sprintf("%d", cores[0]), r.Faults[cores[0]]
		for _, c := range cores[1:] {
			core += fmt.Sprintf(", %d", c)
		}
		return Verdict{VerdictFail, fmt.Sprintf("%d faulted core(s) [cpu %s]: %s", len(r.Faults), core, msg)}
	}

	// 2. Run-control endings decide INCOMPLETE - the stop itself, not
	// a fault verdict, unless rule 1 already fired.
	switch r.Execution {
	case ExecSetupError:
		return Verdict{VerdictIncomplete, "setup/tool problem blocked a valid run (exit 2)"}
	case ExecCancelled:
		if r.Exit == 130 {
			return Verdict{VerdictIncomplete, "run stopped by the operator (exit 130)"}
		}
		return Verdict{VerdictIncomplete, "run stopped by the operator (cancelled)"}
	case ExecInterrupted:
		return Verdict{VerdictIncomplete, "run stopped as collateral of an earlier stop in the same scenario"}
	case ExecRuntimeError:
		if reasons := infraReasons(tool, sensor, thermal); reasons != "" {
			return Verdict{VerdictIncomplete, reasons}
		}
		if strings.TrimSpace(r.AbortMsg) != "" {
			return Verdict{VerdictIncomplete, firstLine(r.AbortMsg)}
		}
		return Verdict{VerdictIncomplete, "run stopped early by a runtime condition (exit 1)"}
	}
	// Findings that report a broken monitor or tool outrank a quiet
	// contract judgement even when the execution claims "completed".
	if reasons := infraReasons(tool, sensor, thermal); reasons != "" {
		return Verdict{VerdictIncomplete, reasons + " reported while the load ended"}
	}

	// 3. Non-zero exit with a completed execution: the tool rejected
	// the outcome. This is the "0 verified checks" family - the load
	// finished, the correctness contract did not.
	if r.Exit != 0 {
		if r.Coverage.Started == 0 {
			return Verdict{VerdictIncomplete, "no load unit started"}
		}
		if r.Coverage.ChecksKnown && r.Coverage.Verified == 0 {
			return Verdict{VerdictUnverified, fmt.Sprintf("load finished but 0 verified checks (%d of %d units completed, exit %d)", r.Coverage.Completed, r.Coverage.Started, r.Exit)}
		}
		return Verdict{VerdictUnverified, fmt.Sprintf("verification contract not met: %d verified checks, validation=%s (exit %d)", r.Coverage.Verified, r.Validation, r.Exit)}
	}

	// 4. Clean exit: judge the contract. Unadjudicated WHEA evidence
	// blocks PASS: the run cannot claim stability on records nobody
	// classified.
	if wheaUnknown > 0 {
		return Verdict{VerdictUnverified, fmt.Sprintf("%d unrecognized WHEA record(s) require adjudication before any stability claim", wheaUnknown)}
	}
	switch r.Validation {
	case ValVerified:
		return Verdict{VerdictPass, fmt.Sprintf("all required load completed and verified: %d confirmed checks across %d unit(s)", r.Coverage.Verified, r.Coverage.Completed)}
	case ValExternalConfirmed:
		return Verdict{VerdictPass, "every external tool's own protocol confirmed its verification ran to completion"}
	case ValPartial:
		return Verdict{VerdictUnverified, fmt.Sprintf("only part of the required load confirmed its checks: %d verified checks", r.Coverage.Verified)}
	case ValUnknown:
		return Verdict{VerdictUnverified, "verification cannot be known from our side (external tool without confirmed protocol evidence)"}
	default: // ValNotPerformed
		return Verdict{VerdictUnverified, "load finished with no data verification declared or performed"}
	}
}

// infraReasons names the run-control causes behind an INCOMPLETE, in
// stable word order; "" when none were reported.
func infraReasons(tool, sensor, thermal int) string {
	var parts []string
	if tool > 0 {
		parts = append(parts, fmt.Sprintf("%d tool/worker failure(s)", tool))
	}
	if sensor > 0 {
		parts = append(parts, fmt.Sprintf("%d sensor-loss finding(s)", sensor))
	}
	if thermal > 0 {
		parts = append(parts, fmt.Sprintf("%d thermal cut-off(s)", thermal))
	}
	if len(parts) == 0 {
		return ""
	}
	return "stopped by run control: " + strings.Join(parts, " + ")
}

// firstLine trims a multi-line reason to its primary line.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
