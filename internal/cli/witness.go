package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// R21 (owner queue 2026-11-09): the hard-lockup witness. A silent freeze
// with no dump leaves a very specific shape in this tool's own artifacts:
// the attempt receipt stays "pending" and session manifests stay "running"
// forever, and journal recovery names the cores that started and never
// finished. Before R21 the operator had to KNOW to run 'journal <file>' to
// see that story, and the OS's own account of an unexpected boot end
// (Kernel-Power 41 / EventLog 6008) was never read at all. This block
// closes both gaps at every load-bearing entry point, BEFORE this attempt
// arms its own receipt - arming is exactly what displaces the pending
// evidence, and a witness printed afterwards would be blind to it.
//
// The honesty rules the block lives by (owner vocabulary):
//   - the previous attempt is INCOMPLETE: it never reported an ending;
//     causes are named only as what each source actually says, never as a
//     verdict about the machine;
//   - shutdown records are time CANDIDATES: TimeCreated is when the later
//     boot wrote the record, this tool keeps no boot boundary, so it never
//     claims which boot a record belongs to;
//   - a readable log that names no such record does NOT mean the end was
//     clean; an unavailable source is stated as the named reason, never
//     zero-filled or skipped;
//   - the records' embedded fields (6008's localized time strings, 41's
//     numeric codes) are never interpreted here;
//   - a clean start costs nothing: with nothing unfinished observable, the
//     block prints nothing and the System log is never queried.
//
// Managed mode inherits this through the scenario/session pipeline: the
// human prose rides the writer that headless mode routes to stderr, so the
// JSONL stream stays one record per line (pinned by the G01 fixtures).

// shutdownRecordsQuery is the seam through which the witness reads the OS's
// unexpected-shutdown records: production uses the whea package's wevtutil
// transport (Windows; other platforms name their own refusal). Tests swap
// the seam; nothing else may.
var shutdownRecordsQuery = func(limit int) ([]whea.Event, error) { return whea.QueryShutdownWitness(limit) }

// witnessShutdownRecords bounds how many newest unexpected-shutdown
// records one block examines; when the bound bites, the block says so.
const witnessShutdownRecords = 10

// witnessShownMarker identifies the evidence the last block spoke about in
// THIS process (attempt id + unfinished session ids). The same unfinished
// evidence is stated once per process - the menu shows it at open, and the
// repeat moments later must not shout it twice - while genuinely new
// unfinished evidence shows again.
var witnessShownMarker string

// witnessMenuQuiet marks the interactive-menu process (R24, owner UX
// contract 2026-11-10): the menu screen NEVER greets the operator with
// the full witness block - findings presence is carried by the always
// visible 'View findings' menu item (red when unread), and at the arm
// moment (after the plan/confirm) this one-liner speaks BEFORE the
// pending evidence is displaced, naming what exists, stating nothing
// was changed, and pointing at the full story. The scripted entry
// points keep the full block byte-identical: the contract change
// belongs to the interactive user, and the pinned machine-facing
// wording stays.
var witnessMenuQuiet = false

// witnessMenuCounts names the unfinished local records in one line
// (never a cause, never a machine verdict).
func witnessMenuCounts(ev unfinishedEvidence) string {
	names := []string{}
	if ev.pending != nil {
		names = append(names, fmt.Sprintf("pending attempt record %s", ev.pending.AttemptID))
	}
	if n := len(ev.running); n > 0 {
		names = append(names, fmt.Sprintf("%d unfinished session manifest(s)", n))
	}
	return strings.Join(names, ", ")
}

// printPreviousAttemptWitness is the scripted-entry variant: it prints the
// block (prefixed by a blank line) exactly where an entry point is about
// to arm its attempt, and prints nothing - at zero query cost - when
// nothing unfinished is observable. In the menu process it prints the
// compact one-liner instead (witnessMenuQuiet), with the same
// once-per-evidence marker: before any arm the evidence is always named.
func printPreviousAttemptWitness(out io.Writer) {
	if witnessMenuQuiet {
		ev := collectUnfinished()
		if ev.pending == nil && len(ev.running) == 0 {
			return
		}
		sort.Strings(ev.markerIDs)
		marker := strings.Join(ev.markerIDs, ";")
		if marker == witnessShownMarker {
			return
		}
		witnessShownMarker = marker
		fmt.Fprintf(out, "\nUnfinished evidence from a previous attempt is present: %s. This attempt changed nothing and claims no cause: the verdict stands INCOMPLETE, the records are listed under menu 'View findings', and 'journal <file>' / 'report' render the full story.\n", witnessMenuCounts(ev))
		return
	}
	block, marker := previousAttemptWitness()
	if block == "" || marker == witnessShownMarker {
		return
	}
	witnessShownMarker = marker
	fmt.Fprint(out, "\n"+block)
}

// unfinishedEvidence is what the local durable records admit about the
// last attempt: never a conclusion, just the records that say "started,
// no ending".
type unfinishedEvidence struct {
	pending   *lastResult // pending attempt receipt (nil = none readable)
	running   []sessionManifest
	markerIDs []string
	earliest  time.Time // earliest readable start of the unfinished evidence (zero = unknown)
	maxBeat   time.Time // latest core beat among recovered crashed cores (zero = none seen)
}

// collectUnfinished gathers the LOCAL durable evidence only (pending
// receipt, running manifests, marker ids, earliest readable start):
// cheap enough for a per-frame menu count and the compact line, and
// with zero journal reads and zero System-log queries. A clean start
// returns a zero evidence with both lists empty.
func collectUnfinished() unfinishedEvidence {
	ev := unfinishedEvidence{}
	r, rerr := loadLastResult()
	if rerr == nil && r.State == receiptPending {
		ev.pending = r
		ev.markerIDs = append(ev.markerIDs, "attempt:"+r.AttemptID)
		if t, terr := time.Parse(time.RFC3339Nano, r.StartedAt); terr == nil {
			ev.earliest = t
		}
	}
	manifests, _ := listSessionManifests()
	for _, m := range manifests {
		if m.State != manifestRunning {
			continue
		}
		ev.running = append(ev.running, m)
		ev.markerIDs = append(ev.markerIDs, "session:"+m.SessionID)
		if t, terr := time.Parse(time.RFC3339Nano, m.Started); terr == nil && (ev.earliest.IsZero() || t.Before(ev.earliest)) {
			ev.earliest = t
		}
	}
	return ev
}

// previousAttemptWitness collects the whole witness from local durable
// records first and the System log second, and renders it as one block.
// Returns ("", "") when a clean start shows nothing unfinished.
func previousAttemptWitness() (string, string) {
	ev := collectUnfinished()
	if ev.pending == nil && len(ev.running) == 0 {
		return "", ""
	}
	sort.Strings(ev.markerIDs)

	var lines []string
	add := func(s string) { lines = append(lines, s) }
	add("The previous attempt never reported an ending. This tool reads its own durable records before touching the machine:")

	if ev.pending != nil {
		add(fmt.Sprintf("  attempt receipt: state %q - attempt %s of recipe %q started %s, never finalized. This tool cannot tell from here whether that process still runs somewhere or ended without reporting: both stay possible.",
			ev.pending.State, ev.pending.AttemptID, ev.pending.RecipeID, ev.pending.StartedAt))
	}
	ev.readJournals(add)
	for _, m := range ev.running {
		add(fmt.Sprintf("  session manifest %s: started %s, state still %q - never finalized (history is never rewritten from here)", m.SessionID, m.Started, m.State))
	}
	ev.osWitness(add)

	start := "unknown (the durable record carries no readable start time)"
	if !ev.earliest.IsZero() {
		start = ev.earliest.UTC().Format(time.RFC3339)
	}
	add(fmt.Sprintf("  previous attempt: INCOMPLETE - started %s, never reported an ending.", start))
	add("  What this block does NOT say: whether that process is still running somewhere, ended without finalizing its record, or the machine stopped answering - this tool observed none of those and states none as fact, and nothing here is graded into a machine verdict. The records above were only read: nothing on disk was changed to produce this block; 'journal <file>' and 'report <journal>' render the full story from them.")
	return strings.Join(lines, "\n") + "\n", strings.Join(ev.markerIDs, ";")
}

// readJournals opens every journal file named by the unfinished evidence
// (a pending receipt names its logs, a running manifest names its runs)
// and renders what recovery says about cores that started and never
// finished. Damage is said loudly, in the same words the 'journal' command
// uses; a file that cannot be read is named as unreadable, never skipped.
func (ev *unfinishedEvidence) readJournals(add func(string)) {
	seen := map[string]bool{}
	var paths []string
	if ev.pending != nil {
		for _, log := range ev.pending.Logs {
			if p := journal.Path(log); !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	for _, m := range ev.running {
		for _, r := range m.Runs {
			p := r.Journal
			if p == "" && r.Log != "" {
				p = journal.Path(r.Log)
			}
			if p != "" && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	for _, p := range paths {
		recs, iss := journal.ReadAll(p)
		if len(recs) == 0 && iss.ReadErr != "" {
			add(fmt.Sprintf("  journal %s: cannot be read (%v): the crashed-run story from this file is unavailable", p, iss.ReadErr))
			continue
		}
		if !iss.Empty() {
			var dmg strings.Builder
			printJournalIssues(&dmg, iss)
			add("  " + strings.TrimRight(dmg.String(), "\n"))
		}
		fileHadLine := false
		for _, rep := range journal.RecoverRuns(recs) {
			var crashed []journal.CoreOutcome
			for _, c := range rep.Cores {
				if c.Status == journal.Crashed {
					crashed = append(crashed, c)
				}
			}
			if len(crashed) == 0 {
				if !rep.Ended {
					// A run whose ending was never journaled while
					// its cores carry terminal states is still part
					// of the story: it never reported itself over.
					add(fmt.Sprintf("  journal %s: run %s (%s): never journaled its own ending, though every named core carries a terminal record", p, rep.RunID, rep.Test))
					fileHadLine = true
				}
				continue
			}
			fileHadLine = true
			label := rep.Test
			if rep.Phase != "" {
				label = fmt.Sprintf("%s, phase %q", label, rep.Phase)
			}
			add(fmt.Sprintf("  journal %s: run %s (%s)%s:", p, rep.RunID, label, ev.attribution(rep)))
			for _, c := range crashed {
				wl := c.Workload
				if wl == "" {
					wl = "workload unknown"
				}
				if c.LastBeat.IsZero() {
					add(fmt.Sprintf("    core %d (%s): CRASHED - never reported progress", c.Core, wl))
					continue
				}
				if c.LastBeat.After(ev.maxBeat) {
					ev.maxBeat = c.LastBeat
				}
				add(fmt.Sprintf("    core %d (%s): CRASHED - last seen %s at %d iterations",
					c.Core, wl, c.LastBeat.UTC().Format(time.RFC3339), c.LastIters))
			}
		}
		if !fileHadLine && len(recs) > 0 {
			// Honest contrast: this journal's runs all ended cleanly -
			// what never finalized is the attempt record itself.
			add(fmt.Sprintf("  journal %s: every run it holds reported its own ending - what never finalized is the attempt record itself, not a load that died mid-window", p))
		}
		if !fileHadLine && len(recs) == 0 && iss.Empty() {
			add(fmt.Sprintf("  journal %s: no valid records there: the attempt named by the durable evidence journaled nothing readable in this file", p))
		}
	}
	if ev.pending != nil && len(ev.pending.Logs) == 0 && len(ev.running) == 0 {
		add("  journal: the pending receipt names no log files, so no journal story is readable from it (unknown, never assumed)")
	}
}

// attribution states, per unfinished run, whether the journal's own
// provenance can bind it to the pending attempt. A journal without
// run_meta, or a run from a different plan hash, is said as such: the run
// is still listed (the file is append-only and shared across attempts),
// but never claimed as THIS attempt's without evidence.
func (ev *unfinishedEvidence) attribution(rep journal.RunReport) string {
	if ev.pending == nil || len(ev.pending.PlanHashes) == 0 {
		return ""
	}
	if rep.Meta == nil || rep.Meta.PlanHash == "" {
		return " (no run_meta provenance in this journal: attempt binding unknown)"
	}
	for _, h := range ev.pending.PlanHashes {
		if h == rep.Meta.PlanHash {
			return ""
		}
	}
	return " (run_meta plan hash names a different experiment: an older unfinished run in the same file, never attributed to this attempt)"
}

// osWitness asks the System log for the OS's own records of an unexpected
// boot end - exactly one query per witness, only ever reached because
// local evidence already says something never ended. Every branch names
// itself: records as time candidates, a clean-and-empty answer WITHOUT it
// being read as "the shutdown was clean", and a refusal as the source's
// own error text.
func (ev *unfinishedEvidence) osWitness(add func(string)) {
	recs, err := shutdownRecordsQuery(witnessShutdownRecords)
	if err != nil {
		add(fmt.Sprintf("  OS shutdown witness (Kernel-Power 41 / EventLog 6008): NOT AVAILABLE - %v", err))
		add(fmt.Sprintf("  what the OS witness says: it could not be consulted (%v): the end of that attempt stays unexplained by the OS side, named as unknown.", err))
		return
	}
	var inWindow, older, undated []whea.Event
	for _, e := range recs {
		switch {
		case e.Time.IsZero():
			undated = append(undated, e)
		case !ev.earliest.IsZero() && e.Time.Before(ev.earliest):
			older = append(older, e)
		default:
			inWindow = append(inWindow, e)
		}
	}
	for _, e := range inWindow {
		line := fmt.Sprintf("  OS shutdown witness: %s, event %d, log record %d, record time %s",
			e.Provider, e.EventID, e.RecordID, e.Time.UTC().Format(time.RFC3339))
		if !ev.maxBeat.IsZero() && e.Time.After(ev.maxBeat) {
			line += fmt.Sprintf(" (%s after the unfinished attempt's last journal beat at %s)",
				e.Time.Sub(ev.maxBeat).Round(time.Millisecond), ev.maxBeat.UTC().Format(time.RFC3339))
		}
		add(line)
	}
	if len(inWindow) > 0 || len(undated) > 0 {
		add("    These records are written by Windows on the boot AFTER an unexpected end of the previous one: the time above is when that later boot wrote the record, not the moment of the fault. This tool keeps no boot boundary, so it never claims which boot a record belongs to - these are time candidates only. The records' embedded fields (localized time strings, numeric codes) are left exactly as the OS wrote them and are never read for meaning here.")
	}
	if len(inWindow) == 0 && len(undated) == 0 {
		bound := fmt.Sprintf("among the %d newest records of this kind", witnessShutdownRecords)
		if !ev.earliest.IsZero() {
			bound = fmt.Sprintf("at or after %s", ev.earliest.UTC().Format(time.RFC3339))
		}
		add(fmt.Sprintf("  OS shutdown witness: the System log answers and names no unexpected-shutdown record %s. A log that answers and shows none does NOT mean the end was clean: Windows writes these records on the later boot and is not guaranteed to write one at all.", bound))
	}
	if len(older) > 0 {
		add(fmt.Sprintf("  %d unexpected-shutdown record(s) in the fetched batch predate the unfinished attempt's start: different moments, not part of this story", len(older)))
	}
	if len(undated) > 0 {
		add(fmt.Sprintf("  %d record(s) of this kind carried no readable time: kept as evidence by log record id, never dropped, never dated", len(undated)))
	}
	if len(recs) >= witnessShutdownRecords {
		add(fmt.Sprintf("  coverage bound: only the %d newest records of this kind were fetched; anything older was not examined", witnessShutdownRecords))
	}
	switch {
	case len(inWindow) > 0:
		add(fmt.Sprintf("  what the OS witness says: it names %d unexpected-shutdown record(s) whose record times follow the unfinished window - a time relation stated by the OS, not a cause stated by this tool.", len(inWindow)))
	case len(undated) > 0:
		add("  what the OS witness says: it answered, but nothing in its answer is dated into the unfinished window (undated records above): the OS side stays unknown for this window.")
	default:
		add("  what the OS witness says: the readable System log names no unexpected-shutdown record for this window: absence is named, not read as a clean end.")
	}
}
