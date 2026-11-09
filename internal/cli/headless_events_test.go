package cli

import (
	"bufio"
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

// registerG01Fake registers a quick in-process workload and returns
// its cleanup.
func registerG01Fake(t *testing.T, name string) {
	t.Helper()
	workload.Register(&workload.Workload{Name: name,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	t.Cleanup(func() { workload.Delete(name) })
}

func g01AffinityGuard(t *testing.T) {
	t.Helper()
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
}

// The acceptance line: every stdout line of a managed run is one
// valid JSON record, wearing the common envelope, with a monotonic
// seq and one session id from the tracker onward. Human output
// flowed to stderr instead.
func TestManagedStreamEveryLineIsJSON(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01b-fake")

	req := sessionRequest()
	req.Session.Workload = "g01b-fake"
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)
	outDir := filepath.Join(dir, "results")

	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	inR, inW, _ := os.Pipe() // managed mode reads commands here: keep it open, the GUI is "listening"
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	code := runCmd([]string{"-request", reqPath, "-events", "jsonl", "-output", outDir})
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	inW.Close()
	outW.Close()
	errW.Close()
	stdout, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	if code != 0 {
		t.Fatalf("managed run exit = %d\nstderr:\n%s", code, stderr)
	}

	var envs []eventEnvelope
	sc := bufio.NewScanner(strings.NewReader(string(stdout)))
	for sc.Scan() {
		line := sc.Text()
		var env eventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("stdout line is not one JSON record: %v\n%s", err, line)
		}
		envs = append(envs, env)
	}
	if len(envs) == 0 {
		t.Fatalf("no events at all; stderr:\n%s", stderr)
	}
	if strings.Contains(string(stdout), "\x1b") {
		t.Error("ANSI leaked into the event stream")
	}
	// R19: the end-of-run evidence block is human prose - it must
	// reach the human channel (stderr, where the managed swap routes
	// every print) and must never appear on the machine stream, whose
	// every line stays one JSON record above.
	if strings.Contains(string(stdout), "Where the evidence lives") {
		t.Errorf("evidence block leaked into the JSONL stream:\n%s", stdout)
	}
	if !strings.Contains(string(stderr), "Where the evidence lives") {
		t.Errorf("the managed run lost its end-of-run evidence block:\n%s", stderr)
	}
	// R21: the scratch start here has no unfinished evidence, so the
	// hard-lockup witness must be entirely silent - on both streams. A
	// clean start pays nothing and pollutes nothing.
	if strings.Contains(string(stdout), "never reported an ending") || strings.Contains(string(stderr), "never reported an ending") {
		t.Errorf("hard-lockup witness shown on a clean managed start:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	types := map[string]bool{}
	lastSeq := 0
	session := ""
	for _, e := range envs {
		if e.SchemaVersion != eventSchemaVersion {
			t.Errorf("event %s schema = %d", e.Type, e.SchemaVersion)
		}
		if e.Seq != lastSeq+1 {
			t.Errorf("seq jumped: %d after %d", e.Seq, lastSeq)
		}
		lastSeq = e.Seq
		if _, terr := time.Parse(time.RFC3339Nano, e.TS); terr != nil {
			t.Errorf("event %s ts %q is not RFC3339 UTC", e.Type, e.TS)
		}
		if e.SessionID != "" {
			if session != "" && e.SessionID != session {
				t.Errorf("stream carries two session ids: %s then %s", session, e.SessionID)
			}
			session = e.SessionID
		}
		types[e.Type] = true
	}
	for _, want := range []string{"resolved", "started", "phase_started", "phase_finished", "completed"} {
		if !types[want] {
			t.Errorf("stream lacks event type %q (types seen: %v)", want, types)
		}
	}
	if session == "" {
		t.Error("no event carried the session id")
	}
	if !strings.Contains(string(stderr), "Starting session") {
		t.Errorf("human output did not flow to stderr:\n%s", stderr)
	}

	// completed: durable-first contract - the result file exists on
	// disk and carries the same session truth.
	last := envs[len(envs)-1]
	if last.Type != "completed" {
		t.Errorf("stream ended with %q, not completed", last.Type)
	}
	var payload struct {
		SessionID  string `json:"session_id"`
		State      string `json:"state"`
		ResultFile string `json:"result_file"`
		Runs       []struct {
			Journal string `json:"journal"`
			Exists  bool   `json:"exists"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(mustJSON(t, last.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != session || payload.State != "completed" {
		t.Errorf("completed payload: %+v", payload)
	}
	if len(payload.Runs) != 1 || !payload.Runs[0].Exists {
		t.Errorf("completed output paths wrong: %+v", payload.Runs)
	}
	if _, err := os.Stat(payload.ResultFile); err != nil {
		t.Errorf("result file missing at %s: %v", payload.ResultFile, err)
	}
	rb, err := os.ReadFile(payload.ResultFile)
	if err == nil {
		var back map[string]any
		if err := json.Unmarshal(rb, &back); err != nil || back["session_id"] != session {
			t.Errorf("result file does not mirror the completed record: %v", err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A GUI that stops reading (pipe read end closed - the crash case)
// must never strand the load: the run finishes, the result file and
// the manifest are the durable answer, and the break is said out
// loud on stderr.
func TestManagedBrokenStreamStillDurable(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01b-brk")

	req := sessionRequest()
	req.Session.Workload = "g01b-brk"
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, req)
	outDir := filepath.Join(dir, "res")

	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	outR.Close() // the "GUI" died: the read end is gone before any event
	code := runCmd([]string{"-request", reqPath, "-events", "jsonl", "-output", outDir})
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	inW.Close()
	outW.Close()
	errW.Close()
	stderr, _ := io.ReadAll(errR)
	if code != 0 {
		t.Fatalf("broken stream changed the verdict: exit %d\n%s", code, stderr)
	}
	if !strings.Contains(string(stderr), "event stream broken") {
		t.Errorf("the break went silent:\n%s", stderr)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "result-") {
		t.Errorf("durable result missing despite the break: %v", entries)
	}
}

// Managed-mode flags are refused before anything runs: an unknown
// stream mode, and -events/-output without a request (the only
// managed entry in this build).
func TestManagedFlagRefusals(t *testing.T) {
	chdirTemp(t)
	dir := t.TempDir()
	reqPath := writeRequestFile(t, dir, sessionRequest())
	if code := runCmd([]string{"-request", reqPath, "-events", "yaml"}); code != 2 {
		t.Errorf("unknown events mode exit = %d, want 2", code)
	}
	if code := runCmd([]string{"-events", "jsonl"}); code != 2 {
		t.Errorf("-events without -request exit = %d, want 2", code)
	}
	if code := runCmd([]string{"-output", dir}); code != 2 {
		t.Errorf("-output without -request exit = %d, want 2", code)
	}
	if _, err := loadLastRun(); err == nil {
		t.Error("flag refusals ran late: the slot was already touched")
	}
}

// A scenario request streams the same contract: resolved names the
// cycle-expanded run count, each phase gets its own phase_id.
func TestManagedStreamScenarioPhases(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01b-sc")

	// Windows sized to survive scheduling on a weak shared runner
	// (same mechanism as the sessionRequest comment: a starved worker
	// that never completes its mandatory check honestly refuses exit 0).
	cfg := config.Config{Test: "general", Duration: "1s", Workers: 1, ControlCore: intPtr(-1), LogFile: "sc.log"}
	req := lastRun{Version: lastRunVersion, Kind: lastRunScenario, Config: &cfg,
		Phases: []config.Phase{{Name: "one", A: "g01b-sc", Cores: []int{0}, Duration: "750ms"},
			{Name: "two", A: "g01b-sc", Cores: []int{0}, Duration: "750ms"}},
		Cycles: 2}
	dir := t.TempDir()

	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	code := runCmd([]string{"-request", writeRequestFile(t, dir, req), "-events", "jsonl", "-output", dir})
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	inW.Close()
	outW.Close()
	errW.Close()
	stdout, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	if code != 0 {
		t.Fatalf("scenario managed run exit = %d\n%s", code, stderr)
	}
	// R19: the scenario pipeline prints its shared evidence block
	// once per attempt - on the human channel only. The JSONL stream
	// above stays strictly one record per line (that parse loop IS
	// the proof: a human line in stdout would fail it).
	if strings.Contains(string(stdout), "Where the evidence lives") {
		t.Errorf("evidence block leaked into the JSONL stream:\n%s", stdout)
	}
	if n := strings.Count(string(stderr), "Where the evidence lives"); n != 1 {
		t.Errorf("evidence block shown %d times on stderr, want exactly 1 for the whole scenario:\n%s", n, stderr)
	}
	// R21: scratch start, no unfinished evidence: the hard-lockup
	// witness stays silent on both streams (clean starts pay nothing).
	if strings.Contains(string(stdout), "never reported an ending") || strings.Contains(string(stderr), "never reported an ending") {
		t.Errorf("hard-lockup witness shown on a clean managed scenario start:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	phases := map[string]bool{}
	var resolved map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		var env eventEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, line)
		}
		if env.Type == "phase_started" {
			phases[env.PhaseID] = true
		}
		if env.Type == "resolved" {
			resolved, _ = env.Payload.(map[string]any)
		}
	}
	if !phases["one"] || !phases["two"] {
		t.Errorf("phase ids = %v, want one and two", phases)
	}
	if resolved == nil || resolved["runs"].(float64) != 4 {
		t.Errorf("resolved payload should count 2 phases x 2 cycles: %v", resolved)
	}
}

func intPtr(i int) *int { return &i }
