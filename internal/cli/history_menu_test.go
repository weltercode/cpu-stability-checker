package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// The history screen lists the sessions from their manifests (not
// from the current journal), newest first, and selecting one offers
// the session-bound report - the acceptance line "последние 10
// sessions видны; report привязан к выбранной сессии".
func TestHistoryMenuListsAndReports(t *testing.T) {
	chdirTemp(t)
	const newID, oldID = "20261017-100000-aaaaaaaaaaaaaaaa", "20261016-100000-bbbbbbbbbbbbbbbb"
	jp := "s1.log.journal.jsonl"
	writeSessionJournal(t, jp, "run-s1", newID, false)
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion, SessionID: newID,
		Kind: "run", Started: "2026-10-17T10:00:00Z", State: "completed", Validation: string(plan.ValVerified),
		Runs: []manifestRun{{Log: "s1.log", Journal: jp}}}); err != nil {
		t.Fatal(err)
	}
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion, SessionID: oldID,
		Kind: "session", Started: "2026-10-16T10:00:00Z", State: manifestRunning}); err != nil {
		t.Fatal(err)
	}

	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("5\n1\n1\n\nq\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		"Session history",
		localWant(t, "2026-10-17T10:00:00Z") + newID + " · run · no errors found",
		localWant(t, "2026-10-16T10:00:00Z") + oldID + " · session · not finished",
		"core report for session " + newID,
		"1 run(s) attributed to this session",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history screen lost %q:\n%s", want, out)
		}
	}
	if strings.Index(out, newID) > strings.Index(out, oldID) {
		t.Error("history not newest-first")
	}
	// Reading history changed nothing on disk.
	m, err := loadSessionManifest(oldID)
	if err != nil || m.State != manifestRunning {
		t.Errorf("history read rewrote the crashed session: %+v err %v", m, err)
	}
}

// More than ten manifests: exactly ten are listed and the truncation
// is said out loud. An empty history says there is none instead of
// an empty screen.
func TestHistoryMenuTenLimitAndEmpty(t *testing.T) {
	chdirTemp(t)
	for i := range 11 {
		if err := saveSessionManifest(&sessionManifest{Version: manifestVersion,
			SessionID: fmt.Sprintf("20261017-1000%02d-0000000000000000", i),
			Kind:      "run", Started: "2026-10-17T10:00:00Z", State: "completed",
			Validation: string(plan.ValVerified)}); err != nil {
			t.Fatal(err)
		}
	}
	out := captureStdoutAndRun(t, func() {
		menuLoop(bufio.NewReader(strings.NewReader("5\nq\n0\n")), config.Default())
	})
	if !strings.Contains(out, "showing 10 of 11") {
		t.Errorf("truncation not stated:\n%s", out)
	}

	chdirTemp(t)
	out = captureStdoutAndRun(t, func() {
		menuLoop(bufio.NewReader(strings.NewReader("5\n\n0\n")), config.Default())
	})
	if !strings.Contains(out, "No sessions yet") {
		t.Errorf("empty history not stated:\n%s", out)
	}
}

// A session whose manifest holds no request cannot be replayed -
// and says so instead of replaying whatever stale slot lastrun.json
// happens to hold.
func TestHistoryMenuReplayRefusedWithoutRequest(t *testing.T) {
	chdirTemp(t)
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion, SessionID: "S-noreq",
		Kind: "run", Started: "2026-10-17T10:00:00Z", State: "completed",
		Validation: string(plan.ValVerified)}); err != nil {
		t.Fatal(err)
	}
	// A stale slot exists; replaying it would answer a question
	// nobody asked, so the refusal must name the manifest instead.
	stale := lastRun{Kind: lastRunSession, RecipeID: "stale-slot", Config: &config.Config{},
		Session: &SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
			Workload: "int", ThreadsPerCore: 1, Duration: time.Second, Passes: 1}}
	if err := saveLastRun(stale); err != nil {
		t.Fatal(err)
	}
	out := captureStdoutAndRun(t, func() {
		menuLoop(bufio.NewReader(strings.NewReader("5\n1\n3\nq\nq\n0\n")), config.Default())
	})
	if !strings.Contains(out, "Repeat unavailable") {
		t.Errorf("replay without a stored request went unrefused:\n%s", out)
	}
	if lr, err := loadLastRun(); err != nil || lr.RecipeID != "stale-slot" {
		t.Errorf("the refused replay touched the slot: %+v err %v", lr, err)
	}
}

// Replaying a history session goes through the ordinary entry
// points: the saved request becomes the replay slot, the run is a
// NEW session (fresh id, fresh manifest), and the old manifest is
// untouched.
func TestHistoryMenuReplayRunsAsNewSession(t *testing.T) {
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
	workload.Register(&workload.Workload{Name: "hist-replay-fake",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	defer workload.Delete("hist-replay-fake")

	control := -1
	req := &lastRun{Kind: lastRunSession, RecipeID: "recipe-hist",
		Session: &SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
			Workload: "hist-replay-fake", ThreadsPerCore: 1, Duration: 50 * time.Millisecond,
			Passes: 1, ControlCore: &control, LogFile: "hr.log"},
		Config: &config.Config{}}
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion, SessionID: "20260101-000000-0000000000000000",
		Kind: "session", Started: "2026-01-01T00:00:00Z", State: "completed",
		Validation: string(plan.ValVerified), RecipeID: "recipe-hist", Request: req}); err != nil {
		t.Fatal(err)
	}

	out := captureStdoutAndRun(t, func() {
		menuLoop(bufio.NewReader(strings.NewReader("5\n1\n3\nq\nq\n0\n")), config.Default())
	})
	if !strings.Contains(out, "Starting session") {
		t.Errorf("the replay never started a session:\n%s", out)
	}
	if lr, err := loadLastRun(); err != nil || lr.RecipeID != "recipe-hist" {
		t.Errorf("replay did not make the history request the slot: %+v err %v", lr, err)
	}
	list, warns := listSessionManifests()
	if len(warns) > 0 {
		t.Fatalf("manifest warnings: %v", warns)
	}
	if len(list) != 2 {
		t.Fatalf("history = %d manifests, want the old one plus the fresh replay session", len(list))
	}
	if list[0].SessionID == "20260101-000000-0000000000000000" {
		t.Error("the replay reused the old session id instead of minting a fresh one")
	}
	if list[0].RecipeID != "recipe-hist" || list[0].Kind != "session" {
		t.Errorf("the replay session manifest lost its binding: %+v", list[0])
	}
}

// The state wording never upgrades: verified completions are the
// only "no errors found", unverified ones stay yellow and named,
// and an unknown state keeps its own words.
func TestSessionStateWordsNoUpgrade(t *testing.T) {
	cases := []struct {
		state, val, wantText, wantColor string
	}{
		{"completed", string(plan.ValVerified), "no errors found", ""},
		{"completed", string(plan.ValPartial), "completed, verification not confirmed", tui.FGYellow},
		{"completed", "", "completed, verification not confirmed", tui.FGYellow},
		{manifestRunning, "", "not finished (crash or still running?)", tui.FGYellow},
		{"unknown", "", "result unknown", tui.FGYellow},
		{"runtime_error", string(plan.ValVerified), "verification error", tui.FGBrightRed},
		{"some_future_state", "", "some_future_state", tui.FGYellow},
	}
	for _, c := range cases {
		m := sessionManifest{State: c.state, Validation: c.val}
		text, color := sessionStateWords(m)
		if text != c.wantText || color != c.wantColor {
			t.Errorf("state %q/%q = %q/%q, want %q/%q", c.state, c.val, text, color, c.wantText, c.wantColor)
		}
	}
}

// localWant mirrors the display contract from the test side: the line
// leads with the LOCAL reading of the manifest's UTC start, then the
// id unchanged. If a machine somehow ran in UTC the strings coincide -
// either way the id itself must never be the converted one.
func localWant(t *testing.T, utc string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, utc)
	if err != nil {
		t.Fatal(err)
	}
	return ts.In(time.Local).Format("02.01.2006 15:04") + " · "
}

// The conversion is honest in both directions: only parseable UTC
// stamps get a local reading (and a non-UTC offset converts just the
// same), unknown moments yield nothing - never an invented clock.
func TestLocalClockTextHonest(t *testing.T) {
	for _, bad := range []string{"", "not a time", "2026-10-17 10:00"} {
		if got := localClockText(bad); got != "" {
			t.Errorf("localClockText(%q) = %q, want empty (unknown, never invented)", bad, got)
		}
	}
	for _, good := range []string{"2026-10-17T10:00:00Z", "2026-10-17T10:00:00+04:00"} {
		ts, err := time.Parse(time.RFC3339, good)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := localClockText(good), ts.In(time.Local).Format("02.01.2006 15:04"); got != want {
			t.Errorf("localClockText(%q) = %q, want local %q", good, got, want)
		}
	}
}

// Details state what the manifest knows and mark what it does not:
// no finish time after a crash, unknown exit, missing files said as
// missing.
func TestSessionDetailsHonestFields(t *testing.T) {
	chdirTemp(t)
	m := &sessionManifest{Version: manifestVersion, SessionID: "S-det", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: manifestRunning,
		Runs: []manifestRun{{Name: "pass 1", Log: "gone.log", Journal: "gone.log.journal.jsonl"}}}
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	printSessionDetails(&buf, "S-det")
	got := buf.String()
	for _, want := range []string{
		"not recorded",
		"exit code: unknown",
		"verification: unknown",
		"recipe: unbound",
		"request: not saved",
		"missing",
		// The UTC stamp stays printed (it is the file truth), and the
		// local reading is appended beside it, not over it.
		"start: 2026-10-17T10:00:00Z (local ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("details lost %q:\n%s", want, got)
		}
	}
	// Read-only: the manifest on disk is byte-identical.
	before, _ := os.ReadFile(manifestPath("S-det"))
	printSessionDetails(&buf, "S-det")
	after, _ := os.ReadFile(manifestPath("S-det"))
	if string(before) != string(after) {
		t.Error("printSessionDetails modified the manifest")
	}
}
