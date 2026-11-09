package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// R21 (owner queue 2026-11-09): the hard-lockup witness. These tests pin
// the honesty contract of the block a load-bearing entry point prints
// BEFORE arming its own receipt: the previous attempt is stated
// INCOMPLETE with each source's answer named separately; shutdown records
// appear only as time candidates with the boot-boundary caveat; a clean
// and empty System-log answer is named WITHOUT being read as a clean end;
// an unavailable source names itself; and a clean start prints nothing and
// queries nothing. The fake seam replaces wevtutil everywhere - the real
// query acceptance lives in the whea package's named real-log test.

// setWitnessSeam swaps the shutdown-record seam (and resets the
// once-per-evidence marker) for one test, restoring both after.
func setWitnessSeam(t *testing.T, fn func(limit int) ([]whea.Event, error)) *int {
	t.Helper()
	prevFn, prevMarker := shutdownRecordsQuery, witnessShownMarker
	calls := 0
	shutdownRecordsQuery = func(limit int) ([]whea.Event, error) {
		calls++
		return fn(limit)
	}
	witnessShownMarker = ""
	t.Cleanup(func() {
		shutdownRecordsQuery, witnessShownMarker = prevFn, prevMarker
	})
	return &calls
}

// armPendingReceipt writes a pending receipt for a dead attempt through
// the real writer, so the fixture is byte-identical to what a crash
// leaves. The returned receipt is left unfinalized on purpose.
func armPendingReceipt(t *testing.T, recipe string, logs, hashes []string) *lastResult {
	t.Helper()
	tr := newAttemptReceipt(recipe, 1, logs, hashes)
	if tr == nil {
		t.Fatal("receipt refused: recipe id empty")
	}
	raw, err := os.ReadFile(lastResultPath)
	if err != nil {
		t.Fatal(err)
	}
	var r lastResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func writeManifestFixture(t *testing.T, m sessionManifest) {
	t.Helper()
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(historyDir, m.SessionID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJournalFixture emits real journal records (one JSON object per
// line) so recovery decodes the fixture exactly as it decodes a real
// file - including the absence of core_done/run_end, which IS the
// crashed story.
func writeJournalFixture(t *testing.T, logFile string, lines ...string) string {
	t.Helper()
	path := journal.Path(logFile)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func jrec(r journal.Record) string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// crashedRunLines is the journal of one run that started, beat once, and
// never recorded an ending: the shape a hard lockup leaves.
func crashedRunLines(runID string, beat time.Time, planHash string) []string {
	core := 5
	ts := beat.UTC().Format(time.RFC3339Nano)
	return []string{
		jrec(journal.Record{Schema: 1, Type: journal.RunStart, RunID: runID, Test: "general", Cores: []int{core}, TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.RunMeta, RunID: runID, Meta: &journal.RunMetaFields{PlanHash: planHash}, TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.CoreStart, RunID: runID, Core: &core, Workload: "int", TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.CoreBeat, RunID: runID, Core: &core, Iters: 1234, TS: ts}),
	}
}

func endedRunLines(runID string, when time.Time) []string {
	core := 5
	ts := when.UTC().Format(time.RFC3339Nano)
	return []string{
		jrec(journal.Record{Schema: 1, Type: journal.RunStart, RunID: runID, Test: "general", Cores: []int{core}, TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.CoreStart, RunID: runID, Core: &core, Workload: "int", TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.CoreDone, RunID: runID, Core: &core, Workload: "int", Result: journal.OK, TS: ts}),
		jrec(journal.Record{Schema: 1, Type: journal.RunEnd, RunID: runID, TS: ts}),
	}
}

func TestPreviousAttemptWitnessTable(t *testing.T) {
	// The pending receipt is armed through the real writer, so its
	// StartedAt is stamped at arm-time (now-ish): every fixture time is
	// therefore anchored to this same base so the window arithmetic is
	// exact regardless of when the suite runs.
	base := time.Now().UTC()
	kp41 := whea.Event{
		Time:     base.Add(2 * time.Minute), // written on the NEXT boot
		Provider: "Microsoft-Windows-Kernel-Power", EventID: 41, RecordID: 220001,
	}
	el6008 := whea.Event{
		Time:     base.Add(2 * time.Minute).Add(time.Second),
		Provider: "EventLog", EventID: 6008, RecordID: 220009,
	}
	undated := whea.Event{Provider: "EventLog", EventID: 6008, RecordID: 220010}

	for _, tc := range []struct {
		name    string
		seam    func(limit int) ([]whea.Event, error)
		setup   func(t *testing.T) // after chdirTemp + scratch receipt
		want    []string
		notWant []string
		calls   int
	}{
		{
			name: "clean start: nothing unreadable, nothing unfinished",
			seam: func(int) ([]whea.Event, error) {
				t.Error("a clean start must never query the System log")
				return nil, nil
			},
			want:  nil,
			calls: 0,
		},
		{
			name: "final receipt: yesterday's ending is not this block's business",
			setup: func(t *testing.T) {
				tr := newAttemptReceipt("recipe-done", 1, []string{"old.log"}, nil)
				tr.finish(0, []plan.Result{{Exit: 0, Execution: plan.ExecCompleted, Validation: plan.ValVerified}})
			},
			seam: func(int) ([]whea.Event, error) {
				t.Error("a finished attempt must never query the System log")
				return nil, nil
			},
			want:  nil,
			calls: 0,
		},
		{
			name: "pending + crashed cores + OS records: the full hard-lockup story",
			setup: func(t *testing.T) {
				jp := writeJournalFixture(t, "lost.log", crashedRunLines("run-crashed", base, "hash-A")...)
				armPendingReceipt(t, "recipe-x", []string{"lost.log"}, []string{"hash-A"})
				_ = jp
			},
			seam: func(int) ([]whea.Event, error) { return []whea.Event{kp41, el6008}, nil },
			want: []string{
				"The previous attempt never reported an ending",
				`state "pending"`,
				"run run-crashed (general):",
				"core 5 (int): CRASHED - last seen " + base.Format(time.RFC3339) + " at 1234 iterations",
				"Microsoft-Windows-Kernel-Power, event 41, log record 220001",
				"EventLog, event 6008, log record 220009",
				"2m0s after the unfinished attempt's last journal beat at " + base.Format(time.RFC3339), // time relation, with bounds
				"boot AFTER an unexpected end of the previous one",
				"time candidates only",
				"never claims which boot a record belongs to",
				"never read for meaning here", // embedded fields stay uninterpreted
				"previous attempt: INCOMPLETE - started",
				"time relation stated by the OS, not a cause stated by this tool",
				"What this block does NOT say",
				"nothing on disk was changed to produce this block",
			},
			notWant: []string{
				"caused by", " because ", "diagnos", "lost power", "crashed due",
				"WHEA", // this block is not the WHEA observation vocabulary
			},
			calls: 1,
		},
		{
			name: "no beat recorded: 'never reported progress', and no fake delta",
			setup: func(t *testing.T) {
				core := 7
				writeJournalFixture(t, "nb.log",
					jrec(journal.Record{Schema: 1, Type: journal.RunStart, RunID: "run-nb", Test: "general", TS: base.Format(time.RFC3339Nano)}),
					jrec(journal.Record{Schema: 1, Type: journal.CoreStart, RunID: "run-nb", Core: &core, Workload: "int", TS: base.Format(time.RFC3339Nano)}))
				armPendingReceipt(t, "recipe-nb", []string{"nb.log"}, nil)
			},
			seam:    func(int) ([]whea.Event, error) { return []whea.Event{kp41}, nil },
			want:    []string{"core 7 (int): CRASHED - never reported progress"},
			notWant: []string{"after the unfinished attempt's last journal beat"},
			calls:   1,
		},
		{
			name: "readable log, no record for the window: absence named, never 'clean'",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "al.log", crashedRunLines("run-al", base, "h")...)
				armPendingReceipt(t, "recipe-al", []string{"al.log"}, []string{"h"})
			},
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"does NOT mean the end was clean", "absence is named, not read as a clean end"},
			calls: 1,
		},
		{
			name: "source refuses: the refusal's own words become the named reason",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "err.log", crashedRunLines("run-err", base, "h")...)
				armPendingReceipt(t, "recipe-err", []string{"err.log"}, []string{"h"})
			},
			seam: func(int) ([]whea.Event, error) {
				return nil, fmt.Errorf("wevtutil: Access is denied because the log is in use")
			},
			want: []string{"NOT AVAILABLE - wevtutil: Access is denied", "named as unknown"},
			// The refusal text travels verbatim as the source's own words;
			// the block adds no interpretation of its own.
			notWant: []string{"caused by", "diagnos"},
			calls:   1,
		},
		{
			name: "older records filtered out with a named sentence, not silently",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "old.log", crashedRunLines("run-old", base, "h")...)
				armPendingReceipt(t, "recipe-old", []string{"old.log"}, []string{"h"})
			},
			seam: func(int) ([]whea.Event, error) {
				old := kp41
				old.Time = base.Add(-2 * time.Hour) // predates the attempt entirely
				return []whea.Event{old}, nil
			},
			want: []string{
				"predate the unfinished attempt's start: different moments, not part of this story",
				"does NOT mean the end was clean",
			},
			notWant: []string{"log record 220001, record time"},
			calls:   1,
		},
		{
			name: "undated record kept as evidence: never dropped, never dated",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "ud.log", crashedRunLines("run-ud", base, "h")...)
				armPendingReceipt(t, "recipe-ud", []string{"ud.log"}, []string{"h"})
			},
			seam:  func(int) ([]whea.Event, error) { return []whea.Event{undated}, nil },
			want:  []string{"carried no readable time: kept as evidence by log record id, never dropped, never dated", "OS side stays unknown for this window"},
			calls: 1,
		},
		{
			name: "fetch bound stated when it bites",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "cap.log", crashedRunLines("run-cap", base, "h")...)
				armPendingReceipt(t, "recipe-cap", []string{"cap.log"}, []string{"h"})
			},
			seam: func(int) ([]whea.Event, error) {
				var many []whea.Event
				for i := range witnessShutdownRecords {
					e := kp41
					e.RecordID = 1000 + i
					many = append(many, e)
				}
				return many, nil
			},
			want:  []string{"coverage bound: only the 10 newest records of this kind were fetched"},
			calls: 1,
		},
		{
			name: "journal damage said loudly, in the same words as 'journal'",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "dmg.log", append(crashedRunLines("run-dmg", base, "h"), `{"type": broken json`)...)
				armPendingReceipt(t, "recipe-dmg", []string{"dmg.log"}, []string{"h"})
			},
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"JOURNAL DAMAGE", "the story below is missing pieces"},
			calls: 1,
		},
		{
			name:  "receipt names a journal that is simply not there: named absent, never skipped",
			setup: func(t *testing.T) { armPendingReceipt(t, "recipe-gone", []string{"gone.log"}, nil) },
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want: []string{
				journal.Path("gone.log") + ": cannot be read",
				"crashed-run story from this file is unavailable",
			},
			calls: 1,
		},
		{
			name: "all runs ended but the attempt never finalized: the contrast is said",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "fin.log", endedRunLines("run-fin", base)...)
				armPendingReceipt(t, "recipe-fin", []string{"fin.log"}, nil)
			},
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"every run it holds reported its own ending - what never finalized is the attempt record itself"},
			calls: 1,
		},
		{
			name: "attribution by run_meta: a foreign unfinished run is never claimed",
			setup: func(t *testing.T) {
				writeJournalFixture(t, "attr.log", crashedRunLines("run-other", base, "hash-DIFFERENT")...)
				armPendingReceipt(t, "recipe-attr", []string{"attr.log"}, []string{"hash-MINE"})
			},
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"run_meta plan hash names a different experiment: an older unfinished run in the same file, never attributed to this attempt"},
			calls: 1,
		},
		{
			name: "journal without provenance: attempt binding stated as unknown",
			setup: func(t *testing.T) {
				core := 3
				writeJournalFixture(t, "noprov.log",
					jrec(journal.Record{Schema: 1, Type: journal.RunStart, RunID: "run-np", Test: "general", TS: base.Format(time.RFC3339Nano)}),
					jrec(journal.Record{Schema: 1, Type: journal.CoreStart, RunID: "run-np", Core: &core, Workload: "int", TS: base.Format(time.RFC3339Nano)}))
				armPendingReceipt(t, "recipe-np", []string{"noprov.log"}, []string{"hash-np"})
			},
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"no run_meta provenance in this journal: attempt binding unknown"},
			calls: 1,
		},
		{
			name: "a manifest stuck running triggers on its own and names itself",
			setup: func(t *testing.T) {
				writeManifestFixture(t, sessionManifest{
					Version: manifestVersion, SessionID: "2026-11-09T12-00-00Z-deadbeef", Kind: "run",
					Started: base.Format(time.RFC3339), State: manifestRunning,
					Runs: []manifestRun{{Name: "run", Log: "man.log", Journal: journal.Path("man.log")}},
				})
				writeJournalFixture(t, "man.log", crashedRunLines("run-man", base, "h")...)
			},
			seam: func(int) ([]whea.Event, error) { return nil, nil },
			want: []string{
				`session manifest 2026-11-09T12-00-00Z-deadbeef: started ` + base.Format(time.RFC3339) + `, state still "running"`,
				"never finalized (history is never rewritten from here)",
				"core 5 (int): CRASHED - last seen",
			},
			calls: 1,
		},
		{
			name:  "a pending receipt that names no logs: unknown stated, never assumed",
			setup: func(t *testing.T) { armPendingReceipt(t, "recipe-nologs", nil, nil) },
			seam:  func(int) ([]whea.Event, error) { return nil, nil },
			want:  []string{"names no log files, so no journal story is readable from it (unknown, never assumed)"},
			calls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdirTemp(t)
			useScratchReceipt(t)
			calls := setWitnessSeam(t, tc.seam)
			if tc.setup != nil {
				tc.setup(t)
			}
			var buf strings.Builder
			printPreviousAttemptWitness(&buf)
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("witness missing %q:\n%s", want, out)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("witness stated forbidden %q:\n%s", notWant, out)
				}
			}
			if *calls != tc.calls {
				t.Errorf("System-log queried %d times, want %d", *calls, tc.calls)
			}
			if tc.want == nil && out != "" {
				t.Errorf("witness printed on a start that had nothing unfinished:\n%s", out)
			}
		})
	}
}

// The witness READS everything and rewrites nothing: the pending receipt
// is the evidence itself, and producing the block must leave it byte-for-byte
// intact until the next attempt legitimately replaces it.
func TestWitnessLeavesEvidenceUntouched(t *testing.T) {
	chdirTemp(t)
	path := useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) { return nil, nil })
	armPendingReceipt(t, "recipe-keep", nil, nil)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	printPreviousAttemptWitness(&buf)
	if buf.Len() == 0 {
		t.Fatal("pending receipt produced no witness")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the witness rewrote the receipt it was reading")
	}
	if _, merr := os.Stat(historyDir); merr == nil {
		if entries, _ := os.ReadDir(historyDir); len(entries) > 0 {
			t.Errorf("the witness wrote into the history dir: %v", entries)
		}
	}
}

// Same unfinished evidence: stated once per process (menu open shows it,
// the repeat moments later must not shout it twice). New evidence (a
// different attempt id) speaks again.
func TestWitnessShownOncePerEvidence(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) { return nil, nil })
	armPendingReceipt(t, "recipe-once", nil, nil)

	var out strings.Builder
	printPreviousAttemptWitness(&out)
	printPreviousAttemptWitness(&out)
	if n := strings.Count(out.String(), "never reported an ending. This tool reads"); n != 1 {
		t.Fatalf("same unfinished evidence printed %d times, want 1:\n%s", n, out.String())
	}

	armPendingReceipt(t, "recipe-new", nil, nil) // a genuinely newer unfinished attempt
	printPreviousAttemptWitness(&out)
	if n := strings.Count(out.String(), "never reported an ending. This tool reads"); n != 2 {
		t.Fatalf("new unfinished evidence did not print again: %d:\n%s", n, out.String())
	}
}

// The scripted single run shows the witness BEFORE its own receipt arms
// (which overwrites the pending evidence), exactly once, and with no
// wevtutil in sight - the seam proves the query count.
func TestRunCmdWitnessBeforeArming(t *testing.T) {
	chdirTemp(t)
	recPath := useScratchReceipt(t)
	setWitnessSeam(t, func(int) ([]whea.Event, error) {
		return []whea.Event{{Time: time.Now().UTC().Add(2 * time.Minute), Provider: "EventLog", EventID: 6008, RecordID: 5}}, nil
	})
	armPendingReceipt(t, "recipe-dead", []string{"dead-before.log"}, nil)

	code, out := captureAnswer(t, func() int {
		return runCmd([]string{"-test", "general", "-a", "int", "-duration", "100ms",
			"-cores", "0", "-control", "-1", "-log", "after.log"})
	})
	if code != 0 {
		t.Fatalf("run exit = %d, want 0\n%s", code, out)
	}
	if n := strings.Count(out, "never reported an ending. This tool reads"); n != 1 {
		t.Fatalf("witness printed %d times, want exactly 1:\n%s", n, out)
	}
	for _, want := range []string{
		`"recipe-dead"`, // the dead attempt is named (its receipt was read before arming replaced it)
		journal.Path("dead-before.log") + ": cannot be read",
		"previous attempt: INCOMPLETE",
		"EventLog, event 6008, log record 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("runCmd witness missing %q:\n%s", want, out)
		}
	}
	// The NEW attempt ended and finalized its own receipt: the old pending
	// evidence was legitimately replaced only AFTER being printed.
	raw, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatal(err)
	}
	var r lastResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.State != receiptFinal || r.RecipeID == "recipe-dead" {
		t.Errorf("receipt after the run = state %q recipe %q, want a final receipt of the new attempt", r.State, r.RecipeID)
	}
}
