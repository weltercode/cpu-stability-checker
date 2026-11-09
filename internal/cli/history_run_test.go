package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// onlyManifest reads back the exactly-one session history record a
// test just produced.
func onlyManifest(t *testing.T) sessionManifest {
	t.Helper()
	list, warns := listSessionManifests()
	if len(warns) > 0 {
		t.Fatalf("manifest warnings: %v", warns)
	}
	if len(list) != 1 {
		t.Fatalf("history holds %d manifests, want exactly 1", len(list))
	}
	return list[0]
}

// A real short runCmd leaves a finalized manifest bound to the
// saved recipe, naming the run's own log and journal - and the
// engine stamped the session id into the journal itself, so runs
// stay attributable to their session even when several sessions
// share one log file (menu plan section 6).
func TestRunCmdWritesSessionManifest(t *testing.T) {
	chdirTemp(t)
	if code := runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
		"-cores", "0", "-control", "-1", "-log", "ok.log"}); code != 0 {
		t.Fatalf("runCmd exit = %d, want 0", code)
	}
	m := onlyManifest(t)
	if m.Kind != "run" || m.State != "completed" {
		t.Errorf("manifest kind/state = %q/%q, want run/completed", m.Kind, m.State)
	}
	if m.Exit == nil || *m.Exit != 0 || m.Finished == "" || m.Started == "" {
		t.Errorf("manifest terminal fields wrong: %+v", m)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if m.RecipeID != lr.RecipeID || m.Request == nil || m.Request.RecipeID != lr.RecipeID {
		t.Errorf("manifest not bound to the saved recipe %q: %+v (load err %v)", lr.RecipeID, m, err)
	}
	if len(m.Runs) != 1 || m.Runs[0].Log != "ok.log" || m.Runs[0].Journal != journal.Path("ok.log") {
		t.Errorf("manifest runs = %+v, want the run's own log and journal", m.Runs)
	}
	b, err := os.ReadFile(journal.Path("ok.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fmt.Sprintf(`"session_id":%q`, m.SessionID)) {
		t.Errorf("journal does not carry session %s", m.SessionID)
	}
}

// A session's manifest names its child runs by the session id (the
// log files carry it too, so same-second sessions cannot collide on
// disk), and with no recipe on disk it embeds the actual request
// UNBOUND and claims no recipe id it never saved.
func TestSessionManifestNamesLogsAndFinalizes(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})

	workload.Register(&workload.Workload{Name: "hist-session-fake",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("hist-session-fake")

	// 750ms, not 50ms: the first Actions runner's pinned-audit step
	// aborted this session with exit=1 exactly once (no local repro at
	// the time), and the later 2-core-oversubscription reproduction
	// showed the mechanism: a 50ms window can expire before the
	// session's worker completes its window at all on a starved
	// machine, and the engine honestly refuses the clean exit 0. The
	// panel line cannot name that cause; the assertions here are
	// untouched, only the window is sized to scheduling reality.
	control := -1
	session := SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
		Workload: "hist-session-fake", ThreadsPerCore: 1, Duration: 750 * time.Millisecond,
		Passes: 1, ControlCore: &control, LogFile: "s.log"}
	topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}},
	}}
	if code := runSessionWithPanel(session, topology, nil, nil, "recipe-sess"); code != 0 {
		dumpSessionDiag(t, "s.log")
		t.Fatalf("session exit = %d", code)
	}
	m := onlyManifest(t)
	if m.Kind != "session" || m.State != "completed" || m.Exit == nil || *m.Exit != 0 {
		t.Errorf("session manifest: kind=%q state=%q exit=%v", m.Kind, m.State, m.Exit)
	}
	if m.RecipeID != "" || m.Request == nil || m.Request.Kind != lastRunSession {
		t.Errorf("unbound session claims a recipe or lost its request: %+v", m)
	}
	if len(m.Runs) != 1 {
		t.Fatalf("manifest runs = %+v, want one child run", m.Runs)
	}
	child := m.Runs[0]
	if !strings.Contains(child.Log, m.SessionID) || child.Journal != journal.Path(child.Log) {
		t.Errorf("child log %q is not named by session %s", child.Log, m.SessionID)
	}
	b, err := os.ReadFile(child.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fmt.Sprintf(`"session_id":%q`, m.SessionID)) {
		t.Errorf("child journal does not carry session %s", m.SessionID)
	}
}

// dumpSessionDiag prints the durable records of a FAILED session run.
// The panel line only carries "exit=1, verification=unknown", which is
// one label shared by several real causes (cancel, a runtime condition,
// an aborted coverage fold). The journal and the run's own log name the
// reason per unit, so a red on a machine that cannot be re-run locally
// (the first Actions runner aborted this session once and no local
// repro under shuffle, 2-CPU or 1-CPU affinity reproduced it) has to
// bring its own evidence: a red run now prints why it stopped. This is
// instrumentation, not a fix: the mechanism stays unnamed until the
// durable records name it (R07 lesson from the whea fakeQ race).
func dumpSessionDiag(t *testing.T, logFile string) {
	t.Helper()
	for _, f := range []string{logFile, journal.Path(logFile)} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Logf("diag: cannot read %s: %v", f, err)
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line != "" {
				t.Logf("diag %s: %s", f, line)
			}
		}
	}
	if list, warns := listSessionManifests(); len(list) > 0 {
		for _, m := range list {
			t.Logf("diag manifest: %+v", m)
			t.Logf("diag manifest runs: %+v", m.Runs)
		}
		for _, w := range warns {
			t.Logf("diag manifest warning: %v", w)
		}
	}
}

// A session that dies in setup is still history: setup_error, exit
// 2, no invented runs - the label that never blames hardware for a
// broken setup.
func TestSessionSetupFailureIsHistory(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	session := SessionOptions{Mode: sessionAllCore, Passes: 0, Duration: time.Second,
		PhysicalCores: []int{0}, Workload: "int", ThreadsPerCore: 1}
	if code := runSessionWithPanel(session, sysinfo.Topology{}, nil, nil, ""); code != 2 {
		t.Fatalf("setup refusal exit = %d, want 2", code)
	}
	m := onlyManifest(t)
	if m.State != "setup_error" || m.Exit == nil || *m.Exit != 2 || m.Finished == "" {
		t.Errorf("setup-failure manifest: %+v", m)
	}
	if len(m.Runs) != 0 {
		t.Errorf("setup-failure manifest lists runs that never existed: %+v", m.Runs)
	}
}

// The tracker's lifecycle vocabulary: running until finalized, the
// worst observed execution after, "unknown" when the CLI never saw
// a verdict (transition runs), setup_error for setup refusals.
// A nil tracker is inert: call sites pass it when no manifest is
// wanted.
func TestTrackerFinishSemantics(t *testing.T) {
	chdirTemp(t)
	lr := lastRun{Kind: lastRunScenario, RecipeID: "recipe-t"}
	st, err := newSessionTracker("scenario", &lr, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := onlyManifest(t)
	if m.State != manifestRunning || m.Finished != "" || m.Exit != nil {
		t.Errorf("fresh tracker wrote terminal fields: %+v", m)
	}

	st.setRuns([]manifestRun{{Name: "a", Log: "a.log", Journal: "a.log.journal.jsonl"}})
	st.finish(1, []plan.Result{
		{Execution: plan.ExecCompleted, Validation: plan.ValVerified},
		{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Exit: 1},
	})
	m = onlyManifest(t)
	if m.State != "runtime_error" || m.Validation != "partial" || m.Exit == nil || *m.Exit != 1 || m.Finished == "" {
		t.Errorf("finalized tracker: %+v", m)
	}
	if len(m.Runs) != 1 || m.RecipeID != "recipe-t" || m.Request == nil {
		t.Errorf("finalized tracker lost runs or recipe binding: %+v", m)
	}

	st2, err := newSessionTracker("run", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st2.finish(0, nil) // transition mode: the verdict lives in the journal
	m2, err := loadSessionManifest(st2.m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if m2.State != "unknown" || m2.Exit == nil || *m2.Exit != 0 {
		t.Errorf("unjudgeable session: %+v", m2)
	}

	st3, err := newSessionTracker("session", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st3.finishSetup(2)
	m3, err := loadSessionManifest(st3.m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if m3.State != "setup_error" || m3.Exit == nil || *m3.Exit != 2 {
		t.Errorf("setup refusal: %+v", m3)
	}

	var nilT *sessionTracker
	nilT.setRuns(nil)
	nilT.finish(0, nil)
	nilT.finishSetup(2)
}

// The scenario orchestration finalizes the manifest with the worst
// state across cycles and lists the phases the session will write.
func TestRunCyclesFinalizesSessionManifest(t *testing.T) {
	chdirTemp(t)
	phase := func(name string) plan.Options {
		return plan.Options{TestName: "general", Phase: name, A: "int", LogFile: name + ".log", PlanHash: "h-" + name}
	}
	opts := []plan.Options{phase("a"), phase("b")}
	lr := lastRun{Kind: lastRunScenario, RecipeID: "recipe-cy"}
	st, err := newSessionTracker("scenario", &lr, scenarioRuns(opts))
	if err != nil {
		t.Fatal(err)
	}
	execute := func(opt plan.Options) plan.Result {
		if opt.Phase == "b" {
			return plan.Result{Execution: plan.ExecRuntimeError, Validation: plan.ValPartial, Exit: 1}
		}
		return plan.Result{Execution: plan.ExecCompleted, Validation: plan.ValVerified}
	}
	if code := runCycles(opts, 2, -1, io.Discard, execute, false, nil, st); code != 1 {
		t.Fatalf("runCycles exit = %d, want 1", code)
	}
	m := onlyManifest(t)
	if m.State != "runtime_error" || m.Validation != "partial" || m.Exit == nil || *m.Exit != 1 {
		t.Errorf("scenario manifest outcome: %+v", m)
	}
	if len(m.Runs) != 2 || m.Runs[0].Name != "a" || m.Runs[0].Journal != journal.Path("a.log") {
		t.Errorf("scenario manifest phase list: %+v", m.Runs)
	}
}

// A session whose process dies before finalizing leaves a manifest
// that still says running, with no finish time and no exit code:
// readers show it as unfinished/unknown, and nothing rewrites or
// guesses on its behalf (the crash story section 6 demands).
func TestUnfinalizedManifestStaysRunning(t *testing.T) {
	chdirTemp(t)
	if _, err := newSessionTracker("run", nil, nil); err != nil {
		t.Fatal(err)
	}
	// The "process" dies here: nothing ever finalizes this tracker.
	m := onlyManifest(t)
	if m.State != manifestRunning || m.Finished != "" || m.Exit != nil {
		t.Errorf("crashed session was rewritten or guessed at: %+v", m)
	}
}
