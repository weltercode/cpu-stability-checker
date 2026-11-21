package plan

// FinalInput is what an executor reports at run end: the facts, never a
// pre-cooked exit code. One classification rule set lives here, so the
// threaded and the subprocess engine can never drift apart.
type FinalInput struct {
	// Execution is the run-level ending (completed, cancelled,
	// setup_error, runtime_error). Per-unit endings go in Units.
	Execution Execution
	// Reason is the human-readable cause of an early stop (feeds the
	// compatibility AbortMsg).
	Reason string
	// Units are the planned slots with their ending and confirmed
	// check counts.
	Units []UnitInput
	// Findings are the observed facts (faults, WHEA, setup errors...).
	Findings []Finding
}

// UnitInput is one planned slot as the executor saw it end.
type UnitInput struct {
	Execution Execution
	// Checks counts confirmed verified checks (self-checking
	// iterations). Never timer beats.
	Checks uint64
	// ChecksKnown is false when the count cannot be known from our
	// side (external tools): unknown is not zero.
	ChecksKnown bool
	// VerificationRequired marks the slot's checks as part of the
	// declared contract (a real workload, not idle or pure load).
	VerificationRequired bool
	// Evidence carries what an external adapter's protocol proved for
	// this slot (workload's Ev* vocabulary as a string; "" for
	// built-in workloads, which verify by their own counts). Unknown
	// evidence is never promoted to confirmed, and confirmed only
	// ever comes from a protocol the adapter parses.
	Evidence string
	// Mem is the slot's memory-engine accounting, translated from the
	// engine's own mem_done record by the executor. nil = the slot
	// never reported accounting (no engine, a killed child, an older
	// journal): the run's memory coverage for this slot stays
	// unknown, and Finalize counts it in UnknownSlots - never zero.
	Mem *MemAccounting
}

// unitMem collects the reported memory accounting of the units for
// FoldRAM; a unit with nil Mem is unreported and counted by the
// unit count minus the reported one, never folded in as a zero.
func unitMem(units []UnitInput) []*MemAccounting {
	observed := make([]*MemAccounting, 0, len(units))
	for _, u := range units {
		if u.Mem != nil {
			observed = append(observed, u.Mem)
		}
	}
	return observed
}

// FoldRAM folds per-slot memory-engine accounting into run-level
// facts (R29's rule; R34 makes it the ONLY entry point: the one-shot
// executor reaches it through Finalize's units, the session-shape
// executors through a scan of their children's own journal records -
// nobody rebuilds the arithmetic). observed holds one entry per slot
// whose accounting was reported; unreportedSlots counts planned slots
// whose accounting was never observed - absence is unknown, never
// folded in as a zero. A run where nobody reported returns nil: no
// block, no zeros; absence of the block means "not observed", not
// "the engine touched nothing". The sums are cumulative byte-work;
// LastPass is a floor over the slots that PROVED a pass volume. None
// of this is a claim about unique physical-RAM coverage.
func FoldRAM(observed []*MemAccounting, unreportedSlots int) *RAMTotals {
	if len(observed) == 0 {
		return nil
	}
	if unreportedSlots < 0 {
		unreportedSlots = 0
	}
	mem := &RAMTotals{EngineSlots: len(observed), UnknownSlots: unreportedSlots}
	for _, u := range observed {
		mem.Requested += u.Requested
		mem.Allocated += u.Allocated
		mem.Touched += u.Touched
		mem.Verified += u.Verified
		if u.VerifiedUnique != nil {
			mem.LastPass += *u.VerifiedUnique
		}
	}
	return mem
}

// Finalize derives the full Result, including the compatibility view,
// from the reported facts. Exit semantics (stable contract):
//
//	0 — the declared contract of the mode was fulfilled
//	1 — a fault was found, the run was cancelled/stopped early, or a
//	    mandatory verification did not complete
//	2 — configuration/tool/setup error blocking a valid run
//
// (130 stays on the signal path, which force-quits before any Result.)
//
// Faults only ever collects findings with confirmed logical-CPU
// attribution; run-level findings (a shared external process faulting)
// decide the exit code but blacklist nobody. A06: a memory-region
// finding (AttrMemoryRegion, observed_on a CPU) indicts the memory,
// not the CPU that happened to observe it - it decides the exit code
// but never enters this map.
func Finalize(in FinalInput) Result {
	res := Result{Execution: in.Execution, Findings: in.Findings}

	unknownEvidence := false
	for _, u := range in.Units {
		res.Coverage.Planned++
		if u.Execution != ExecSkipped {
			res.Coverage.Started++
		}
		if u.Execution == ExecCompleted {
			res.Coverage.Completed++
		}
		if u.VerificationRequired {
			if u.ChecksKnown {
				res.Coverage.Verified += u.Checks
			} else {
				unknownEvidence = true
			}
		}
	}
	// Coverage checks are only "known" when every required unit could
	// report them; one external slot makes the whole count unknown.
	res.Coverage.ChecksKnown = !unknownEvidence

	// Memory-engine accounting (R29): the fold runs through FoldRAM,
	// the single rule every executor uses (R34: the session-shape
	// executors fold the same facts from their journal scan WITHOUT
	// rebuilding the arithmetic - two formulas would be two chances
	// to drift).
	observed := unitMem(in.Units)
	res.Coverage.Mem = FoldRAM(observed, len(in.Units)-len(observed))

	// Faults: confirmed per-CPU findings only.
	for _, f := range in.Findings {
		if f.Attribution.Kind != AttrLogicalCPU || f.Attribution.Confidence != ConfConfirmed {
			continue
		}
		switch f.Kind {
		case FindDataMismatch, FindToolFailure, FindSyntheticProbe:
			if f.Attribution.Index == nil {
				continue
			}
			if res.Faults == nil {
				res.Faults = make(map[int]string)
			}
			if _, seen := res.Faults[*f.Attribution.Index]; !seen {
				res.Faults[*f.Attribution.Index] = f.Message
			}
		}
	}

	res.Validation = validation(in, unknownEvidence)

	// Exit classification, one rule set for both engines.
	setup := in.Execution == ExecSetupError
	for _, u := range in.Units {
		if u.Execution == ExecSetupError {
			setup = true
		}
	}
	faulted := len(res.Faults) > 0
	for _, f := range in.Findings {
		switch f.Kind {
		// FindPCIeObs is deliberately absent: a corrected PCIe WHEA
		// record is evidence, recorded separately, but per plan P02 it
		// does not condemn a CPU stability run.
		case FindDataMismatch, FindToolFailure, FindSyntheticProbe, FindWHEA, FindWHEAUnknown, FindThermalStop, FindSensorLoss:
			faulted = true
		}
	}
	// A required slot that did not reach its end is an incomplete
	// mandatory check even when the run itself "completed".
	missedRequired := false
	for _, u := range in.Units {
		if u.VerificationRequired && u.Execution != ExecCompleted && u.Execution != ExecSkipped {
			missedRequired = true
		}
	}
	// A required slot that completed its window but confirmed zero
	// checks never fulfilled its verification contract either: the
	// checks it promised simply never happened, so the run as a whole
	// did not complete a mandatory verification. An external slot
	// without a countable check total is NOT this - its contract was
	// to run the tool, and the validation claim says unknown instead
	// of a pass (the deliberate exit-0-with-unknown contract).
	unverifiedRequired := false
	for _, u := range in.Units {
		if u.VerificationRequired && u.ChecksKnown && u.Execution == ExecCompleted && u.Checks == 0 {
			unverifiedRequired = true
		}
	}
	switch {
	case setup:
		res.Exit = 2
	case faulted, missedRequired, unverifiedRequired, in.Execution != ExecCompleted:
		// A fault, a cancellation, a runtime stop or any unit that
		// did not complete its window: never a clean 0.
		res.Exit = 1
	default:
		res.Exit = 0
	}
	res.Aborted = in.Execution != ExecCompleted && in.Execution != ExecSkipped
	res.AbortMsg = in.Reason
	return res
}

// CombineRAM folds per-run memory accounting into one attempt-level
// view (R29: the receipt and the run summary must show the byte
// totals without anybody re-reading journals). Slots stay separate:
// each run accounted its own slots, so summing across a phase list
// sums distinct slot-settles and never double-counts one settle. A
// run with no accounting at all is not silently zero: once the fold
// exists at all, that run's PLANNED slots join UnknownSlots - their
// accounting was not observed, stated as unknown, never as zeros and
// never carrying invented bytes. Returns nil when NO run reported
// anything - the block's absence then honestly means "not observed",
// exactly like a nil Coverage.Mem.
func CombineRAM(results []Result) *RAMTotals {
	var out *RAMTotals
	var unobserved int // planned slots of runs whose accounting was never observed
	for i := range results {
		m := results[i].Coverage.Mem
		if m == nil {
			unobserved += results[i].Coverage.Planned
			continue
		}
		if out == nil {
			cp := *m
			out = &cp
			continue
		}
		out.EngineSlots += m.EngineSlots
		out.UnknownSlots += m.UnknownSlots
		out.Requested += m.Requested
		out.Allocated += m.Allocated
		out.Touched += m.Touched
		out.Verified += m.Verified
		out.LastPass += m.LastPass
	}
	if out != nil {
		out.UnknownSlots += unobserved
	}
	return out
}

// validation applies the honesty rules PER UNIT, never on the aggregate:
// one slot's checks can never "cover" another required slot that
// confirmed nothing. verified needs EVERY required slot to have
// completed AND confirmed its own checks; missing evidence caps the
// claim at partial (or unknown when nothing was confirmed); a mode that
// declared no checking says so instead of faking a pass. The one
// promotion above unknown: when every required slot's external protocol
// evidence is "confirmed-complete" (the workload package's EvConfirmed
// vocabulary, compared as a string so the model layer stays
// dependency-light) and every slot completed, the tool's own
// verification is confirmed - still not our verified count, so it gets
// its own honest kind. A known-count slot mixed with external slots
// never promotes: different standards do not merge.
func validation(in FinalInput, unknownEvidence bool) Validation {
	required := 0
	allRequiredCompleted := true
	verifiedSlots := 0 // completed required slots that confirmed their own checks
	confirmedAny := false
	for _, u := range in.Units {
		if !u.VerificationRequired {
			continue
		}
		required++
		if u.Execution != ExecCompleted {
			allRequiredCompleted = false
		}
		if u.ChecksKnown && u.Checks > 0 {
			// Checks confirmed before a later fault still count as
			// confirmed observations - they just cannot make the slot
			// a fulfilled contract.
			confirmedAny = true
			if u.Execution == ExecCompleted {
				verifiedSlots++
			}
		}
	}
	if required == 0 {
		return ValNotPerformed
	}
	if !unknownEvidence {
		if confirmedAny {
			if allRequiredCompleted && verifiedSlots == required {
				return ValVerified
			}
			return ValPartial
		}
		return ValNotPerformed
	}
	// unknownEvidence: at least one required slot could not report a
	// count from our side (an external tool).
	if allRequiredCompleted {
		confirmed := true
		for _, u := range in.Units {
			if u.VerificationRequired && u.Evidence != "confirmed-complete" {
				confirmed = false
				break
			}
		}
		if confirmed {
			return ValExternalConfirmed
		}
	}
	if confirmedAny {
		return ValPartial
	}
	return ValUnknown
}
