package subprocess

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// TestWorkerHelperProcess is not a real test: it is the child side of the
// ExecuteSubprocess tests. The "executable" is this test binary, and this
// test re-dispatches the worker arguments to WorkerCmd.
func TestWorkerHelperProcess(t *testing.T) {
	if os.Getenv("CPU_TEST_WORKER_CHILD") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	// mirror the CLI dispatch: the "worker" command word is not part of
	// WorkerCmd's arguments
	if len(args) > 0 && args[0] == "worker" {
		args = args[1:]
	}
	os.Exit(WorkerCmd(args))
}

// TestFaultHelperProcess is not a real test: it is the fake "external stress
// program" that always reports a fault (exit code 1).
func TestFaultHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_FAULT_HELPER") != "1" {
		return
	}
	os.Exit(1)
}

// faultSpec builds an external spec that re-runs this test binary as a
// fault-reporting "external" program.
func faultSpec() workload.ExternalSpec {
	return workload.ExternalSpec{
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestFaultHelperProcess"},
		Env:       map[string]string{"GO_WANT_FAULT_HELPER": "1"},
		FaultExit: []int{1},
	}
}

// TestLegacyToolHelperProcess is not a real test: it is the fake legacy
// external tool - it runs briefly and exits cleanly with an exit code
// that means nothing to us. It replaces ping as the prop of the
// evidence test (audit A07): worker bookkeeping about an external tool
// is hermetic and needs no system console tool or loopback; the real
// cmd/ping process behavior stays tested in internal/workload.
func TestLegacyToolHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LEGACY_TOOL") != "1" {
		return
	}
	time.Sleep(1500 * time.Millisecond) // a brief real run, then a clean exit
	os.Exit(0)
}

// TestYCHelperProcess is not a real test: it is the fake y-cruncher
// speaking the v0.8 stress protocol (the same line-for-line sequence
// measured against the pinned build) for the adapter tests here.
func TestYCHelperProcess(t *testing.T) {
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

// ycProbeSpec runs this test binary as a fake y-cruncher through the
// real ycruncher-v0.8 adapter. Startup options sit after the test
// flags so the adapter prepends nothing and the flags still parse.
func ycProbeSpec() workload.ExternalSpec {
	return workload.ExternalSpec{
		Command: os.Args[0],
		Adapter: "ycruncher-v0.8",
		Args:    []string{"-test.run=TestYCHelperProcess", "skip-warnings", "pause:-2", "colors:0", "status:none"},
		Env:     map[string]string{"GO_WANT_YC_HELPER": "1"},
	}
}

// useTestBinaryAsWorker makes ExecuteSubprocess spawn this test binary as
// its children instead of the real cpu-stability executable.
func useTestBinaryAsWorker(t *testing.T) {
	t.Helper()
	prevExe, prevPrefix := selfExecutable, workerArgsPrefix
	selfExecutable = func() (string, error) { return os.Args[0], nil }
	workerArgsPrefix = []string{"-test.run=TestWorkerHelperProcess", "--"}
	t.Cleanup(func() {
		selfExecutable, workerArgsPrefix = prevExe, prevPrefix
	})
}

// restoreProcessState undoes the side effects WorkerCmd has when it is called
// in-process: pinning the whole test binary and capping the scheduler.
func restoreProcessState(t *testing.T) {
	t.Helper()
	savedProcs := runtime.GOMAXPROCS(0)
	all := make([]int, runtime.NumCPU())
	for i := range all {
		all[i] = i
	}
	t.Cleanup(func() {
		runtime.GOMAXPROCS(savedProcs)
		if err := sysinfo.SetAffinity(all); err != nil {
			t.Errorf("restore affinity: %v", err)
		}
	})
}

// chdirTemp switches the working directory to a private temp dir so that the
// fixed config.DefaultPath ("config.json") resolves inside it, and restores
// it when the test finishes.
func chdirTemp(t *testing.T) string {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("restore chdir: %v", err)
		}
	})
	return dir
}

func writeDefaultConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(config.DefaultPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", config.DefaultPath, err)
	}
}

// readRecoveredRun reads a journal (main file plus worker fragments)
// and requires exactly one run report.
func readRecoveredRun(t *testing.T, path string) journal.RunReport {
	t.Helper()
	records, iss := journal.ReadAll(path)
	if len(records) == 0 && iss.ReadErr != "" {
		t.Fatalf("open journal: %v", iss.ReadErr)
	}
	runs := journal.RecoverRuns(records)
	if len(runs) != 1 {
		t.Fatalf("journal has %d runs, want exactly 1", len(runs))
	}
	return runs[0]
}

func TestWorkerArgs(t *testing.T) {
	workerArgsPrefix = []string{"-test.run=TestWorkerHelperProcess", "--"}
	t.Cleanup(func() { workerArgsPrefix = nil })

	got := strings.Join(workerArgs(3, "float", 5*time.Second, 0, 0, "j.jsonl", "run-42", 2, nil), " ")
	want := "-test.run=TestWorkerHelperProcess -- worker --core 3 --workload float " +
		"--duration 5s --journal j.jsonl --run-id run-42 --workers 2"
	if got != want {
		t.Fatalf("workerArgs() = %q, want %q", got, want)
	}

	got = strings.Join(workerArgs(3, "float", 5*time.Second, 2*time.Second, 0, "j.jsonl", "run-42", 1, nil), " ")
	if !strings.HasSuffix(got, "--workers 1 --settle 2s") {
		t.Errorf("workerArgs() with settle = %q, want it to end with --settle 2s", got)
	}

	// A configured beat cadence rides to the child; the default (0) does
	// not appear at all, keeping old command lines byte-identical.
	got = strings.Join(workerArgs(3, "float", 5*time.Second, 0, 10*time.Second, "j.jsonl", "run-42", 1, nil), " ")
	if !strings.HasSuffix(got, "--workers 1 --beat 10s") {
		t.Errorf("workerArgs() with beat = %q, want it to end with --beat 10s", got)
	}
}

func TestWorkerCmdSetupErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing core", []string{"--workload", "float", "--duration", "1s"}},
		{"core out of range", []string{"--core", "9999", "--workload", "float", "--duration", "1s"}},
		{"missing workload", []string{"--core", "0", "--duration", "1s"}},
		{"bad duration", []string{"--core", "0", "--workload", "float", "--duration", "nonsense"}},
		{"zero duration", []string{"--core", "0", "--workload", "float", "--duration", "0s"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := WorkerCmd(tt.args); code != 2 {
				t.Fatalf("WorkerCmd() code = %d, want 2", code)
			}
		})
	}
}

func TestWorkerCmdRunsAndReports(t *testing.T) {
	restoreProcessState(t)
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "float", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-1",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}

	rep := readRecoveredRun(t, journalPath)
	if rep.RunID != "rid-1" {
		t.Fatalf("run id = %q, want the one passed on the command line", rep.RunID)
	}
	if len(rep.Cores) != 1 || rep.Cores[0].Core != 0 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].Workload != "float" {
		t.Fatalf("workload = %q, want float", rep.Cores[0].Workload)
	}
}

func TestWorkerCmdReportsFault(t *testing.T) {
	restoreProcessState(t)
	workload.RegisterExternal(map[string]workload.ExternalSpec{"fake-fault": faultSpec()})
	t.Cleanup(func() { workload.Delete("fake-fault") })

	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	code := WorkerCmd([]string{
		"--core", "0", "--workload", "fake-fault", "--duration", "10s",
		"--journal", journalPath, "--run-id", "rid-2",
	})
	if code != 1 {
		t.Fatalf("WorkerCmd() code = %d, want 1 for a faulting workload", code)
	}

	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.Fault {
		t.Fatalf("recovered cores = %+v, want core 0 fault", rep.Cores)
	}
}

// The child records the adapter's protocol evidence in its own
// core_done. A legacy adapter proves nothing about the tool's
// self-verification, so the record says "unknown" explicitly Ã¢â‚¬â€ never a
// silent gap the parent could misread as success.
func TestWorkerCmdExternalEvidenceUnknown(t *testing.T) {
	restoreProcessState(t)
	spec := workload.ExternalSpec{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestLegacyToolHelperProcess$"},
		Env:     map[string]string{"GO_WANT_LEGACY_TOOL": "1"},
	}
	workload.RegisterExternal(map[string]workload.ExternalSpec{"legacy-probe": spec})
	t.Cleanup(func() { workload.Delete("legacy-probe") })

	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	if code := WorkerCmd([]string{
		"--core", "0", "--workload", "legacy-probe", "--duration", "30s",
		"--journal", journalPath, "--run-id", "rid-ev1",
	}); code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0 (the tool exited clean): %+v", code, spec)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].Evidence != workload.EvUnknown {
		t.Errorf("evidence = %q, want explicit unknown for a legacy adapter", rep.Cores[0].Evidence)
	}
}

// An adapter that confirms the tool's completion format reports
// EvConfirmed; the child must persist it so the parent's verdict can
// honestly claim external_confirmed from the journal alone.
func TestWorkerCmdExternalEvidenceConfirmed(t *testing.T) {
	restoreProcessState(t)
	workload.Register(&workload.Workload{
		Name:         "ev-probe",
		Verification: workload.VerifyExternal,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			if er, ok := rep.(workload.EvidenceReporter); ok {
				er.ReportEvidence(workload.EvPartial)
				er.ReportEvidence(workload.EvConfirmed)
			}
			<-ctx.Done()
			return 0, nil
		},
	})
	t.Cleanup(func() { workload.Delete("ev-probe") })

	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	if code := WorkerCmd([]string{
		"--core", "0", "--workload", "ev-probe", "--duration", "600ms",
		"--journal", journalPath, "--run-id", "rid-ev2",
	}); code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0: %+v", code, code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].Evidence != workload.EvConfirmed {
		t.Errorf("evidence = %q, want confirmed-complete", rep.Cores[0].Evidence)
	}
}

// The real y-cruncher adapter through the child: the completion banner
// plus clean exit becomes confirmed evidence in the child's fragment,
// so the parent's verdict can honestly claim external_confirmed from
// the journal alone.
func TestWorkerCmdYCruncherAdapterConfirmed(t *testing.T) {
	restoreProcessState(t)
	workload.RegisterExternal(map[string]workload.ExternalSpec{"yc-probe": ycProbeSpec()})
	t.Cleanup(func() { workload.Delete("yc-probe") })

	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	if code := WorkerCmd([]string{
		"--core", "0", "--workload", "yc-probe", "--duration", "30s",
		"--journal", journalPath, "--run-id", "rid-yc",
	}); code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].Evidence != workload.EvConfirmed {
		t.Errorf("evidence = %q, want confirmed-complete via the real adapter", rep.Cores[0].Evidence)
	}
}

// The parent's scan carries the child's evidence out of the journal:
// a confirmed-complete record arrives intact in coreOutcomeRec, and an
// older record without the field reads as "" (unknown), never as
// confirmed.
func TestScanCoreOutcomesEvidence(t *testing.T) {
	main := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := os.Create(main)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	jw := journal.NewWriter(f, "run-ev")
	jw.RunStart("general", "", []int{0, 1}, 1, "1s", false)
	jw.CoreDoneChecks(0, journal.OK, "", time.Second, nil, "confirmed-complete", "")
	jw.CoreDone(1, journal.OK, "") // legacy record: no evidence field
	jw.RunEnd(0)
	jw.Close()

	out, iss := scanCoreOutcomes(main, "run-ev")
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	if out[0].evidence != "confirmed-complete" {
		t.Errorf("core 0 evidence = %q, want confirmed-complete", out[0].evidence)
	}
	if out[1].evidence != "" {
		t.Errorf("legacy core evidence = %q, want empty (unknown)", out[1].evidence)
	}
}

func TestExecuteSubprocessSettle(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	// settle holds the pinned core idle, then load starts and the worker
	// must record exactly one wake measurement for that transition
	opt := plan.Options{
		TestName: "general",
		Cores:    []int{0},
		A:        "int",
		B:        "int",
		Workers:  1,
		Duration: 400 * time.Millisecond,
		Settle:   300 * time.Millisecond,
		LogFile:  "run.log",
	}
	if code := ExecuteSubprocess(opt); code != 0 {
		t.Fatalf("ExecuteSubprocess() code = %d, want 0", code)
	}

	rep := readRecoveredRun(t, filepath.Join(dir, journal.Path("run.log")))
	if len(rep.Cores) != 1 {
		t.Fatalf("recovered %d cores, want 1", len(rep.Cores))
	}
	if len(rep.Cores[0].WakeMS) != 1 {
		t.Fatalf("core wake records = %v, want exactly 1 measured wake", rep.Cores[0].WakeMS)
	}
}

func TestExecuteSubprocessRotate(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	// rotation must complete cleanly: every child gets its exclusive
	// windows and still writes core_done ok (the freeze mechanism itself
	// is proven in the procjob package's CPU-time test)
	opt := plan.Options{
		TestName: "general",
		Cores:    []int{0, 1},
		A:        "float",
		B:        "float",
		Workers:  1,
		Duration: 300 * time.Millisecond,
		Rotate:   true,
		LogFile:  "run.log",
	}
	if code := ExecuteSubprocess(opt); code != 0 {
		t.Fatalf("ExecuteSubprocess() code = %d, want 0", code)
	}

	records, _ := journal.ReadAll(filepath.Join(dir, journal.Path("run.log")))

	rep := journal.RecoverRuns(records)
	if len(rep) != 1 {
		t.Fatalf("journal has %d runs, want exactly 1", len(rep))
	}
	if len(rep[0].Cores) != 2 {
		t.Fatalf("recovered %d cores, want 2", len(rep[0].Cores))
	}
	for _, c := range rep[0].Cores {
		if c.Status != journal.OK {
			t.Errorf("core %d = %s, want ok", c.Core, c.Status)
		}
	}
}

func TestExecuteSubprocessOK(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	opt := plan.Options{
		TestName: "general",
		Cores:    []int{0, 1},
		A:        "float",
		B:        "float",
		Workers:  1,
		Duration: 400 * time.Millisecond,
		LogFile:  "run.log",
	}
	if res := ExecuteSubprocessWithResult(opt); res.Exit != 0 {
		t.Fatalf("ExecuteSubprocess() exit = %d, want 0", res.Exit)
	} else {
		// The children counted their own self-checking iterations and
		// the parent recovered them from the journal: a clean
		// subprocess run must report the same verified validation the
		// threaded engine does, not a bare exit 0.
		if res.Validation != plan.ValVerified {
			t.Errorf("Validation = %q, want verified", res.Validation)
		}
		if res.Coverage.Planned != 2 || res.Coverage.Completed != 2 || !res.Coverage.ChecksKnown || res.Coverage.Verified == 0 {
			t.Errorf("Coverage = %+v, want 2/2 completed with known checks > 0", res.Coverage)
		}
		if res.Execution != plan.ExecCompleted {
			t.Errorf("Execution = %q, want completed", res.Execution)
		}
	}

	jpath := filepath.Join(dir, journal.Path("run.log"))
	records, _ := journal.ReadAll(jpath)

	// the core_* records must come from child processes, not from us
	for _, rec := range records {
		if rec.Type == journal.CoreStart && rec.PID == os.Getpid() {
			t.Fatal("core_start written by this process, want child PIDs")
		}
	}

	rep := journal.RecoverRuns(records)
	if len(rep) != 1 {
		t.Fatalf("journal has %d runs, want exactly 1", len(rep))
	}
	run := rep[0]
	if !run.Ended || run.Exit != 0 {
		t.Fatalf("run ended=%v exit=%d, want ended with exit 0", run.Ended, run.Exit)
	}
	if len(run.Cores) != 2 {
		t.Fatalf("recovered %d cores, want 2", len(run.Cores))
	}
	for _, c := range run.Cores {
		if c.Status != journal.OK || c.Workload != "float" {
			t.Fatalf("core %d = %s %q, want ok float", c.Core, c.Status, c.Workload)
		}
	}

	// Single-owner files: the main journal must hold NO core records at
	// all Ã¢â‚¬â€ the parent owns it, each child owned its own fragment. If a
	// core record ever lands in main again, the no-shared-writer rule
	// is broken and append atomicity is silently assumed again.
	mf, err := os.Open(jpath)
	if err != nil {
		t.Fatalf("open main journal: %v", err)
	}
	mainRecs, _ := journal.ParseAll(mf, jpath)
	mf.Close()
	for _, rec := range mainRecs {
		if rec.Type == journal.CoreStart || rec.Type == journal.CoreDone || rec.Type == journal.CoreBeat {
			t.Errorf("main journal carries %s: workers must own their fragments", rec.Type)
		}
	}
	if files := journal.FragmentFiles(jpath, run.RunID); len(files) != 2 {
		t.Errorf("fragment files = %v, want one per core", files)
	}
	if len(run.Expected) != 2 || len(run.Missing) != 0 {
		t.Errorf("Expected = %v Missing = %v, want 2 registered and none missing", run.Expected, run.Missing)
	}
}

func TestExecuteSubprocessFault(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skipf("test pins cores 2,3; machine has %d CPUs", runtime.NumCPU())
	}
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	// the children load the config from the (shared) working directory, so
	// the faulting external workload is defined there
	cfg := config.Default()
	cfg.External = map[string]workload.ExternalSpec{"fake-fault": faultSpec()}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	writeDefaultConfig(t, string(data))

	// cores deliberately not starting at 0: Result.Faults must be keyed
	// by logical CPU (what the scenario blacklist acts on), not by slot
	opt := plan.Options{
		TestName: "general",
		Cores:    []int{2, 3},
		A:        "fake-fault",
		B:        "fake-fault",
		Workers:  1,
		Duration: 30 * time.Second, // the children fault long before this
		LogFile:  "run.log",
	}
	res := ExecuteSubprocessWithResult(opt)
	if res.Exit != 1 {
		t.Fatalf("ExecuteSubprocessWithResult() exit = %d, want 1 for faulting children", res.Exit)
	}
	if !res.Aborted {
		t.Errorf("Result.Aborted = false, want true")
	}
	if len(res.Faults) != 2 {
		t.Fatalf("Result.Faults = %v, want faults on both logical CPUs", res.Faults)
	}
	for _, logical := range []int{2, 3} {
		if _, ok := res.Faults[logical]; !ok {
			t.Errorf("Result.Faults = %v, want keyed by logical CPU %d", res.Faults, logical)
		}
	}

	rep := readRecoveredRun(t, filepath.Join(dir, journal.Path("run.log")))
	if rep.Ended && rep.Exit != 1 {
		t.Fatalf("run exit = %d, want 1", rep.Exit)
	}
	if len(rep.Cores) != 2 {
		t.Fatalf("recovered %d cores, want 2", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if c.Status != journal.Fault {
			t.Fatalf("core %d status = %q, want fault", c.Core, c.Status)
		}
	}
}

// TestP95HelperProcess is not a real test: it is the fake Prime95
// writing the measured results.txt protocol for the child-side
// adapter test below.
func TestP95HelperProcess(t *testing.T) {
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

// p95ProbeSpec runs this test binary as a fake Prime95 through the
// real prime95-v30.19 adapter, writing findings into dir.
func p95ProbeSpec(dir string) workload.ExternalSpec {
	return workload.ExternalSpec{
		Command:   os.Args[0],
		Adapter:   "prime95-v30.19",
		Args:      []string{"-test.run=TestP95HelperProcess", "--", "-t"},
		Dir:       dir,
		FaultFile: "results.txt",
		Env:       map[string]string{"GO_WANT_P95_HELPER": "1"},
	}
}

// The child fragment carries the honest Prime95 ceiling through the
// real adapter: partial evidence, never confirmed-complete, because
// the torture protocol has no completion format to confirm.
func TestWorkerCmdP95AdapterPartial(t *testing.T) {
	restoreProcessState(t)
	workload.RegisterExternal(map[string]workload.ExternalSpec{"p95-probe": p95ProbeSpec(t.TempDir())})
	t.Cleanup(func() { workload.Delete("p95-probe") })

	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	if code := WorkerCmd([]string{
		"--core", "0", "--workload", "p95-probe", "--duration", "30s",
		"--journal", journalPath, "--run-id", "rid-p95",
	}); code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].Evidence != workload.EvPartial {
		t.Errorf("evidence = %q, want partial via the real adapter (torture never self-completes)", rep.Cores[0].Evidence)
	}
}

// The child fragment carries the kernel identity and the verified-block
// accounting, so the parent's recovered story says which implementation
// did the work and counts only verified blocks.
func TestWorkerCmdKernelIdentity(t *testing.T) {
	restoreProcessState(t)
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")
	if code := WorkerCmd([]string{
		"--core", "0", "--workload", "int", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-kv",
	}); code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	if rep.Cores[0].KernelVersion != "int-chains-v2" {
		t.Errorf("fragment kernel = %q, want int-chains-v2", rep.Cores[0].KernelVersion)
	}
	if rep.Cores[0].Checks == nil || *rep.Cores[0].Checks == 0 {
		t.Errorf("fragment checks = %v, want counted verified blocks", rep.Cores[0].Checks)
	}
}
