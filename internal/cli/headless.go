package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/weltercode/cpu-stability-checker/internal/catalog"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// G01 (menu plan section 9): the headless interface a future GUI
// drives as a process protocol. The request contract reuses the
// saved-recipe shape - the one shape this tool already proves it can
// replay faithfully - so run, repeat and GUI input can never drift
// apart on what a complete request means.

// requestSchemaVersion names the request envelope this build speaks.
// It travels as the recipe's own version field: one number, one
// truth about the shape.
const requestSchemaVersion = lastRunVersion

// loadRequest reads one request file for the headless contract.
// Three refusals happen before anything could load: the path must
// be absolute (a relative one would make the run depend on an
// undocumented working directory), the JSON is decoded strictly (an
// unknown field is a refusal, not a shrug - a newer caller must not
// be silently misunderstood), and the shape must carry everything a
// faithful replay needs.
func loadRequest(path string) (*lastRun, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("request path %q is not absolute: the headless contract never depends on a working directory", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read request: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var lr lastRun
	if err := dec.Decode(&lr); err != nil {
		return nil, fmt.Errorf("request is not readable: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("request has trailing data after the JSON object")
	}
	if err := validateRecipe(&lr); err != nil {
		return nil, fmt.Errorf("request %s", err.Error())
	}
	return &lr, nil
}

// requestProfileGate refuses a request naming a catalog profile this
// build cannot honor, with the card's own reason - before any load.
// Unknown availability refuses too: "cannot tell" is never presented
// as "ready".
func requestProfileGate(lr *lastRun) error {
	if lr.ProfileID == "" {
		return nil // no card claimed: nothing to gate
	}
	p, ok := catalog.Get(lr.ProfileID)
	if !ok {
		return fmt.Errorf("request names unknown profile %q", lr.ProfileID)
	}
	if p.Availability != catalog.Available {
		if p.Reason != "" {
			return fmt.Errorf("profile %q is %s: %s", lr.ProfileID, p.Availability, p.Reason)
		}
		return fmt.Errorf("profile %q is %s", lr.ProfileID, p.Availability)
	}
	return nil
}

// headlessRunRequest is the request-driven run entry. With
// -events jsonl it runs in managed mode (G01b): the real stdout is
// captured for the JSONL event stream, os.Stdout is swapped to
// stderr so every human line flows there automatically, and the
// durable result file lands before the completed event. Without
// -events the human output stays as usual. Either way the
// execution IS the repeat pipeline: the request becomes the replay
// slot and runs through exactly the code a menu repeat uses, so a
// headless run cannot drift from an interactive one - same gates,
// same recipe discipline, same session manifest.
func headlessRunRequest(path, eventsMode, outputDir string) int {
	if eventsMode != "" && eventsMode != "jsonl" {
		fmt.Fprintf(os.Stderr, "unknown -events mode %q: this build streams 'jsonl' only\n", eventsMode)
		return 2
	}
	if eventsMode == "jsonl" {
		// One run session at a time (section 9 v1 policy): the
		// refusal arrives before anything loads, naming the live
		// holder. OS-backed ownership (B08): the handle is held for
		// the whole operation, and the load choke point joins the
		// same operation instead of stacking a second lock.
		release, lerr := acquireRunLockShared()
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "run refused: %v\n", lerr)
			return 2
		}
		defer release()
	}
	lr, err := loadRequest(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request refused: %v\n", err)
		return 2
	}
	if err := requestProfileGate(lr); err != nil {
		fmt.Fprintf(os.Stderr, "request refused: %v\n", err)
		return 2
	}
	if lr.RecipeID == "" {
		// A fresh request gets a fresh identity, like every other
		// entry point; a request carrying its id replays under it.
		lr.RecipeID = newRecipeID()
	}
	if err := saveLastRun(*lr); err != nil {
		noteSaveFailure(err)
	}
	if eventsMode == "jsonl" {
		real := os.Stdout
		os.Stdout = os.Stderr // every human line now flows to stderr; the sink owns the real stream
		currentEvents = newEventSink(real)
		stopBeat := startHeartbeat()
		stopReader := startCommandReader()
		defer func() {
			stopBeat()
			// stop() closes the session end to end: late stdin commands
			// get an honest "run finished" (never a silent apply), and
			// the cancels this session's own control channel produced
			// clear with it - they must never arm the process-wide
			// pending slot for the next unrelated run (audit A01).
			stopReader()
			// Durable first (section 9): the result file is on disk
			// before the completed record hits the stream, so a GUI
			// that never reads a line still finds the answer.
			writeResultAndComplete(outputDir)
			currentEvents.close()
			currentEvents = nil
			os.Stdout = real
		}()
	}
	// R03a (audit B06): execute the request loaded and gated above,
	// NOT a re-read of the repeat slot. If the save failed the slot
	// may hold an older recipe - executing it would run a different
	// experiment than the caller asked for. The slot is only the
	// durable promise that the NEXT crash can be repeated.
	return runLastRun(lr, nil)
}

// writeResultAndComplete reports the session's final truth to the
// stream: read back from the manifest (the durable record), never
// from memory that a crash could have skipped. A session that never
// got an identity gets an honest error record instead of a fake
// completed.
func writeResultAndComplete(outputDir string) {
	s := currentEvents
	s.mu.Lock()
	sessionID := s.session
	s.mu.Unlock()
	if sessionID == "" {
		s.emit("error", "", errorPayload{
			Message: "the session never started: no completed result exists to report",
		}, false)
		return
	}
	m, err := loadSessionManifest(sessionID)
	if err != nil {
		s.emit("error", "", errorPayload{
			Message: fmt.Sprintf("session manifest unreadable: %v", err),
		}, false)
		return
	}
	var files []resultRun
	for _, r := range m.Runs {
		jp := r.Journal
		if jp == "" && r.Log != "" {
			jp = journal.Path(r.Log)
		}
		_, serr := os.Stat(jp)
		files = append(files, resultRun{Name: r.Name, Log: r.Log, Journal: jp, Exists: serr == nil})
	}
	var planHashes []string
	if r, rerr := loadLastResult(); rerr == nil && m.RecipeID != "" && r.RecipeID == m.RecipeID {
		planHashes = r.PlanHashes
	}
	dir := outputDir
	if dir == "" {
		dir = "."
	}
	if merr := os.MkdirAll(dir, 0o755); merr != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot create -output dir %q: %v\n", dir, merr)
	}
	resultPath := filepath.Join(dir, "result-"+sessionID+".json")
	payload := resultPayload{
		SessionID:     sessionID,
		Kind:          m.Kind,
		State:         m.State,
		Started:       m.Started,
		Finished:      m.Finished, // "" = never finalized: unknown, never invented
		Exit:          m.Exit,     // null = never ended observably
		Validation:    m.Validation,
		RecipeID:      m.RecipeID,
		ProfileID:     m.ProfileID,
		PlanHashes:    planHashes,
		Runs:          files,
		DroppedEvents: s.dropped.Load(), // honest count of what a slow reader missed
		ResultFile:    resultPath,
	}
	if aerr := atomicWriteJSON(resultPath, payload); aerr != nil {
		fmt.Fprintf(os.Stderr, "warning: result file not written: %v\n", aerr)
		payload.ResultFileError = aerr.Error()
	}
	s.emit("completed", "", payload, false)
}

// resultPayload is the durable answer of a managed run: the same
// struct lands in the result file and in the completed record, so
// the GUI reads one schema from both places. Core keys never omit:
// "" and null are the honest unknowns.
type resultPayload struct {
	SessionID  string   `json:"session_id"`
	Kind       string   `json:"kind"`
	State      string   `json:"state"`
	Started    string   `json:"started"`
	Finished   string   `json:"finished"`
	Exit       *int     `json:"exit"`
	Validation string   `json:"validation"`
	RecipeID   string   `json:"recipe_id"`
	ProfileID  string   `json:"profile_id"`
	PlanHashes []string `json:"plan_hashes"`
	// Runs lists the child runs with their files; a listed file
	// that went missing says exists=false, never substitutes.
	Runs          []resultRun `json:"runs"`
	DroppedEvents int64       `json:"dropped_events"`
	ResultFile    string      `json:"result_file"`
	// ResultFileError stays empty unless the durable write itself
	// failed - the one case where the stream knows the file is not
	// there to find.
	ResultFileError string `json:"result_file_error"`
}

type resultRun struct {
	Name    string `json:"name"`
	Log     string `json:"log"`
	Journal string `json:"journal"`
	Exists  bool   `json:"exists"`
}

// atomicWriteJSON writes with the discipline every durable artifact
// of this tool uses: temp file, Sync, replace-rename - a crash
// leaves either the old file, the new file, or an uncounted temp.
func atomicWriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir, base := filepath.Split(path)
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
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// validateCmd answers "would this request run here, right now?"
// without starting any load: the same gates the run itself applies,
// in the same order - strict request shape, profile availability,
// external tools, the compile pipeline, resource refusals. Expected
// request errors arrive in the structured answer as well as on
// stderr; the exit code stays a secondary signal.
func validateCmd(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	request := fs.String("request", "", "absolute path of the request file to validate")
	asJSON := fs.Bool("json", false, "structured answer on stdout")
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "validate takes no positional arguments")
		return 2
	}
	if *request == "" {
		fmt.Fprintln(os.Stderr, "validate needs -request <absolute path>")
		return 2
	}

	type answer struct {
		SchemaVersion int      `json:"schema_version"`
		OK            bool     `json:"ok"`
		Kind          string   `json:"kind"` // "" = the request never got readable
		Runs          int      `json:"runs"` // 0 = nothing resolved
		PlanHashes    []string `json:"plan_hashes"`
		Errors        []string `json:"errors"` // [] = accepted; every refusal is named here
	}
	ans := answer{SchemaVersion: requestSchemaVersion}
	fail := func(format string, a ...any) {
		ans.Errors = append(ans.Errors, fmt.Sprintf(format, a...))
	}

	lr, err := loadRequest(*request)
	if err != nil {
		fail("%v", err)
	} else {
		ans.Kind = lr.Kind
		if err := requestProfileGate(lr); err != nil {
			fail("%v", err)
		}
		var hashes []string
		if lr.Kind == lastRunSession {
			if msg := externalToolMissing(lr.Config.External, lr.Session.Workload); msg != "" {
				fail("%s", msg)
			}
			topology, terr := sysinfo.DiscoverTopology()
			if terr != nil {
				fail("cannot discover CPU topology: %v", terr)
			} else if runs, berr := buildSessionRuns(*lr.Session, topology); berr != nil {
				fail("%v", berr)
			} else {
				ans.Runs = len(runs)
				for i, run := range runs {
					opt, oerr := sessionRunOptions(*lr.Session, run, sessionLogFile(lr.Session.LogFile, "validate", run))
					if oerr != nil {
						fail("run %d: %v", i+1, oerr)
						continue
					}
					hashes = append(hashes, planHash(*lr, opt))
				}
			}
		} else if lr.Kind == lastRunHandoff {
			// S6: the manifest loaded complete (strict reader) AND
			// its run mode is open. validate resolves the same
			// options the tracked run will execute (same builder, so
			// the plan hash the GUI gets is the hash the run will
			// journal) and admits the shared section against today's
			// memory - offering a Run button that could only refuse
			// later is exactly what this gate exists to prevent.
			// Core existence and llc provenance stay a NAMED refusal
			// at run time (the drift gate): a request replaying onto
			// another machine is not malformed, it is a different
			// machine.
			hopt := handoffRunOptions(*lr.Config, lr.Handoff)
			if aerr := plan.HandoffAdmission(hopt); aerr != nil {
				fail("%v", aerr)
			}
			ans.Runs = 1
			hashes = append(hashes, planHash(*lr, hopt))
		} else {
			cfg := *lr.Config
			cfg.Phases = lr.Phases
			opts, _, cerr := compileScenario(cfg)
			if cerr != nil {
				fail("%v", cerr)
			} else {
				for _, e := range scenarioMemoryRefusals(cfg.External, opts) {
					fail("%s", e)
				}
				cycles := max(lr.Cycles, 1)
				ans.Runs = len(opts) * cycles
				for _, opt := range opts {
					hashes = append(hashes, planHash(*lr, opt))
				}
			}
		}
		ans.PlanHashes = hashes
	}
	ans.OK = len(ans.Errors) == 0

	if *asJSON {
		out, merr := json.MarshalIndent(ans, "", "  ")
		if merr != nil {
			fmt.Fprintf(os.Stderr, "cannot encode answer: %v\n", merr)
			return 2
		}
		fmt.Println(string(out))
	} else {
		if ans.OK {
			fmt.Printf("request is valid: %s, %d run(s).\n", ans.Kind, ans.Runs)
		} else {
			for _, e := range ans.Errors {
				fmt.Printf("refused: %s\n", e)
			}
		}
	}
	if !ans.OK {
		return 2
	}
	return 0
}
