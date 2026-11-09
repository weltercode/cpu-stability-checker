package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// writeRequestFile marshals r strictly into dir and returns the
// absolute path the contract demands.
func writeRequestFile(t *testing.T, dir string, r lastRun) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "request.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sessionRequest is the preconditions fixture shared by the managed/
// headless/repeat/lock tests: their real subject is the durable
// plumbing (request, stream, receipt, lock takeover), and every one
// of them requires a CLEAN completed run to exist first.
//
// The window matters: on the first Actions runs (R07) and in a local
// reproduction that oversubscribed a 2-core set, fixed ~50ms windows
// sometimes starved the worker goroutine past the deadline - it
// completed zero checks (the "int" case says not_performed then) or
// never reached its window end, and the engine honestly refused the
// clean exit 0 these tests promise. That refusal is correct product
// behavior: a run whose mandatory check never happened may not be
// claimed as clean. The window is therefore sized for scheduling
// reality, not for the dev box's latency; the assertions themselves
// are untouched.
func sessionRequest() lastRun {
	control := -1
	return lastRun{Version: lastRunVersion, Kind: lastRunSession,
		Config: &config.Config{},
		Session: &SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
			Workload: "int", ThreadsPerCore: 1, Duration: time.Second,
			Passes: 1, ControlCore: &control, LogFile: "hr.log"}}
}

// The request contract refuses before anything could load: relative
// paths (the run must never depend on an undocumented cwd), unknown
// fields (a newer caller must not be silently misunderstood),
// trailing data, and versions this build cannot replay faithfully.
func TestLoadRequestStrict(t *testing.T) {
	dir := t.TempDir()

	if _, err := loadRequest("request.json"); err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("relative path accepted: %v", err)
	}

	good, err := json.Marshal(sessionRequest())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "request.json")
	if err := os.WriteFile(p, good, 0o644); err != nil {
		t.Fatal(err)
	}
	if lr, err := loadRequest(p); err != nil || lr.Kind != lastRunSession {
		t.Errorf("valid request refused: %v (%+v)", err, lr)
	}

	if err := os.WriteFile(p, []byte(`{"version":2,"kind":"session","time_travel":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRequest(p); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Errorf("unknown field accepted: %v", err)
	}

	if err := os.WriteFile(p, append(good, []byte(" trailing")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRequest(p); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("trailing data accepted: %v", err)
	}

	future := sessionRequest()
	future.Version = lastRunVersion + 1
	if _, err := loadRequest(writeRequestFile(t, dir, future)); err == nil ||
		!strings.Contains(err.Error(), "newer tool") {
		t.Errorf("future version accepted: %v", err)
	}
	stale := sessionRequest()
	stale.Version = 1
	if _, err := loadRequest(writeRequestFile(t, dir, stale)); err == nil ||
		!strings.Contains(err.Error(), "version-1") {
		t.Errorf("version-1 request accepted: %v", err)
	}
}

// A refused request must leave nothing behind: no slot displacement,
// no manifest, no load. Flag mixing and positional arguments are
// refused too - the request file is the whole question.
func TestRunCmdRequestRefusalsLeaveNothingBehind(t *testing.T) {
	chdirTemp(t)
	dir := t.TempDir()

	bad := writeRequestFile(t, dir, lastRun{Version: lastRunVersion, Kind: "teleport",
		Config: &config.Config{}})
	if code := runCmd([]string{"-request", bad}); code != 2 {
		t.Errorf("unknown kind exit = %d, want 2", code)
	}
	if _, err := loadLastRun(); err == nil {
		t.Error("a refused request displaced the replay slot")
	}
	if list, _ := listSessionManifests(); len(list) != 0 {
		t.Error("a refused request wrote history")
	}

	good := writeRequestFile(t, dir, sessionRequest())
	if code := runCmd([]string{"-request", good, "-a", "int"}); code != 2 {
		t.Errorf("request+flag mixing exit = %d, want 2", code)
	}
	if code := runCmd([]string{"-request", good, "positional"}); code != 2 {
		t.Errorf("request+positional exit = %d, want 2", code)
	}
	if code := runCmd([]string{"-request", "relative.json"}); code != 2 {
		t.Errorf("relative request path exit = %d, want 2", code)
	}

	// A card the catalog marks unavailable is refused with its own
	// reason, before any load.
	gated := sessionRequest()
	gated.ProfileID = "mix-simultaneous"
	if code := runCmd([]string{"-request", writeRequestFile(t, dir, gated)}); code != 2 {
		t.Errorf("unavailable profile exit = %d, want 2", code)
	}
	if _, err := loadLastRun(); err == nil {
		t.Error("the profile gate ran after the slot was already displaced")
	}
}

// A valid request runs through the repeat pipeline: the slot holds
// the request (fresh identity stamped), the session manifest is
// written like any other session, and the outcome is real.
func TestRunCmdRequestRunsThroughRepeatPipeline(t *testing.T) {
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
	workload.Register(&workload.Workload{Name: "g01a-fake",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("g01a-fake")

	req := sessionRequest()
	req.Session.Workload = "g01a-fake"
	dir := t.TempDir()
	if code := runCmd([]string{"-request", writeRequestFile(t, dir, req)}); code != 0 {
		t.Fatalf("headless run exit = %d, want 0", code)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.RecipeID == "" || lr.Kind != lastRunSession {
		t.Errorf("slot did not take the request with a fresh identity: %+v", lr)
	}
	m := onlyManifest(t)
	if m.Kind != "session" || m.State != "completed" || m.RecipeID != lr.RecipeID {
		t.Errorf("headless session manifest wrong: %+v", m)
	}
}

// validate answers the same gates as the run, structured, without
// starting any load.
func TestValidateCmd(t *testing.T) {
	chdirTemp(t)
	dir := t.TempDir()

	capture := func(args []string) (int, string) {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		code := validateCmd(args)
		w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		return code, string(out)
	}

	good := writeRequestFile(t, dir, sessionRequest())
	code, out := capture([]string{"-request", good, "-json"})
	if code != 0 {
		t.Fatalf("valid request exit = %d:\n%s", code, out)
	}
	var ans struct {
		SchemaVersion int      `json:"schema_version"`
		OK            bool     `json:"ok"`
		Kind          string   `json:"kind"`
		Runs          int      `json:"runs"`
		PlanHashes    []string `json:"plan_hashes"`
		Errors        []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("answer is not pure JSON: %v\n%s", err, out)
	}
	if !ans.OK || ans.Kind != "session" || ans.Runs != 1 || len(ans.PlanHashes) != 1 ||
		ans.PlanHashes[0] == "" || ans.SchemaVersion != requestSchemaVersion {
		t.Errorf("answer wrong: %s", out)
	}

	bad := writeRequestFile(t, dir, lastRun{Version: lastRunVersion, Kind: lastRunScenario,
		Config: &config.Config{}, Phases: []config.Phase{{Name: "x", A: "no-such-workload"}}})
	code, out = capture([]string{"-request", bad, "-json"})
	if code == 0 {
		t.Errorf("unknown workload accepted:\n%s", out)
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil || ans.OK || len(ans.Errors) == 0 {
		t.Errorf("refusal not structured: %v\n%s", err, out)
	}

	gated := sessionRequest()
	gated.ProfileID = "mix-simultaneous" // still an honest "not yet" card after P07f
	code, out = capture([]string{"-request", writeRequestFile(t, dir, gated), "-json"})
	if code == 0 || !strings.Contains(out, "mix-simultaneous") {
		t.Errorf("unavailable profile not refused by name:\n%s", out)
	}

	if code := validateCmd([]string{}); code != 2 {
		t.Errorf("missing -request exit = %d, want 2", code)
	}
	if code := validateCmd([]string{"-request", good, "extra"}); code != 2 {
		t.Errorf("positional exit = %d, want 2", code)
	}
}
