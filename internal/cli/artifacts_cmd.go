package cli

// artifacts (owner queue item 5, R36, 2026-11-17): the read-only
// evidence inventory. The question it answers is "what has this
// directory accumulated, and what does the tool's own metadata still
// point at" - journal by journal, with each journal's <journal>.workers/
// fragment tree counted as part of the journal's own story (ReadAll
// merges the fragments, and NOTHING in the tool ever deletes them:
// fragments are evidence, not crash residue).
//
// Hard honesty rules for this command:
//   - it reads, prints and counts. It never deletes, moves, renames
//     or truncates anything, carries no delete/prune flag, and no line
//     of its output is a statement that a file may be removed safely:
//     removal is only ever the owner's separate named decision;
//   - observation, never judgment: size and last-write time are
//     facts; age is never framed as stale, junk or cleanable;
//   - reference classification ("history manifest X names this file",
//     "the last-attempt receipt names it", "the replay slot names it")
//     comes only from metadata this scan successfully read.
//     UNREFERENCED therefore means only "no reference found in what
//     I could read": legacy artifacts (history predates the session
//     manifests), untracked entry points and removed manifests all
//     explain it, and a manifest that lists a file which went missing
//     still establishes the reference;
//   - scope stays stated: this scan sees the directory's own entries,
//     history/, and each journal's two-level fragment tree. Anywhere
//     else (a run pointed at -output elsewhere, unrelated user files
//     in other directories) is OUT OF VIEW: unknown, not absent;
//   - absence is absence: a singleton that is not there says so and
//     no zeros are substituted.
//
// scanArtifacts never writes: not a temp file, not a lock, nothing.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

const (
	journalSuffix = ".journal.jsonl" // journal.Path's rule, restated once
	workersSuffix = ".workers"       // journal.FragmentsDir's rule: <journal>+.workers
	bundlePrefix  = "bug-report-"    // bundleCmd's default export-directory name
)

func artifactsCmd(args []string) int {
	fs := flag.NewFlagSet("artifacts", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if refusePositionals(fs, "artifacts") {
		return 2
	}
	scan, err := scanArtifacts()
	if err != nil {
		fmt.Fprintf(os.Stderr, "artifacts: %v\n", err)
		return 2
	}
	if *asJSON {
		if err := writeJSONAnswer(scan); err != nil {
			fmt.Fprintf(os.Stderr, "cannot encode artifacts: %v\n", err)
			return 2
		}
		return 0
	}
	printArtifactsText(os.Stdout, scan)
	return 0
}

// --- the shape (JSON and text render the same model) ---

type artifactRefs struct {
	Sessions   []string `json:"history_sessions,omitempty"` // session ids of manifests naming the path
	LastResult bool     `json:"lastresult"`                 // the last-attempt receipt names it
	LastRun    bool     `json:"lastrun_slot"`               // the replay slot names it
}

func (r artifactRefs) any() bool {
	return len(r.Sessions) > 0 || r.LastResult || r.LastRun
}

type workersTree struct {
	Dir       string `json:"dir"`
	RunDirs   int    `json:"run_dirs"`
	Fragments int    `json:"fragment_files"`
	Bytes     uint64 `json:"bytes"`
	Unscanned int    `json:"deeper_entries_unscanned,omitempty"` // anything below the two known levels: counted, never sized
}

type artifactGroup struct {
	Journal  string       `json:"journal"`
	Present  bool         `json:"journal_present"` // false only for tree-rooted groups (orphan trees)
	Bytes    uint64       `json:"journal_bytes,omitempty"`
	Modified string       `json:"modified_local,omitempty"`
	Log      *logSide     `json:"log,omitempty"`
	Workers  *workersTree `json:"workers,omitempty"`
	Refs     artifactRefs `json:"references"`
}

type logSide struct {
	Name     string       `json:"name"`
	Present  bool         `json:"present"`
	Bytes    uint64       `json:"bytes,omitempty"`
	Modified string       `json:"modified_local,omitempty"`
	Refs     artifactRefs `json:"references"`
}

type singleton struct {
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Bytes    uint64 `json:"bytes,omitempty"`
	Modified string `json:"modified_local,omitempty"`
	Note     string `json:"note,omitempty"` // unreadable metadata etc.; "" = plain file fact
}

type exportDir struct {
	Dir      string `json:"dir"`
	Files    int    `json:"direct_files"`
	Bytes    uint64 `json:"direct_bytes"`
	Subdirs  int    `json:"subdirs_unscanned"` // never sized: out of the scan's stated scope
	Modified string `json:"modified_local,omitempty"`
}

type declaredPath struct {
	Session string `json:"session"`
	Path    string `json:"path"`
	Kind    string `json:"kind"` // journal | log
}

type artifactsScan struct {
	Schema    int      `json:"schema"`
	Scope     []string `json:"scope"`
	Rules     []string `json:"rules"` // the honesty rules, in the machine answer too
	UnrefRule string   `json:"unreferenced_rule"`

	Groups      []artifactGroup `json:"journal_groups"`
	OrphanTrees []artifactGroup `json:"orphan_worker_trees"` // trees whose main journal file is not here
	StrayLogs   []logSide       `json:"stray_log_files"`     // *.log with no journal sibling
	Singletons  []singleton     `json:"singletons"`
	History     struct {
		Manifests int      `json:"manifests"`
		Bytes     uint64   `json:"bytes"`
		Unnamed   int      `json:"child_runs_without_file_names"` // the manifest never learned these children's files
		Warnings  []string `json:"warnings,omitempty"`            // corrupt manifests: their references are UNKNOWN, not absent
	} `json:"history"`
	DeclaredAbsent []declaredPath `json:"manifest_paths_not_on_disk"` // referenced anyway: absence of the file does not erase the reference
	Exports        []exportDir    `json:"exports"`

	CoverageNotes []string `json:"reference_coverage_notes"` // receipts unreadable -> their references stay unknown
	Totals        struct {
		Groups        int    `json:"journal_groups"`
		OrphanTrees   int    `json:"orphan_worker_trees"`
		StrayLogs     int    `json:"stray_log_files"`
		Exports       int    `json:"export_dirs"`
		ObservedBytes uint64 `json:"observed_bytes_this_scan_only"`
	} `json:"totals"`
}

// --- the scan (read-only by construction) ---

func scanArtifacts() (*artifactsScan, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("cannot read the working directory: %w", err)
	}
	type fileStat struct {
		size uint64
		mod  time.Time
	}
	rootFiles := map[string]fileStat{}
	rootDirs := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			rootDirs[e.Name()] = true
			continue
		}
		if info, ierr := e.Info(); ierr == nil {
			rootFiles[e.Name()] = fileStat{size: uint64(info.Size()), mod: info.ModTime()}
		}
		// An entry whose stat failed mid-scan (a file that vanished
		// between listing and stat): it is NOT counted - absence of a
		// measurement is not a zero measurement.
	}

	// Reference sets, built from DECLARED metadata regardless of what
	// the disk holds: a manifest that lists a vanished journal still
	// references it (Sol review 2026-11-17: filtering absent entries
	// before building the references would label live evidence
	// UNREFERENCED).
	scan := &artifactsScan{Schema: 1}
	histRef := map[string][]string{}
	refFlags := map[string]*artifactRefs{} // path -> receipt/slot flags
	addHist := func(path, session string) {
		if path == "" {
			return
		}
		c := artifactPathID(path)
		histRef[c] = append(histRef[c], session)
	}

	manifests, warns := listSessionManifests()
	for _, m := range manifests {
		scan.History.Manifests++
		for _, r := range m.Runs {
			jp := r.Journal
			if jp == "" && r.Log != "" {
				jp = journal.Path(r.Log) // the naming rule is public: deriving it is not a guess
			}
			if jp == "" && r.Log == "" {
				scan.History.Unnamed++ // unknown: never counted as absent, never assumed
				continue
			}
			addHist(jp, m.SessionID)
			addHist(r.Log, m.SessionID)
			// Declared-presence is judged on the DECLARED path itself
			// (a manifest written by a run with an absolute -journal
			// names an absolute path; a bare filename lookup would
			// turn a live file into a phantom "missing" entry - Sol
			// review 2026-11-17): present out of scope still counts
			// as present, and its references simply live outside
			// this scan's view.
			if jp != "" && !artifactPathExists(jp) {
				scan.DeclaredAbsent = append(scan.DeclaredAbsent, declaredPath{Session: m.SessionID, Path: jp, Kind: "journal"})
			}
			if r.Log != "" && !artifactPathExists(r.Log) {
				scan.DeclaredAbsent = append(scan.DeclaredAbsent, declaredPath{Session: m.SessionID, Path: r.Log, Kind: "log"})
			}
		}
	}
	scan.History.Warnings = warns
	if len(warns) > 0 {
		scan.CoverageNotes = append(scan.CoverageNotes,
			fmt.Sprintf("%d manifest(s) could not be read: anything only THEY reference is UNKNOWN, not unreferenced", len(warns)))
	}

	// The receipts' references are read as path tokens only: a recipe
	// that is invalid for REPLAY still declares the paths it carries.
	if raw, rerr := os.ReadFile(lastResultPath); rerr == nil {
		var tok struct {
			Logs []string `json:"logs"`
		}
		if json.Unmarshal(raw, &tok) != nil {
			scan.CoverageNotes = append(scan.CoverageNotes, lastResultPath+": not readable as JSON: its references are UNKNOWN, not absent")
		}
		for _, l := range tok.Logs {
			markFlag(refFlags, l, true, false)
			markFlag(refFlags, journal.Path(l), true, false)
		}
	} else if !os.IsNotExist(rerr) {
		scan.CoverageNotes = append(scan.CoverageNotes, fmt.Sprintf("%s: cannot read (%v): its references are UNKNOWN", lastResultPath, rerr))
	}
	if raw, rerr := os.ReadFile(lastrunPath); rerr == nil {
		var tok struct {
			Session *struct {
				LogFile string `json:"log_file"`
			} `json:"session"`
			Handoff *struct {
				Journal string `json:"journal"`
			} `json:"handoff"`
		}
		if json.Unmarshal(raw, &tok) != nil {
			scan.CoverageNotes = append(scan.CoverageNotes, lastrunPath+": not readable as JSON: its references are UNKNOWN, not absent")
		}
		if tok.Session != nil && tok.Session.LogFile != "" {
			markFlag(refFlags, tok.Session.LogFile, false, true)
			markFlag(refFlags, journal.Path(tok.Session.LogFile), false, true)
		}
		if tok.Handoff != nil && tok.Handoff.Journal != "" {
			markFlag(refFlags, tok.Handoff.Journal, false, true)
		}
	} else if !os.IsNotExist(rerr) {
		scan.CoverageNotes = append(scan.CoverageNotes, fmt.Sprintf("%s: cannot read (%v): its references are UNKNOWN", lastrunPath, rerr))
	}
	// A scenario slot stores its phases WITHOUT per-phase log paths
	// (they are derived at replay time): those references cannot be
	// extracted here and stay honestly unknown.
	// (Stated once in the rules; no silent guessing.)

	// Journal groups: every root journal file plus its fragment tree
	// plus its sibling log, counted as ONE evidence story.
	groupedByLog := map[string]bool{}
	for name := range rootFiles {
		if !strings.HasSuffix(name, journalSuffix) {
			continue
		}
		g := artifactGroup{Journal: name, Present: true,
			Bytes: rootFiles[name].size, Modified: localStampText(rootFiles[name].mod)}
		logName := strings.TrimSuffix(name, journalSuffix)
		groupedByLog[logName] = true
		if ls, ok := rootFiles[logName]; ok {
			g.Log = &logSide{Name: logName, Present: true, Bytes: ls.size, Modified: localStampText(ls.mod)}
		} else {
			g.Log = &logSide{Name: logName, Present: false} // absent: unknown where the log went, never zero
		}
		if tree := measureWorkersTree(name + workersSuffix); tree != nil {
			g.Workers = tree
		}
		g.Refs = refsFor(histRef, refFlags, name)
		if g.Log != nil {
			g.Log.Refs = refsFor(histRef, refFlags, logName)
		}
		scan.Groups = append(scan.Groups, g)
	}
	sort.Slice(scan.Groups, func(i, j int) bool { return scan.Groups[i].Journal < scan.Groups[j].Journal })

	// Fragment trees whose main journal file is not in this directory:
	// the story may still be complete inside the fragments (ReadAll
	// reads them without the main file). Not orphans in the junk
	// sense: groups with the journal side absent.
	for name := range rootDirs {
		if !strings.HasSuffix(name, workersSuffix) {
			continue
		}
		main := strings.TrimSuffix(name, workersSuffix)
		if _, ok := rootFiles[main]; ok {
			continue // already counted inside its journal group
		}
		tree := measureWorkersTree(name)
		if tree == nil {
			continue
		}
		g := artifactGroup{Journal: main, Present: false}
		g.Workers = tree
		g.Refs = refsFor(histRef, refFlags, main)
		scan.OrphanTrees = append(scan.OrphanTrees, g)
	}
	sort.Slice(scan.OrphanTrees, func(i, j int) bool { return scan.OrphanTrees[i].Journal < scan.OrphanTrees[j].Journal })

	// *.log files that are nobody's journal sibling: possibly a log
	// whose journal went missing, possibly not the tool's file at all
	// - the name shape is an observation, not an identity.
	for name := range rootFiles {
		if !strings.HasSuffix(name, ".log") || groupedByLog[name] {
			continue
		}
		ls := rootFiles[name]
		scan.StrayLogs = append(scan.StrayLogs, logSide{Name: name, Present: true,
			Bytes: ls.size, Modified: localStampText(ls.mod), Refs: refsFor(histRef, refFlags, name)})
	}
	sort.Slice(scan.StrayLogs, func(i, j int) bool { return scan.StrayLogs[i].Name < scan.StrayLogs[j].Name })

	// Root singletons: present = a file fact; absent = stated, never
	// zero-filled. Names come from the live path vars (the same
	// singletons every other route reads).
	scan.Singletons = []singleton{
		singletonFor(lastrunPath, "the replay slot (one request deep-replay repeats)"),
		singletonFor(lastResultPath, "the last attempt's outcome receipt"),
		singletonFor(findingsPath, "the findings inbox store"),
	}

	// history/: manifest-only accounting (the manifests themselves are
	// permanent history records: listing them is reading the index).
	if ents, herr := os.ReadDir(historyDir); herr == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if info, ierr := e.Info(); ierr == nil {
				scan.History.Bytes += uint64(info.Size())
			}
		}
	} else if !os.IsNotExist(herr) {
		scan.CoverageNotes = append(scan.CoverageNotes, fmt.Sprintf("%s: cannot read (%v)", historyDir, herr))
	}

	// Export bundles: directory observations; contents are not
	// recursively sized (that is outside this scan's stated scope).
	for name := range rootDirs {
		if !strings.HasPrefix(name, bundlePrefix) {
			continue
		}
		exp := exportDir{Dir: name}
		if ents, eerr := os.ReadDir(name); eerr == nil {
			for _, e := range ents {
				if e.IsDir() {
					exp.Subdirs++
					continue
				}
				exp.Files++
				if info, ierr := e.Info(); ierr == nil {
					exp.Bytes += uint64(info.Size())
				}
			}
			if info, ierr := os.Stat(name); ierr == nil {
				exp.Modified = localStampText(info.ModTime())
			}
		} else {
			scan.CoverageNotes = append(scan.CoverageNotes, fmt.Sprintf("export %s: cannot read (%v)", name, eerr))
		}
		scan.Exports = append(scan.Exports, exp)
	}
	sort.Slice(scan.Exports, func(i, j int) bool { return scan.Exports[i].Dir < scan.Exports[j].Dir })
	sort.Slice(scan.DeclaredAbsent, func(i, j int) bool {
		if scan.DeclaredAbsent[i].Session != scan.DeclaredAbsent[j].Session {
			return scan.DeclaredAbsent[i].Session < scan.DeclaredAbsent[j].Session
		}
		return scan.DeclaredAbsent[i].Path < scan.DeclaredAbsent[j].Path
	})

	// Totals over observed bytes only.
	scan.Totals.Groups = len(scan.Groups)
	scan.Totals.OrphanTrees = len(scan.OrphanTrees)
	scan.Totals.StrayLogs = len(scan.StrayLogs)
	scan.Totals.Exports = len(scan.Exports)
	var total uint64
	for _, g := range scan.Groups {
		total += g.Bytes
		if g.Log != nil && g.Log.Present {
			total += g.Log.Bytes
		}
		if g.Workers != nil {
			total += g.Workers.Bytes
		}
	}
	for _, t := range scan.OrphanTrees {
		if t.Workers != nil {
			total += t.Workers.Bytes
		}
	}
	for _, l := range scan.StrayLogs {
		total += l.Bytes
	}
	for _, s := range scan.Singletons {
		if s.Present {
			total += s.Bytes
		}
	}
	total += scan.History.Bytes
	for _, e := range scan.Exports {
		total += e.Bytes
	}
	scan.Totals.ObservedBytes = total

	scan.Scope = []string{
		"this directory's own entries (one level)",
		historyDir + "/ manifests (manifest-only: journals are never re-scanned at list time)",
		"each journal's <journal>.workers/<run-id>/ fragment tree (two levels; deeper entries counted, not sized)",
		"anything else - other directories, a run pointed at -output elsewhere - is OUT OF VIEW: unknown, not absent",
	}
	scan.Rules = []string{
		"this command reads, prints and counts only: it never deletes, moves, renames or truncates anything",
		"no line of this output says any file may be removed safely; removal is only the operator's separate decision",
		"size and last-write time are observations; age is never a judgment, and no recommendation about what to do with any file is in this command's vocabulary",
		"a scenario replay slot stores its phases without per-phase log paths: those references cannot be extracted at read time and stay UNKNOWN",
	}
	scan.UnrefRule = "UNREFERENCED here means only: no reference found in the metadata this scan successfully read. Legacy artifacts (history predates the session manifests), untracked entry points and removed manifests all explain it. It is NOT a deletion-safety assessment."
	return scan, nil
}

func markFlag(m map[string]*artifactRefs, path string, result, slot bool) {
	if path == "" {
		return
	}
	c := artifactPathID(path)
	refs := m[c]
	if refs == nil {
		refs = &artifactRefs{}
		m[c] = refs
	}
	if result {
		refs.LastResult = true
	}
	if slot {
		refs.LastRun = true
	}
}

func refsFor(hist map[string][]string, flags map[string]*artifactRefs, path string) artifactRefs {
	c := artifactPathID(path)
	refs := artifactRefs{Sessions: hist[c]}
	if f := flags[c]; f != nil {
		refs.LastResult, refs.LastRun = f.LastResult, f.LastRun
	}
	return refs
}

// measureWorkersTree counts the two known levels (<tree>/<run-id>/
// *.jsonl) and reports anything deeper as counted-but-unsized rather
// than pretending the tree fits the known shape.
func measureWorkersTree(dir string) *workersTree {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	t := &workersTree{Dir: dir}
	for _, runDir := range ents {
		if !runDir.IsDir() {
			t.Unscanned++ // a stray file at the run level: observed, not folded into the known shape
			continue
		}
		t.RunDirs++
		files, ferr := os.ReadDir(filepath.Join(dir, runDir.Name()))
		if ferr != nil {
			continue // unreadable run dir: unmeasured, never zero
		}
		for _, f := range files {
			if f.IsDir() {
				t.Unscanned++
				continue
			}
			t.Fragments++
			if info, ierr := f.Info(); ierr == nil {
				t.Bytes += uint64(info.Size())
			}
		}
	}
	return t
}

func singletonFor(path, role string) singleton {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return singleton{Name: filepath.Base(path), Present: false, Note: role}
	}
	if err != nil {
		return singleton{Name: filepath.Base(path), Present: false, Note: role + ": cannot stat (" + err.Error() + "): unknown"}
	}
	if info.IsDir() {
		return singleton{Name: filepath.Base(path), Present: false, Note: role + ": a DIRECTORY stands at this path: not a usable file (unknown state)"}
	}
	return singleton{Name: filepath.Base(path), Present: true, Bytes: uint64(info.Size()), Modified: localStampText(info.ModTime()), Note: role}
}

func localStampText(t time.Time) string {
	return t.In(time.Local).Format("02.01.2006 15:04")
}

// artifactPathID puts a DECLARED path (relative or absolute, however
// the manifest spelled it) and an OBSERVED root filename onto one
// identity, so a manifest that stored an absolute path to a file in
// THIS directory still finds its group instead of leaving the live
// artifact UNREFERENCED and falsely declared absent. Display keeps
// the original spelling; only matching is normalized. On
// case-insensitive platforms the comparison folds case; identity is
// lexical (absolute + Clean), never a guessed basename match that
// could conflate files in different directories.
func artifactPathID(p string) string {
	id := p
	if abs, err := filepath.Abs(p); err == nil {
		id = abs
	}
	id = filepath.Clean(id)
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

// artifactPathExists judges the declared path itself (os.Stat on the
// spelled path resolves relative-to-cwd and absolute alike): present
// out of scope is still present.
func artifactPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// --- rendering ---

func printArtifactsText(w io.Writer, s *artifactsScan) {
	fmt.Fprintln(w, "artifacts — read-only inventory of the evidence this directory holds. Nothing here is a statement that any file may be removed safely, and this command removes nothing.")
	for _, r := range s.Rules {
		fmt.Fprintf(w, "  rule: %s\n", r)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "scope (what this scan can see; anything outside it is unknown, not absent):")
	for _, sc := range s.Scope {
		fmt.Fprintf(w, "  · %s\n", sc)
	}
	fmt.Fprintln(w)

	if len(s.Groups) == 0 && len(s.OrphanTrees) == 0 && len(s.StrayLogs) == 0 {
		fmt.Fprintln(w, "journal evidence: none found within the scan scope (a journal written elsewhere, e.g. by a run with -output, is OUT OF VIEW: unknown).")
	}
	for _, g := range s.Groups {
		fmt.Fprintf(w, "journal %s — %s, last write %s\n", g.Journal, human.Bytes(g.Bytes), g.Modified)
		if g.Log != nil {
			if g.Log.Present {
				fmt.Fprintf(w, "  log %s — %s, last write %s\n", g.Log.Name, human.Bytes(g.Log.Bytes), g.Log.Modified)
			} else {
				fmt.Fprintf(w, "  log %s — not on disk (its fate is unknown: removed elsewhere? never assumed)\n", g.Log.Name)
			}
		}
		printWorkers(w, g.Workers)
		printRefs(w, g.Refs)
	}
	for _, g := range s.OrphanTrees {
		fmt.Fprintf(w, "fragment tree %s%s — main journal file NOT present here (the story may still be complete inside the fragments)\n", g.Journal, workersSuffix)
		printWorkers(w, g.Workers)
		printRefs(w, g.Refs)
	}
	for _, l := range s.StrayLogs {
		fmt.Fprintf(w, "loose log %s — %s, last write %s (no journal sibling found: it may predate a lost journal, or never be the tool's - unknown)\n",
			l.Name, human.Bytes(l.Bytes), l.Modified)
		printRefs(w, l.Refs)
	}

	fmt.Fprintf(w, "\npermanently-named singletons (one file each, read by every route that replays or reports):\n")
	for _, sg := range s.Singletons {
		if sg.Present {
			fmt.Fprintf(w, "  %s — %s, last write %s (%s)\n", sg.Name, human.Bytes(sg.Bytes), sg.Modified, sg.Note)
		} else {
			fmt.Fprintf(w, "  %s — not present (%s)\n", sg.Name, sg.Note)
		}
	}
	fmt.Fprintf(w, "  %s/ — %d manifest(s), %s (permanent history records: reading them never rewrites them)\n",
		historyDir, s.History.Manifests, human.Bytes(s.History.Bytes))
	if s.History.Unnamed > 0 {
		fmt.Fprintf(w, "  history lists %d child run(s) whose files the manifest never learned (unknown, never assumed)\n", s.History.Unnamed)
	}
	for _, w2 := range s.History.Warnings {
		fmt.Fprintln(w, "  notice: "+w2)
	}
	for _, d := range s.DeclaredAbsent {
		fmt.Fprintf(w, "  manifest %s lists a %s this directory no longer holds: %s (the reference stands; the report says MISSING)\n", d.Session, d.Kind, d.Path)
	}

	if len(s.Exports) > 0 {
		fmt.Fprintln(w, "\nexport bundles (sanitized copies made by 'bundle'; direct files only, subdirs not sized):")
		for _, e := range s.Exports {
			fmt.Fprintf(w, "  %s — %d file(s), %s direct, %d subdir(s) unscanned, last write %s\n",
				e.Dir, e.Files, human.Bytes(e.Bytes), e.Subdirs, e.Modified)
		}
	}

	if len(s.CoverageNotes) > 0 {
		fmt.Fprintln(w)
		for _, n := range s.CoverageNotes {
			fmt.Fprintln(w, "  notice: "+n)
		}
	}

	fmt.Fprintf(w, "\ntotals within this scan only: %d journal group(s), %d fragment tree(s) without a main journal, %d loose log(s), %d export(s) — %s observed.\n",
		s.Totals.Groups, s.Totals.OrphanTrees, s.Totals.StrayLogs, s.Totals.Exports, human.Bytes(s.Totals.ObservedBytes))
	fmt.Fprintln(w, "remember: "+s.UnrefRule)
}

func printWorkers(w io.Writer, t *workersTree) {
	if t == nil {
		return
	}
	fmt.Fprintf(w, "  workers tree %s — %d run dir(s), %d fragment(s), %s (fragments are journal content: ReadAll merges them; the tool never deletes them)\n",
		t.Dir, t.RunDirs, t.Fragments, human.Bytes(t.Bytes))
	if t.Unscanned > 0 {
		fmt.Fprintf(w, "  workers tree %s — %d entr(y/ies) below the two known levels: counted, NOT sized (out of scope)\n", t.Dir, t.Unscanned)
	}
}

func printRefs(w io.Writer, r artifactRefs) {
	if !r.any() {
		fmt.Fprintln(w, "  references: UNREFERENCED (see the rule at the end of this screen)")
		return
	}
	parts := make([]string, 0, 3)
	if len(r.Sessions) > 0 {
		parts = append(parts, fmt.Sprintf("history manifest(s): %s", strings.Join(r.Sessions, ", ")))
	}
	if r.LastResult {
		parts = append(parts, "lastresult.json")
	}
	if r.LastRun {
		parts = append(parts, "lastrun.json slot")
	}
	fmt.Fprintf(w, "  references: %s\n", strings.Join(parts, "; "))
}
