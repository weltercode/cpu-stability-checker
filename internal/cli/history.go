package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// U02 (menu plan section 6): every user session leaves a manifest -
// unique id, what was actually requested, when it started and ended,
// how it ended, and which logs/journals its child runs wrote. The
// history screen reads these manifests (scanning the directory IS
// the index: it can never go stale and is rebuilt automatically if
// files are added or removed), never the other way around: reading
// history never rewrites a manifest and never deletes a file.
//
// The manifest is deliberately separate from the two C3 artifacts:
// lastrun.json stays the single replay slot, lastresult.json stays
// the disposable "how did the LAST attempt end" receipt. A manifest
// is the permanent per-session history record; they share
// observations (ids, states) but not storage authority.

// historyDir holds one manifest JSON per session, named by the
// session id. Like lastrun.json/lastresult.json it lives in the
// working directory; it is a runtime artifact, not user config.
const historyDir = "history"

// manifestVersion versions the manifest shape. A newer file than
// this build is reported honestly (its fields are unknown, not
// guessed); an older accepted shape would be migrated explicitly,
// never silently reinterpreted.
const manifestVersion = 1

// Manifest states reuse the execution vocabulary of the C3 receipt
// (plan.Exec*) plus "running": a manifest still saying "running"
// after a crash is read as unfinished/unknown WITHOUT rewriting it
// and without inventing a finish time - the same pending semantics
// that make the receipt honest about power loss.
const manifestRunning = "running"

// sessionManifest is one session's history record.
type sessionManifest struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	// Kind names the entry point that ran: run | plan | session |
	// scenario. It is provenance for the history list, not a
	// behavior switch.
	Kind           string `json:"kind"`
	RecipeID       string `json:"recipe_id,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	ProfileVersion string `json:"profile_version,omitempty"`
	// Started/Finished are UTC RFC3339. Finished stays empty while
	// the session runs and forever after a crash: an invented finish
	// time would be a lie stamped into permanent history.
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
	// State is manifestRunning until the session finalizes; then it
	// carries the same execution vocabulary the receipt uses
	// (completed / runtime_error / setup_error / interrupted /
	// cancelled).
	State string `json:"state"`
	// Exit and Validation stay absent (nil / "") until the session
	// actually ends: a crashed session has no exit code, and the
	// history screen must show "unknown", not zero.
	Exit       *int   `json:"exit,omitempty"`
	Validation string `json:"validation,omitempty"`
	// Request is the full replay request (the same recipe shape
	// lastrun.json holds): reachability alone is not
	// reproducibility, so a session in history carries what it ran.
	Request *lastRun `json:"request,omitempty"`
	// Runs lists the child runs this one session produced, with the
	// log and journal each wrote. A listed file that later goes
	// missing stays listed: the report says "missing", never
	// substitutes another file.
	Runs []manifestRun `json:"runs,omitempty"`
}

type manifestRun struct {
	Name    string `json:"name,omitempty"` // phase or pass label
	Log     string `json:"log,omitempty"`
	Journal string `json:"journal,omitempty"`
}

// newSessionID mints the session identity: UTC second plus 8 random
// hex bytes, so two sessions started in the same second are still
// distinct (menu plan section 11). Unlike newRecipeID - where a
// random failure only weakens a binding - a session id names files:
// a failure is returned and the caller must refuse BEFORE any load,
// never fall back to a bare timestamp that could silently merge two
// sessions into one history record.
func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cannot mint a unique session id: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// manifestPath is the file for one session id.
func manifestPath(sessionID string) string {
	return filepath.Join(historyDir, sessionID+".json")
}

// saveSessionManifest writes (or finalizes) one session's manifest
// atomically: temp file in the history directory, Sync, rename. A
// crash at any point leaves either the old file, the new file, or a
// temp file no reader counts - never a half-written manifest.
//
// Collision protection: if the target file parses and belongs to a
// DIFFERENT session id, the write is refused - one id, one session.
// Rewriting the same id is the finalize path (running -> final),
// which is exactly what the lifecycle needs.
func saveSessionManifest(m *sessionManifest) error {
	if m.SessionID == "" {
		return fmt.Errorf("manifest without a session id: refusing to write an anonymous history record")
	}
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", historyDir, err)
	}
	final := manifestPath(m.SessionID)
	if old, err := loadSessionManifest(m.SessionID); err == nil && old.SessionID != m.SessionID {
		// Unreachable while the filename is the id, but the check
		// states the invariant the lifecycle relies on.
		return fmt.Errorf("%s already holds session %q: refusing to overwrite another session's history", final, old.SessionID)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode manifest: %w", err)
	}
	tmp, err := os.CreateTemp(historyDir, m.SessionID+".json.tmp*")
	if err != nil {
		return fmt.Errorf("cannot create temp manifest: %w", err)
	}
	name := tmp.Name()
	defer func() {
		if name != "" {
			os.Remove(name) // failed write leaves nothing countable
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot write manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("cannot sync manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close manifest: %w", err)
	}
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("cannot save manifest %s: %w", final, err)
	}
	name = ""
	return nil
}

// loadSessionManifest reads one manifest by id. A missing file, a
// corrupt file and a future-version file each return a distinct
// error the history screen can state honestly.
func loadSessionManifest(sessionID string) (sessionManifest, error) {
	var m sessionManifest
	b, err := os.ReadFile(manifestPath(sessionID))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s is not readable: %w", manifestPath(sessionID), err)
	}
	if m.Version > manifestVersion {
		return m, fmt.Errorf("%s was written by a newer tool (manifest version %d)", manifestPath(sessionID), m.Version)
	}
	if m.Version < manifestVersion {
		// No older shape exists yet; when one is added it gets an
		// explicit limited reader here, never silent reinterpretation.
		return m, fmt.Errorf("%s is manifest version %d, which this build cannot read faithfully", manifestPath(sessionID), m.Version)
	}
	return m, nil
}

// listSessionManifests scans the history directory and returns every
// readable manifest newest-first (the id starts with a UTC second,
// so id order IS time order), plus one warning per unreadable file.
// A corrupt manifest never breaks the list: it is reported and the
// rest of history stays available. The scan never writes or deletes
// anything - reading history is read-only by contract.
func listSessionManifests() ([]sessionManifest, []string) {
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no history yet is not an error
		}
		return nil, []string{fmt.Sprintf("cannot read %s: %v", historyDir, err)}
	}
	var out []sessionManifest
	var warns []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue // temp files and strays are not manifests
		}
		id := strings.TrimSuffix(name, ".json")
		m, err := loadSessionManifest(id)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", filepath.Join(historyDir, name), err))
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID > out[j].SessionID })
	return out, warns
}

// sessionTracker owns one session's manifest across its lifetime:
// created running when the session starts, finalized when it ends.
// Like the journal and the outcome receipt, a manifest failure never
// breaks the run itself: the warning says history will miss this
// session and the run continues unaffected.
type sessionTracker struct {
	m sessionManifest
}

// newSessionTracker mints the session id and writes the running
// manifest BEFORE anything loads. Unlike the recipe id - where a
// random-source failure only weakens a binding - a session id names
// files and stamps journals: on failure the caller must refuse with
// a setup error, never fall back to a bare timestamp that could
// silently merge two sessions into one history record.
func newSessionTracker(kind string, lr *lastRun, runs []manifestRun) (*sessionTracker, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	t := &sessionTracker{m: sessionManifest{
		Version:   manifestVersion,
		SessionID: id,
		Kind:      kind,
		Started:   time.Now().UTC().Format(time.RFC3339),
		State:     manifestRunning,
		Runs:      runs,
	}}
	// The managed event stream names the session on every record
	// from this moment on (nil-safe: unmanaged runs pay nothing).
	if currentEvents != nil {
		currentEvents.setSession(id)
	}
	if lr != nil {
		t.m.RecipeID, t.m.ProfileID, t.m.ProfileVersion = lr.RecipeID, lr.ProfileID, lr.ProfileVersion
		t.m.Request = lr
	}
	t.save("started")
	return t, nil
}

// setRuns fills in the child run list once the entry point knows
// the actual log files (sessions build them after the tracker
// starts). Rewriting the same session id is the finalize path, not
// a collision.
func (t *sessionTracker) setRuns(runs []manifestRun) {
	if t == nil {
		return
	}
	t.m.Runs = runs
	t.save("updated")
}

// finish closes the session with the worst execution observed over
// its child runs (the same severity vocabulary as the receipt).
// Results that never revealed how they ended - transition runs,
// whose verdict lives in the journal - leave state "unknown": the
// exit code is recorded, the manifest does not guess the rest.
func (t *sessionTracker) finish(exit int, results []plan.Result) {
	if t == nil {
		return
	}
	exec, val, _, _ := worstOutcome(results)
	if exec == "" {
		exec = "unknown"
	}
	t.m.State, t.m.Validation = exec, val
	t.finishRecord(exit)
}

// finishSetup closes a session that died before any run executed:
// setup_error is the one label that never blames hardware for a
// broken setup.
func (t *sessionTracker) finishSetup(exit int) {
	if t == nil {
		return
	}
	t.m.State = string(plan.ExecSetupError)
	t.finishRecord(exit)
}

func (t *sessionTracker) finishRecord(exit int) {
	t.m.Finished = time.Now().UTC().Format(time.RFC3339)
	t.m.Exit = &exit
	t.save("finalized")
}

func (t *sessionTracker) save(what string) {
	if t == nil {
		return
	}
	if err := saveSessionManifest(&t.m); err != nil {
		fmt.Fprintf(os.Stderr, "warning: session manifest not %s (%v); the run result itself is unaffected\n", what, err)
	}
}
