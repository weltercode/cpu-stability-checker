package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// reportCmd turns the crash journal into the question a long run actually
// asks: which logical CPUs have been loaded, and which of them failed.
// The 'journal' command shows every run's story; this one aggregates all
// runs into one row per core — the final per-core verdict. With -json the
// same aggregation comes out machine-readable for external tooling.
func reportCmd(args []string) int {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the aggregation as JSON instead of text")
	diff := fs.String("diff", "", "compare per-core outcomes with a second journal (runs/faults/exposure)")
	anomalies := fs.Bool("anomalies", false, "diagnostic: flag clock dips against each phase's own median")
	session := fs.String("session", "", "report one session by id: the manifest lists its journals and run_meta.session_id attributes the runs (U02 history)")
	fs.Parse(args)
	if *session != "" {
		if fs.NArg() >= 1 {
			fmt.Fprintln(os.Stderr, "report -session takes no journal path: the session's manifest lists its own files")
			return 2
		}
		if *diff != "" {
			fmt.Fprintln(os.Stderr, "report -session cannot be combined with -diff yet")
			return 2
		}
		return reportSession(*session, *asJSON, *anomalies)
	}
	path := ""
	if fs.NArg() >= 1 {
		path = fs.Arg(0)
	}
	if path == "" {
		path = journal.Path(config.Load(config.DefaultPath).LogFile)
	}
	recs, issA := journal.ReadAll(path)
	if len(recs) == 0 && issA.ReadErr != "" {
		fmt.Fprintf(os.Stderr, "cannot open journal %q: %v\n", path, issA.ReadErr)
		return 2
	}
	reports := journal.RecoverRuns(recs)
	if *diff != "" {
		return reportDiff(path, issA, reports, *diff)
	}
	dips := map[int][]journal.ClockDip{}
	if *anomalies {
		dips = journal.DetectClockAnomalies(reports)
	}
	if *asJSON {
		js := summaryToJSON(path, journal.SummarizeCores(reports), dips, issA, wheaSummary(reports))
		js.Provenance = provenanceList(reports)
		js.RunDetails = runDetails(reports)
		out, err := json.MarshalIndent(js, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode report: %v\n", err)
			return 2
		}
		fmt.Println(string(out))
		return 0
	}
	if len(reports) == 0 && issA.Empty() {
		fmt.Printf("journal %s: no valid records\n", path)
		return 0
	}
	writeReportText(os.Stdout, path, reports, issA, dips)
	return 0
}

// writeReportText renders the full text report to any writer: the
// terminal path and the bundle's in-memory capture share this single
// renderer, so the bundle can never disagree with what `report`
// printed on the operator's screen (R09).
func writeReportText(w io.Writer, path string, reports []journal.RunReport, iss *journal.Issues, dips map[int][]journal.ClockDip) {
	fmt.Fprintf(w, "core report for %s:\n", path)
	printJournalIssues(w, iss)
	printProvenance(w, reports)
	journal.PrintSummary(w, journal.SummarizeCores(reports))
	printWheaSummary(w, reports)
	printAnomalies(w, dips)
}

// wheaSummaryJSON aggregates the hardware-error evidence across all
// runs of a journal. The observation field is the honest lens: the
// counts only mean something when observation was available.
// "unknown" means the journal carries no state records (an old
// journal, or runs that never watched) — never "no errors".
type wheaSummaryJSON struct {
	Corrected   int    `json:"corrected"`
	Uncorrected int    `json:"uncorrected"`
	Unknown     int    `json:"unknown"` // severity or nature unrecognized: adjudicate
	PCIe        int    `json:"pcie_observed,omitempty"`
	Observation string `json:"observation"` // available | degraded | unavailable | unknown
	Reason      string `json:"reason,omitempty"`
}

// wheaStateRank orders observation states worst-last: never having
// looked is worse than a degraded look.
func wheaStateRank(s string) int {
	switch s {
	case "available":
		return 0
	case "degraded":
		return 1
	case "unavailable":
		return 2
	default: // "" (never watched) or "initializing" (armed, no completed cycle yet): unproven
		return 3
	}
}

// wheaSummary folds every run's WHEA evidence into one view, taking
// the worst observation state seen anywhere: one degraded window
// makes the aggregate counts suspect.
func wheaSummary(reports []journal.RunReport) *wheaSummaryJSON {
	if len(reports) == 0 {
		return nil
	}
	w := wheaSummaryJSON{Observation: "unknown"}
	worst := -1
	for _, rep := range reports {
		for _, ev := range rep.Whea {
			switch ev.Severity {
			case "corrected":
				w.Corrected++
				if ev.Nature == "pcie" {
					w.PCIe++
				}
			case "uncorrected":
				w.Uncorrected++
			default:
				w.Unknown++
			}
		}
		if r := wheaStateRank(rep.WheaState); r > worst {
			worst = r
			if rep.WheaState == "" {
				w.Observation = "unknown"
				w.Reason = "journal carries no whea_state records"
			} else {
				w.Observation = rep.WheaState
				w.Reason = rep.WheaStateReason
			}
		}
	}
	return &w
}

// printWheaSummary renders the aggregate WHEA view for the text
// report, with the observation state right beside the counts so a
// zero can never be read as "clean" without a watching window.
func printWheaSummary(w interface{ Write([]byte) (int, error) }, reports []journal.RunReport) {
	s := wheaSummary(reports)
	if s == nil {
		return
	}
	if s.Corrected == 0 && s.Uncorrected == 0 && s.Unknown == 0 {
		fmt.Fprintf(w, "WHEA: no records; observation: %s\n", s.Observation)
	} else {
		fmt.Fprintf(w, "WHEA: %d corrected, %d uncorrected, %d unknown; observation: %s\n",
			s.Corrected, s.Uncorrected, s.Unknown, s.Observation)
	}
	if s.Reason != "" {
		fmt.Fprintf(w, "  observation reason: %s\n", s.Reason)
	}
	if s.Uncorrected > 0 {
		fmt.Fprintln(w, "  UNCORRECTED hardware error evidence in this journal — adjudicate before trusting any run")
	}
}

// printProvenance renders the run_meta provenance for the text report:
// which build wrote this journal, which config and seed were in force,
// and on what machine. Runs that share one provenance fold into one
// line; a journal without run_meta records says "unknown" — an older
// journal never gets a fabricated identity.
func printProvenance(w interface{ Write([]byte) (int, error) }, reports []journal.RunReport) {
	if len(reports) == 0 {
		return
	}
	seen := map[string]int{}
	order := []string{}
	for _, rep := range reports {
		line := journal.FormatProvenance(rep.Meta)
		if _, ok := seen[line]; !ok {
			order = append(order, line)
		}
		seen[line]++
	}
	for _, line := range order {
		if n := seen[line]; n > 1 {
			fmt.Fprintf(w, "%s · %d run(s)\n", line, n)
		} else {
			fmt.Fprintln(w, line)
		}
	}
}

// printAnomalies renders the diagnostic dip summary. It never changes
// the exit code: dips are leads to inspect, not faults.
func printAnomalies(w io.Writer, dips map[int][]journal.ClockDip) {
	if len(dips) == 0 {
		return
	}
	cores := make([]int, 0, len(dips))
	for c := range dips {
		cores = append(cores, c)
	}
	sort.Ints(cores)
	fmt.Fprintln(w, "clock dips (diagnostic only, not faults):")
	for _, c := range cores {
		worst := dips[c][0]
		fmt.Fprintf(w, "  core %d: %d window(s) below 60%% of their phase median, worst %.0f MHz vs median %.0f MHz\n",
			c, len(dips[c]), worst.ClockMHz, worst.MedianMHz)
	}
}

// The JSON shape mirrors the text report: one object per logical CPU with
// nested stat blocks. Blocks are absent (not zero-filled) when the journal
// holds no samples for them — absence means "never observed", a zero would
// lie. The shape is meant to stay stable so nightly runs can be diffed.
// reportSchemaVersion versions the JSON report shape itself (the
// journal is versioned per record). Bump on any breaking field change
// so external tooling can tell old envelopes from new honestly.
const reportSchemaVersion = 1

type reportJSON struct {
	Schema        int              `json:"schema"` // this report envelope's shape version
	Journal       string           `json:"journal"`
	Runs          int              `json:"runs"`
	Unended       int              `json:"unended_runs"`
	JournalIssues *journal.Issues  `json:"journal_issues,omitempty"` // holes in the story (absent = intact)
	Whea          *wheaSummaryJSON `json:"whea,omitempty"`           // hardware-error evidence + how much it means (absent = no runs)
	// Provenance lists the distinct run_meta records across the
	// journal's runs (folded with run counts). Absent = no run carried
	// provenance (an older journal): identity unknown, never guessed.
	Provenance []provenanceJSON `json:"provenance,omitempty"`
	// RunDetails is the per-run coverage view: which algorithm
	// (kernel) version produced each core, how much memory was
	// actually verified, transitions, and evidence. It is per-run on
	// purpose — summing verified bytes across runs would double-count
	// the same region re-checked in another run. Absent = not rendered.
	RunDetails []runDetailJSON  `json:"run_details,omitempty"`
	Cores      []reportCoreJSON `json:"cores"`
}

// provenanceJSON is one distinct provenance identity and how many runs
// share it. Meta is null when the runs predate run_meta (unknown).
type provenanceJSON struct {
	Runs int                    `json:"runs"`
	Meta *journal.RunMetaFields `json:"meta"`
}

// runDetailJSON is one run's coverage story.
type runDetailJSON struct {
	RunID string `json:"run_id"`
	Test  string `json:"test"`
	Phase string `json:"phase,omitempty"`
	Ended bool   `json:"ended"`
	Exit  int    `json:"exit"`
	// Validation is the run's structured verification claim from the
	// journal's run_end record: "unknown" when the journal predates
	// the field (absence is never rendered as a pass or a failure).
	Validation string                 `json:"validation"`
	Meta       *journal.RunMetaFields `json:"provenance,omitempty"` // null/absent = unknown
	Cores      []runCoreCoverageJSON  `json:"cores"`
}

// runCoreCoverageJSON carries what one core's run actually verified.
// Every optional block is absent when the journal does not say: mem
// absent = the engine did not run here (unknown, not zero); ram absent
// = no ram kernel ran (unknown, NOT "clean"); bursts absent = no burst
// records (unknown, not "zero transitions").
type runCoreCoverageJSON struct {
	Core          int                    `json:"core"`
	Status        string                 `json:"status"`
	KernelVersion string                 `json:"kernel_version,omitempty"` // "" = unknown (older journal / legacy)
	Checks        *uint64                `json:"checks,omitempty"`         // confirmed checks; nil = unknown
	Evidence      string                 `json:"evidence,omitempty"`       // external protocol evidence ("" = none recorded)
	Mem           *journal.MemAccounting `json:"mem,omitempty"`
	Ram           *journal.RamEvidence   `json:"ram,omitempty"`
	Bursts        *burstsJSON            `json:"bursts,omitempty"`
	Leases        *leasesJSON            `json:"leases,omitempty"` // P07e handoff leases; absent = none recorded (unknown for pre-P07e journals, never "zero")
}

// burstsJSON separates the parent's commanded count from the worker's
// verified count: the two views are independent records and are never
// merged into one number.
type burstsJSON struct {
	Commanded int `json:"commanded"` // session_burst records (parent view)
	Recorded  int `json:"recorded"`  // burst_done records (worker view)
	Verified  int `json:"verified"`  // burst_done records that closed complete
}

// leasesJSON summarizes one slot's P07e block-ownership leases: every
// lease the parent commanded on this core, how many got their
// expected ack, and how many failed (kernel mismatch or channel
// death - the record's error text says which).
type leasesJSON struct {
	Commanded int `json:"commanded"` // session_lease records (parent view)
	Acked     int `json:"acked"`
	Failed    int `json:"failed"`
}

type reportCoreJSON struct {
	Core        int            `json:"core"`
	Runs        int            `json:"runs"`
	Faults      int            `json:"faults"`
	Crashed     int            `json:"crashed"`
	Interrupted int            `json:"interrupted,omitempty"`
	Skipped     int            `json:"skipped,omitempty"`
	SetupErrors int            `json:"setup_errors,omitempty"`
	ExposureMS  int64          `json:"exposure_ms,omitempty"` // measured load time; legacy completed runs use planned time
	LastFault   string         `json:"last_fault,omitempty"`
	Wake        *statsJSON     `json:"wake_ms,omitempty"`
	Clock       *statsJSON     `json:"clock_mhz,omitempty"` // observed cycle rate (TSC-based busyness proxy), not the boost clock
	CoreClock   *coreClockJSON `json:"core_clock_mhz,omitempty"`
	Idle        *idleJSON      `json:"idle_pct,omitempty"`
	Dips        *dipsJSON      `json:"clock_dips,omitempty"` // diagnostic only; never a verdict
}

type dipsJSON struct {
	N         int     `json:"n"`
	WorstMHz  float64 `json:"worst_mhz"`
	MedianMHz float64 `json:"median_mhz"`
}

type statsJSON struct {
	N   int     `json:"n"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

type idleJSON struct {
	N   int     `json:"n"`
	Avg float64 `json:"avg"` // weighted by each window's length
	Max float64 `json:"max"`
}

// coreClockJSON is the producer-reported real core clock (HWiNFO/WMI):
// a different quantity from the observed cycle rate in clock_mhz, and
// the sources say exactly who reported it.
type coreClockJSON struct {
	N       int      `json:"n"`
	Min     float64  `json:"min"`
	Avg     float64  `json:"avg"`
	Max     float64  `json:"max"`
	Sources []string `json:"sources"`
}

// summaryToJSON maps the text aggregation onto the JSON shape. Pure, so
// the mapping is testable without a journal file.
func summaryToJSON(path string, sum journal.Summary, dips map[int][]journal.ClockDip, iss *journal.Issues, whea *wheaSummaryJSON) reportJSON {
	out := reportJSON{Schema: reportSchemaVersion, Journal: path, Runs: sum.Runs, Unended: sum.Unended, Cores: make([]reportCoreJSON, 0, len(sum.Cores)), Whea: whea}
	if !iss.Empty() {
		out.JournalIssues = iss
	}
	for _, cs := range sum.Cores {
		rc := reportCoreJSON{
			Core:        cs.Core,
			Runs:        cs.Runs,
			Faults:      cs.Faults,
			Crashed:     cs.Crashed,
			Interrupted: cs.Interrupted,
			Skipped:     cs.Skipped,
			SetupErrors: cs.SetupErrors,
			ExposureMS:  cs.Exposure.Milliseconds(),
			LastFault:   cs.LastFault,
		}
		if cs.WakeN > 0 {
			rc.Wake = &statsJSON{N: cs.WakeN, Min: cs.WakeMinMS, Avg: cs.WakeAvgMS, Max: cs.WakeMaxMS}
		}
		if cs.ClockN > 0 {
			rc.Clock = &statsJSON{N: cs.ClockN, Min: cs.ClockMinMHz, Avg: cs.ClockAvgMHz, Max: cs.ClockMaxMHz}
		}
		if cs.CoreClockN > 0 {
			rc.CoreClock = &coreClockJSON{N: cs.CoreClockN, Min: cs.CoreClockMinMHz, Avg: cs.CoreClockAvgMHz, Max: cs.CoreClockMaxMHz, Sources: cs.CoreClockSources}
		}
		if cs.IdleN > 0 {
			rc.Idle = &idleJSON{N: cs.IdleN, Avg: cs.IdleAvgPct, Max: cs.IdleMaxPct}
		}
		if d := dips[cs.Core]; len(d) > 0 {
			rc.Dips = &dipsJSON{N: len(d), WorstMHz: d[0].ClockMHz, MedianMHz: d[0].MedianMHz}
		}
		out.Cores = append(out.Cores, rc)
	}
	return out
}

// provenanceList folds the journal's runs by provenance identity: runs
// written by the same build/config/machine share one entry with a run
// count. A run with no run_meta contributes a null-meta entry (unknown)
// — never a fabricated identity.
func provenanceList(reports []journal.RunReport) []provenanceJSON {
	if len(reports) == 0 {
		return nil
	}
	byLine := map[string]*provenanceJSON{}
	var order []string
	for _, rep := range reports {
		line := journal.FormatProvenance(rep.Meta)
		entry := byLine[line]
		if entry == nil {
			entry = &provenanceJSON{Meta: rep.Meta}
			byLine[line] = entry
			order = append(order, line)
		}
		entry.Runs++
	}
	out := make([]provenanceJSON, 0, len(order))
	for _, line := range order {
		out = append(out, *byLine[line])
	}
	return out
}

// runDetails renders the per-run coverage view: algorithm (kernel)
// version, verified memory volume/passes, transitions and evidence,
// per core. It is per-run so verified bytes are never summed across
// runs that re-checked the same region, and every unknown stays absent
// rather than zero.
func runDetails(reports []journal.RunReport) []runDetailJSON {
	if len(reports) == 0 {
		return nil
	}
	out := make([]runDetailJSON, 0, len(reports))
	for _, rep := range reports {
		rd := runDetailJSON{RunID: rep.RunID, Test: rep.Test, Phase: rep.Phase, Ended: rep.Ended, Exit: rep.Exit, Validation: rep.Validation, Meta: rep.Meta}
		if rd.Validation == "" {
			rd.Validation = "unknown" // a journal without the record: unknown, never a guess
		}
		for _, c := range rep.Cores {
			rc := runCoreCoverageJSON{Core: c.Core, Status: c.Status, KernelVersion: c.KernelVersion, Checks: c.Checks, Evidence: c.Evidence, Mem: c.Mem, Ram: c.Ram}
			if len(c.Bursts) > 0 || len(c.SessionObs) > 0 {
				b := &burstsJSON{Commanded: len(c.SessionObs), Recorded: len(c.Bursts)}
				for _, bu := range c.Bursts {
					if bu.Complete {
						b.Verified++
					}
				}
				rc.Bursts = b
			}
			if len(c.LeaseObs) > 0 {
				l := &leasesJSON{Commanded: len(c.LeaseObs)}
				for _, o := range c.LeaseObs {
					if o.Acked {
						l.Acked++
					} else {
						l.Failed++
					}
				}
				rc.Leases = l
			}
			rd.Cores = append(rd.Cores, rc)
		}
		out = append(out, rd)
	}
	return out
}

// reportDiff compares this journal against a second one, core by core.
// The honest part is the warnings: "core 5 got worse" only means
// something when both journals actually ran the same work for a
// comparable time, and when neither lost runs to a crash mid-count.
func reportDiff(pathA string, issA *journal.Issues, reportsA []journal.RunReport, pathB string) int {
	recsB, issB := journal.ReadAll(pathB)
	if len(recsB) == 0 && issB.ReadErr != "" {
		fmt.Fprintf(os.Stderr, "cannot open journal %q: %v\n", pathB, issB.ReadErr)
		return 2
	}
	reportsB := journal.RecoverRuns(recsB)

	rows, warnings := diffSummaries(
		journal.SummarizeCores(reportsA), journal.SummarizeCores(reportsB),
		phaseSet(reportsA), phaseSet(reportsB))

	fmt.Printf("diff %s  vs  %s:\n", pathA, pathB)
	printJournalIssues(os.Stdout, issA)
	printJournalIssues(os.Stdout, issB)
	for _, r := range rows {
		fmt.Printf("  core %d: A %d run(s) %d fault(s) %s | B %d run(s) %d fault(s) %s | %s\n",
			r.Core, r.ARuns, r.AFaults, r.AExp.Truncate(time.Millisecond),
			r.BRuns, r.BFaults, r.BExp.Truncate(time.Millisecond), r.Note)
	}
	for _, w := range warnings {
		fmt.Println("warning: " + w)
	}
	// WHEA evidence comparability: the counts only mean something when
	// both sides actually watched, and uncorrected hardware evidence
	// differs independently of per-core fault counts.
	if wa, wb := wheaSummary(reportsA), wheaSummary(reportsB); wa != nil && wb != nil {
		if wa.Observation != "available" || wb.Observation != "available" {
			fmt.Printf("warning: WHEA observation not available on both sides (%s vs %s): hardware-error counts are not comparable\n",
				wa.Observation, wb.Observation)
		}
		if wa.Uncorrected != wb.Uncorrected {
			fmt.Printf("warning: uncorrected WHEA records differ (%d vs %d): hardware evidence differs independently of core faults\n",
				wa.Uncorrected, wb.Uncorrected)
		}
	}
	for _, w := range comparabilityWarnings(reportsA, reportsB) {
		fmt.Println("warning: " + w)
	}
	return 0
}

// comparabilityWarnings checks the axes the plan demands before a
// "A vs B" comparison means anything: same tool build (algorithm
// versions), same config profile (hash), same verified memory volume
// and pass coverage, same core coverage. A difference here does NOT
// rank stability - it says the two runs are not the same experiment,
// so per-core fault deltas must not be read as a verdict. Unknown on
// either side is reported as unknown, never assumed equal.
func comparabilityWarnings(reportsA, reportsB []journal.RunReport) []string {
	var warns []string

	hashA, hashB := knownSet(reportsA, func(m *journal.RunMetaFields) string { return m.ConfigHash }),
		knownSet(reportsB, func(m *journal.RunMetaFields) string { return m.ConfigHash })
	if len(hashA) > 0 && len(hashB) > 0 && !sameSet(hashA, hashB) {
		warns = append(warns, fmt.Sprintf("config profile differs (%s vs %s): the two runs used different config.json — not the same profile",
			setText(hashA), setText(hashB)))
	} else if len(hashA) == 0 || len(hashB) == 0 {
		warns = append(warns, "config profile unknown on at least one side (no run_meta config hash): profile comparability unknown")
	}

	// A11: the plan hash covers the ACTUAL experiment - request +
	// resolved plan - so CLI overrides, menu choices and recipe drift
	// are visible here even when config.json never moved. Equal plan
	// hashes are the strongest comparability claim this tool makes;
	// a missing one is unknown, and the absence of a warning is never
	// proof of comparability.
	phA, phB := knownSet(reportsA, func(m *journal.RunMetaFields) string { return m.PlanHash }),
		knownSet(reportsB, func(m *journal.RunMetaFields) string { return m.PlanHash })
	if len(phA) > 0 && len(phB) > 0 && !sameSet(phA, phB) {
		warns = append(warns, fmt.Sprintf("resolved plans differ (plan sha256 %s vs %s): the two runs were not the same experiment — CPU sets, algorithms, phases, seeds, passes or criteria differ",
			setText(phA), setText(phB)))
	} else if len(phA) == 0 || len(phB) == 0 {
		warns = append(warns, "plan identity unknown on at least one side (journals without plan_hash): comparability is NOT proven by the absence of a warning")
	}

	// The same plan on a different CPU layout is a different
	// experiment (SMT siblings, CCX placement and core counts all
	// change what a pinned run actually stresses).
	tA, tB := knownSet(reportsA, func(m *journal.RunMetaFields) string { return m.TopologyFP }),
		knownSet(reportsB, func(m *journal.RunMetaFields) string { return m.TopologyFP })
	if len(tA) > 0 && len(tB) > 0 && !sameSet(tA, tB) {
		warns = append(warns, fmt.Sprintf("CPU topology differs (%s vs %s): the same plan on a different layout is a different experiment",
			setText(tA), setText(tB)))
	}

	revA, revB := knownSet(reportsA, func(m *journal.RunMetaFields) string { return m.BuildRev }),
		knownSet(reportsB, func(m *journal.RunMetaFields) string { return m.BuildRev })
	if len(revA) > 0 && len(revB) > 0 && !sameSet(revA, revB) {
		warns = append(warns, fmt.Sprintf("tool build differs (%s vs %s): algorithm/kernel versions may differ between the runs",
			setText(revA), setText(revB)))
	}

	kernA, kernB := kernelVersions(reportsA), kernelVersions(reportsB)
	if len(kernA) > 0 && len(kernB) > 0 && !sameSet(kernA, kernB) {
		warns = append(warns, fmt.Sprintf("algorithm (kernel) versions differ (%s vs %s): results are not directly comparable",
			setText(kernA), setText(kernB)))
	}

	// Memory coverage compares the LARGEST single run's verified
	// volume, not the journal aggregate: an aggregate can hide a
	// different run count or a different per-run budget behind equal
	// totals (audit A11 - two different experiments must not slip
	// through without a warning).
	volsA, volsB := verifiedPerRun(reportsA), verifiedPerRun(reportsB)
	switch {
	case len(volsA) == 0 && len(volsB) == 0:
		// neither side verified engine memory: nothing to compare here.
	case len(volsA) == 0 || len(volsB) == 0:
		warns = append(warns, "memory verification coverage present on one side only: the runs did not verify the same kind of work")
	default:
		a, b := maxUint(volsA), maxUint(volsB)
		if a != b {
			lo, hi := min(a, b), max(a, b)
			if hi == 0 || float64(hi-lo)/float64(hi) > 0.2 {
				warns = append(warns, fmt.Sprintf("verified memory volume per run differs (largest run %s vs %s): coverage not comparable (equal aggregates can hide different run counts or budgets)",
					human.Bytes(lo), human.Bytes(hi)))
			}
		}
	}

	// A11: exact core SETS, not counts. Two journals covering three
	// cores each on different cores tested different machines'
	// muscles; an equal count hid that.
	setA, setB := coreSet(reportsA), coreSet(reportsB)
	if !sameIntSet(setA, setB) {
		warns = append(warns, fmt.Sprintf("core coverage differs (%s vs %s): the runs did not test the same cores (equal counts of different sets are not the same coverage)",
			human.Ints(sortedInts(setA)), human.Ints(sortedInts(setB))))
	}
	return warns
}

// knownSet collects non-empty provenance strings ("" = unknown, never
// added) across a journal's runs.
func knownSet(reports []journal.RunReport, pick func(*journal.RunMetaFields) string) map[string]bool {
	out := map[string]bool{}
	for _, rep := range reports {
		if rep.Meta == nil {
			continue
		}
		if v := pick(rep.Meta); v != "" {
			out[v] = true
		}
	}
	return out
}

func kernelVersions(reports []journal.RunReport) map[string]bool {
	out := map[string]bool{}
	for _, rep := range reports {
		for _, c := range rep.Cores {
			if c.KernelVersion != "" {
				out[c.KernelVersion] = true
			}
		}
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func setText(s map[string]bool) string {
	names := make([]string, 0, len(s))
	for k := range s {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// verifiedPerRun lists each run's verified-unique memory volume (runs
// with no memory evidence are skipped: unknown, not zero). An empty
// list means no run on this side verified engine memory.
func verifiedPerRun(reports []journal.RunReport) []uint64 {
	var out []uint64
	for _, rep := range reports {
		var total uint64
		seen := false
		for _, c := range rep.Cores {
			if c.Mem != nil && c.Mem.VerifiedUnique != nil {
				total += *c.Mem.VerifiedUnique
				seen = true
			}
		}
		if seen {
			out = append(out, total)
		}
	}
	return out
}

func maxUint(v []uint64) uint64 {
	var m uint64
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// coreSet is the exact set of logical cores a journal's runs touched.
// Comparing SETS (not counts) is what makes "same coverage" an honest
// claim (audit A11).
func coreSet(reports []journal.RunReport) map[int]bool {
	seen := map[int]bool{}
	for _, rep := range reports {
		for _, c := range rep.Cores {
			seen[c.Core] = true
		}
	}
	return seen
}

func sameIntSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedInts(s map[int]bool) []int {
	out := make([]int, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// diffRow is one logical CPU's side-by-side outcome across two journals.
type diffRow struct {
	Core           int
	ARuns, AFaults int
	BRuns, BFaults int
	AExp, BExp     time.Duration
	Note           string
}

// diffSummaries pairs the per-core summaries by core number and states
// the fault delta. Cores present in only one journal are shown with zero
// exposure on the other side and an explicit note — absence is not a
// pass.
func diffSummaries(a, b journal.Summary, aPhases, bPhases []string) ([]diffRow, []string) {
	byA := make(map[int]journal.CoreSummary, len(a.Cores))
	for _, cs := range a.Cores {
		byA[cs.Core] = cs
	}
	byB := make(map[int]journal.CoreSummary, len(b.Cores))
	for _, cs := range b.Cores {
		byB[cs.Core] = cs
	}

	var warnings []string
	var aTot, bTot time.Duration
	for _, cs := range a.Cores {
		aTot += cs.Exposure
	}
	for _, cs := range b.Cores {
		bTot += cs.Exposure
	}
	switch {
	case aTot == 0 || bTot == 0:
		warnings = append(warnings, "one journal has no recorded exposure: fault counts are not comparable")
	case aTot*2 < bTot || bTot*2 < aTot:
		warnings = append(warnings, fmt.Sprintf("total exposure differs more than 2x (%s vs %s): fault counts are not directly comparable",
			aTot.Truncate(time.Second), bTot.Truncate(time.Second)))
	}
	if a.Unended > 0 || b.Unended > 0 {
		warnings = append(warnings, fmt.Sprintf("unended runs present (%d vs %d): crashed runs skew the counts", a.Unended, b.Unended))
	}
	for _, sum := range []journal.Summary{a, b} {
		incomplete := false
		for _, core := range sum.Cores {
			incomplete = incomplete || core.Interrupted > 0 || core.SetupErrors > 0
		}
		if incomplete {
			warnings = append(warnings, "interrupted runs or setup errors present: partial coverage is not a stability pass")
			break
		}
	}
	if !sameStrings(aPhases, bPhases) {
		warnings = append(warnings, "phase/test sets differ: the two journals did not run the same work")
	}

	cores := make([]int, 0, len(byA)+len(byB))
	for c := range byA {
		cores = append(cores, c)
	}
	for c := range byB {
		if _, dup := byA[c]; !dup {
			cores = append(cores, c)
		}
	}
	sort.Ints(cores)

	rows := make([]diffRow, 0, len(cores))
	for _, c := range cores {
		sa, inA := byA[c]
		sb, inB := byB[c]
		r := diffRow{Core: c, ARuns: sa.Runs, AFaults: sa.Faults, AExp: sa.Exposure,
			BRuns: sb.Runs, BFaults: sb.Faults, BExp: sb.Exposure}
		switch {
		case !inB:
			r.Note = "only in A"
		case !inA:
			r.Note = "only in B"
		case sb.Faults > sa.Faults:
			r.Note = fmt.Sprintf("+%d fault(s) in B", sb.Faults-sa.Faults)
		case sb.Faults < sa.Faults:
			r.Note = fmt.Sprintf("-%d fault(s) in B", sa.Faults-sb.Faults)
		default:
			r.Note = "same"
		}
		rows = append(rows, r)
	}
	return rows, warnings
}

// phaseSet lists the distinct phase (or test, for single runs) labels a
// journal actually ran, sorted — the comparability check's input.
func phaseSet(reports []journal.RunReport) []string {
	seen := map[string]bool{}
	for _, rep := range reports {
		label := rep.Phase
		if label == "" {
			label = "test:" + rep.Test
		}
		seen[label] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
