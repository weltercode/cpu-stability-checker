package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// G01 (menu plan section 11): "JSON schema is compatible with the
// fixtures". The fixtures under testdata/g01 are the contract a GUI
// is built against: request files, the event envelope, every event
// payload, the durable result file, the control command and the
// info answers. These tests pin fixture <-> Go struct compatibility
// in both directions - a fixture key the structs no longer produce
// fails, and a struct key missing from the fixture fails - so a
// rename that would silently break a GUI fails the gate instead.

// fixturesDir is pinned absolute at package init, before any test
// chdirs into a temp directory: the fixtures belong to the source
// tree, not to whatever cwd a test happens to run in.
var fixturesDir = mustAbs(filepath.Join("testdata", "g01"))

func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		panic(err)
	}
	return abs
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// keyPaths flattens a JSON document into its key paths, with array
// elements unified under "[]" so one sample element stands for the
// shape of every element.
func keyPaths(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	paths := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				paths[p] = true
				walk(p, sub)
			}
		case []any:
			for _, el := range x {
				walk(prefix+"[]", el)
			}
		}
	}
	walk("", v)
	return paths
}

func samePaths(t *testing.T, what string, want, got map[string]bool) {
	t.Helper()
	var missing, extra []string
	for p := range want {
		if !got[p] {
			missing = append(missing, p)
		}
	}
	for p := range got {
		if !want[p] {
			extra = append(extra, p)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("%s schema drifted: fixture-only %v, build-only %v", what, missing, extra)
	}
}

// The request fixtures must load through the strict request reader:
// a GUI built from these files gets accepted by this build.
func TestRequestFixturesLoad(t *testing.T) {
	for name, wantKind := range map[string]string{
		"request_session.json":  lastRunSession,
		"request_scenario.json": lastRunScenario,
		"request_handoff.json":  lastRunHandoff,
	} {
		lr, err := loadRequest(filepath.Join(fixturesDir, name))
		if err != nil {
			t.Errorf("%s refused by the strict reader: %v", name, err)
			continue
		}
		if lr.Kind != wantKind {
			t.Errorf("%s kind = %q, want %q", name, lr.Kind, wantKind)
		}
	}
}

// The control command fixture decodes strictly into the command
// shape the reader answers.
func TestCommandFixtureDecodes(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(string(fixtureBytes(t, "command_cancel.json"))))
	dec.DisallowUnknownFields()
	var cmd streamCommand
	if err := dec.Decode(&cmd); err != nil {
		t.Fatalf("command_cancel.json does not decode: %v", err)
	}
	if cmd.Type != "cancel" || cmd.SessionID == "" || cmd.CommandID == "" {
		t.Errorf("command fixture wrong: %+v", cmd)
	}
}

// Every payload/result/envelope fixture round-trips through its Go
// struct with the identical key set: the fixture describes exactly
// what the struct emits, nothing more and nothing less.
func TestFixturesRoundTripThroughStructs(t *testing.T) {
	for _, tc := range []struct {
		file   string
		target any
	}{
		{"envelope.json", &eventEnvelope{}},
		{"result_file.json", &resultPayload{}},
		{"payload_resolved.json", &resolvedPayload{}},
		{"payload_started.json", &startedPayload{}},
		{"payload_phase_started_session.json", &sessionPhaseStartedPayload{}},
		{"payload_phase_started_scenario.json", &scenarioPhaseStartedPayload{}},
		{"payload_phase_started_handoff.json", &handoffPhaseStartedPayload{}},
		{"payload_handoff_lease.json", &handoffLeasePayload{}},
		{"payload_phase_finished.json", &phaseFinishedPayload{}},
		{"payload_progress.json", &progressPayload{}},
		{"payload_cancelling.json", &cancellingPayload{}},
		{"payload_cancel_ack.json", &cancelAckPayload{}},
		{"payload_command_error.json", &commandErrorPayload{}},
		{"payload_error.json", &errorPayload{}},
		{"payload_finding.json", &findingPayload{}},
	} {
		b := fixtureBytes(t, tc.file)
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(tc.target); err != nil {
			t.Errorf("%s does not decode into its struct: %v", tc.file, err)
			continue
		}
		back, err := json.Marshal(tc.target)
		if err != nil {
			t.Errorf("%s: struct will not re-encode: %v", tc.file, err)
			continue
		}
		samePaths(t, tc.file, keyPaths(t, b), keyPaths(t, back))
	}
}

// The live stream must wear the fixtures: run a managed session and
// a managed scenario, and every emitted payload's key set equals
// its fixture. The envelope of every event stays inside the
// envelope fixture's keys, with the mandatory ones always present.
func TestStreamMatchesFixtures(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	g01AffinityGuard(t)
	registerG01Fake(t, "g01d-pin")

	// A session run: resolved/started/phase_started(session)/
	// phase_finished/completed, plus the honest error record of a
	// managed run whose workload vanished before the tracker.
	req := sessionRequest()
	req.Session.Workload = "g01d-pin"
	dir := t.TempDir()
	gui := startManagedGUI(t, writeRequestFile(t, dir, req), filepath.Join(dir, "res"))
	seen := map[string]eventEnvelope{}
	for {
		env, ok := gui.nextEvent()
		if !ok {
			break
		}
		if _, dup := seen[env.Type]; !dup {
			seen[env.Type] = env
		}
		if env.Type == "completed" {
			break
		}
	}
	if code := gui.finish(); code != 0 {
		t.Fatalf("pinning session run exit = %d", code)
	}
	checkStreamAgainstFixtures(t, seen)

	// A scenario run: the scenario-shaped phase_started.
	cfg := config.Config{Test: "general", Duration: "50ms", Workers: 1, ControlCore: intPtr(-1), LogFile: "pin.log"}
	sreq := lastRun{Version: lastRunVersion, Kind: lastRunScenario, Config: &cfg,
		Phases: []config.Phase{{Name: "one", A: "g01d-pin", Cores: []int{0}, Duration: "50ms"}}}
	sdir := t.TempDir()
	sgui := startManagedGUI(t, writeRequestFile(t, sdir, sreq), filepath.Join(sdir, "res"))
	sseen := map[string]eventEnvelope{}
	for {
		env, ok := sgui.nextEvent()
		if !ok {
			break
		}
		if _, dup := sseen[env.Type]; !dup {
			sseen[env.Type] = env
		}
		if env.Type == "completed" {
			break
		}
	}
	if code := sgui.finish(); code != 0 {
		t.Fatalf("pinning scenario run exit = %d", code)
	}
	if ps, ok := sseen["phase_started"]; ok {
		b, _ := json.Marshal(ps.Payload)
		samePaths(t, "stream phase_started (scenario)", keyPaths(t, fixtureBytes(t, "payload_phase_started_scenario.json")), keyPaths(t, b))
	} else {
		t.Error("scenario stream lacks phase_started")
	}

	// A cancelled run: cancelling, cancel_ack and command_error all
	// wear their fixtures.
	req2 := sessionRequest()
	req2.Session.Workload = "g01d-pin"
	req2.Session.Duration = 30 * time.Second // the cancel ends it
	cdir := t.TempDir()
	cgui := startManagedGUI(t, writeRequestFile(t, cdir, req2), filepath.Join(cdir, "res"))
	started := cgui.waitFor("started")
	cgui.send(map[string]any{"type": "selftest"})
	cgui.send(map[string]any{"type": "cancel", "session_id": started.SessionID, "command_id": "p1"})
	cseen := map[string]eventEnvelope{}
	for {
		env, ok := cgui.nextEvent()
		if !ok {
			break
		}
		if _, dup := cseen[env.Type]; !dup {
			cseen[env.Type] = env
		}
		if env.Type == "completed" {
			break
		}
	}
	cgui.finish()
	for _, typ := range []string{"cancelling", "cancel_ack", "command_error"} {
		env, ok := cseen[typ]
		if !ok {
			t.Errorf("cancel stream lacks %q (types: %s)", typ, cgui.typesSeen())
			continue
		}
		b, _ := json.Marshal(env.Payload)
		samePaths(t, "stream "+typ, keyPaths(t, fixtureBytes(t, "payload_"+typ+".json")), keyPaths(t, b))
	}
}

// A managed run whose request passes the shape gates but names an
// external tool that is not on disk is refused before the session
// identity exists - and the stream says so with the honest error
// record, never a fake completed. (A workload that fails inside the
// executor is different: there the session exists, and the completed
// record carries its real setup_error state.)
func TestStreamErrorRecordMatchesFixture(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	dir := t.TempDir()
	req := sessionRequest()
	req.Session.Workload = "g01d-ghost"
	req.Config.External = map[string]workload.ExternalSpec{"g01d-ghost": {
		Command: filepath.Join(dir, "definitely-not-here.exe"),
	}}
	gui := startManagedGUI(t, writeRequestFile(t, dir, req), filepath.Join(dir, "res"))
	var errEnv eventEnvelope
	for {
		env, ok := gui.nextEvent()
		if !ok {
			break
		}
		if env.Type == "error" {
			errEnv = env
		}
		if env.Type == "completed" {
			t.Fatal("a run that never started reported completed")
		}
	}
	if code := gui.finish(); code != 2 {
		t.Errorf("unrunnable request exit = %d, want 2", code)
	}
	if errEnv.Type != "error" {
		t.Fatalf("no error record (types: %s)", gui.typesSeen())
	}
	b, _ := json.Marshal(errEnv.Payload)
	samePaths(t, "stream error", keyPaths(t, fixtureBytes(t, "payload_error.json")), keyPaths(t, b))
}

// The info answers wear their fixtures too: the envelope keys and
// the item keys a GUI reads are exactly the fixture's.
func TestInfoAnswersMatchFixtures(t *testing.T) {
	chdirTemp(t)

	code, out, _ := runCapture(t, func() int { return catalogCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("catalog exit %d", code)
	}
	samePaths(t, "catalog answer", keyPaths(t, fixtureBytes(t, "answer_catalog.json")), keyPaths(t, []byte(out)))

	code, out, stderr := runCapture(t, func() int { return topologyCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("topology exit %d: %s", code, stderr)
	}
	samePaths(t, "topology answer", keyPaths(t, fixtureBytes(t, "answer_topology.json")), keyPaths(t, []byte(out)))

	code, out, stderr = runCapture(t, func() int { return capsCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("caps exit %d: %s", code, stderr)
	}
	samePaths(t, "caps answer", keyPaths(t, fixtureBytes(t, "answer_caps.json")), keyPaths(t, []byte(out)))

	// History needs one recorded session so the item shape shows up.
	if err := saveSessionManifest(newTestManifest("20260101-000000-0000000000000001")); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCapture(t, func() int { return historyCmd([]string{"-json"}) })
	if code != 0 {
		t.Fatalf("history exit %d", code)
	}
	samePaths(t, "history answer", keyPaths(t, fixtureBytes(t, "answer_history.json")), keyPaths(t, []byte(out)))

	// Validate answers against the session request fixture.
	code, out, stderr = runCapture(t, func() int {
		return validateCmd([]string{"-request", filepath.Join(fixturesDir, "request_session.json"), "-json"})
	})
	if code != 0 {
		t.Fatalf("validate on its own fixture refused: %s\n%s", out, stderr)
	}
	samePaths(t, "validate answer", keyPaths(t, fixtureBytes(t, "answer_validate.json")), keyPaths(t, []byte(out)))
}

// checkStreamAgainstFixtures compares every seen event payload that
// has a fixture against that fixture's key set.
func checkStreamAgainstFixtures(t *testing.T, seen map[string]eventEnvelope) {
	t.Helper()
	envFix := keyPaths(t, fixtureBytes(t, "envelope.json"))
	payloadFile := map[string]string{
		"resolved":       "payload_resolved.json",
		"started":        "payload_started.json",
		"phase_finished": "payload_phase_finished.json",
		"completed":      "result_file.json",
	}
	for typ, env := range seen {
		for _, req := range []string{"schema_version", "seq", "ts", "type"} {
			if !envFix[req] {
				t.Fatalf("envelope fixture lacks the mandatory key %q", req)
			}
		}
		if file, ok := payloadFile[typ]; ok {
			b, err := json.Marshal(env.Payload)
			if err != nil {
				t.Fatal(err)
			}
			samePaths(t, "stream "+typ, keyPaths(t, fixtureBytes(t, file)), keyPaths(t, b))
		}
	}
	if ps, ok := seen["phase_started"]; ok {
		b, _ := json.Marshal(ps.Payload)
		samePaths(t, "stream phase_started (session)", keyPaths(t, fixtureBytes(t, "payload_phase_started_session.json")), keyPaths(t, b))
	}
}
