package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
)

// R19 (owner queue 2026-11-09): every entry point closes a run with
// ONE shared evidence block - journal paths (existence said, not
// assumed), the report/bundle sharing route, and per-phase pointers
// for WHEA windows that were not cleanly available. The pins below
// hold the honesty rules the block lives by: state words verbatim,
// recorded reasons quoted verbatim, unknown reasons stated as
// unrecorded, no merged verdict, no elevation promise, and the
// managed JSONL stream untouched.

func TestAppendWheaObsMergeRules(t *testing.T) {
	var obs []wheaObs
	appendWheaObs(&obs, "a", "", "whatever")  // never watched: not an observation to report per phase
	appendWheaObs(&obs, "b", "available", "") // clean window: no pointer needed
	if len(obs) != 0 {
		t.Fatalf("clean/never-watched windows collected: %+v", obs)
	}
	appendWheaObs(&obs, "p1", "degraded", "wevtutil exit 15001")
	appendWheaObs(&obs, "p2", "degraded", "wevtutil exit 15001") // same story: one line, both named
	if len(obs) != 1 || len(obs[0].phases) != 2 {
		t.Fatalf("consecutive identical stories did not merge: %+v", obs)
	}
	appendWheaObs(&obs, "p3", "degraded", "other failure") // a different reason is a different story
	if len(obs) != 2 {
		t.Fatalf("distinct reason must open a new entry: %+v", obs)
	}
	appendWheaObs(&obs, "p4", "available", "") // a clean phase between does not erase the earlier story
	appendWheaObs(&obs, "p5", "degraded", "other failure")
	if len(obs) != 2 || len(obs[1].phases) != 2 {
		t.Fatalf("phase attribution drifted: %+v", obs)
	}
	appendWheaObs(nil, "p6", "degraded", "x") // nil sink: capture-off callers never crash
}

// captureWheaWindow must attribute observations to the run that made
// them. runctl.Current() survives the run (the slot keeps the last
// Guard), so a stale or foreign Guard must never speak for a phase -
// and a never-watched window must stay silent even under a matching
// identity, because "never watched" is nothing to report per phase.
func TestCaptureWheaWindowAttribution(t *testing.T) {
	g := runctl.New(context.Background(), plan.Options{PlanHash: "r19-attribution-A"}, runctl.Deps{})
	t.Cleanup(g.Finish) // never started: Finish is the documented cleanup path

	var obs []wheaObs
	captureWheaWindow("phase-x", "r19-attribution-B", &obs) // a DIFFERENT plan's phase
	if len(obs) != 0 {
		t.Errorf("a foreign Guard spoke for another run's phase: %+v", obs)
	}
	captureWheaWindow("phase-y", "r19-attribution-A", &obs) // matching plan, but the window was never watched
	if len(obs) != 0 {
		t.Errorf("never-watched (state \"\") was reported as an observation: %+v", obs)
	}
	captureWheaWindow("phase-z", "", &obs) // unset want-hash (test callers): never crash
	if len(obs) != 0 {
		t.Errorf("unset identity should not invent an observation: %+v", obs)
	}
	captureWheaWindow("phase-w", "r19-attribution-A", nil) // capture-off callers never crash
}

func TestPrintRunEvidenceHonestyContract(t *testing.T) {
	dir := t.TempDir()
	realJournal := filepath.Join(dir, "journal-live.jsonl")
	if err := os.WriteFile(realJournal, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ghostJournal := filepath.Join(dir, "journal-never-written.jsonl")

	var buf bytes.Buffer
	runs := []manifestRun{
		{Name: "int-all", Log: "a.log", Journal: realJournal},
		{Name: "float-all", Log: "a.log", Journal: realJournal}, // shared file: ONE line naming both
		{Name: "warm", Log: "b.log", Journal: ghostJournal},
	}
	obs := []wheaObs{
		{phases: []string{"warm"}, state: "degraded", reason: "wevtutil query failed: exit 15001 syntax error at position <end>"},
		{phases: []string{"int-all", "float-all"}, state: "initializing", reason: "WHEA monitoring armed: first query cycle not completed yet"},
		{phases: []string{"burst 1/3"}, state: "unavailable", reason: ""},
	}
	printRunEvidence(&buf, runs, obs)
	out := buf.String()

	for _, want := range []string{
		realJournal + " (int-all, float-all)",                                           // grouped under the one file they share
		ghostJournal + " (warm)  [no file there: this attempt journaled nothing to it]", // existence said, not assumed
		"last result:",
		"cpu-stability report",
		"cpu-stability bundle",
		"safe to send",     // the bundle framing: the scrubbed folder, not the raw journal
		"[state degraded]", // vocabulary verbatim, never paraphrased
		`"wevtutil query failed: exit 15001 syntax error at position <end>"`, // reason quoted verbatim
		"[state initializing]",
		"proves nothing about coverage either way", // armed-unproven: a zero count is not coverage
		"[state unavailable]",
		"could not look",         // zero records must not read as zero events
		"no reason was recorded", // missing reason stated as missing, never filled in
		"whea_state records",     // the journal stays the authoritative record
	} {
		if !strings.Contains(out, want) {
			t.Errorf("evidence block missing %q:\n%s", want, out)
		}
	}
	// The elevation pointer: shown on Windows when a window was not
	// available, phrased as a lever and explicitly never a diagnosis;
	// on other platforms the sentence simply is not there.
	if runtime.GOOS == "windows" {
		for _, want := range []string{"as administrator", "never why", "never a verdict about the machine"} {
			if !strings.Contains(out, want) {
				t.Errorf("Windows elevation pointer missing %q:\n%s", want, out)
			}
		}
	} else if strings.Contains(out, "administrator") {
		t.Errorf("elevation sentence shown on %s:\n%s", runtime.GOOS, out)
	}
	// No merged per-phase WHEA verdict anywhere: windows keep their
	// own words, and the block never grades the machine.
	for _, banned := range []string{"WHEA: PASS", "WHEA: FAIL", "WHEA verdict", "hardware verdict"} {
		if strings.Contains(out, banned) {
			t.Errorf("evidence block stated a combined verdict (%q):\n%s", banned, out)
		}
	}

	// A clean run still gets paths and sharing, but NO WHEA section:
	// silence about observation quality is only honest when every
	// window was available - which is the caller's contract here.
	var clean bytes.Buffer
	printRunEvidence(&clean, runs, nil)
	if strings.Contains(clean.String(), "WHEA hardware-error observation") {
		t.Errorf("WHEA section shown with nothing to say:\n%s", clean.String())
	}
	for _, want := range []string{"journal:", "cpu-stability bundle"} {
		if !strings.Contains(clean.String(), want) {
			t.Errorf("clean-state evidence block missing %q:\n%s", want, clean.String())
		}
	}
}

func TestPrintRunEvidencePhaseListCapped(t *testing.T) {
	obs := []wheaObs{{state: "degraded", reason: "same failure all the way down"}}
	for i := range 10 {
		obs[0].phases = append(obs[0].phases, string(rune('a'+i)))
	}
	var buf bytes.Buffer
	printRunEvidence(&buf, nil, obs)
	out := buf.String()
	if !strings.Contains(out, "+2 more windows with the identical state, each named in the journal") {
		t.Errorf("long identical window list not capped with an honest pointer:\n%s", out)
	}
	if strings.Contains(out, "phase "+string(rune('a'+9))) {
		t.Errorf("cap was ignored, all phases spelled out:\n%s", out)
	}
}

// The scripted single run closes with exactly one evidence block: the
// journal file the run actually wrote is named WITHOUT the
// nothing-journaled note, and the sharing routes are in. (Whether a
// 100ms window shows its own WHEA state depends on when wevtutil's
// first cycle completed - deliberately NOT pinned here; the state
// plumbing itself is pinned at the renderer and merge levels above.)
func TestRunCmdEndsWithEvidenceBlock(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)

	code, out := captureAnswer(t, func() int {
		return runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
			"-cores", "0", "-control", "-1", "-log", "ev.log"})
	})
	if code != 0 {
		t.Fatalf("run exit = %d, want 0\n%s", code, out)
	}
	if n := strings.Count(out, "Where the evidence lives"); n != 1 {
		t.Fatalf("evidence block printed %d times, want exactly 1:\n%s", n, out)
	}
	jp := journal.Path("ev.log")
	if !strings.Contains(out, jp) {
		t.Errorf("the run's own journal %q is not named:\n%s", jp, out)
	}
	if strings.Contains(out, "[no file there") {
		t.Errorf("a journal this run wrote is described as absent:\n%s", out)
	}
	if _, err := os.Stat(jp); err != nil {
		t.Fatalf("journal %q really is missing: %v", jp, err)
	}
	for _, want := range []string{"cpu-stability report", "cpu-stability bundle", "safe to send", "last result:"} {
		if !strings.Contains(out, want) {
			t.Errorf("runCmd closing block missing %q:\n%s", want, out)
		}
	}
}
