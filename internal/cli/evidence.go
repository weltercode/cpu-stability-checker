package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/runctl"
)

// R19 (owner queue 2026-11-09): when a run ends, the operator used to
// hunt for two things: WHERE the durable evidence lives (and how to
// share it safely), and WHAT the run could not observe - a WHEA
// window that was not cleanly available changes what "zero records"
// means, and the elevation pointer belongs exactly there and nowhere
// else. Every entry point closes with ONE shared block, so the menu,
// the scripted run, the scenario and the session can never drift
// apart in what they promise.
//
// The block never grades a machine moment and never merges per-phase
// observation into a combined WHEA verdict: each window carries its
// own state word and its own recorded reason. In managed mode it
// stays off the JSONL stream automatically - these lines go through
// the very writer the scenario summary already uses, and the headless
// entry routes that writer's stream (os.Stdout) to stderr while the
// sink owns the real stdout.

// wheaObs is one observation story: consecutive runs whose Guard
// reported the same state with the same recorded reason share one
// entry, so a 16-core walk that lost WHEA the same way is stated once
// with the phases named - never sixteen identical lines, and never
// one merged verdict.
type wheaObs struct {
	phases []string
	state  string
	reason string
}

// wheaObsPhasesNamed caps the phases spelled out on one line; beyond
// that the count is stated and the exact list stays in the journal
// (which is the authoritative record anyway).
const wheaObsPhasesNamed = 8

// captureWheaWindow records what the just-finished run's Guard
// observed about WHEA, attributed to that run's phase label. The
// PlanHash comparison is the attribution guard: runctl.Current()
// survives the run (the slot keeps the last Guard until the next
// engine arms one), so a stale Guard from an earlier run must never
// speak for a phase that never had it - if the live Guard belongs to
// a different plan, this phase has nothing in-process to say, and
// silence is the honest answer. An available window needs no pointer;
// "" means this process never watched at all.
func captureWheaWindow(phase, wantPlanHash string, obs *[]wheaObs) {
	if obs == nil {
		return
	}
	g := runctl.Current()
	if g == nil {
		return
	}
	if h := g.PlanHash(); wantPlanHash != "" && h != "" && h != wantPlanHash {
		return // the live Guard is some other run's: nothing to attribute
	}
	state, reason := g.WheaWindow()
	appendWheaObs(obs, phase, state, reason)
}

// appendWheaObs is the merge rule, kept apart from the plumbing so it
// can be pinned directly: clean windows (available, or never watched)
// add nothing; consecutive identical stories merge under one line.
func appendWheaObs(obs *[]wheaObs, phase, state, reason string) {
	if obs == nil || state == "" || state == "available" {
		return
	}
	if n := len(*obs); n > 0 {
		last := &(*obs)[n-1]
		if last.state == state && last.reason == reason {
			last.phases = append(last.phases, phase)
			return
		}
	}
	*obs = append(*obs, wheaObs{phases: []string{phase}, state: state, reason: reason})
}

// printRunEvidence closes every entry point with the same block: the
// journal paths this attempt wrote (grouped when runs share one
// file, existence checked by reading the directory only - a run that
// refused before any load wrote nothing, and the line says so rather
// than pointing at a phantom), the sharing how-to (report to read,
// bundle to send - the scrubbed folder, not the raw journal), the
// durable last result, and the per-phase WHEA pointer for windows
// that were not cleanly available.
func printRunEvidence(out io.Writer, runs []manifestRun, obs []wheaObs) {
	fmt.Fprintln(out, "\nWhere the evidence lives, and how to share it:")

	// Journal paths in first-seen order, the phases that wrote into
	// each named under it (a multi-cycle scenario re-runs the same
	// phases through the same files: one line, not one per cycle).
	type journalLine struct {
		path   string
		phases []string
	}
	var seen []journalLine
	for _, r := range runs {
		if r.Journal == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = "run"
		}
		found := false
		for i := range seen {
			if seen[i].path == r.Journal {
				seen[i].phases = append(seen[i].phases, name)
				found = true
				break
			}
		}
		if !found {
			seen = append(seen, journalLine{path: r.Journal, phases: []string{name}})
		}
	}
	for _, j := range seen {
		note := ""
		if st, err := os.Stat(j.path); err != nil {
			note = "  [no file there: this attempt journaled nothing to it]"
			if !os.IsNotExist(err) {
				note = fmt.Sprintf("  [cannot check this path: %v]", err)
			}
		} else if !st.Mode().IsRegular() {
			note = "  [present but NOT a regular file]"
		}
		fmt.Fprintf(out, "  journal:   %s (%s)%s\n", j.path, strings.Join(j.phases, ", "), note)
	}
	fmt.Fprintf(out, "  last result: %s (the durable receipt of the latest attempt: the 'repeat' screen shows how it ended before replaying, and 'cpu-stability bundle' carries a scrubbed copy of it)\n", lastResultPath)
	fmt.Fprintln(out, "  readable:  'cpu-stability report <journal>' renders every run, finding and verdict word from that journal")
	fmt.Fprintln(out, "  sharing:   'cpu-stability bundle <journal>' gathers journal + report + last result + config into one identity-scrubbed folder - that folder, not the raw journal, is what is safe to send")

	if len(obs) == 0 {
		return
	}
	fmt.Fprintln(out, "WHEA hardware-error observation - only the windows that were NOT cleanly available:")
	for _, o := range obs {
		ph := o.phases
		suffix := ""
		if len(ph) > wheaObsPhasesNamed {
			suffix = fmt.Sprintf(" (+%d more windows with the identical state, each named in the journal)", len(ph)-wheaObsPhasesNamed)
			ph = ph[:wheaObsPhasesNamed]
		}
		labels := make([]string, len(ph))
		for i, p := range ph {
			labels[i] = fmt.Sprintf("%q", p)
		}
		// The state WORD travels verbatim from the journal's
		// vocabulary; an unrecognized one stays shown as-is rather
		// than being smoothed into a known shape.
		what := "reported an out-of-vocabulary state (this build cannot explain it - it stays visible, never repaired): " + o.state
		switch o.state {
		case "unavailable":
			what = "watching hardware-error records was not possible at all: zero WHEA records here means 'could not look', not 'nothing happened'"
		case "degraded":
			what = "watching kept failing or falling behind inside these windows: records may have been missed exactly when they mattered"
		case "initializing":
			what = "monitoring was armed but never completed a full query cycle: a WHEA count of zero here proves nothing about coverage either way"
		}
		reason := "no reason was recorded for this window"
		if o.reason != "" {
			reason = "recorded reason: " + fmt.Sprintf("%q", o.reason)
		}
		fmt.Fprintf(out, "  - phases %s [state %s]: %s (%s%s)\n",
			strings.Join(labels, ", "), o.state, what, reason, suffix)
	}
	fmt.Fprintln(out, "    Every state transition is in the journal (whea_state records); 'cpu-stability whea' lists the records that could be read at all.")
	if runtime.GOOS == "windows" {
		// A pointer, never a diagnosis: the states above say what
		// THIS run observed, not why. Localized wevtutil prose is
		// never classified here; whether elevation would change
		// anything is for the operator to test - what can be stated
		// is that on Windows permissions are the lever that makes
		// the System event log readable to a program at all.
		fmt.Fprintln(out, "    On Windows, if access permissions kept this observation away, running the program elevated (as administrator) is the lever that can change it - the states above name what this run observed, never why, and never a verdict about the machine.")
	}
}
