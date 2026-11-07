package subprocess

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// P08 readiness end-to-end: transition mode through the real entry
// point - two cores, two prepared worker processes each, two
// commanded bursts per worker on one prepared schedule. The journal
// must carry the prepared timeline plus BOTH views of every burst,
// and the recovered report must distinguish commanded transitions
// from observations. Exit 0 only because every burst really
// verified.
func TestExecuteSessionE2E(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "run.log.journal.jsonl")

	opt := plan.Options{
		TestName:   "general",
		A:          "ram-constant",
		Cores:      []int{0, 1},
		Workers:    1,
		PerProcess: true,
		Memory:     "132MiB", // 2*64 MiB process allowance + 4 MiB data; lifecycle test, not a RAM stress run
		MemLock:    "off",
		RamSeed:    4242,
		BeatEvery:  50 * time.Millisecond,
		LogFile:    filepath.Join(dir, "run.log"),
	}
	sched := transition.BuildSchedule(2, 60*time.Millisecond, 150*time.Millisecond, 0, 0)
	exit := ExecuteSession(opt, sched, 0)
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	recs, iss := journal.ReadAll(jp)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	rep := runs[0]
	if rep.Schedule == nil || len(rep.Schedule.Windows) != 2 {
		t.Fatalf("prepared schedule missing: %+v", rep.Schedule)
	}
	if len(rep.Cores) != 2 {
		t.Fatalf("cores = %d, want 2", len(rep.Cores))
	}
	for _, c := range rep.Cores {
		if c.Status != journal.OK {
			t.Errorf("core %d status = %s, want ok", c.Core, c.Status)
		}
		if len(c.SessionObs) != 2 {
			t.Errorf("core %d parent observations = %d, want 2", c.Core, len(c.SessionObs))
		}
		for _, o := range c.SessionObs {
			if !o.Complete || o.Checks == nil || *o.Checks == 0 {
				t.Errorf("core %d observation incomplete: %+v", c.Core, o)
			}
		}
		if len(c.Bursts) != 2 {
			t.Errorf("core %d worker burst records = %d, want 2", c.Core, len(c.Bursts))
		}
		if c.Mem == nil || c.Mem.Seed == nil || *c.Mem.Seed != 4242 {
			t.Errorf("core %d session mem_done/seed wrong: %+v", c.Core, c.Mem)
		}
		if c.Ram == nil || c.Ram.Total != 0 {
			t.Errorf("core %d clean evidence missing: %+v", c.Core, c.Ram)
		}
	}
}

// Fault in transition mode: one core runs the fault probe - the
// run must exit 1 (a real fault outranks everything), the faulted
// core's story says fault, and the healthy core's verified bursts
// stay recorded (one core's fault never erases another's work).
func TestExecuteSessionFaultExit(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)

	opt := plan.Options{
		TestName:   "general",
		A:          "fault",
		Cores:      []int{0},
		Workers:    1,
		PerProcess: true,
		BeatEvery:  50 * time.Millisecond,
		LogFile:    filepath.Join(dir, "run.log"),
	}
	sched := transition.BuildSchedule(2, 0, 300*time.Millisecond, 0, 0)
	if exit := ExecuteSession(opt, sched, 0); exit != 1 {
		t.Fatalf("exit = %d, want 1 (a fault probe must surface as a fault)", exit)
	}
	recs, _ := journal.ReadAll(journal.Path(opt.LogFile))
	runs := journal.RecoverRuns(recs)
	c := runs[0].Cores[0]
	if c.Status != journal.Fault {
		t.Errorf("core status = %s, want fault", c.Status)
	}
	if len(c.SessionObs) != 1 || c.SessionObs[0].Complete {
		t.Errorf("faulted burst observation wrong: %+v", c.SessionObs)
	}
}

// CLI-level validation lives in the cli package; here the entry
// point itself refuses: unknown workload, empty schedule, and a
// budget that cannot serve the requested cores are setup errors
// (exit 2), never a silently degraded session.
func TestExecuteSessionRefusals(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	base := plan.Options{
		TestName: "general", Cores: []int{0}, Workers: 1, PerProcess: true,
		LogFile: filepath.Join(dir, "run.log"),
	}
	if exit := ExecuteSession(base, transition.BuildSchedule(1, time.Second, time.Second, 0, 0), 0); exit != 2 {
		t.Errorf("unknown workload: exit = %d, want 2", exit)
	}
	opt := base
	opt.A = "ram-constant"
	if exit := ExecuteSession(opt, transition.Schedule{}, 0); exit != 2 {
		t.Errorf("empty schedule: exit = %d, want 2", exit)
	}
	// ram-constant with a budget the engine refuses outright (the
	// machine cannot cover it plus the reserve): honest exit 2,
	// never a memory-less session.
	opt.Memory = "900GiB"
	opt.MemLock = "off"
	if exit := ExecuteSession(opt, transition.BuildSchedule(1, time.Second, time.Second, 0, 0), 0); exit != 2 {
		t.Errorf("impossible budget: exit = %d, want 2", exit)
	}
}

// The scenario pipeline's view of transition mode: ExecuteSessionResult
// reports the same run as a plan.Result, classified by the shared
// Finalize - clean sessions verified, a fault carries confirmed per-CPU
// attribution the blacklist can use, a pre-spawn refusal is a setup
// error and never a fault.
func TestExecuteSessionResult(t *testing.T) {
	t.Run("clean sessions complete with verified coverage", func(t *testing.T) {
		dir := chdirTemp(t)
		useTestBinaryAsWorker(t)

		opt := plan.Options{
			TestName: "general", A: "ram-constant",
			Cores: []int{0, 1}, Workers: 1, PerProcess: true,
			Memory: "132MiB", MemLock: "off",
			BeatEvery: 50 * time.Millisecond,
			LogFile:   filepath.Join(dir, "run.log"),
			Bursts:    2, BurstGap: 60 * time.Millisecond, BurstRun: 150 * time.Millisecond,
		}
		res := ExecuteSessionResult(opt, nil)
		if res.Exit != 0 {
			t.Fatalf("exit = %d, want 0 (%+v)", res.Exit, res)
		}
		if res.Execution != plan.ExecCompleted {
			t.Errorf("execution = %s, want completed", res.Execution)
		}
		if res.Coverage.Planned != 2 || res.Coverage.Completed != 2 {
			t.Errorf("coverage = %+v, want 2 planned / 2 completed sessions", res.Coverage)
		}
		if !res.Coverage.ChecksKnown || res.Coverage.Verified == 0 {
			t.Errorf("coverage checks = %+v, want known non-zero verified checks", res.Coverage)
		}
		if len(res.Faults) != 0 {
			t.Errorf("faults = %v, want none", res.Faults)
		}
	})

	t.Run("fault carries confirmed per-CPU attribution", func(t *testing.T) {
		dir := chdirTemp(t)
		useTestBinaryAsWorker(t)

		opt := plan.Options{
			TestName: "general", A: "fault",
			Cores: []int{0}, Workers: 1, PerProcess: true,
			BeatEvery: 50 * time.Millisecond,
			LogFile:   filepath.Join(dir, "run.log"),
			Bursts:    2, BurstGap: 0, BurstRun: 300 * time.Millisecond,
		}
		res := ExecuteSessionResult(opt, nil)
		if res.Exit != 1 {
			t.Fatalf("exit = %d, want 1", res.Exit)
		}
		if res.Execution != plan.ExecRuntimeError {
			t.Errorf("execution = %s, want runtime_error", res.Execution)
		}
		if msg, ok := res.Faults[0]; !ok || !strings.Contains(msg, "injected fault") {
			t.Fatalf("faults = %v, want core 0 blacklisted with the injected fault", res.Faults)
		}
		// A06: the built-in prober proves the per-CPU pipeline (the
		// blacklist path above), but its kind says synthetic_probe -
		// no report may render it as a real data mismatch.
		var found bool
		for _, f := range res.Findings {
			if f.Kind == plan.FindSyntheticProbe && f.Attribution.Kind == plan.AttrLogicalCPU &&
				f.Attribution.Index != nil && *f.Attribution.Index == 0 &&
				f.Attribution.Confidence == plan.ConfConfirmed {
				found = true
			}
			if f.Kind == plan.FindDataMismatch {
				t.Errorf("the prober was recorded as a real mismatch: %+v", f)
			}
		}
		if !found {
			t.Errorf("findings = %+v, want a confirmed logical-CPU mismatch", res.Findings)
		}
	})

	t.Run("pre-spawn refusal is a setup error", func(t *testing.T) {
		dir := chdirTemp(t)
		useTestBinaryAsWorker(t)

		opt := plan.Options{
			TestName: "general", A: "no-such-workload",
			Cores: []int{0}, PerProcess: true,
			LogFile: filepath.Join(dir, "run.log"),
			Bursts:  1, BurstGap: time.Second, BurstRun: time.Second,
		}
		res := ExecuteSessionResult(opt, nil)
		if res.Execution != plan.ExecSetupError || res.Exit != 2 {
			t.Fatalf("refusal outcome = %+v, want setup_error exit 2", res)
		}
		if len(res.Faults) != 0 {
			t.Errorf("faults = %v, want nobody blacklisted for a setup error", res.Faults)
		}
	})
}
