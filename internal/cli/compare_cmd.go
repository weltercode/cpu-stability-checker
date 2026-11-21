package cli

// compare (owner queue item 3, 2026-11-17): the session-vs-session
// history view. The question it answers is not "which one PASSed" but
// "what did these two recorded sessions ACTUALLY test, and are they
// even the same experiment" - coverage, configuration and provenance,
// side by side. Everything it renders is DERIVED AT READ TIME from
// the manifests and the journals they list (history stays immutable,
// and old lean manifests work unchanged), and it reuses the pinned
// journal-diff engine (attribution by run_meta, the A11 largest-run
// coverage rule, exact core SETS, the comparability axes) instead of
// growing a second arithmetic that could drift from the first.
//
// Honesty rules this command exists to enforce:
//   - comparability is never PROVEN, only warned about: the card
//     states each provenance axis as known-runs-of-total-runs per
//     side, because equal sets of KNOWN hashes across partially
//     unknown runs prove nothing (a run whose hash was never
//     recorded is unknown, not equal, and an axis unknown on either
//     side silences its warning by design);
//   - memory coverage compares the largest single run (audit A11),
//     never the pair's aggregates, and nothing is summed ACROSS the
//     two sessions;
//   - a listed journal that went missing says so and keeps that
//     side's coverage INCOMPLETE - never substituted, never zeroed;
//   - read-only like every history screen: nothing is written,
//     nothing deleted.

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func compareCmd(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	a := fs.String("a", "", "first session id (the ids are listed by 'history')")
	b := fs.String("b", "", "second session id")
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "compare") {
		return 2
	}
	if *a == "" || *b == "" {
		fmt.Fprintln(os.Stderr, "compare needs -a <session-id> and -b <session-id> (the ids are listed by 'history')")
		return 2
	}
	if *a == *b {
		fmt.Fprintln(os.Stderr, "compare -a and -b name the same session: one side answers nothing")
		return 2
	}
	sideA, err := loadCompareSide(*a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	sideB, err := loadCompareSide(*b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

	notices := compareNotices(sideA.runs, sideB.runs)
	if notices == nil {
		notices = []string{} // present-empty, not null: the LIST is empty, and an empty list still proves nothing
	}
	if *asJSON {
		ans := struct {
			Schema        int           `json:"schema"`
			A             *compareSide  `json:"a"`
			B             *compareSide  `json:"b"`
			Notices       []string      `json:"notices"` // the shared comparability model; an empty list proves NOTHING
			NoticesRule   string        `json:"notices_rule"`
			ProvenanceA   []compareAxis `json:"provenance_a"` // known runs of total runs, per axis
			ProvenanceB   []compareAxis `json:"provenance_b"`
			MemoryRule    string        `json:"memory_rule"`
			FullDiffRoute string        `json:"full_diff_route"`
		}{
			Schema: 1, A: sideA, B: sideB, Notices: notices,
			NoticesRule:   "no notice below PROVES comparability, and the absence of a notice is not proof the experiments matched",
			ProvenanceA:   sideA.provenance(),
			ProvenanceB:   sideB.provenance(),
			MemoryRule:    "memory compares each side's largest single run (audit A11); aggregates are never compared and nothing is summed across the pair",
			FullDiffRoute: "cpu-stability report <journalA> -diff <journalB> for the full per-core journal comparison (runs of OTHER sessions included, unlike this card; the journals are listed in files[])",
		}
		if err := writeJSONAnswer(ans); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode compare: %v\n", err)
			return 2
		}
		return 0
	}
	printCompareText(os.Stdout, sideA, sideB, notices)
	return 0
}

// compareSide is one session as the compare card sees it: manifest
// identity words plus everything derived from its attributed runs.
// Absent pointer/empty-slice fields mean UNKNOWN (never zero); a
// listed file that went missing keeps that side INCOMPLETE.
type compareSide struct {
	SessionID      string            `json:"session_id"`
	Kind           string            `json:"kind"`
	State          string            `json:"state"` // manifest vocabulary; the text card renders the honest words
	Started        string            `json:"started"`
	Finished       string            `json:"finished,omitempty"` // absent = never finalized (crash, or still running)
	Exit           *int              `json:"exit,omitempty"`     // null = the session never ended observably
	Validation     string            `json:"validation,omitempty"`
	Files          []sessionFileView `json:"files"`
	MissingFiles   int               `json:"missing_files"`              // listed but gone: coverage incomplete, never zero
	UnnamedFiles   int               `json:"unnamed_files"`              // the manifest never learned these children's files
	AttributedRuns int               `json:"attributed_runs"`            // run_meta.session_id matched, per the shared loader
	ForeignRuns    int               `json:"foreign_runs_in_same_files"` // counted and excluded, never merged
	CoreCoverage   []int             `json:"core_coverage,omitempty"`    // exact set; absent = no attributed run said (unknown, not empty)
	Memory         *compareMem       `json:"memory,omitempty"`           // absent = no attributed run reported engine accounting (unknown, never zero)

	runs []journal.RunReport // attributed runs, kept for the shared notice/provenance model (never serialized)
}

// compareMem keeps the A11 comparison shape: the largest single run's
// verified volume decides, and the accounted/total split states how
// much of the side is even known. Deliberately no aggregate: a
// session-wide sum would be exactly the number audit A11 warns
// readers away from on a comparison screen.
type compareMem struct {
	RunsAccounted int    `json:"runs_accounted"`
	RunsTotal     int    `json:"runs_total"`
	LargestRun    uint64 `json:"largest_run_verified_bytes"` // cumulative byte-work within that one run
}

// compareAxis states one provenance axis by COMPLETENESS, not by
// value: how many of the side's attributed runs actually recorded
// it. Equal value-sets across partially unknown runs prove nothing
// (Sol review 2026-11-17), so the card shows the denominator.
type compareAxis struct {
	Axis  string `json:"axis"`
	Known int    `json:"known_runs"`
	Total int    `json:"total_runs"`
}

// loadCompareSide loads one session's manifest and its attributed
// runs through the same loader the session report uses. A missing or
// unreadable manifest is a setup-style refusal: compare never invents
// a side.
func loadCompareSide(sessionID string) (*compareSide, error) {
	m, err := loadSessionManifest(sessionID)
	if err != nil {
		return nil, fmt.Errorf("cannot read session %q: %v", sessionID, err)
	}
	files, runs := loadSessionRuns(m)
	side := &compareSide{
		SessionID: m.SessionID, Kind: m.Kind, State: m.State,
		Started: m.Started, Finished: m.Finished, Exit: m.Exit, Validation: m.Validation,
		Files: files, AttributedRuns: len(runs), runs: runs,
	}
	for _, fv := range files {
		switch fv.Status {
		case "missing":
			side.MissingFiles++
		case "unnamed":
			side.UnnamedFiles++
		}
		side.ForeignRuns += fv.NotAttributed
	}
	if len(runs) > 0 {
		if set := coreSet(runs); len(set) > 0 {
			side.CoreCoverage = sortedInts(set)
		}
		if vols := verifiedPerRun(runs); len(vols) > 0 {
			side.Memory = &compareMem{
				RunsTotal:     len(runs),
				RunsAccounted: len(vols), // runs without accounting are absent from the fold: unknown, not zero
				LargestRun:    maxUint(vols),
			}
		}
	}
	return side, nil
}

// provenance states each comparability axis as KNOWN runs out of the
// side's attributed runs - the completeness view the known-set
// warnings cannot express. An axis at 0 of N is unknown, not
// absent-equal.
func (s *compareSide) provenance() []compareAxis {
	picks := []struct {
		name string
		pick func(*journal.RunMetaFields) string
	}{
		{"config hash", func(m *journal.RunMetaFields) string { return m.ConfigHash }},
		{"plan sha256", func(m *journal.RunMetaFields) string { return m.PlanHash }},
		{"CPU topology", func(m *journal.RunMetaFields) string { return m.TopologyFP }},
		{"tool build", func(m *journal.RunMetaFields) string { return m.BuildRev }},
	}
	out := make([]compareAxis, 0, len(picks))
	for _, p := range picks {
		known := 0
		for _, rep := range s.runs {
			if rep.Meta != nil && p.pick(rep.Meta) != "" {
				known++
			}
		}
		out = append(out, compareAxis{Axis: p.name, Known: known, Total: len(s.runs)})
	}
	return out
}

// compareNotices is exactly the shared model the journal diff prints:
// the diffSummaries notices (phase sets, crash counts), the WHEA
// comparability, and comparabilityWarnings verbatim - so the compare
// screen cannot drift from the pinned report wording, and both routes
// answer with one arithmetic.
func compareNotices(a, b []journal.RunReport) []string {
	_, warns := diffSummaries(journal.SummarizeCores(a), journal.SummarizeCores(b), phaseSet(a), phaseSet(b))
	warns = append(warns, wheaNotices(a, b)...)
	warns = append(warns, comparabilityWarnings(a, b)...)
	return warns
}

// printCompareText renders the side-by-side card. The wording never
// upgrades and never judges: no "better", no "worse", no comparability
// claim - facts, known/total completeness, and the shared warnings.
func printCompareText(w io.Writer, a, b *compareSide, notices []string) {
	fmt.Fprintf(w, "compare sessions %s vs %s (read-only; no notice below PROVES the two are comparable, and their absence proves nothing either):\n\n", a.SessionID, b.SessionID)
	printCompareSide(w, "A", a)
	printCompareSide(w, "B", b)

	fmt.Fprintln(w, "\nmemory coverage compares the LARGEST single run per side (audit A11: equal aggregates can hide different run counts or budgets); nothing here is summed across the pair:")
	fmt.Fprintf(w, "  A: %s\n", memCompareLine(a))
	fmt.Fprintf(w, "  B: %s\n", memCompareLine(b))

	fmt.Fprintln(w, "\nprovenance recorded on each side's attributed runs (known of total; a run that never recorded an axis is unknown, never assumed equal to the known ones):")
	fmt.Fprintf(w, "  A: %s\n", provenanceLine(a.provenance()))
	fmt.Fprintf(w, "  B: %s\n", provenanceLine(b.provenance()))

	fmt.Fprintln(w, "\nnotices (the same comparability model the journal diff prints; they warn, they never prove):")
	if len(notices) == 0 {
		fmt.Fprintln(w, "  none - which is NOT a comparability proof (an axis unknown on either side silences its warning by design)")
	}
	for _, n := range notices {
		fmt.Fprintln(w, "  warning: "+n)
	}
	fmt.Fprintln(w, "\nfull per-core comparison of whole journals (runs of other sessions INCLUDED, unlike this card): cpu-stability report <journalA> -diff <journalB>")
}

func printCompareSide(w io.Writer, tag string, s *compareSide) {
	state, _ := sessionStateWords(sessionManifest{State: s.State, Validation: s.Validation})
	when := localClockText(s.Started)
	if when == "" {
		when = s.Started // unparseable stamp: show it as stored, invent no clock
	}
	exit := "unknown"
	if s.Exit != nil {
		exit = fmt.Sprintf("%d", *s.Exit)
	}
	verif := s.Validation
	if verif == "" {
		verif = "unknown"
	}
	fmt.Fprintf(w, "%s: %s · %s · %s · started %s · %s · exit %s · verification %s\n",
		tag, s.SessionID, kindWord(s.Kind), state, when, finishWord(s), exit, verif)
	fmt.Fprintf(w, "   attributed runs: %d", s.AttributedRuns)
	if s.ForeignRuns > 0 {
		fmt.Fprintf(w, " (%d more in the same files belong to other sessions or predate attribution - counted, excluded)", s.ForeignRuns)
	}
	fmt.Fprintln(w)
	if s.MissingFiles > 0 || s.UnnamedFiles > 0 {
		fmt.Fprintf(w, "   files: %d listed but missing, %d unnamed - that side's coverage is INCOMPLETE (unknown, not zero)\n", s.MissingFiles, s.UnnamedFiles)
	}
	if s.CoreCoverage == nil {
		fmt.Fprintln(w, "   cores actually loaded: unknown (no attributed run reported them)")
	} else {
		fmt.Fprintf(w, "   cores actually loaded: %s\n", human.Ints(s.CoreCoverage))
	}
}

// finishWord mirrors the history screens' honesty: a side that never
// finalized says so, and no finish time is invented.
func finishWord(s *compareSide) string {
	if s.Finished == "" {
		return "not finished (crash or still running?)"
	}
	if lt := localClockText(s.Finished); lt != "" {
		return "ended " + lt
	}
	return "ended " + s.Finished
}

func memCompareLine(s *compareSide) string {
	if s.Memory == nil {
		return "no memory-engine accounting recorded (unknown - absent means not observed, never zero)"
	}
	line := fmt.Sprintf("largest single run verified=%s; accounted runs %d of %d",
		human.Bytes(s.Memory.LargestRun), s.Memory.RunsAccounted, s.Memory.RunsTotal)
	if s.Memory.RunsAccounted < s.Memory.RunsTotal {
		line += fmt.Sprintf(" (%d run(s) without accounting: unknown, not zero)", s.Memory.RunsTotal-s.Memory.RunsAccounted)
	}
	return line
}

func provenanceLine(axes []compareAxis) string {
	parts := make([]string, 0, len(axes))
	for _, ax := range axes {
		parts = append(parts, fmt.Sprintf("%s %d of %d", ax.Axis, ax.Known, ax.Total))
	}
	return strings.Join(parts, ", ")
}
