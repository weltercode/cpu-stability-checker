package cli

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/config"
)

// useScratchObservations points the landmark store at the test's
// temp directory (same discipline as lastResultPath).
func useScratchObservations(t *testing.T) string {
	t.Helper()
	prev := observationsPath
	observationsPath = "observations.json" // inside the chdirTemp dir
	t.Cleanup(func() { observationsPath = prev })
	return observationsPath
}

func slotForProfile(recipeID, profileID, profileVersion string) error {
	lr := lastRun{Kind: lastRunSession, RecipeID: recipeID, Config: &config.Config{},
		Session: &SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0},
			Workload: "int", ThreadsPerCore: 1, Duration: time.Second, Passes: 1},
		ProfileID: profileID, ProfileVersion: profileVersion}
	return saveLastRun(lr)
}

// finalReceipt builds a minimal FINAL receipt the way finish() left it.
func finalReceipt(recipeID, attemptID, finishedAt, verdict string) lastResult {
	exit := 0
	return lastResult{Version: lastResultVersion, State: receiptFinal, RecipeID: recipeID,
		AttemptID: attemptID, FinishedAt: finishedAt, Exit: &exit,
		Verdict: verdict, VerdictReason: "test seam"}
}

// The landmark records the LAST attempt whatever its word was, and a
// staler attempt never displaces a newer record.
func TestObservationLastAttemptWins(t *testing.T) {
	chdirTemp(t)
	useScratchObservations(t)
	if err := slotForProfile("R1", "cpu-per-core", "v1"); err != nil {
		t.Fatal(err)
	}
	recordObservation(finalReceipt("R1", "A1", "2026-11-19T10:00:00Z", "PASS"))
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "PASS (attempt A1") {
		t.Errorf("first landmark not recorded:\n%s", s)
	}
	recordObservation(finalReceipt("R1", "A2", "2026-11-19T11:00:00Z", "FAIL"))
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "FAIL (attempt A2") {
		t.Errorf("the last attempt must win, whatever its word:\n%s", s)
	}
	recordObservation(finalReceipt("R1", "A3", "2026-11-19T09:00:00Z", "PASS"))
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "FAIL (attempt A2") {
		t.Errorf("a staler attempt displaced a newer record:\n%s", s)
	}
}

// Attribution is the recipe binding, fail-closed: a displaced slot or
// a manual-wizard request (no profile) records NOTHING and guesses
// no card.
func TestObservationUnattributableRecordsNothing(t *testing.T) {
	chdirTemp(t)
	path := useScratchObservations(t)
	if err := slotForProfile("R2", "", ""); err != nil {
		t.Fatal(err)
	}
	recordObservation(finalReceipt("R1", "A1", "2026-11-19T10:00:00Z", "PASS")) // displaced recipe
	recordObservation(finalReceipt("R2", "A2", "2026-11-19T10:00:00Z", "PASS")) // no profile on the slot
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an unattributable attempt created a landmark")
	}
}

// A corrupt store is replaced on write, and read as unknown until
// then - never as zero, never as "never tested".
func TestObservationCorruptStore(t *testing.T) {
	chdirTemp(t)
	path := useScratchObservations(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "unknown (") {
		t.Errorf("corrupt store read as something other than unknown:\n%s", s)
	}
	if err := slotForProfile("R1", "cpu-per-core", "v1"); err != nil {
		t.Fatal(err)
	}
	recordObservation(finalReceipt("R1", "A1", "2026-11-19T10:00:00Z", "FAIL"))
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "FAIL (attempt A1") {
		t.Errorf("the fresh record did not replace the corrupt store:\n%s", s)
	}
	if s := observationSummary("ram-random"); s != "last attempt here: no observation on record" {
		t.Errorf("a card with no record reads: %q", s)
	}
}

// A store from another format version stays unknown until a fresh
// record replaces it.
func TestObservationForeignVersion(t *testing.T) {
	chdirTemp(t)
	path := useScratchObservations(t)
	if err := os.WriteFile(path, []byte(`{"version":99,"profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := observationSummary("cpu-per-core"); !strings.Contains(s, "unknown (") {
		t.Errorf("foreign-version store read as something other than unknown:\n%s", s)
	}
}

// observations.json is protected identity for the removal guard: even
// a manifest that NAMES it as its evidence cannot carry it out - the
// landmark store is this machine's view, not a bundle file.
func TestObservationsProtectedFromHistoryClear(t *testing.T) {
	chdirTemp(t)
	useScratchObservations(t)
	if err := saveSessionManifest(&sessionManifest{Version: manifestVersion,
		SessionID: "20261119-100000-0000000000000000", Kind: "run",
		Started: "2026-11-19T10:00:00Z", State: "completed",
		Validation: "verified",
		Runs:       []manifestRun{{Name: "landmark", Log: observationsPathName()}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(observationsPath, []byte(`{"version":1,"profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := captureAnswer(t, func() int { return historyCmd([]string{"-clear", "-yes"}) })
	if code != 0 {
		t.Fatalf("clear exit = %d:\n%s", code, out)
	}
	if _, err := os.Stat(observationsPath); err != nil {
		t.Errorf("history clear removed observations.json:\n%s", out)
	}
	if !strings.Contains(out, "observations.json") {
		t.Errorf("the protection was not stated:\n%s", out)
	}
}

func observationsPathName() string { return observationsPath }

// Sol review discriminator: an un-stampable fresh attempt never
// displaces a valid record; a valid fresh stamp heals an unreadable
// old one; equal valid times let the newer write win.
func TestAttemptNewerTable(t *testing.T) {
	cases := []struct {
		prev, fresh string
		want        bool
	}{
		{"2026-11-19T10:00:00Z", "2026-11-19T11:00:00Z", true},
		{"2026-11-19T11:00:00Z", "2026-11-19T10:00:00Z", false},
		{"2026-11-19T10:00:00Z", "2026-11-19T10:00:00Z", true},
		{"not a stamp", "2026-11-19T10:00:00Z", true},
		{"2026-11-19T10:00:00Z", "not a stamp", false},
		{"not a stamp", "not a stamp", false},
	}
	for _, c := range cases {
		if got := attemptNewer(c.prev, c.fresh); got != c.want {
			t.Errorf("attemptNewer(%q, %q) = %v, want %v", c.prev, c.fresh, got, c.want)
		}
	}
}

// A landmark record that cannot name its word or attempt renders as
// unknown with a reason - never as blank fields presented as content.
func TestObservationIncompleteRecord(t *testing.T) {
	chdirTemp(t)
	useScratchObservations(t)
	if err := os.WriteFile(observationsPath, []byte(`{"version":1,"profiles":{"cpu-all":{"verdict":"","attempt_id":""}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := observationSummary("cpu-all"); !strings.Contains(s, "unknown (the landmark record is incomplete)") {
		t.Errorf("an incomplete record rendered as content:\n%s", s)
	}
}

// The card screens carry the landmark: the details view always - with
// the honest absence when there is no record - and the main menu's
// quick-check row when there is something to say.
func TestObservationsRenderOnCardScreens(t *testing.T) {
	chdirTemp(t)
	useScratchObservations(t)
	ui := &screenUI{}
	cards := categoryCards(catalog.CPU, catalog.Normal)
	in := bufio.NewReader(strings.NewReader("x\n")) // back, never consent
	out := captureStdout(t, func() { cardScreen(in, ui, cards[0]) })
	if !strings.Contains(out, "last attempt here: no observation on record") {
		t.Errorf("the empty store rendered as something else:\n%s", out)
	}
	if err := slotForProfile("R1", cards[0].ID, cards[0].Version); err != nil {
		t.Fatal(err)
	}
	recordObservation(finalReceipt("R1", "A1", "2026-11-19T10:00:00Z", "FAIL"))
	in = bufio.NewReader(strings.NewReader("x\n"))
	out = captureStdout(t, func() { cardScreen(in, ui, cards[0]) })
	if !strings.Contains(out, "last attempt here: FAIL (attempt A1") {
		t.Errorf("the recorded landmark is missing from the details screen:\n%s", out)
	}
	if err := slotForProfile("R2", "quick-check", "v1"); err != nil {
		t.Fatal(err)
	}
	recordObservation(finalReceipt("R2", "A2", "2026-11-19T11:00:00Z", "PASS"))
	out = captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "last attempt here: PASS (2026-11-19T11:00:00Z") {
		t.Errorf("the quick-check inline landmark is missing:\n%s", out)
	}
}
