package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/tui"
)

// R24 (owner UX contract 2026-11-10): unfinished evidence is PRESENCE in
// the menu (one red item with a count), not a text wall on the greeting
// screen. This file is the inbox behind that item: the scan re-reads the
// durable evidence every time (the evidence itself is the authority),
// while findings.json stores ONLY the operator's read/dismissed flags -
// never a copy of the evidence, and nothing here may touch the journals,
// manifests or receipts on disk. Deleting an item removes the
// notification, never the story: 'journal <file>' / 'report' still render
// everything the evidence says.
//
// The honesty rules this inherits from R21 (unchanged): unfinished means
// INCOMPLETE with no cause named; OS shutdown records are time candidates
// only; absence of a record is never "clean"; the coverage bound of the
// fetched batch is always stated.

const findingsFileVersion = 1

// findingsPath is a var so tests can point it at a scratch file.
var findingsPath = "findings.json"

type findingFlag struct {
	ID   string `json:"id"`
	Read bool   `json:"read,omitempty"`
	// Deleted is a tombstone for a dismissed notification: the next scan
	// of the same evidence id stays dismissed instead of resurfacing.
	Deleted bool `json:"deleted,omitempty"`
}

type findingsFile struct {
	Version int           `json:"version"`
	Entries []findingFlag `json:"entries"`
}

// loadFindingFlags reads the flag store. A missing file is the clean
// default; a corrupt or foreign-version store is reported as an error and
// treated as empty - findings then RESURFACE, which is the safe direction
// (only operator flags are lost, never evidence).
func loadFindingFlags() (map[string]findingFlag, error) {
	b, err := os.ReadFile(findingsPath)
	if os.IsNotExist(err) {
		return map[string]findingFlag{}, nil
	}
	if err != nil {
		return map[string]findingFlag{}, err
	}
	var f findingsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return map[string]findingFlag{}, fmt.Errorf("%s is not readable: %w", findingsPath, err)
	}
	if f.Version != findingsFileVersion {
		return map[string]findingFlag{}, fmt.Errorf("%s has version %d (this build writes %d)", findingsPath, f.Version, findingsFileVersion)
	}
	flags := map[string]findingFlag{}
	for _, e := range f.Entries {
		flags[e.ID] = e
	}
	return flags, nil
}

// saveFindingFlags writes the flag store atomically (temp + Sync +
// rename), the same discipline as every other durable record here.
func saveFindingFlags(flags map[string]findingFlag) error {
	f := findingsFile{Version: findingsFileVersion}
	for _, e := range flags {
		f.Entries = append(f.Entries, e)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].ID < f.Entries[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir, base := filepath.Split(findingsPath)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, findingsPath)
}

// finding is one formatted inbox row plus the durable record behind it.
type finding struct {
	ID    string // stable evidence id: "attempt:<id>" / "session:<id>"
	When  time.Time
	Shape string // what the unfinished attempt was (never invented)
	Error string // the one honest finding line
	// detail builds the R21 witness story scoped to this record:
	manifest *sessionManifest
	receipt  *lastResult
}

// scanFindings re-reads the durable evidence and returns one row per
// unfinished record, newest first (evidence with no readable start sorts
// last - unknown, never assumed). Deleted flags are filtered by the
// callers that list/count; the scan itself is the full truth.
func scanFindings() []finding {
	ev := collectUnfinished()
	var out []finding
	if ev.pending != nil {
		f := finding{
			ID:    "attempt:" + ev.pending.AttemptID,
			Shape: "attempt receipt (pending)",
			Error: "started, never finalized - the record says it cannot tell whether that process still runs or ended without reporting (unknown, never assumed)",
		}
		if t, err := time.Parse(time.RFC3339Nano, ev.pending.StartedAt); err == nil {
			f.When = t
		}
		f.receipt = ev.pending
		out = append(out, f)
	}
	for _, m := range ev.running {
		m := m
		f := finding{
			ID:    "session:" + m.SessionID,
			Shape: findingShape(m),
			Error: fmt.Sprintf("state still %q - started %s, never finalized (history is never rewritten from here)", m.State, m.Started),
		}
		if t, err := time.Parse(time.RFC3339Nano, m.Started); err == nil {
			f.When = t
		}
		f.manifest = &m
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].When.Equal(out[j].When) {
			return out[i].ID < out[j].ID
		}
		return out[i].When.After(out[j].When)
	})
	return out
}

// findingShape names what the unfinished session was, from its own
// manifest (profile title when the manifest names a catalog profile,
// otherwise the recorded request shape). Never a guess: an unknown shape
// is stated as the bare kind.
func findingShape(m sessionManifest) string {
	if m.ProfileID != "" {
		if p, ok := catalog.Get(m.ProfileID); ok && p.Title != "" {
			return p.Title
		}
	}
	switch m.Kind {
	case "session":
		if m.Request != nil && m.Request.Session != nil {
			s := m.Request.Session
			mode := "all cores"
			if s.Mode == sessionPerCore {
				mode = "per core"
			}
			return fmt.Sprintf("session · %s · %s · %d core(s) · %s per pass", mode, s.Workload, len(s.PhysicalCores), human.Dur(s.Duration))
		}
	case "scenario":
		if m.Request != nil {
			return fmt.Sprintf("scenario · %d phase(s)", len(m.Request.Phases))
		}
	case "handoff":
		return "handoff · shared-memory block transfer"
	case "run", "plan":
		if m.Request != nil && len(m.Request.Phases) > 0 {
			return fmt.Sprintf("%s · %d phase(s)", m.Kind, len(m.Request.Phases))
		}
	}
	return m.Kind
}

// findingsUnreadCount is the menu badge source: unfinished evidence whose
// flag is not read and not deleted. Zero System-log queries, local reads
// only - it runs on every menu frame.
func findingsUnreadCount() int {
	items := scanFindings()
	flags, _ := loadFindingFlags()
	n := 0
	for _, f := range items {
		if fl := flags[f.ID]; !fl.Read && !fl.Deleted {
			n++
		}
	}
	return n
}

// findingsMenuItem is the top-menu row (R24): always present, count in
// the TEXT (never color alone), red paint above zero.
func findingsMenuItem(ui *screenUI) string {
	n := findingsUnreadCount()
	if n > 0 {
		return ui.paint(tui.FGBrightRed, fmt.Sprintf("  7) View findings - %d unread unfinished attempt(s)", n))
	}
	return "  7) View findings - none unread"
}

func setFindingFlag(id string, read, deleted bool) {
	flags, _ := loadFindingFlags()
	flags[id] = findingFlag{ID: id, Read: read, Deleted: deleted}
	if err := saveFindingFlags(flags); err != nil {
		fmt.Fprintf(os.Stderr, "findings flag not saved: %v\n", err)
	}
}

func markAllFindingsRead() {
	flags, _ := loadFindingFlags()
	changed := false
	for _, f := range scanFindings() {
		if fl := flags[f.ID]; fl.Deleted {
			continue
		}
		flags[f.ID] = findingFlag{ID: f.ID, Read: true}
		changed = true
	}
	if !changed {
		return
	}
	if err := saveFindingFlags(flags); err != nil {
		fmt.Fprintf(os.Stderr, "findings flag not saved: %v\n", err)
	}
}

// findingWhen renders the row's date-time exactly once, UTC (the same
// clock every durable record here uses).
func findingWhen(t time.Time) string {
	if t.IsZero() {
		return "time unknown"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// findingDetail scopes the R21 witness machinery to ONE record: the same
// pinned honest lines (journal recovery, unreadable files named, the
// crashed-core story, OS shutdown candidates with the coverage bound) -
// just for this finding instead of the whole greeting block. Reading a
// finding changes nothing on disk, exactly like the witness itself.
func findingDetail(f finding) []string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	add("finding: " + f.ID)
	add("  started: " + findingWhen(f.When))
	add("  test: " + f.Shape)
	add("  finding: " + f.Error)
	ev := unfinishedEvidence{}
	if f.receipt != nil {
		ev.pending = f.receipt
		ev.earliest = f.When
	}
	if f.manifest != nil {
		ev.running = append(ev.running, *f.manifest)
		ev.earliest = f.When
	}
	ev.readJournals(add)
	ev.osWitness(add)
	return lines
}

// findingsMenu is the inbox screen: formatted rows, newest first,
// "date-time · test type · finding" per the owner's contract; per finding
// the actions are exactly read / delete(notification only) / back, plus
// a whole-list mark-read. The scan re-runs on every frame: the durable
// evidence, not the flag file, is the authority.
func findingsMenu(in *bufio.Reader, ui *screenUI) int {
	note := ""
	for {
		items := scanFindings()
		flags, ferr := loadFindingFlags()
		if ferr != nil {
			note = fmt.Sprintf("%v - flags reset: previously read/dismissed findings are shown again (the evidence itself was never touched)", ferr)
		}
		var shown []finding
		for _, f := range items {
			if fl := flags[f.ID]; fl.Deleted {
				continue
			}
			shown = append(shown, f)
		}
		lines := []string{"--- Findings - unfinished attempts and their evidence (newest first) ---"}
		if len(shown) == 0 {
			if len(items) == 0 {
				lines = append(lines, "  no unfinished attempt evidence in this directory")
			} else {
				// Dismissed notifications hide themselves; saying "no
				// evidence" here would be false - the evidence is still
				// exactly where it was.
				lines = append(lines, "  all findings are dismissed - the evidence on disk was never touched: 'journal <file>' and 'report' still render the full story")
			}
			lines = append(lines, "  (deleted rows dismiss only the notification: journals, manifests and receipts on disk are never rewritten)")
		}
		unread := 0
		for i, f := range shown {
			mark := ""
			if fl := flags[f.ID]; !fl.Read {
				mark = "  [new]"
				unread++
			}
			lines = append(lines, fmt.Sprintf("  %d) %s · %s · %s%s", i+1, findingWhen(f.When), f.Shape, f.Error, mark))
		}
		lines = append(lines, "", fmt.Sprintf("  a) mark all as read (%d unread)   q) back", unread))
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Findings: number to open, a = all read, q = back: ")

		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			return 0
		}
		switch {
		case choice == "" || choice == "q" || choice == "0":
			return 0
		case choice == "a":
			markAllFindingsRead()
			note = "All current findings marked read."
			continue
		}
		n, err := strconv.Atoi(choice)
		if err != nil || n < 1 || n > len(shown) {
			note = "Enter a listed number, a, or q."
			continue
		}
		if code, dismissed := findingDetailMenu(in, ui, shown[n-1]); code != 0 {
			return code
		} else if dismissed {
			note = "Dismissed: this notification is gone; the evidence stays on disk untouched."
		}
	}
}

// findingDetailMenu shows one finding and its two state actions. Read
// and delete are flags on the NOTIFICATION; the detail body itself is
// the read-only R21 story for this record. Returns (exit code,
// dismissed): a dismissal note is stated by the LIST screen, not a
// screen that is about to close.
func findingDetailMenu(in *bufio.Reader, ui *screenUI, f finding) (int, bool) {
	note := ""
	for {
		lines := findingDetail(f)
		lines = append(lines,
			"",
			"  (reading changed nothing: these records were only read; 'journal <file>' and 'report' render the full story from them)",
			"",
			"  r) mark as read   d) dismiss this notification (the on-disk evidence stays exactly as it is)   q) back to the list")
		if note != "" {
			lines = append(lines, "", note)
			note = ""
		}
		ui.screenPrompt(lines, "Action [q]: ")
		line, readErr := in.ReadString('\n')
		choice := strings.ToLower(strings.TrimSpace(line))
		if readErr != nil && choice == "" {
			return 0, false
		}
		switch choice {
		case "r":
			setFindingFlag(f.ID, true, false)
			note = "Marked read."
		case "d":
			setFindingFlag(f.ID, false, true)
			return 0, true
		default: // "", q, 0 or anything else: back
			return 0, false
		}
	}
}
