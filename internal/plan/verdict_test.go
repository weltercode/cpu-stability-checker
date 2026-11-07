package plan

import (
	"strings"
	"testing"
)

// The P13 dictionary, as a table: every Result shape reads as exactly
// one word, and no structured result (exit code, findings or finished
// phases) ever reads UNKNOWN.
func TestVerdictDictionary(t *testing.T) {
	cpu := func(cpuID int) Attribution {
		return Attribution{Kind: AttrLogicalCPU, Index: IntPtr(cpuID), Confidence: ConfConfirmed}
	}
	tests := []struct {
		name string
		res  *Result
		want VerdictKind
		in   []string // substrings the reason must carry
	}{
		{
			name: "verified completion passes",
			res: &Result{Execution: ExecCompleted, Validation: ValVerified, Exit: 0,
				Coverage: Coverage{Started: 20, Completed: 20, Verified: 4211, ChecksKnown: true}},
			want: VerdictPass, in: []string{"4211"},
		},
		{
			name: "data mismatch is FAIL",
			res: &Result{Execution: ExecRuntimeError, Validation: ValPartial, Exit: 1,
				Findings: []Finding{{Kind: FindDataMismatch, Source: "workload", Message: "int-chain reference differs", Attribution: cpu(5)}},
				Faults:   map[int]string{5: "int-chain reference differs"}},
			want: VerdictFail, in: []string{"data mismatch"},
		},
		{
			name: "confirmed WHEA record is FAIL",
			res: &Result{Execution: ExecRuntimeError, Validation: ValPartial, Exit: 1,
				Findings: []Finding{WHEAFinding("uncorrected", "cpu", "WHEA uncorrected cpu record 42")}},
			want: VerdictFail, in: []string{"WHEA"},
		},
		{
			name: "injected synthetic probe FAILs but never reads as hardware",
			res: &Result{Execution: ExecCompleted, Validation: ValPartial, Exit: 1,
				Findings: []Finding{{Kind: FindSyntheticProbe, Source: "runner", Message: "injected bit flip", Attribution: cpu(2)}}},
			want: VerdictFail, in: []string{"not a hardware defect"},
		},
		{
			name: "worker/tool failure is INCOMPLETE not FAIL",
			res: &Result{Execution: ExecRuntimeError, Validation: ValPartial, Exit: 1, AbortMsg: "core 7 child exited with code 1067",
				Findings: []Finding{{Kind: FindToolFailure, Source: "runner", Message: "worker process crashed: exit 1067", Attribution: cpu(7)}}},
			want: VerdictIncomplete, in: []string{"tool/worker failure"},
		},
		{
			name: "thermal cut-off is a monitor stop, not a CPU fault",
			res: &Result{Execution: ExecRuntimeError, Validation: ValPartial, Exit: 1,
				Findings: []Finding{{Kind: FindThermalStop, Source: "temperature", Message: "95.0°C >= 85.0°C", Attribution: Attribution{Kind: AttrRun, Confidence: ConfConfirmed}}}},
			want: VerdictIncomplete, in: []string{"thermal cut-off"},
		},
		{
			name: "sensor loss is INCOMPLETE",
			res: &Result{Execution: ExecRuntimeError, Validation: ValPartial, Exit: 1,
				Findings: []Finding{{Kind: FindSensorLoss, Source: "temperature", Message: "no fresh sensor", Attribution: Attribution{Kind: AttrRun, Confidence: ConfConfirmed}}}},
			want: VerdictIncomplete, in: []string{"sensor-loss"},
		},
		{
			name: "setup problem is INCOMPLETE",
			res:  &Result{Execution: ExecSetupError, Exit: 2, AbortMsg: "unknown workload"},
			want: VerdictIncomplete, in: []string{"setup"},
		},
		{
			name: "operator stop is INCOMPLETE",
			res:  &Result{Execution: ExecCancelled, Exit: 130, AbortMsg: "user esc"},
			want: VerdictIncomplete, in: []string{"operator"},
		},
		{
			name: "incident shape: all bursts completed, none verified",
			res: &Result{Execution: ExecCompleted, Validation: ValNotPerformed, Exit: 1,
				Coverage: Coverage{Started: 20, Completed: 20, Verified: 0, ChecksKnown: true}},
			want: VerdictUnverified, in: []string{"0 verified checks"},
		},
		{
			name: "partial verification cannot pass",
			res: &Result{Execution: ExecCompleted, Validation: ValPartial, Exit: 1,
				Coverage: Coverage{Started: 20, Completed: 20, Verified: 7, ChecksKnown: true}},
			want: VerdictUnverified, in: []string{"contract not met"},
		},
		{
			name: "unadjudicated WHEA evidence blocks PASS",
			res: &Result{Execution: ExecCompleted, Validation: ValVerified, Exit: 0,
				Coverage: Coverage{Started: 4, Completed: 4, Verified: 12, ChecksKnown: true},
				Findings: []Finding{WHEAFinding("weird", "mystery", "unrecognized record")}},
			want: VerdictUnverified, in: []string{"adjudication"},
		},
		{
			name: "legacy Faults map without Findings still reads FAIL",
			res:  &Result{Exit: 1, Faults: map[int]string{3: "mismatch at iter 42"}},
			want: VerdictFail, in: []string{"cpu 3", "mismatch at iter 42"},
		},
		{
			name: "a fault outranks the cancellation that followed it",
			res: &Result{Execution: ExecCancelled, Validation: ValPartial, Exit: 1,
				Findings: []Finding{{Kind: FindDataMismatch, Source: "workload", Message: "mismatch", Attribution: cpu(1)}}},
			want: VerdictFail,
		},
		{
			name: "external tool run without our verification is UNVERIFIED",
			res: &Result{Execution: ExecCompleted, Validation: ValUnknown, Exit: 0,
				Coverage: Coverage{Started: 1, Completed: 1, ChecksKnown: false}},
			want: VerdictUnverified,
		},
		{
			name: "external tool confirmed by its own protocol passes",
			res: &Result{Execution: ExecCompleted, Validation: ValExternalConfirmed, Exit: 0,
				Coverage: Coverage{Started: 1, Completed: 1, ChecksKnown: false}},
			want: VerdictPass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.res.Verdict()
			if v.Kind != tt.want {
				t.Fatalf("verdict = %s (%s), want %s", v.Kind, v.Reason, tt.want)
			}
			for _, s := range tt.in {
				if !strings.Contains(v.Reason, s) {
					t.Errorf("reason %q must contain %q", v.Reason, s)
				}
			}
			// No structured result - one that carries an exit code,
			// findings or finished phases - may ever read UNKNOWN.
			if v.Kind == VerdictUnknown {
				t.Errorf("structured result must never read UNKNOWN")
			}
		})
	}
	if v := (*Result)(nil).Verdict(); v.Kind != VerdictUnknown {
		t.Errorf("missing result = %v, want UNKNOWN", v)
	}
}

// A Result whose only evidence is the compatibility Faults map, a
// non-zero exit and no Findings must still be explained - the P13
// defect was exactly a summary that printed "aborted (exit 1)" and
// kept the reason to itself.
func TestVerdictExplainsEveryNonZeroExit(t *testing.T) {
	cases := []*Result{
		{Exit: 1, Execution: ExecRuntimeError, Findings: []Finding{{Kind: FindToolFailure, Message: "child crashed"}}},
		{Exit: 1, Execution: ExecCompleted, Coverage: Coverage{Started: 2, Completed: 2, ChecksKnown: true}},
		{Exit: 2, Execution: ExecSetupError},
		{Exit: 130, Execution: ExecCancelled},
		{Exit: 1, Execution: ExecInterrupted},
		{Exit: 7},
	}
	for i, res := range cases {
		v := res.Verdict()
		if v.Kind == VerdictUnknown || v.Kind == VerdictPass {
			t.Errorf("case %d: exit %d reads %s - a known non-zero exit is never UNKNOWN and never PASS", i, res.Exit, v.Kind)
		}
		if strings.TrimSpace(v.Reason) == "" {
			t.Errorf("case %d: exit %d has no reason: %q", i, res.Exit, v.String())
		}
	}
}

func TestWorstVerdictOrder(t *testing.T) {
	if got := WorstVerdict(VerdictPass, VerdictUnverified, VerdictIncomplete, VerdictPass); got != VerdictIncomplete {
		t.Errorf("worst = %s, want INCOMPLETE", got)
	}
	if got := WorstVerdict(VerdictIncomplete, VerdictFail); got != VerdictFail {
		t.Errorf("worst = %s, want FAIL", got)
	}
	if got := WorstVerdict(VerdictPass, VerdictUnverified); got != VerdictUnverified {
		t.Errorf("worst = %s, want UNVERIFIED", got)
	}
	if got := WorstVerdict(VerdictPass, VerdictPass); got != VerdictPass {
		t.Errorf("worst = %s, want PASS", got)
	}
	if got := WorstVerdict(VerdictUnknown, VerdictPass); got != VerdictPass {
		t.Errorf("UNKNOWN must not outvote a PASS by itself: %s", got)
	}
}
