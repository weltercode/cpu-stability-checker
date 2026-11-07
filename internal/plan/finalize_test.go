package plan

import "testing"

func TestFinalize(t *testing.T) {
	cases := []struct {
		name       string
		in         FinalInput
		wantExit   int
		wantExec   Execution
		wantVal    Validation
		wantFaults []int // logical CPUs that must be blacklisted
		wantCov    Coverage
		aborted    bool
	}{
		{
			name: "clean verified run",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 100, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecCompleted, Checks: 200, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValVerified,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, Verified: 300, ChecksKnown: true},
		},
		{
			name: "external tool ran to the deadline but verified nothing we can see",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 0, ChecksKnown: false, VerificationRequired: true},
			}},
			wantExit: 0, wantVal: ValUnknown, // the contract (run the tool) was met;
			// exit 0 is legal only because the report says validation=unknown.
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 0, ChecksKnown: false},
		},
		{
			name: "pure load declares no checking",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 0, ChecksKnown: true, VerificationRequired: false},
			}},
			wantExit: 0, wantVal: ValNotPerformed,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 0, ChecksKnown: true},
		},
		{
			name: "idle-only run",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecSkipped, VerificationRequired: false},
			}},
			wantExit: 0, wantVal: ValNotPerformed,
			wantCov: Coverage{Planned: 1, ChecksKnown: true},
		},
		{
			name: "cancelled with real checks already done",
			in: FinalInput{Execution: ExecCancelled, Reason: "user interrupt", Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 50, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecInterrupted, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 1, wantExec: ExecCancelled, wantVal: ValPartial, aborted: true,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 1, Verified: 50, ChecksKnown: true},
		},
		{
			name: "data mismatch on one pinned core is a confirmed fault",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "fault",
				Units: []UnitInput{
					{Execution: ExecRuntimeError, Checks: 7, ChecksKnown: true, VerificationRequired: true},
					{Execution: ExecInterrupted, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindDataMismatch, Source: "workload", Message: "got x want y",
					Attribution: Attribution{Kind: AttrLogicalCPU, Index: IntPtr(5), Confidence: ConfConfirmed}}},
			},
			wantExit: 1, wantExec: ExecRuntimeError, wantVal: ValPartial, wantFaults: []int{5}, aborted: true,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 0, Verified: 7, ChecksKnown: true},
		},
		{
			name: "shared external process faulting is run-level: exit 1, nobody blacklisted",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "tool fault",
				Units: []UnitInput{
					{Execution: ExecRuntimeError, ChecksKnown: false, VerificationRequired: true},
					{Execution: ExecInterrupted, ChecksKnown: false, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindToolFailure, Source: "tool", Message: "y-cruncher reported a fault",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfConfirmed}}},
			},
			wantExit: 1, wantVal: ValUnknown, aborted: true,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 0, ChecksKnown: false},
		},
		{
			name: "setup error blocks a valid run and is never a CPU fault",
			in: FinalInput{Execution: ExecSetupError, Reason: "missing tool",
				Units: []UnitInput{
					{Execution: ExecSetupError, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindSetupError, Source: "workload", Message: "exe not found",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfConfirmed}}},
			},
			wantExit: 2, wantExec: ExecSetupError, wantVal: ValNotPerformed, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, ChecksKnown: true},
		},
		{
			name: "whea critical with our checks still complete: verified plus a finding",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "whea",
				Units: []UnitInput{
					{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindWHEA, Source: "whea", Message: "Critical",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfUnknown}}},
			},
			wantExit: 1, wantVal: ValVerified, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			name: "corrected cpu-class WHEA finding without abort: the result is still not clean",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindWHEA, Source: "whea", Message: "corrected memory",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfUnknown}}},
			},
			wantExit: 1, wantExec: ExecCompleted, wantVal: ValVerified,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			name: "corrected PCIe WHEA observation: recorded separately, does not condemn a CPU run",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindPCIeObs, Source: "whea", Message: "corrected pcie",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfUnknown}}},
			},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValVerified,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			name: "unclassified WHEA evidence: adjudication, never an automatic pass",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindWHEAUnknown, Source: "whea", Message: "unknown vendor section",
					Attribution: Attribution{Kind: AttrRun, Confidence: ConfUnknown}}},
			},
			wantExit: 1, wantExec: ExecCompleted, wantVal: ValVerified,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			name: "external adapter confirmed the tool's completion: honest external_confirmed claim",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true, Evidence: "confirmed-complete"},
				},
			},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValExternalConfirmed,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, ChecksKnown: false},
		},
		{
			name: "external evidence unknown stays unknown, never promoted by a clean exit",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true, Evidence: "unknown"},
				},
			},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValUnknown,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, ChecksKnown: false},
		},
		{
			name: "one unconfirmed external slot keeps the whole run unknown",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true, Evidence: "confirmed-complete"},
					{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true, Evidence: "partial"},
				},
			},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValUnknown,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, ChecksKnown: false},
		},
		{
			name: "our verified checks plus confirmed external: capped at partial, different standards never merge",
			in: FinalInput{Execution: ExecCompleted,
				Units: []UnitInput{
					{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
					{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true, Evidence: "confirmed-complete"},
				},
			},
			wantExit: 0, wantExec: ExecCompleted, wantVal: ValPartial,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, Verified: 10, ChecksKnown: false},
		},
		{
			name: "cancelled run with confirmed external evidence: the claim stays unknown",
			in: FinalInput{Execution: ExecCancelled, Reason: "ctrl-c",
				Units: []UnitInput{
					{Execution: ExecInterrupted, ChecksKnown: false, VerificationRequired: true, Evidence: "confirmed-complete"},
				},
			},
			wantExit: 1, wantExec: ExecCancelled, wantVal: ValUnknown, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, ChecksKnown: false},
		},
		{
			// A02: a completed window that confirmed nothing never
			// looks like a pass: the mandatory verification simply
			// did not happen, so the exit says so too.
			name: "required workload ran zero iterations: nothing was confirmed, never a pass",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 0, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 1, wantVal: ValNotPerformed,
			wantCov: Coverage{Planned: 1, Started: 1, Completed: 1, Verified: 0, ChecksKnown: true},
		},
		{
			// A02: one slot's checks must never "cover" another
			// required slot that verified nothing: the aggregate sum
			// used to claim verified here.
			name: "one required slot verified nothing while its peer verified: partial, not verified",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecCompleted, Checks: 0, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 1, wantVal: ValPartial,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, Verified: 10, ChecksKnown: true},
		},
		{
			// A policy-skipped required slot caps the claim (it did
			// not verify) but is not a failure: the scenario chose it.
			name: "required slot skipped by policy: capped at partial, exit stays clean",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecSkipped, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 0, wantVal: ValPartial,
			wantCov: Coverage{Planned: 2, Started: 1, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			// The external exit-0 contract stands only while every
			// countable slot kept its own contract: a built-in slot
			// that confirmed nothing still fails the run.
			name: "zero built-in slot next to an external unknown: the built-in gap decides",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 0, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true},
			}},
			wantExit: 1, wantVal: ValUnknown,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, ChecksKnown: false},
		},
		{
			name: "one unknown slot caps the claim at partial even with real checks",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecCompleted, ChecksKnown: false, VerificationRequired: true},
			}},
			wantExit: 0, wantVal: ValPartial,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 2, Verified: 10, ChecksKnown: false},
		},
		{
			name: "run completed but a required slot was interrupted: incomplete mandatory check",
			in: FinalInput{Execution: ExecCompleted, Units: []UnitInput{
				{Execution: ExecCompleted, Checks: 10, ChecksKnown: true, VerificationRequired: true},
				{Execution: ExecInterrupted, ChecksKnown: true, VerificationRequired: true},
			}},
			wantExit: 1, wantVal: ValPartial,
			wantCov: Coverage{Planned: 2, Started: 2, Completed: 1, Verified: 10, ChecksKnown: true},
		},
		{
			// A06: a mismatch in engine memory indicts the memory,
			// not the CPU that observed it: exit 1, zero blacklist
			// entries, the observer kept only as observed_on.
			name: "memory-region mismatch: exit 1, no CPU blacklisted",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "ram mismatch",
				Units: []UnitInput{
					{Execution: ExecRuntimeError, Checks: 3, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindDataMismatch, Source: "workload", Message: "word differs",
					Attribution: Attribution{Kind: AttrMemoryRegion, Region: "worker=0/region=2", Confidence: ConfConfirmed, ObservedOn: IntPtr(5)}}},
			},
			wantExit: 1, wantVal: ValPartial, wantFaults: nil, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, Verified: 3, ChecksKnown: true},
		},
		{
			// A06: the synthetic prober keeps its documented soak
			// contract (it blacklists to prove the pipeline) but its
			// kind says "probe", never "real data mismatch".
			name: "synthetic probe: blacklists by contract, never a data-mismatch claim",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "probe fired",
				Units: []UnitInput{
					{Execution: ExecRuntimeError, ChecksKnown: true, VerificationRequired: false},
				},
				Findings: []Finding{{Kind: FindSyntheticProbe, Source: "workload", Message: "injected fault",
					Attribution: Attribution{Kind: AttrLogicalCPU, Index: IntPtr(2), Confidence: ConfConfirmed}}},
			},
			wantExit: 1, wantVal: ValNotPerformed, wantFaults: []int{2}, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, ChecksKnown: true},
		},
		{
			name: "inferred attribution never populates the blacklist",
			in: FinalInput{Execution: ExecRuntimeError, Reason: "fault",
				Units: []UnitInput{
					{Execution: ExecRuntimeError, Checks: 3, ChecksKnown: true, VerificationRequired: true},
				},
				Findings: []Finding{{Kind: FindDataMismatch, Source: "whea", Message: "cache error near cpu 5",
					Attribution: Attribution{Kind: AttrLogicalCPU, Index: IntPtr(5), Confidence: ConfInferred}}},
			},
			wantExit: 1, wantVal: ValPartial, wantFaults: nil, aborted: true,
			wantCov: Coverage{Planned: 1, Started: 1, Verified: 3, ChecksKnown: true},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			res := Finalize(tt.in)
			if res.Exit != tt.wantExit {
				t.Errorf("Exit = %d, want %d", res.Exit, tt.wantExit)
			}
			if tt.wantExec != "" && res.Execution != tt.wantExec {
				t.Errorf("Execution = %q, want %q", res.Execution, tt.wantExec)
			}
			if res.Validation != tt.wantVal {
				t.Errorf("Validation = %q, want %q", res.Validation, tt.wantVal)
			}
			if got := len(res.Faults); got != len(tt.wantFaults) {
				t.Errorf("Faults = %v, want CPUs %v", res.Faults, tt.wantFaults)
			}
			for _, cpu := range tt.wantFaults {
				if _, ok := res.Faults[cpu]; !ok {
					t.Errorf("Faults missing confirmed CPU %d: %v", cpu, res.Faults)
				}
			}
			if res.Coverage != tt.wantCov {
				t.Errorf("Coverage = %+v, want %+v", res.Coverage, tt.wantCov)
			}
			if res.Aborted != tt.aborted {
				t.Errorf("Aborted = %v, want %v", res.Aborted, tt.aborted)
			}
		})
	}
}

// The compatibility view must keep the old invariants the scenario and
// soak engines act on: Exit==1 with per-CPU faults only when the
// attribution is confirmed, and the reason string survives as AbortMsg.
func TestFinalizeCompatibilityView(t *testing.T) {
	res := Finalize(FinalInput{Execution: ExecRuntimeError, Reason: "fault on core 3",
		Units: []UnitInput{
			{Execution: ExecRuntimeError, Checks: 4, ChecksKnown: true, VerificationRequired: true},
		},
		Findings: []Finding{
			{Kind: FindDataMismatch, Source: "workload", Message: "first",
				Attribution: Attribution{Kind: AttrLogicalCPU, Index: IntPtr(3), Confidence: ConfConfirmed}},
			{Kind: FindDataMismatch, Source: "workload", Message: "second",
				Attribution: Attribution{Kind: AttrLogicalCPU, Index: IntPtr(3), Confidence: ConfConfirmed}},
		}})
	if res.AbortMsg != "fault on core 3" {
		t.Errorf("AbortMsg = %q, want the reason", res.AbortMsg)
	}
	if res.Faults[3] != "first" {
		t.Errorf("Faults[3] = %q, want the first message", res.Faults[3])
	}
	if len(res.Findings) != 2 {
		t.Errorf("findings must all survive, got %d", len(res.Findings))
	}
}

// WHEAFinding maps one observed record onto the finding kind the
// policy needs: uncorrected and corrected cpu-class condemn the run,
// corrected PCIe is recorded separately without blame, and anything
// unrecognized goes to adjudication. Attribution is always run-level
// unknown: WHEA never blacklists a core.
func TestWHEAFinding(t *testing.T) {
	tests := []struct {
		severity, nature string
		want             FindingKind
	}{
		{"uncorrected", "cpu", FindWHEA},
		{"uncorrected", "pcie", FindWHEA},
		{"uncorrected", "unknown", FindWHEA},
		{"corrected", "cpu", FindWHEA},
		{"corrected", "cache", FindWHEA},
		{"corrected", "interconnect", FindWHEA},
		{"corrected", "memory", FindWHEA},
		{"corrected", "pcie", FindPCIeObs},
		{"corrected", "unknown", FindWHEAUnknown},
		{"unknown", "cpu", FindWHEAUnknown},
		{"unknown", "pcie", FindWHEAUnknown},
		{"unknown", "unknown", FindWHEAUnknown},
		{"", "", FindWHEAUnknown},
	}
	for _, tt := range tests {
		f := WHEAFinding(tt.severity, tt.nature, "m")
		if f.Kind != tt.want {
			t.Errorf("WHEAFinding(%q, %q) kind = %q, want %q", tt.severity, tt.nature, f.Kind, tt.want)
		}
		if f.Attribution.Kind != AttrRun || f.Attribution.Confidence != ConfUnknown {
			t.Errorf("WHEAFinding(%q, %q) attribution = %+v, want run/unknown (WHEA never blames a core)",
				tt.severity, tt.nature, f.Attribution)
		}
	}
}
