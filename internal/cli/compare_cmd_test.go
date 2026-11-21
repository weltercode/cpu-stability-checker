package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func captureCompare(t *testing.T, args []string) (int, string) {
	t.Helper()
	return captureAnswer(t, func() int { return compareCmd(args) })
}

// writeCompareJournal writes one complete attributed run with chosen
// provenance, cores and (optionally) per-core engine accounting.
// memBytes maps core -> verified-unique volume; a core absent from
// the map records no accounting (unknown, never zero).
func writeCompareJournal(t *testing.T, path, runID string, meta journal.RunMetaFields, cores []int, memBytes map[int]uint64, appendMode bool) {
	t.Helper()
	var f *os.File
	var err error
	if appendMode {
		f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	} else {
		f, err = os.Create(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	jw := journal.NewWriter(f, runID)
	jw.RunStart("general", "", cores, 1, "30s", false)
	jw.RunMeta(meta)
	for _, c := range cores {
		jw.CoreStart(c, "int")
		if v, ok := memBytes[c]; ok {
			acct := journal.MemAccounting{VerifiedUnique: &v}
			jw.MemDone(c, acct)
		}
		jw.CoreDone(c, journal.OK, "")
	}
	jw.RunEnd(0)
	jw.Close()
}

func saveCompareManifest(t *testing.T, m *sessionManifest) {
	t.Helper()
	if err := saveSessionManifest(m); err != nil {
		t.Fatal(err)
	}
}

// Sol's discriminating check (R35 review 2026-11-17): equal KNOWN
// value-sets across PARTIALLY-unknown provenance warn nothing - the
// silence is by design. So the card itself must carry the
// completeness view: side A recorded the config hash on ONE of its
// two attributed runs; side B on both. The shared warnings stay
// silent (the known sets match), while the card says "1 of 2" vs
// "2 of 2" and states out loud that silence proves nothing.
func TestCompareProvenanceCompletenessVisible(t *testing.T) {
	chdirTemp(t)
	full := func(sess string) journal.RunMetaFields {
		return journal.RunMetaFields{SessionID: sess, ConfigHash: "cfg-1",
			PlanHash: "p-1", TopologyFP: "t-1", BuildRev: "r-1"}
	}
	ja := "a.log.journal.jsonl"
	writeCompareJournal(t, ja, "run-a1", full("S-A"), []int{6}, nil, false)
	partial := full("S-A")
	partial.ConfigHash = "" // this run never recorded the config axis: unknown
	writeCompareJournal(t, ja, "run-a2", partial, []int{6}, nil, true)
	jb := "b.log.journal.jsonl"
	writeCompareJournal(t, jb, "run-b1", full("S-B"), []int{6}, nil, false)
	writeCompareJournal(t, jb, "run-b2", full("S-B"), []int{6}, nil, true)

	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-A", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed", Validation: "verified",
		Runs: []manifestRun{{Log: "a.log", Journal: ja}}})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-B", Kind: "run",
		Started: "2026-10-16T10:00:00Z", State: "completed", Validation: "verified",
		Runs: []manifestRun{{Log: "b.log", Journal: jb}}})

	code, text := captureCompare(t, []string{"-a", "S-A", "-b", "S-B"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (read-only screen):\n%s", code, text)
	}
	// The completeness view: same KNOWN hash on both sides, but a
	// different fraction of the runs actually recorded it.
	if !strings.Contains(text, "config hash 1 of 2") || !strings.Contains(text, "config hash 2 of 2") {
		t.Errorf("per-axis completeness missing (the silence below must not hide this):\n%s", text)
	}
	// And the shared model IS silent about the config axis here - by
	// design the known-set comparison sees equal sets. What must NOT
	// happen is silence about everything: the pinned WHEA line (every
	// plain journal pair carries it) proves compare renders the same
	// comparability model as the journal diff, not a softer copy.
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "warning:") && strings.Contains(line, "config") {
			t.Errorf("equal known sets across partially-unknown runs warned anyway:\n%s", line)
		}
	}
	if !strings.Contains(text, "WHEA observation not available on both sides") {
		t.Errorf("compare drifted from the shared diff model (the WHEA notice must appear here exactly as reportDiff prints it):\n%s", text)
	}
}

// Audit A11 on the card: exact core SETS. Two sides covering the
// same NUMBER of cores but different cores tested different muscles,
// and the shared diff engine says so verbatim.
func TestCompareCoreSetsDifferWarn(t *testing.T) {
	chdirTemp(t)
	meta := func(sess string) journal.RunMetaFields {
		return journal.RunMetaFields{SessionID: sess, PlanHash: "p-1", TopologyFP: "t-1", BuildRev: "r-1"}
	}
	ja := "a.log.journal.jsonl"
	writeCompareJournal(t, ja, "run-a", meta("S-A"), []int{0, 1}, nil, false)
	jb := "b.log.journal.jsonl"
	writeCompareJournal(t, jb, "run-b", meta("S-B"), []int{2, 3}, nil, false)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-A", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Journal: ja}}})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-B", Kind: "run",
		Started: "2026-10-16T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Journal: jb}}})

	code, text := captureCompare(t, []string{"-a", "S-A", "-b", "S-B"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	for _, want := range []string{
		"core coverage differs",
		"equal counts of different sets are not the same coverage",
		"cores actually loaded: 0,1", // human.Ints renders the exact set, per side
		"cores actually loaded: 2,3",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
}

// A journal the manifest lists but the disk no longer has, plus a
// child whose files the manifest never learned: that side keeps its
// holes LOUD and its coverage unknown - never substituted, never
// zeroed. The other side keeps its real numbers.
func TestCompareMissingJournalKeepsHolesLoud(t *testing.T) {
	chdirTemp(t)
	jb := "b.log.journal.jsonl"
	writeCompareJournal(t, jb, "run-b", journal.RunMetaFields{SessionID: "S-B"}, []int{6}, nil, false)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-A", Kind: "session",
		Started: "2026-10-17T10:00:00Z", State: "completed",
		Runs: []manifestRun{
			{Name: "gone", Journal: "gone.log.journal.jsonl"}, // listed but missing
			{Name: "unnamed"}, // the manifest never learned its files
		}})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-B", Kind: "run",
		Started: "2026-10-16T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Journal: jb}}})

	code, text := captureCompare(t, []string{"-a", "S-A", "-b", "S-B"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	for _, want := range []string{
		"1 listed but missing, 1 unnamed",
		"that side's coverage is INCOMPLETE (unknown, not zero)",
		"attributed runs: 0",
		"cores actually loaded: unknown (no attributed run reported them)",
		"no memory-engine accounting recorded (unknown - absent means not observed, never zero)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "cores actually loaded: 6") {
		t.Errorf("the healthy side lost its real coverage:\n%s", text)
	}
}

// The JSON answer keeps the honesty shape: memory compares the
// LARGEST single run (A11) - never the side's sum - accounted runs
// are split out, an absent memory block stays absent (unknown),
// provenance rides as known-of-total per axis, and notices is an
// empty array that still proves nothing.
func TestCompareJSONKeepsA11AndUnknowns(t *testing.T) {
	chdirTemp(t)
	meta := func(sess string) journal.RunMetaFields {
		return journal.RunMetaFields{SessionID: sess, ConfigHash: "cfg-1", PlanHash: "p-1", TopologyFP: "t-1", BuildRev: "r-1"}
	}
	ja := "a.log.journal.jsonl"
	// Two accounted runs of 1 MiB and 2 MiB: A11 says the DECIDER is
	// 2 MiB (largest single run), never the 3 MiB aggregate.
	writeCompareJournal(t, ja, "run-a1", meta("S-A"), []int{6}, map[int]uint64{6: 1 << 20}, false)
	partial := meta("S-A")
	partial.ConfigHash = ""
	writeCompareJournal(t, ja, "run-a2", partial, []int{6}, map[int]uint64{6: 2 << 20}, true)
	jb := "b.log.journal.jsonl"
	writeCompareJournal(t, jb, "run-b", meta("S-B"), []int{6}, nil, false)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-A", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Journal: ja}}})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-B", Kind: "run",
		Started: "2026-10-16T10:00:00Z", State: "completed",
		Runs: []manifestRun{{Journal: jb}}})

	code, text := captureCompare(t, []string{"-a", "S-A", "-b", "S-B", "-json"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, text)
	}
	var got struct {
		Schema      int           `json:"schema"`
		Notices     []string      `json:"notices"`
		ProvenanceA []compareAxis `json:"provenance_a"`
		A           struct {
			SessionID      string `json:"session_id"`
			AttributedRuns int    `json:"attributed_runs"`
			CoreCoverage   []int  `json:"core_coverage"`
			Memory         *struct {
				RunsAccounted int    `json:"runs_accounted"`
				RunsTotal     int    `json:"runs_total"`
				LargestRun    uint64 `json:"largest_run_verified_bytes"`
			} `json:"memory"`
		} `json:"a"`
		B struct {
			SessionID string      `json:"session_id"`
			Memory    interface{} `json:"memory"`
		} `json:"b"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, text)
	}
	if got.Schema != 1 || got.A.SessionID != "S-A" || got.A.AttributedRuns != 2 {
		t.Errorf("json side wrong: %s", text)
	}
	if got.Notices == nil {
		t.Errorf("notices must be an EMPTY ARRAY, not null: %s", text)
	}
	if len(got.A.CoreCoverage) != 1 || got.A.CoreCoverage[0] != 6 {
		t.Errorf("core coverage wrong: %+v", got.A.CoreCoverage)
	}
	if got.A.Memory == nil {
		t.Fatal("A lost its engine accounting fold")
	}
	if got.A.Memory.LargestRun != 2<<20 || got.A.Memory.RunsAccounted != 2 || got.A.Memory.RunsTotal != 2 {
		t.Errorf("A11 largest-run rule broken (want 2 MiB decider, got %+v)", got.A.Memory)
	}
	if got.B.Memory != nil {
		t.Errorf("B recorded no accounting: memory must be ABSENT (unknown), got %v", got.B.Memory)
	}
	var cfgAxis *compareAxis
	for i, ax := range got.ProvenanceA {
		if ax.Axis == "config hash" {
			cfgAxis = &got.ProvenanceA[i]
		}
	}
	if cfgAxis == nil || cfgAxis.Known != 1 || cfgAxis.Total != 2 {
		t.Errorf("json must carry the KNOWN-of-total view the silence cannot: %+v", got.ProvenanceA)
	}
}

// Setup refusals: compare never invents a side. Every wrong shape
// exits 2 with the reason, before any comparison is printed.
func TestCompareRefusals(t *testing.T) {
	chdirTemp(t)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: "S-A", Kind: "run",
		Started: "2026-10-17T10:00:00Z", State: "completed"})
	cases := []struct {
		name string
		args []string
	}{
		{"no -a", []string{"-b", "S-B"}},
		{"no -b", []string{"-a", "S-A"}},
		{"same session both sides", []string{"-a", "S-A", "-b", "S-A"}},
		{"positional refused", []string{"S-A", "S-B"}},
		{"unknown -a", []string{"-a", "nope", "-b", "S-A"}},
		{"unknown -b", []string{"-a", "S-A", "-b", "nope"}},
	}
	for _, tc := range cases {
		code, text := captureCompare(t, tc.args)
		if code != 2 {
			t.Errorf("%s: exit = %d, want 2:\n%s", tc.name, code, text)
		}
	}
}

// The menu route: the session menu's compare picks the partner from
// the newest-first history list (number or exact id, fragments
// refused), renders the SAME read-only card, and changes nothing on
// disk.
func TestMenuSessionCompareScripted(t *testing.T) {
	chdirTemp(t)
	const newID, oldID = "20261017-100000-aaaaaaaaaaaaaaaa", "20261016-100000-bbbbbbbbbbbbbbbb"
	jp := "s1.log.journal.jsonl"
	writeCompareJournal(t, jp, "run-s1", journal.RunMetaFields{SessionID: newID, PlanHash: "h-1"}, []int{6}, nil, false)
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: newID,
		Kind: "run", Started: "2026-10-17T10:00:00Z", State: "completed", Validation: "verified",
		Runs: []manifestRun{{Log: "s1.log", Journal: jp}}})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: oldID,
		Kind: "session", Started: "2026-10-16T10:00:00Z", State: "completed", Validation: "verified"})

	out := captureStdoutAndRun(t, func() {
		// 5 = history, 1 = newest session, 4 = compare, 1 = partner
		// number, \n dismisses the card, q/q back out, 0 quits the menu.
		if code := menuLoop(bufio.NewReader(strings.NewReader("6\n1\n4\n1\n\nq\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	for _, want := range []string{
		fmt.Sprintf("Compare %s with which session", newID),
		fmt.Sprintf("compare sessions %s vs %s (read-only", newID, oldID),
		"attributed runs: 1",
		"attributed runs: 0",
		"cores actually loaded: unknown (no attributed run reported them)",
		"no notice below PROVES",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("menu compare lost %q:\n%s", want, out)
		}
	}
	// Read-only like every history screen: the compare wrote no new
	// manifest and rewrote no existing one.
	ents, err := os.ReadDir("history")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Errorf("compare wrote history files: %d entries", len(ents))
	}
	m, err := loadSessionManifest(oldID)
	if err != nil || m.State != "completed" || m.Validation != "verified" || m.Started != "2026-10-16T10:00:00Z" {
		t.Errorf("compare rewrote the partner manifest: %+v err %v", m, err)
	}
}

// A fragment id is refused by name: two sessions sharing a prefix
// must never be silently merged into one guessed partner.
func TestMenuSessionCompareRejectsFragment(t *testing.T) {
	chdirTemp(t)
	const newID, oldID = "20261017-100000-aaaaaaaaaaaaaaaa", "20261016-100000-bbbbbbbbbbbbbbbb"
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: newID,
		Kind: "run", Started: "2026-10-17T10:00:00Z", State: "completed", Validation: "verified"})
	saveCompareManifest(t, &sessionManifest{Version: manifestVersion, SessionID: oldID,
		Kind: "run", Started: "2026-10-16T10:00:00Z", State: "completed", Validation: "verified"})

	out := captureStdoutAndRun(t, func() {
		if code := menuLoop(bufio.NewReader(strings.NewReader("6\n1\n4\n2026\nq\nq\n0\n")), config.Default()); code != 0 {
			t.Errorf("menu exit = %d, want 0", code)
		}
	})
	if !strings.Contains(out, "No such session") {
		t.Errorf("fragment id not refused:\n%s", out)
	}
	if strings.Contains(out, "compare sessions") {
		t.Errorf("a fragment opened a guessed comparison:\n%s", out)
	}
}
