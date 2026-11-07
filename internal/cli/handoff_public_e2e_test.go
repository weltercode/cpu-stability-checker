package cli

// S6 end-to-end: the PUBLIC handoff run mode on the real executable,
// in a sandboxed working directory. (a) The bare 'handoff' command's
// public form is now a tracked attempt: its journal carries
// run_meta identity, the history manifest and the outcome receipt
// exist and bind to the saved recipe. (b) A kind=handoff request
// file runs end-to-end under -events jsonl with the full managed
// lifecycle (resolved/started/phase_started, the lease steps as
// handoff_lease records, phase_finished, completed) and the durable
// result file. validate and the run agree on one plan hash - and
// they agree even though the request names a DIFFERENT journal file:
// the journal path is transport, excluded from the experiment's
// identity, exactly like LogFile is in planHash.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func TestE2EPublicHandoffRun(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real two-process handoff")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()

	// (a) the bare public form: no -helper, no request file.
	run := exec.Command(exe, "handoff", "-cores", "0,1", "-blocks", "4", "-block-size", "4KiB",
		"-rounds", "1", "-seed", "11", "-journal", "hpub.log")
	run.Dir = cwd
	var out, outErr strings.Builder
	run.Stdout, run.Stderr = &out, &outErr
	if err := run.Run(); err != nil {
		t.Fatalf("public handoff exit: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), outErr.String())
	}
	if !strings.Contains(out.String(), "durable manifest saved") {
		t.Errorf("the public run did not state its durable manifest:\n%s", out.String())
	}

	jpath := journal.Path(filepath.Join(cwd, "hpub.log"))
	recs, iss := journal.ReadAll(jpath)
	if iss.ReadErr != "" {
		t.Fatalf("journal %s: %s", jpath, iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("journaled runs = %d, want 1", len(reports))
	}
	rep := reports[0]
	if rep.Test != "handoff" || !rep.Ended || rep.Exit != 0 || rep.Validation != "verified" {
		t.Errorf("journaled outcome wrong: test=%q ended=%v exit=%d validation=%q", rep.Test, rep.Ended, rep.Exit, rep.Validation)
	}
	// The tracked difference from helper-era generations: run_meta
	// names the session and the plan hash INSIDE the journal.
	if rep.Meta == nil || rep.Meta.SessionID == "" || len(rep.Meta.PlanHash) != 64 {
		t.Fatalf("the public run's journal lacks run_meta identity: %+v", rep.Meta)
	}
	var leases, acked int
	for _, c := range rep.Cores {
		leases += len(c.LeaseObs)
		for _, o := range c.LeaseObs {
			if o.Acked {
				acked++
			}
		}
	}
	if leases != 8 || acked != 8 {
		t.Errorf("lease records = %d (acked %d), want 8/8", leases, acked)
	}

	// The durable manifest the run saved before load.
	lrb, err := os.ReadFile(filepath.Join(cwd, "lastrun.json"))
	if err != nil {
		t.Fatalf("no lastrun.json after the public run: %v", err)
	}
	var lr lastRun
	if err := json.Unmarshal(lrb, &lr); err != nil {
		t.Fatalf("lastrun.json unreadable: %v", err)
	}
	if lr.Kind != lastRunHandoff || lr.Handoff == nil || lr.RecipeID == "" {
		t.Fatalf("manifest kind/recipe wrong: kind=%q recipe=%q", lr.Kind, lr.RecipeID)
	}

	// The session the journal names really recorded itself in this
	// run's own directory, kind "handoff" and bound to the recipe.
	mb, err := os.ReadFile(filepath.Join(cwd, "history", rep.Meta.SessionID+".json"))
	if err != nil {
		t.Fatalf("no session manifest for %q: %v", rep.Meta.SessionID, err)
	}
	var man sessionManifest
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatalf("session manifest unreadable: %v", err)
	}
	if man.Kind != "handoff" || man.State != "completed" || man.RecipeID != lr.RecipeID {
		t.Errorf("session manifest wrong: kind=%q state=%q recipe=%q", man.Kind, man.State, man.RecipeID)
	}
	if man.Exit == nil || *man.Exit != 0 {
		t.Errorf("session manifest exit = %v, want 0", man.Exit)
	}
	if len(man.Runs) != 1 || filepath.Base(man.Runs[0].Journal) != filepath.Base(jpath) || man.Runs[0].Log != "" {
		t.Errorf("session manifest run list wrong (no invented .log): %+v", man.Runs)
	}

	// The outcome receipt binds to the recipe and carries the SAME
	// plan hash the journal's run_meta states - receipt, manifest and
	// journal tell one experiment, not three guesses.
	rb, err := os.ReadFile(filepath.Join(cwd, "lastresult.json"))
	if err != nil {
		t.Fatalf("no lastresult.json after the public run: %v", err)
	}
	var rec lastResult
	if err := json.Unmarshal(rb, &rec); err != nil {
		t.Fatalf("lastresult.json unreadable: %v", err)
	}
	if rec.RecipeID != lr.RecipeID || rec.State != receiptFinal || rec.RunsFinished != 1 ||
		rec.Exit == nil || *rec.Exit != 0 {
		t.Errorf("receipt binding wrong: %+v", rec)
	}
	if len(rec.PlanHashes) != 1 || rec.PlanHashes[0] != rep.Meta.PlanHash {
		t.Errorf("receipt hash %v does not equal the journal run_meta hash %q", rec.PlanHashes, rep.Meta.PlanHash)
	}

	// (b) the managed replay: the saved manifest becomes the request,
	// pointed at a DIFFERENT journal file (transport is not identity).
	req := lr
	req.Handoff.Journal = "hpub2.log.journal.jsonl"
	reqPath := filepath.Join(cwd, "req.json")
	abs, err := filepath.Abs(reqPath)
	if err != nil {
		t.Fatal(err)
	}
	qb, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reqPath, qb, 0o644); err != nil {
		t.Fatal(err)
	}

	// validate (the GUI gate) accepts the kind and resolves the
	// run's hash - the same hash (a) journalized, although the
	// request names another journal file.
	vo, err := exec.Command(exe, "validate", "-request", abs, "-json").Output()
	if err != nil {
		t.Fatalf("validate -request (exit err): %v\n%s", err, vo)
	}
	var ans struct {
		OK         bool     `json:"ok"`
		Kind       string   `json:"kind"`
		Runs       int      `json:"runs"`
		PlanHashes []string `json:"plan_hashes"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal(vo, &ans); err != nil {
		t.Fatalf("validate -json output: %v\n%s", err, vo)
	}
	if !ans.OK || ans.Kind != lastRunHandoff || ans.Runs != 1 || len(ans.PlanHashes) != 1 {
		t.Fatalf("validate answer wrong: %+v", ans)
	}
	if ans.PlanHashes[0] != rep.Meta.PlanHash {
		t.Errorf("validate hash != journaled run_meta hash (the journal file must not move the identity):\nvalidate %s\njournal  %s", ans.PlanHashes[0], rep.Meta.PlanHash)
	}

	// The request-driven managed run, with the event stream live.
	// The control pipe (stdin) is held OPEN like a real GUI holds it:
	// closing it is itself a cancel by the G01c contract ("a GUI that
	// vanished is indistinguishable from a GUI that closed the
	// pipe"), and a vanished GUI is not what this case is testing.
	// An OS pipe (not io.Pipe): exec hands the read handle straight
	// to the child, so no relay goroutine could make Run wait on a
	// writer the test never closes.
	mr := exec.Command(exe, "run", "-request", abs, "-events", "jsonl")
	mr.Dir = cwd
	pipeR, pipeW, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	mr.Stdin = pipeR
	var stream, mrErr strings.Builder
	mr.Stdout, mr.Stderr = &stream, &mrErr
	if err := mr.Run(); err != nil {
		t.Fatalf("run -request exit: %v\nstderr:\n%s", err, mrErr.String())
	}
	s := stream.String()
	if strings.Contains(s, "cancelling") {
		t.Errorf("an open control pipe must not read as a vanished GUI:\n%s", s)
	}
	for _, want := range []string{"resolved", "started", "phase_started", "handoff_lease", "phase_finished", "completed"} {
		if !strings.Contains(s, `"`+want+`"`) {
			t.Errorf("managed stream lacks %q:\n%s", want, s)
		}
	}
	if n := strings.Count(s, `"handoff_lease"`); n < 8 {
		t.Errorf("handoff_lease records = %d, want at least 8", n)
	}
	if iStart, iLease := strings.Index(s, `"phase_started"`), strings.Index(s, `"handoff_lease"`); iLease < iStart {
		t.Errorf("lease records reached the stream before the phase started")
	}
	if iDone, iFin := strings.Index(s, `"completed"`), strings.Index(s, `"phase_finished"`); iDone < iFin {
		t.Errorf("the completed record preceded the phase's structured outcome")
	}

	// The completed record: durable result file present, session
	// identity, journal listed with Exists, and the SAME plan hash.
	var env eventEnvelope
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
		t.Fatalf("last stream record unreadable: %v\n%s", err, lines[len(lines)-1])
	}
	if env.Type != "completed" {
		t.Fatalf("last stream record type = %q, want completed", env.Type)
	}
	pj, _ := json.Marshal(env.Payload)
	var done resultPayload
	if err := json.Unmarshal(pj, &done); err != nil {
		t.Fatalf("completed payload unreadable: %v", err)
	}
	if done.State != "completed" || done.Exit == nil || *done.Exit != 0 || done.SessionID == "" {
		t.Errorf("completed payload wrong: state=%q exit=%v session=%q", done.State, done.Exit, done.SessionID)
	}
	if len(done.Runs) != 1 || !done.Runs[0].Exists || filepath.Base(done.Runs[0].Journal) != "hpub2.log.journal.jsonl" {
		t.Errorf("completed run list wrong: %+v", done.Runs)
	}
	if len(done.PlanHashes) != 1 || done.PlanHashes[0] != rep.Meta.PlanHash {
		t.Errorf("completed plan_hashes %v do not carry the experiment's hash %q", done.PlanHashes, rep.Meta.PlanHash)
	}
	if _, err := os.Stat(filepath.Join(cwd, done.ResultFile)); err != nil {
		t.Errorf("the durable result file %q is not there: %v", done.ResultFile, err)
	}

	// The replay generation in the NEW journal carries the new
	// session's identity - and the SAME plan hash as (a): the
	// experiment is one thing, the file that tells it another.
	recs2, iss2 := journal.ReadAll(filepath.Join(cwd, "hpub2.log.journal.jsonl"))
	if iss2.ReadErr != "" {
		t.Fatalf("journal 2: %s", iss2.ReadErr)
	}
	reports2 := journal.RecoverRuns(recs2)
	if len(reports2) != 1 {
		t.Fatalf("journal 2 runs = %d, want 1", len(reports2))
	}
	rep2 := reports2[0]
	if rep2.Meta == nil || rep2.Meta.SessionID != done.SessionID || rep2.Meta.PlanHash != rep.Meta.PlanHash {
		t.Errorf("replay generation identity wrong: %+v (want session %s, hash %s)", rep2.Meta, done.SessionID, rep.Meta.PlanHash)
	}
}
