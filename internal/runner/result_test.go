package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// TestSharedFaultHelperProcess is not a real test: it is the fake
// "external stress program" a SingleInstance spec launches. It always
// reports a hardware fault (exit 1, configured as fault_exit).
func TestSharedFaultHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SHARED_FAULT_HELPER") != "1" {
		return
	}
	os.Exit(1)
}

// TestP95ProbeHelperProcess is not a real test: it is the fake Prime95
// writing the measured results.txt protocol (plain text, no JSON) for
// the honest-ceiling test below.
func TestP95ProbeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_P95_HELPER") != "1" {
		return
	}
	// append, like real Prime95: results.txt survives across runs
	f, err := os.OpenFile("results.txt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	fmt.Fprintln(f, "[Fri Oct 16 03:43:21 2026]")
	fmt.Fprintln(f, "Self-test 8K (thread 1 of 2) passed!")
}

// TestYCProbeHelperProcess is not a real test: it is the fake y-cruncher
// speaking the v0.8 stress protocol (the sequence measured against the
// pinned build) for the adapter end-to-end test below.
func TestYCProbeHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_YC_HELPER") != "1" {
		return
	}
	fmt.Println("y-cruncher v0.8.7 Build 9547-vsc")
	fmt.Println("Component Stress Tester")
	fmt.Println("Allocating Memory...")
	fmt.Println("Iteration: 0  Total Elapsed Time: 0.002 seconds  ( 0.000 minutes )")
	fmt.Println("Running BKT: Passed  Test Speed:  2.31 * 10^10  bits / sec")
	fmt.Println("Test Finished. Waiting for threads to terminate...")
}

// One shared external process serving every slot must produce exactly
// one run-level finding: four invented per-CPU faults would poison the
// scenario blacklist. The exit code still says 1 — the tool did report
// a fault — but Faults stays empty.
func TestSharedInstanceFaultIsRunLevel(t *testing.T) {
	preserveRunnerState(t)
	spec := workload.ExternalSpec{
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestSharedFaultHelperProcess"},
		Env:            map[string]string{"GO_WANT_SHARED_FAULT_HELPER": "1"},
		FaultExit:      []int{1},
		SingleInstance: true,
	}
	workload.RegisterExternal(map[string]workload.ExternalSpec{"shared-fault-probe": spec})
	t.Cleanup(func() { workload.Delete("shared-fault-probe") })

	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0, 1, 2, 3},
		A:           "shared-fault-probe",
		B:           "shared-fault-probe",
		Workers:     1,
		Duration:    5 * time.Second,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "shared.log"),
	}
	res := ExecuteWithResult(opt)

	if res.Exit != 1 {
		t.Fatalf("exit = %d, want 1 (the tool did report a fault): %+v", res.Exit, res)
	}
	if len(res.Faults) != 0 {
		t.Errorf("Faults = %v, want empty: one shared process cannot fault four CPUs", res.Faults)
	}
	toolFacts := 0
	for _, f := range res.Findings {
		if f.Kind == plan.FindToolFailure {
			toolFacts++
			if f.Attribution.Kind != plan.AttrRun {
				t.Errorf("shared fault attributed %v, want run", f.Attribution.Kind)
			}
		}
	}
	if toolFacts != 1 {
		t.Errorf("got %d tool-failure findings, want exactly one for the shared process: %+v", toolFacts, res.Findings)
	}
	if res.Coverage.Planned != 4 || res.Coverage.Started != 4 || res.Coverage.Completed != 0 {
		t.Errorf("Coverage = %+v, want 4 planned/started, none completed", res.Coverage)
	}
	if res.Coverage.ChecksKnown {
		t.Error("an external tool's check count must never be reported as known")
	}
	if res.Validation != plan.ValUnknown {
		t.Errorf("Validation = %q, want unknown", res.Validation)
	}
}

// A clean self-checking run must report verified validation with a real
// confirmed-check count: exit 0 alone used to be the whole story.
func TestCleanRunReportsVerifiedValidation(t *testing.T) {
	preserveRunnerState(t)
	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0, 1},
		A:           "int",
		B:           "int",
		Workers:     1,
		Duration:    1200 * time.Millisecond,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "clean.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("Validation = %q, want verified", res.Validation)
	}
	if res.Coverage.Planned != 2 || res.Coverage.Completed != 2 || !res.Coverage.ChecksKnown || res.Coverage.Verified == 0 {
		t.Errorf("Coverage = %+v, want 2/2 completed with known checks > 0", res.Coverage)
	}
}

// TestLegacyHoldHelperProcess is not a real test: it is the fake "legacy
// stress tool" that stays alive until the run deadline kills it. Ping
// was the original prop, but ping also drags cmd.exe and the loopback
// stack into an honesty-bookkeeping test (audit A07): what this tests
// verifies is what the journal records, so the child is this very
// binary - the tool stays external (unregistered, exit code opaque to
// us, killed at the deadline exactly like a real tool would be).
func TestLegacyHoldHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LEGACY_HOLD") != "1" {
		return
	}
	time.Sleep(30 * time.Second) // outlives every test window on purpose
	os.Exit(0)
}

// An external tool that simply ran until our deadline proves nothing we
// can count: the run may exit 0 (its contract was "run the tool for the
// duration") but validation must read unknown, never verified.
func TestExternalCleanRunValidationUnknown(t *testing.T) {
	preserveRunnerState(t)
	spec := workload.ExternalSpec{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestLegacyHoldHelperProcess$"}, // outlives the run window
		Env:     map[string]string{"GO_WANT_LEGACY_HOLD": "1"},
	}
	workload.RegisterExternal(map[string]workload.ExternalSpec{"legacy-probe": spec})
	t.Cleanup(func() { workload.Delete("legacy-probe") })

	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0, 1},
		A:           "legacy-probe",
		B:           "legacy-probe",
		Workers:     1,
		Duration:    1200 * time.Millisecond,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "ext.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	if res.Validation != plan.ValUnknown {
		t.Errorf("Validation = %q, want unknown", res.Validation)
	}
	if res.Coverage.ChecksKnown || res.Coverage.Verified != 0 {
		t.Errorf("Coverage = %+v, want unknown checks, never counted beats", res.Coverage)
	}
	if res.Coverage.Completed != 2 {
		t.Errorf("Coverage.Completed = %d, want 2 (the tool ran the whole window)", res.Coverage.Completed)
	}

	// The journal must carry the same honesty: a legacy adapter that
	// proved nothing records "unknown" explicitly, so a later reader
	// can never mistake the OK for a confirmed self-verification.
	recs, iss := journal.ReadAll(journal.Path(opt.LogFile))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 2 {
		t.Fatalf("recovered %+v, want one run with two cores", reports)
	}
	for _, c := range reports[0].Cores {
		if c.Evidence != workload.EvUnknown {
			t.Errorf("core %d evidence = %q, want explicit unknown: %+v", c.Core, c.Evidence, c)
		}
	}
}

// An adapter that parses the tool's protocol can report
// confirmed-complete; the engine must carry that evidence into the
// journal and the verdict — and only that far: the claim becomes
// external_confirmed, never our own verified count, and beats stay
// beats.
func TestExternalAdapterEvidenceConfirmed(t *testing.T) {
	preserveRunnerState(t)
	workload.Register(&workload.Workload{
		Name:         "evidence-probe",
		Verification: workload.VerifyExternal,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			if er, ok := rep.(workload.EvidenceReporter); ok {
				er.ReportEvidence(workload.EvPartial)   // the fake protocol confirms a real start
				er.ReportEvidence(workload.EvConfirmed) // and a confirmed completion format
			}
			<-ctx.Done()
			return 0, nil
		},
	})
	t.Cleanup(func() { workload.Delete("evidence-probe") })

	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0},
		A:           "evidence-probe",
		B:           "evidence-probe",
		Workers:     1,
		Duration:    800 * time.Millisecond,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "ev.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	if res.Validation != plan.ValExternalConfirmed {
		t.Errorf("Validation = %q, want external_confirmed: %+v", res.Validation, res)
	}
	if res.Coverage.ChecksKnown || res.Coverage.Verified != 0 {
		t.Errorf("Coverage = %+v, want unknown checks: a confirmed protocol is not our verified count", res.Coverage)
	}

	recs, iss := journal.ReadAll(journal.Path(opt.LogFile))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovered %+v, want one run with one core", reports)
	}
	if got := reports[0].Cores[0].Evidence; got != workload.EvConfirmed {
		t.Errorf("journal evidence = %q, want confirmed-complete", got)
	}
}

// The same lift through the REAL version-specific adapter: a fake tool
// speaking the measured y-cruncher v0.8 protocol (completion banner +
// clean exit) reaches external_confirmed end to end, and the journal
// carries the evidence.
func TestExternalYCruncherAdapterConfirmed(t *testing.T) {
	preserveRunnerState(t)
	spec := workload.ExternalSpec{
		Command: os.Args[0],
		Adapter: "ycruncher-v0.8",
		Args:    []string{"-test.run=TestYCProbeHelperProcess", "skip-warnings", "pause:-2", "colors:0", "status:none"},
		Env:     map[string]string{"GO_WANT_YC_HELPER": "1"},
	}
	workload.RegisterExternal(map[string]workload.ExternalSpec{"yc-probe": spec})
	t.Cleanup(func() { workload.Delete("yc-probe") })

	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0},
		A:           "yc-probe",
		B:           "yc-probe",
		Workers:     1,
		Duration:    800 * time.Millisecond,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "yc.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	if res.Validation != plan.ValExternalConfirmed {
		t.Errorf("Validation = %q, want external_confirmed: %+v", res.Validation, res)
	}
	if res.Coverage.ChecksKnown || res.Coverage.Verified != 0 {
		t.Errorf("Coverage = %+v, want unknown checks even when confirmed: the tool's iterations are not our verified count", res.Coverage)
	}

	recs, iss := journal.ReadAll(journal.Path(opt.LogFile))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovered %+v, want one run with one core", reports)
	}
	if got := reports[0].Cores[0].Evidence; got != workload.EvConfirmed {
		t.Errorf("journal evidence = %q, want confirmed-complete", got)
	}
}

// The honest Prime95 ceiling end to end: a torture run with real
// protocol progress completes cleanly but can only ever claim partial
// evidence - external_confirmed is unreachable through a protocol that
// never self-completes, and the journal carries that limit.
func TestExternalPrime95AdapterCeiling(t *testing.T) {
	preserveRunnerState(t)
	spec := workload.ExternalSpec{
		Command:   os.Args[0],
		Adapter:   "prime95-v30.19",
		Args:      []string{"-test.run=TestP95ProbeHelperProcess", "--", "-t"},
		Dir:       t.TempDir(),
		FaultFile: "results.txt",
		Env:       map[string]string{"GO_WANT_P95_HELPER": "1"},
	}
	workload.RegisterExternal(map[string]workload.ExternalSpec{"p95-probe": spec})
	t.Cleanup(func() { workload.Delete("p95-probe") })

	opt := plan.Options{
		TestName: "general",
		Cores:    []int{0},
		A:        "p95-probe",
		B:        "p95-probe",
		Workers:  1,
		// the fake tool is a test binary: give it room to start,
		// write its findings and exit cleanly before our deadline,
		// so the slot outcome is "completed", not "we killed it"
		Duration:    3 * time.Second,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "p95.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	// The run-level claim stays unknown: the torture protocol never
	// confirms completion, so no slot can reach confirmed-complete and
	// the C1 rule (any unconfirmed external slot keeps the run
	// unknown) holds. The honest per-slot truth - real protocol
	// progress - lives in the journal evidence, not in the run label.
	if res.Validation != plan.ValUnknown {
		t.Errorf("Validation = %q, want unknown: a torture run never self-completes, so external_confirmed must not be claimed: %+v", res.Validation, res)
	}

	recs, iss := journal.ReadAll(journal.Path(opt.LogFile))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovered %+v, want one run with one core", reports)
	}
	if got := reports[0].Cores[0].Evidence; got != workload.EvPartial {
		t.Errorf("journal evidence = %q, want partial", got)
	}
}

// The journal carries the kernel identity and the honest accounting:
// int's checks are verified blocks (every 64th batch plus the final
// tail), not raw iterations, and core_done names the implementation
// that produced the result.
func TestJournalCarriesKernelIdentity(t *testing.T) {
	preserveRunnerState(t)
	opt := plan.Options{
		TestName:    "general",
		Cores:       []int{0},
		A:           "int",
		B:           "int",
		Workers:     1,
		Duration:    300 * time.Millisecond,
		ControlCore: -1,
		LogFile:     filepath.Join(t.TempDir(), "kv.log"),
	}
	res := ExecuteWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0: %+v", res.Exit, res)
	}
	recs, iss := journal.ReadAll(journal.Path(opt.LogFile))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovered %+v, want one run with one core", reports)
	}
	c := reports[0].Cores[0]
	if c.KernelVersion != "int-chains-v2" {
		t.Errorf("journal kernel = %q, want int-chains-v2", c.KernelVersion)
	}
	if c.Checks == nil || *c.Checks == 0 {
		t.Fatalf("journal checks = %v, want counted verified blocks", c.Checks)
	}
	if res.Coverage.Verified != *c.Checks {
		t.Errorf("result verified = %d, journal checks = %d: they must agree", res.Coverage.Verified, *c.Checks)
	}
}
