package subprocess

import (
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// R34 (owner queue 2026-11-17): the live mem fold for the session-shape
// executors (P08 transitions, P07e handoff, Q05 pair switch). The
// session workers already journaled their mem_done facts - exactly one
// per terminal path, written by the kernel that did the work, right
// before core_done - into the same worker fragments the one-shot parent
// scans. Only the parent's reading was missing: the live fold was built
// for the one-shot path, so session-shaped runs ended without the mem
// block while the journal half of their own report showed the facts.
// These executors now read the SAME records and fold them through the
// single rule (plan.FoldRAM); the ack protocol stays untouched, because
// the journal record - written by the engine, never inferred later -
// remains the authority for the numbers.

// scanMemDone reads one run's mem_done records from the main journal
// AND the run's worker fragments, keyed by the slot identity the child
// itself used (last writer wins, the same merge rule as
// scanCoreOutcomes). Records of other runs are excluded by runID: the
// journal files are shared across runs. An unreadable file contributes
// nothing rather than an error: a slot that could not be read stays
// unknown - absence stays absence, and a fold never turns a read
// failure into a zero.
func scanMemDone(mainPath, runID string) map[int]*journal.MemAccounting {
	seen := map[int]*journal.MemAccounting{}
	apply := func(recs []journal.Record) {
		for _, rec := range recs {
			if rec.RunID != runID || rec.Core == nil || rec.Type != journal.MemDone || rec.Mem == nil {
				continue
			}
			m := *rec.Mem
			seen[*rec.Core] = &m
		}
	}
	f, err := os.Open(mainPath)
	if err != nil {
		return seen
	}
	recs, _ := journal.ParseAll(f, mainPath)
	f.Close()
	apply(recs)
	for _, file := range journal.FragmentFiles(mainPath, runID) {
		af, err := os.Open(file)
		if err != nil {
			continue
		}
		frag, _ := journal.ParseAll(af, file)
		af.Close()
		apply(frag)
	}
	return seen
}

// foldRunRAM is the shared live fold the three session-shape executors
// run after Finalize: scan the run's mem_done facts, translate them
// without inventing numbers (planMemFromJournal), and let plan.FoldRAM
// apply the one rule. plannedSlots is how many workers/participants the
// run planned: a worker killed before its terminal path, a core the
// budget partition skipped, or a child that journaled nothing simply
// has no mem_done record, and that slot becomes UnknownSlots. nil when
// nobody reported: a session with no memory engine carries no mem
// block, exactly as before (absence = unknown, never zeros). The fold
// has no say over the verdict - exit and validation were already
// classified from the units by Finalize, and the aggregated
// handoff/switch verdict units stay untouched.
func foldRunRAM(mainPath, runID string, plannedSlots int) *plan.RAMTotals {
	seen := scanMemDone(mainPath, runID)
	if len(seen) == 0 {
		return nil
	}
	observed := make([]*plan.MemAccounting, 0, len(seen))
	for _, m := range seen {
		observed = append(observed, planMemFromJournal(m))
	}
	return plan.FoldRAM(observed, plannedSlots-len(seen))
}
