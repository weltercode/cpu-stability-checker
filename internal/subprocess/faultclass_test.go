package subprocess

import (
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// A06: the workload's registered fault class decides what a worker's
// fault is evidence about. A RAM mismatch must never become a
// confirmed CPU fault: the observation location (observed_on) and the
// blame-bearing attribution are stored separately, and only a
// confirmed logical-CPU attribution can reach the blacklist.
func TestFaultFindingByClass(t *testing.T) {
	ram := &journal.RamEvidence{
		Findings: []journal.RamFinding{{Region: "worker=1/region=2/dest/off=4096", Offset: 4096}},
		Total:    3,
	}
	cases := []struct {
		name         string
		wl           string
		verification workload.Verification
		ram          *journal.RamEvidence
		wantKind     plan.FindingKind
		wantAttr     plan.AttributionKind
		wantRegion   bool // Region must be filled from the ram evidence
		wantObserved bool // ObservedOn set, Index NOT (memory class)
		wantIndex    bool // Index set (compute/synthetic classes)
	}{
		{name: "compute kernel indicts the pinned CPU", wl: "int", verification: workload.VerifyIndependent,
			wantKind: plan.FindDataMismatch, wantAttr: plan.AttrLogicalCPU, wantIndex: true},
		{name: "external tool failure keeps the per-CPU contract", wl: "yc-p4p", verification: workload.VerifyExternal,
			wantKind: plan.FindToolFailure, wantAttr: plan.AttrLogicalCPU, wantIndex: true},
		{name: "ram mismatch is memory evidence with region detail", wl: "ram-constant", verification: workload.VerifyIndependent,
			ram: ram, wantKind: plan.FindDataMismatch, wantAttr: plan.AttrMemoryRegion, wantRegion: true, wantObserved: true},
		{name: "ram mismatch without kept details still stays memory-class", wl: "ram-moving", verification: workload.VerifyIndependent,
			wantKind: plan.FindDataMismatch, wantAttr: plan.AttrMemoryRegion, wantObserved: true},
		{name: "the synthetic prober is pipeline proof, not a mismatch claim", wl: "fault", verification: workload.VerifyNone,
			wantKind: plan.FindSyntheticProbe, wantAttr: plan.AttrLogicalCPU, wantIndex: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := faultFinding(c.wl, 7, c.verification, "mismatch seen", c.ram)
			if f.Kind != c.wantKind {
				t.Errorf("kind = %q, want %q", f.Kind, c.wantKind)
			}
			if f.Attribution.Kind != c.wantAttr {
				t.Errorf("attribution = %q, want %q", f.Attribution.Kind, c.wantAttr)
			}
			if f.Attribution.Confidence != plan.ConfConfirmed {
				t.Errorf("confidence = %q, want confirmed (the observation itself is firm)", f.Attribution.Confidence)
			}
			if c.wantIndex {
				if f.Attribution.Index == nil || *f.Attribution.Index != 7 {
					t.Errorf("index = %v, want the pinned CPU 7", f.Attribution.Index)
				}
			} else {
				if f.Attribution.Index != nil {
					t.Errorf("index = %v, want nil: a memory finding must not point at a CPU", *f.Attribution.Index)
				}
			}
			if c.wantObserved {
				if f.Attribution.ObservedOn == nil || *f.Attribution.ObservedOn != 7 {
					t.Errorf("observed_on = %v, want CPU 7 recorded separately from the blame", f.Attribution.ObservedOn)
				}
			}
			if c.wantRegion {
				if !strings.Contains(f.Attribution.Region, "worker=1/region=2") || !strings.Contains(f.Message, "offset 4096") {
					t.Errorf("region/message lost the ram detail: region=%q msg=%q", f.Attribution.Region, f.Message)
				}
			}
		})
	}
}

// The memory finding must survive Finalize without ever entering the
// faulted-CPU map (the scenario blacklist), while still deciding the
// exit code: the run is not clean, but no CPU is indicted.
func TestMemoryFaultNeverBlacklists(t *testing.T) {
	f := faultFinding("ram-constant", 5, workload.VerifyIndependent, "word differs", nil)
	res := plan.Finalize(plan.FinalInput{
		Execution: plan.ExecRuntimeError, Reason: "ram mismatch",
		Units:    []plan.UnitInput{{Execution: plan.ExecRuntimeError, Checks: 4, ChecksKnown: true, VerificationRequired: true}},
		Findings: []plan.Finding{f},
	})
	if res.Exit != 1 {
		t.Errorf("exit = %d, want 1: a memory fault still makes the run not-clean", res.Exit)
	}
	if len(res.Faults) != 0 {
		t.Errorf("Faults = %v, want nobody blacklisted for a memory-region finding", res.Faults)
	}
	if len(res.Findings) != 1 || res.Findings[0].Attribution.ObservedOn == nil || *res.Findings[0].Attribution.ObservedOn != 5 {
		t.Errorf("the observed_on record must survive Finalize: %+v", res.Findings)
	}
}

// End to end through a real child process: the built-in prober's
// fault arrives classified synthetic (pipeline proof, not a hardware
// claim) while still blacklisting per the soak contract.
func TestSyntheticProberPerProcessE2E(t *testing.T) {
	chdirTemp(t)
	useTestBinaryAsWorker(t)
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "fault", Workers: 1,
		Duration: 30 * time.Second, LogFile: "r01probe.log", PerProcess: true}
	res := ExecuteSubprocessWithResult(opt)
	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1", res.Exit)
	}
	if res.Faults[0] == "" {
		t.Errorf("Faults = %v, want CPU 0 blacklisted (pipeline-proof contract)", res.Faults)
	}
	var probe bool
	for _, f := range res.Findings {
		if f.Kind == plan.FindSyntheticProbe {
			probe = true
		}
		if f.Kind == plan.FindDataMismatch {
			t.Errorf("prober recorded as a real mismatch: %+v", f)
		}
	}
	if !probe {
		t.Errorf("no synthetic_probe finding: %+v", res.Findings)
	}
}

// The session path classifies from the workload identity alone (the
// ERROR ack carries no region detail): a ram-* session fault is
// memory evidence with the CPU recorded as observed_on only.
func TestSessionFinalInputFaultClass(t *testing.T) {
	cases := []struct {
		wl       string
		wantKind plan.FindingKind
		wantAttr plan.AttributionKind
	}{
		{"int", plan.FindDataMismatch, plan.AttrLogicalCPU},
		{"ram-address", plan.FindDataMismatch, plan.AttrMemoryRegion},
		{"fault", plan.FindSyntheticProbe, plan.AttrLogicalCPU},
	}
	for _, c := range cases {
		outcomes := []SessionOutcome{{Fault: "burst verify failed", Checks: 2, ChecksKnown: true, Exit: 1}}
		in := sessionFinalInput(c.wl, []int{9}, outcomes)
		res := plan.Finalize(in)
		if len(res.Findings) != 1 {
			t.Fatalf("%s: findings = %v, want one", c.wl, res.Findings)
		}
		f := res.Findings[0]
		if f.Kind != c.wantKind {
			t.Errorf("%s: kind = %q, want %q", c.wl, f.Kind, c.wantKind)
		}
		if f.Attribution.Kind != c.wantAttr {
			t.Errorf("%s: attribution = %q, want %q", c.wl, f.Attribution.Kind, c.wantAttr)
		}
		switch c.wantAttr {
		case plan.AttrMemoryRegion:
			if len(res.Faults) != 0 {
				t.Errorf("%s: Faults = %v, want no CPU blacklisted", c.wl, res.Faults)
			}
			if f.Attribution.ObservedOn == nil || *f.Attribution.ObservedOn != 9 {
				t.Errorf("%s: observed_on = %v, want CPU 9", c.wl, f.Attribution.ObservedOn)
			}
		case plan.AttrLogicalCPU:
			if res.Faults[9] == "" {
				t.Errorf("%s: Faults = %v, want CPU 9 blacklisted (compute/synthetic contract)", c.wl, res.Faults)
			}
		}
		if res.Exit != 1 {
			t.Errorf("%s: exit = %d, want 1", c.wl, res.Exit)
		}
	}
}
