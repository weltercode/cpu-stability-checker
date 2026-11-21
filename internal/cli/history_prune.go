package cli

// R37 (owner decision 2026-11-18): history must be cleanable from
// inside the program. Most users work inside one run and do not need
// an archive - even a few KB per session pollute a plain machine -
// while the dev machine keeps every log through the same knob
// (history_keep -1 in config.json). This file owns the ONLY deletion
// code paths in the tool:
//
//   - retention: after a session finalizes successfully, the newest
//     history_keep records stay; older ones are removed together with
//     their evidence bundle (log, journal, <journal>.workers tree).
//     Pruning is skipped and said out loud whenever the policy is
//     unknown: broken config.json, an invalid history_keep value -
//     deletion under an unknown policy is exactly the silent fallback
//     this project refuses.
//   - manual clear: from the history menu (exact-word confirmation) or
//     the CLI (history -clear requires -yes). Removes ALL records.
//
// Fail-closed guards shared by both paths (a bundle file stays - with
// a stated reason - unless EVERY check passes):
//   - the spelled path resolves lexically INSIDE the working directory
//     (case-folded on Windows like every other path identity here);
//   - its PHYSICAL location resolves inside the same root: a symlink
//     or junction pointing outside is never followed into - the
//     link/junction itself stays too, its contents are not ours;
//   - permanent singletons (lastrun.json, lastresult.json,
//     findings.json, observations.json), config.json, the history
//     directory itself and
//     the directory root are protected by identity;
//   - a path still referenced by a KEPT session's manifest stays:
//     shared logs/journals/trees survive their younger sibling;
//   - an unreadable (corrupt/foreign-version) manifest is never
//     counted, never auto-removed, and any evidence path appearing in
//     its raw bytes is blocked from bundle deletion - ownership is
//     unprovable, so nothing is assumed; manual clear removes the
//     corrupt RECORD and says its evidence may be orphaned;
//   - only <journal>.workers-shaped trees may be removed as
//     directories; any other directory stays with a reason;
//   - execution is evidence-first, manifest-last: if a bundle removal
//     fails (a locked file on Windows), the manifest STAYS so the
//     reference can be retried - a half-cleaned history is a lie
//     about what is gone.
//
// Every removal, kept path and failure is reported; nothing here is
// silent. Absent paths are absence: nothing to do, not a failure,
// never counted as removed.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// --- policy ---

type keepPolicy struct {
	keep   int
	source string // how the number was decided, disclosed in every notice
	skip   string // non-empty: retention must NOT delete anything
}

// resolveKeepPolicy reads the LIVE config.json at prune time. A
// replay never re-prunes history with the number a past run carried,
// and an unknown policy never deletes: the skip string says why.
func resolveKeepPolicy() keepPolicy {
	if _, err := os.Stat(config.DefaultPath); os.IsNotExist(err) {
		return keepPolicy{keep: config.HistoryKeepDefault,
			source: fmt.Sprintf("no config.json: the default keeps the last %d", config.HistoryKeepDefault)}
	}
	cfg, err := config.LoadOrDefault(config.DefaultPath)
	if err != nil {
		return keepPolicy{skip: fmt.Sprintf("%s is not readable (%v): history retention NOT applied - nothing is removed under an unknown policy", config.DefaultPath, err)}
	}
	// Disclosure for the source line: did the file itself declare the
	// key, or is the number the built-in default?
	declared := " (default, no history_keep key)"
	if raw, rerr := os.ReadFile(config.DefaultPath); rerr == nil {
		var probe struct {
			HistoryKeep *json.RawMessage `json:"history_keep"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.HistoryKeep != nil {
			declared = " (config.json history_keep)"
		}
	}
	switch {
	case cfg.HistoryKeep == config.HistoryKeepAll:
		return keepPolicy{keep: cfg.HistoryKeep, source: "history_keep -1: keep everything"}
	case cfg.HistoryKeep >= 1:
		return keepPolicy{keep: cfg.HistoryKeep,
			source: fmt.Sprintf("keep last %d%s", cfg.HistoryKeep, declared)}
	default: // 0 or below -1: not this vocabulary; -1 and 1+ are the only legal shapes
		return keepPolicy{skip: fmt.Sprintf("%s: history_keep must be -1 (keep everything) or 1 or more, not %d: history retention NOT applied", config.DefaultPath, cfg.HistoryKeep)}
	}
}

// --- the shared guard ---

type targetState int

const (
	targetAbsent targetState = iota // nothing there: nothing to do, not a failure
	targetKeep                      // stays, with a stated reason
	targetDelete                    // every check passed: safe to remove
)

type removalGuard struct {
	rootID   string
	histID   string
	keptRefs map[string]bool // evidence ids still referenced by kept manifests
	corrupt  []string        // raw bytes of unreadable manifests (ownership blockers)
}

func newRemovalGuard(kept []sessionManifest) *removalGuard {
	root, err := filepath.Abs(".")
	if err != nil {
		root = "."
	}
	g := &removalGuard{rootID: artifactPathID(root), histID: artifactPathID(historyDir), keptRefs: map[string]bool{}}
	for _, m := range kept {
		for _, r := range m.Runs {
			if r.Log != "" {
				g.keptRefs[artifactPathID(r.Log)] = true
			}
			j := r.Journal
			if j == "" && r.Log != "" {
				j = journal.Path(r.Log)
			}
			if j != "" {
				g.keptRefs[artifactPathID(j)] = true
				g.keptRefs[artifactPathID(j+workersSuffix)] = true
			}
		}
	}
	return g
}

func (g *removalGuard) protectedWhy(id string) string {
	switch id {
	case g.rootID:
		return "this directory itself"
	case artifactPathID(config.DefaultPath):
		return "config.json (the configuration is never bundle evidence)"
	case artifactPathID(lastrunPath):
		return "the replay slot (lastrun.json)"
	case artifactPathID(lastResultPath):
		return "the outcome receipt (lastresult.json)"
	case artifactPathID(findingsPath):
		return "the findings inbox (findings.json)"
	case artifactPathID(observationsPath):
		return "the per-profile landmarks (observations.json, derived but never bundle evidence)"
	}
	if id == g.histID || strings.HasPrefix(id, g.histID+string(filepath.Separator)) {
		return "inside " + historyDir + "/ (records are removed by name at the manifest step, never as bundle paths)"
	}
	return ""
}

// decide is the fail-closed check: it never removes, it only rules.
// The unit is the run entry's declared path whose ownership claim
// covers every derived spelling of the same bundle.
func (g *removalGuard) decide(spelled, unit string) (st targetState, why string, isDir bool, bytes uint64) {
	if spelled == "" {
		return targetAbsent, "", false, 0
	}
	if _, err := filepath.Abs(spelled); err != nil {
		return targetKeep, "the path cannot be resolved: left alone", false, 0
	}
	id := artifactPathID(spelled)
	if !strings.HasPrefix(id, g.rootID+string(filepath.Separator)) {
		return targetKeep, "declared outside this directory: left exactly where it is", false, 0
	}
	fi, lerr := os.Lstat(spelled)
	if os.IsNotExist(lerr) {
		return targetAbsent, "", false, 0 // absence is absence
	}
	if lerr != nil {
		return targetKeep, fmt.Sprintf("it cannot even be stat'ed (%v): left alone", lerr), false, 0
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return targetKeep, "a symbolic link or junction: its target is not ours to follow or remove", false, 0
	}
	// Physical resolution comes BEFORE the name-based protections: a
	// directory alias (link/junction pointing at ".") inside the root
	// makes a fresh lexical spelling of a protected file pass its
	// name check while physically being exactly that file (Sol
	// implementation review 2026-11-18). Protection, kept-references
	// and the corrupt blocker therefore match BOTH identities.
	phys, perr := filepath.EvalSymlinks(spelled)
	if perr != nil {
		return targetKeep, fmt.Sprintf("its physical location cannot be resolved (%v): left alone", perr), false, 0
	}
	pid := artifactPathID(phys)
	if pid == g.rootID || !strings.HasPrefix(pid, g.rootID+string(filepath.Separator)) {
		return targetKeep, "its real location escapes this directory (a link inside pointing out): never followed, left alone", fi.IsDir(), 0
	}
	if w := g.protectedWhy(id); w != "" {
		return targetKeep, "protected: " + w, false, 0
	}
	if w := g.protectedWhy(pid); w != "" {
		return targetKeep, "protected by its real location: " + w, false, 0
	}
	if g.keptRefs[id] || g.keptRefs[pid] {
		return targetKeep, "still referenced by a kept session", false, 0
	}
	for _, raw := range g.corrupt {
		for _, tok := range []string{spelled, unit, jsonEscape(spelled), jsonEscape(unit), id, pid, artifactPathID(unit)} {
			if tok != "" && strings.Contains(raw, tok) {
				return targetKeep, "an unreadable history record names it too: ownership unproven, left alone", false, 0
			}
		}
	}
	if fi.IsDir() {
		if !strings.HasSuffix(filepath.Base(spelled), workersSuffix) {
			return targetKeep, "a directory this tool does not name (only <journal>.workers trees are bundle-owned)", true, 0
		}
		// RemoveAll recurses: a nested junction/symlink INSIDE the
		// tree would otherwise delete through it. The tree is only
		// removable when every entry matches the public shape
		// (tree -> run dir -> plain files, no links, no deeper
		// nesting): anything unexpected keeps the whole tree with a
		// reason instead of trusting the walk.
		if !treeShapeOK(spelled) {
			return targetKeep, "unexpected entries inside (a link or deeper nesting): RemoveAll would walk blindly, so the tree stays", true, 0
		}
		if t := measureWorkersTree(spelled); t != nil {
			bytes = t.Bytes // the two known levels; deeper entries were never sized
		}
		return targetDelete, "", true, bytes
	}
	return targetDelete, "", false, uint64(fi.Size())
}

// treeShapeOK verifies exactly the shape the journal writer produces:
// tree -> run directory -> plain files. A symlink at any level or a
// directory below the run level fails the shape - conservative by
// design, because passing this gate means RemoveAll may recurse.
func treeShapeOK(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		fi, lerr := os.Lstat(filepath.Join(dir, e.Name()))
		if lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if !fi.IsDir() {
			return false // a stray file at the tree root: not the known shape
		}
		runEnts, rerr := os.ReadDir(filepath.Join(dir, e.Name()))
		if rerr != nil {
			return false
		}
		for _, re := range runEnts {
			rfi, rerr := os.Lstat(filepath.Join(dir, e.Name(), re.Name()))
			if rerr != nil || rfi.Mode()&os.ModeSymlink != 0 || rfi.IsDir() {
				return false // a link or deeper nesting below the run level
			}
		}
	}
	return true
}

// jsonEscape spells a path the way a manifest stores it in JSON (a
// Windows absolute path appears escaped inside the raw file).
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}

// bundleItem is one evidence path paired with its OWNERSHIP UNIT: the
// run entry that declares it. The unreadable-record blocker checks
// the unit, so a corrupt manifest naming the same log blocks the
// removal of that log AND its derived journal/tree - ownership is one
// claim, not three coincidences.
type bundleItem struct{ path, unit string }

// bundleSpells lists one record's evidence paths in removal order:
// the tree, then the journal, then the log - deepest first, so a
// failed journal removal never leaves an owner-less tree behind. The
// tree rule and the journal rule are the tool's own public naming
// rules (journal.Path, FragmentsDir), restated here once.
func bundleSpells(m sessionManifest) []bundleItem {
	var out []bundleItem
	seen := map[string]bool{}
	add := func(p string, unit string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, bundleItem{p, unit})
		}
	}
	for _, r := range m.Runs {
		unit := r.Log
		if unit == "" {
			unit = r.Journal
		}
		j := r.Journal
		if j == "" && r.Log != "" {
			j = journal.Path(r.Log)
		}
		if j != "" {
			add(j+workersSuffix, unit)
			add(j, unit)
		}
		add(r.Log, unit)
	}
	return out
}

// --- reports ---

type removalReport struct {
	records     int // history records handled (readable manifests)
	unreadable  int // corrupt/foreign records (kept by retention, removed by clear)
	temps       int // unfinished temp files in history/: left alone, counted
	files       int
	bytes       uint64
	unnamedRuns int      // child runs whose files the manifest never learned
	notes       []string // every stay-behind and every caveat, stated
	failed      []string
	orphanNotes []string
}

// removeSessionBundle removes one victim's evidence files (deepest
// first). It returns false only when a removal FAILED: then the
// manifest must stay so the reference can be retried. "kept with a
// reason" is not a failure.
func removeSessionBundle(m sessionManifest, g *removalGuard, rr *removalReport, deleting bool) bool {
	for _, r := range m.Runs {
		if r.Log == "" && r.Journal == "" {
			rr.unnamedRuns++ // unknown, never assumed
		}
	}
	ok := true
	for _, sp := range bundleSpells(m) {
		st, why, isDir, by := g.decide(sp.path, sp.unit)
		switch st {
		case targetAbsent:
			// absence is absence: nothing to do, never counted
		case targetKeep:
			rr.notes = append(rr.notes, fmt.Sprintf("%s: %s", sp.path, why))
		case targetDelete:
			if !deleting {
				rr.files++
				rr.bytes += by
				continue
			}
			var err error
			if isDir {
				err = os.RemoveAll(sp.path)
			} else {
				err = os.Remove(sp.path)
			}
			if err != nil {
				rr.failed = append(rr.failed, fmt.Sprintf("%s (%v)", sp.path, err))
				ok = false
				continue
			}
			rr.files++
			rr.bytes += by
		}
	}
	return ok
}

// unreadableRaw collects the raw bytes of every history entry that
// this build cannot read faithfully - the ownership blockers.
func unreadableRaw(goneIDs map[string]bool) ([]string, int, int) {
	ents, err := os.ReadDir(historyDir)
	if err != nil {
		return nil, 0, 0
	}
	var raw []string
	unreadable, temps := 0, 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.Contains(name, ".json.tmp") {
			temps++ // a crashed writer's temp: unknown by contract
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if goneIDs[id] {
			continue // this run's own record: not a blocker for itself
		}
		if _, err := loadSessionManifest(id); err == nil {
			continue
		}
		unreadable++
		if b, rerr := os.ReadFile(filepath.Join(historyDir, name)); rerr == nil {
			raw = append(raw, string(b))
		} else {
			// Cannot even read the bytes: ownership of anything it
			// might name stays UNKNOWN - the blocker list cannot
			// match, so bundle removal proceeds only for paths a
			// readable victim declares; the record itself is never
			// counted, never auto-removed, and the notice says so.
			raw = append(raw, "")
		}
	}
	return raw, unreadable, temps
}

// --- retention ---

// pruneHistory is the ONLY automatic deletion in the tool, and it is
// allowed to run only from a successful finalize (see the hook in
// sessionTracker.finishRecord). Every decision it makes is printed.
func pruneHistory() {
	p := resolveKeepPolicy()
	if p.skip != "" {
		fmt.Fprintln(os.Stderr, "notice: "+p.skip)
		return
	}
	if p.keep == config.HistoryKeepAll {
		return // dev machines keep every log
	}
	list, warns := listSessionManifests()
	if len(list) <= p.keep {
		return
	}
	corrupt, unreadable, temps := unreadableRaw(nil)
	kept, victims := list[:p.keep], list[p.keep:]
	g := newRemovalGuard(kept)
	g.corrupt = corrupt
	rr := &removalReport{unreadable: unreadable, temps: temps}
	for _, m := range victims {
		if removeSessionBundle(m, g, rr, true) {
			if err := os.Remove(manifestPath(m.SessionID)); err != nil && !os.IsNotExist(err) {
				rr.failed = append(rr.failed, fmt.Sprintf("manifest %s (%v)", m.SessionID, err))
			} else {
				rr.records++
			}
		} else {
			rr.notes = append(rr.notes, fmt.Sprintf("manifest %s: its bundle was not fully removable, so the record stays and the removal can be retried", m.SessionID))
		}
	}
	if rr.records > 0 || rr.files > 0 || len(rr.notes) > 0 || len(rr.failed) > 0 {
		fmt.Fprintf(os.Stderr, "notice: history retention (%s): removed %d older session record(s) and %d of their file(s) (%s observed)\n",
			p.source, rr.records, rr.files, human.Bytes(rr.bytes))
	}
	for _, k := range rr.notes {
		fmt.Fprintln(os.Stderr, "notice: history retention: "+k)
	}
	for _, f := range rr.failed {
		fmt.Fprintln(os.Stderr, "warning: retention could not remove "+f+"; the record stays so the reference can be retried")
	}
	if rr.unnamedRuns > 0 {
		fmt.Fprintf(os.Stderr, "notice: %d removed child run(s) never had their files named in the manifest (unknown, never assumed)\n", rr.unnamedRuns)
	}
	if rr.unreadable > 0 {
		fmt.Fprintf(os.Stderr, "notice: %d unreadable history record(s) stay: never counted, never auto-removed; their files cannot be owned safely\n", rr.unreadable)
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "notice: history: "+w)
	}
}

// --- manual clear ---

// clearHistory is the shared engine behind the menu's exact-word
// action and the CLI's -clear/-yes pair. dryRun plans and previews
// WITHOUT touching a byte (decide never deletes; the deleting pass
// repeats the full checks).
func clearHistory(dryRun bool) *removalReport {
	rr := &removalReport{}
	list, warns := listSessionManifests()
	gone := map[string]bool{}
	for _, m := range list {
		gone[m.SessionID] = true
	}
	corrupt, unreadable, temps := unreadableRaw(gone)
	rr.unreadable, rr.temps = unreadable, temps
	g := newRemovalGuard(nil) // nothing is kept: every reference clears
	g.corrupt = corrupt
	for _, m := range list {
		if removeSessionBundle(m, g, rr, !dryRun) {
			if dryRun {
				rr.records++
				continue
			}
			if err := os.Remove(manifestPath(m.SessionID)); err != nil && !os.IsNotExist(err) {
				rr.failed = append(rr.failed, fmt.Sprintf("manifest %s (%v)", m.SessionID, err))
			} else {
				rr.records++
			}
		} else {
			rr.notes = append(rr.notes, fmt.Sprintf("manifest %s: its bundle was not fully removable, so the record stays and the clear can be retried", m.SessionID))
		}
	}
	if !dryRun {
		// Corrupt records: an explicit clear removes the RECORD (the
		// owner named "all entries"), and says its evidence may now
		// be orphaned - the artifacts screen can still inventory it.
		ents, err := os.ReadDir(historyDir)
		if err == nil {
			for _, e := range ents {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".json")
				if gone[id] {
					continue
				}
				if err := os.Remove(filepath.Join(historyDir, e.Name())); err != nil && !os.IsNotExist(err) {
					rr.failed = append(rr.failed, fmt.Sprintf("unreadable record %s (%v)", e.Name(), err))
					continue
				}
				rr.records++
				rr.notes = append(rr.notes, fmt.Sprintf("unreadable record %s was removed; the files it may have owned could NOT be identified: they stay - 'artifacts' lists them", e.Name()))
			}
		}
	}
	for _, w := range warns {
		rr.orphanNotes = append(rr.orphanNotes, w)
	}
	if rr.temps > 0 {
		rr.notes = append(rr.notes, fmt.Sprintf("%d unfinished temp file(s) in %s/ stay: they belong to no complete record (unknown)", rr.temps, historyDir))
	}
	return rr
}

// clearPreview renders the preview both the menu and the CLI show
// BEFORE any -yes/typed word commits the removal.
func clearPreview(rr *removalReport) []string {
	lines := []string{
		fmt.Sprintf("This would remove ALL %d session record(s) and their logs, journals and fragment files (%s observed)", rr.records, human.Bytes(rr.bytes)),
	}
	if rr.unreadable > 0 {
		lines = append(lines, fmt.Sprintf("plus %d unreadable record(s) whose files cannot be identified (their evidence would stay - 'artifacts' lists it)", rr.unreadable))
	}
	lines = append(lines,
		"lastrun.json, lastresult.json, findings.json, observations.json and config.json are NEVER touched.",
		"Anything that cannot be safely owned stays behind, listed with its reason.")
	return lines
}

// clearSummary renders what a real (non-dry) clear did.
func clearSummary(rr *removalReport) []string {
	lines := []string{fmt.Sprintf("history cleared: %d record(s) and %d file(s) removed (%s observed)", rr.records, rr.files, human.Bytes(rr.bytes))}
	for _, k := range rr.notes {
		lines = append(lines, "note: "+k)
	}
	for _, f := range rr.failed {
		lines = append(lines, "FAILED: "+f+" (the record stays so the removal can be retried)")
	}
	if rr.unnamedRuns > 0 {
		lines = append(lines, fmt.Sprintf("%d removed child run(s) never had their files named in the manifest (unknown, never assumed)", rr.unnamedRuns))
	}
	return lines
}
