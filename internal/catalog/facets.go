package catalog

import "regexp"

// Facet is the presentation-side explanation of a built-in profile
// (R39, owner-named: "clean separation... simple|fast tests, advanced
// and etc. Of course some explanations helps too"). Facets are NOT
// part of Profile and never enter the pinned `catalog` CLI JSON: they
// are menu-side explanations keyed by the stable card ID, beside the
// card text, never replacing it.
//
// Wording rules (accepted owner decisions): the cost line never
// promises wall-clock time; a per-core walk "helps localize", it
// never "identifies the faulty core"; the PASS facet states the
// LIMITS beside the word; no facet may drift from bounded evidence
// to "the machine is stable". Existing card disclaimers (TM5 parity,
// bandwidth) stay verbatim in the card text; facets restate them
// honestly, never contradict them. R40 (owner: "What the heck is
// P07 - no one understand this"): NO internal development ID may
// ever appear in user-facing wording — neither in the catalog card
// text nor in a facet; a guard test scans every display field.
//
// Short is the R40 list line: one very short description, sized to
// sit on a single screen row (the width test pins it); the full
// explanation lives on the card's own screen, never in the list.
type Facet struct {
	Short   string // the one-line list row (R40: short names, short lines)
	UseWhen string // when this is the right next step
	Catches string // what a finding here points at - and what it does NOT
	Limits  string // displayed as "PASS means / limits"
}

// facetsByID covers every built-in card (the coverage test pins it),
// including the unavailable ones (their entry names the honest state;
// the screens still show the recorded Reason first) and the diagnostic
// prober (whose words describe the DETECTOR, not the machine).
var facetsByID = map[string]Facet{
	"quick-check": {
		Short:   "first bounded word: minutes, not overnight (CPU-only)",
		UseWhen: "first bounded word on a suspect machine: minutes, not overnight",
		Catches: "all-core integer/float miscompares and idle→load wake-up failures inside its window; CPU-only - it verifies no RAM regions",
		Limits:  "PASS means the three named loads verified clean in this window. It is NOT a stability verdict: after any fault or UNVERIFIED word escalate to the full CPU profiles, and deeper evidence needs those longer profiles too",
	},
	"cpu-all": {
		Short:   "every selected core under the same load at once",
		UseWhen: "a general all-core pass: every selected core under the same load at once",
		Catches: "a compute miscompare somewhere among the selected cores - the all-core walk says 'among them', not which one",
		Limits:  "PASS means every result matched its exact expectation for this load and window; compute only - no RAM claim, no 'stable machine' claim",
	},
	"cpu-per-core": {
		Short:   "core-by-core walk: helps localize a fault",
		UseWhen: "after a fault, an UNVERIFIED word, or a Curve Optimizer edit, to narrow the search down",
		Catches: "a mismatch lands on a concrete core - this helps localize; it does not identify the faulty silicon itself (a core failing here can also mean settings, voltage or heat around it)",
		Limits:  "PASS means each core's compute matched exactly while loaded one after another in this window",
	},
	"cpu-core-cycle": {
		Short:   "each core alone, then with all its threads",
		UseWhen: "SMT questions: each core first with one thread, then with all threads at once",
		Catches: "failures that appear only in one SMT mode (alone versus combined)",
		Limits:  "PASS means both SMT modes verified separately for this window",
	},
	"cpu-transitions": {
		Short:   "instability around idle→load wake-up",
		UseWhen: "idle→load instability: failures around wake-up, not at full load",
		Catches: "a commanded transition that fails to run or acknowledge; whether the OS really parked a core is a SEPARATE observation this profile does not make (an explicit run policy, off by default)",
		Limits:  "PASS means the commands and acknowledgements it verified - not a parking claim, not full-spectrum stability",
	},
	"cpu-all-float": {
		Short:   "float (SSE/AVX) paths get their own verified turn",
		UseWhen: "when the FPU/SSE/AVX paths need their own verified turn",
		Catches: "floating-point miscompares the integer passes can miss; a different failing load usually points at a different failing path",
		Limits:  "PASS means float results were recomputed and compared in each window of this run",
	},
	"cpu-all-sha256": {
		Short:   "different integer units: hash ops against a reference",
		UseWhen: "a different set of integer units than the arithmetic chains: adds, rotates, xor, modular adds",
		Catches: "digest mismatches against the independent scalar reference and published test vectors",
		Limits:  "PASS means every digest matched its reference for this run",
	},
	"ram-constant": {
		Short:   "write one pattern, re-check it: the classic first pass",
		UseWhen: "the classic integrity first pass: write one pattern, re-check it",
		Catches: "cells that cannot hold a written pattern inside this run's engine regions",
		Limits:  "PASS means the pattern survived inside this run's regions - not full-RAM coverage, not a machine verdict; volume and duration are the budget and duration you set",
	},
	"ram-address": {
		Short:   "for suspected address-line faults",
		UseWhen: "when address-line faults are the suspicion: the values depend on the region's address",
		Catches: "value and aliasing faults along address lines inside this run's regions",
		Limits:  "PASS means the address-dependent data verified inside this run's regions only",
	},
	"ram-moving": {
		Short:   "copies between regions: transfer integrity",
		UseWhen: "transfer integrity, not just storage: content is copied between regions",
		Catches: "data corrupted in transit - a write alone can look clean where a copy fails",
		Limits:  "PASS means the copies verified between this run's regions",
	},
	"ram-random": {
		Short:   "seeded random data per run, replayable by seed",
		UseWhen: "independent data per run: a seeded pseudo-random stream, replay reproducible by seed",
		Catches: "value corruption under data that never repeats a pattern",
		Limits:  "PASS means this run's stream verified inside its regions; the seed makes it replayable, not broader",
	},
	"ram-extended": {
		Short:   "the full extended pattern set in one plan",
		UseWhen: "the full extended pattern set in one plan: base cycles, stride, reverse passes, block swaps, walking bits, copies between region pairs, a dwell check",
		Catches: "pattern- and locality-sensitive failures; the dwell check verifies survival across a pause, NOT DRAM retention; cross-core block handoff is not in the set; this is not a Rowhammer test",
		Limits:  "PASS means every phase kernel met its own contract over this run's regions, one shared budget per phase",
	},
	"ram-sustained": {
		Short:   "long cycling run: 18 phases of data kernels",
		UseWhen: "a long cycling run: 18 phases of data kernels, locality contrasts and verified-copy interludes, in a hand-built order",
		Catches: "whatever these kernels find over the window you set; the sequence structure mirrors what the PUBLIC TestMem5 Extreme1 config discloses - TM5's pattern internals are closed and NOT reproduced; whether this finds what TM5 finds is unmeasured, no parity is claimed either way, and the comparison itself is a separate owner decision with destructive-level consequences",
		Limits:  "PASS means each phase closed its verified passes inside its window (a phase closing no full pass reports partial, not nothing); every phase length is the duration you give",
	},
	"ram-extreme-rough": {
		Short:   "TM5 Extreme1's published order, run by our kernels",
		UseWhen: "the published TestMem5 Extreme1 sequence (31 steps) reproduced VERBATIM in order, each step run by our own verified kernel chosen by role",
		Catches: "order reproduced, internals not; whether this finds what TM5 finds is unmeasured - no equivalence is claimed either way, and the comparison itself is a separate owner decision with destructive-level consequences",
		Limits:  "PASS means each slot met its own kernel contract inside its nominal deadline window (the block kernel credits whole verified units whatever the region size; a slot closing no unit says so); the unscheduled RefreshStable test stays excluded exactly as the public config excludes it",
	},
	"mem-quick": {
		Short:   "fast smoke over a tiny buffer only",
		UseWhen: "a fast smoke test: a short check of a small buffer",
		Catches: "gross memory failure - inside a tiny slice of the address space only",
		Limits:  "PASS is evidence for this run's small buffer, NOT for the machine's RAM; it is not overnight validation",
	},
	"mem-bandwidth": {
		Short:   "heat and stress only: not a data check",
		UseWhen: "heat and stress only - deliberately NOT as a data check",
		Catches: "nothing about data correctness: the reads feed a progress checksum, not a verdict",
		Limits:  "the clean word here says the machine survived the load; it says NOTHING about RAM correctness and must never be reported as a memory test",
	},
	"mix-comprehensive": {
		Short:   "sequential CPU→RAM journey under a shared verdict",
		UseWhen: "one scripted sequential journey: CPU, then RAM, transitions and rising load, under a shared verdict",
		Catches: "what each phase's own contract catches; the phases run SEQUENTIALLY - this profile has no simultaneous CPU+RAM load and will not promise one",
		Limits:  "PASS means the phases that ran each met their own contract; the shared verdict is exactly as strong as its weakest named phase",
	},
	"mix-simultaneous": {
		Short:   "not available yet: the screen states why",
		UseWhen: "not available on this tool yet: the screen states the recorded reason, and selecting it changes nothing",
		Catches: "not observable until co-execution exists",
		Limits:  "no PASS can exist for this goal yet - that absence is stated, never guessed",
	},
	"mix-transitions-on-ram": {
		Short:   "not available yet: the screen states why",
		UseWhen: "not available on this tool yet: the screen states the recorded reason, and selecting it changes nothing",
		Catches: "not observable until the lifecycle combination exists",
		Limits:  "no PASS can exist for this goal yet - that absence is stated, never guessed",
	},
	"diag-fault": {
		Short:   "self-check: a deliberate fault proves the reporting path",
		UseWhen: "the pipeline self-check: an intentionally wrong result proving the finding and reporting path works",
		Catches: "a broken finding/reporting chain - a FAIL here is the EXPECTED, correct outcome",
		Limits:  "the words describe the detector, not the machine: a 'clean' word would mean the deliberate fault went UNCAUGHT - that is the bug",
	},
}

// FacetsFor returns the built-in card's facet record. ok=false is the
// honest absence - external engines registered per machine have no
// facet entry, and callers must say so instead of inventing
// capabilities for a tool they do not control.
func FacetsFor(id string) (Facet, bool) {
	f, ok := facetsByID[id]
	return f, ok
}

// internalIDPattern matches development-only identifiers (P07, P07f,
// A05, Q06 and friends) that must never surface in user-facing
// wording (R40, owner-named: "What the heck is P07 - no one
// understand this"). It deliberately does not match product words
// that carry digits (SSE2, TestMem5, Extreme1, 18 phases): only one
// of the project's own queue-letter prefixes fused to digits at a
// word edge, with the optional letter suffix the queues use (P07f).
// InternalIDPattern is exported for the i18n guards (R41): the ban on
// development-only identifiers covers translated display text too.
var InternalIDPattern = regexp.MustCompile(`\b[PAQRC]\d{1,3}[a-z]?\b`)

// DisplayStrings lists every user-facing string of the built-in
// cards - card text fields and facets alike, each tagged with its
// card ID and field. The R40 guard test scans the whole set for
// development IDs; a hit is a defect in the wording, not in the
// test. It is exported so the CLI side can also prove nothing else
// prints these fields with an ID pasted in.
func DisplayStrings() map[string]string {
	out := map[string]string{}
	for _, p := range All() {
		out[p.ID+".title"] = p.Title
		out[p.ID+".summary"] = p.Summary
		out[p.ID+".details"] = p.Details
		out[p.ID+".reason"] = p.Reason
	}
	for id, f := range facetsByID {
		out[id+".short"] = f.Short
		out[id+".when"] = f.UseWhen
		out[id+".catches"] = f.Catches
		out[id+".limits"] = f.Limits
	}
	return out
}
