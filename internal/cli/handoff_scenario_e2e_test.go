package cli

// S5 end-to-end: a handoff phase inside an ordinary scenario config,
// run by the real executable through the scenario command. This is
// the "one execution path" proof: the phase is a config.Phase like
// any other, the single dispatch runs the coordinator, the phase
// journal carries the session's identity, and the lease evidence
// lands in the same file the pipeline lists.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

const handoffScenarioConfig = `{
  "test": "general",
  "workload_a": "ram-handoff",
  "workload_b": "",
  "log_file": "hphase.log",
  "stop_on_fault": true,
  "phases": [
    {
      "name": "h",
      "cores": [0, 1],
      "handoff_blocks": 4,
      "handoff_block_size": "16KiB",
      "handoff_rounds": 1,
      "handoff_seed": 3
    }
  ]
}`

func TestE2EScenarioHandoffPhase(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real two-process handoff")
	}
	requireMemoryAdmission(t, 1<<20) // handoff phase geometry; documented upper bound
	exe := e2eExe(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "config.json"), []byte(handoffScenarioConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dry plan first: the geometry is stated as the promise AND the
	// budget, with the forced all_or_none, before anything runs.
	dry := exec.Command(exe, "scenario", "-dry-run")
	dry.Dir = cwd
	var dryOut, dryErr strings.Builder
	dry.Stdout, dry.Stderr = &dryOut, &dryErr
	if err := dry.Run(); err != nil {
		t.Fatalf("scenario -dry-run: %v\n%s\n%s", err, dryOut.String(), dryErr.String())
	}
	for _, want := range []string{"mode=handoff(4 blocks x", "all_or_none", "seed 3"} {
		if !strings.Contains(dryOut.String(), want) {
			t.Errorf("dry plan lacks %q:\n%s", want, dryOut.String())
		}
	}

	// The real run through the scenario command.
	run := exec.Command(exe, "scenario")
	run.Dir = cwd
	var out, outErr strings.Builder
	run.Stdout, run.Stderr = &out, &outErr
	if err := run.Run(); err != nil {
		t.Fatalf("scenario exit: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), outErr.String())
	}
	if !strings.Contains(out.String(), "handoff: cores 0,1") ||
		!strings.Contains(out.String(), "4 block(s)") {
		t.Errorf("phase header did not state the geometry:\n%s", out.String())
	}
	if strings.Contains(out.String(), "duration=30s") {
		t.Errorf("the handoff phase was advertised a clock criterion:\n%s", out.String())
	}

	// The pipeline's per-phase journal: run identity, 4 handoffs (8
	// acked lease steps), worker fragments, and run_meta carrying the
	// session attribution - the phase is as attributable as any
	// ordinary engine's run.
	jpath := journal.Path(filepath.Join(cwd, "hphase.log"))
	recs, iss := journal.ReadAll(jpath)
	if iss.ReadErr != "" {
		t.Fatalf("journal %s: %s", jpath, iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("journaled runs = %d, want 1", len(reports))
	}
	rep := reports[0]
	if rep.Test != "handoff" {
		t.Errorf("journaled test = %q, want handoff", rep.Test)
	}
	if !rep.Ended || rep.Exit != 0 {
		t.Errorf("journaled exit = %d (ended=%v), want 0/true", rep.Exit, rep.Ended)
	}
	if rep.Validation != "verified" {
		t.Errorf("journaled validation = %q, want verified", rep.Validation)
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
	if rep.Meta == nil || rep.Meta.SessionID == "" || len(rep.Meta.PlanHash) != 64 {
		t.Errorf("run_meta attribution missing or malformed: %+v", rep.Meta)
	}
	// The session the run_meta names really recorded itself in this
	// run's own directory (U02): the phase is attributable from the
	// artifacts alone, not just a string in a record.
	if rep.Meta != nil && rep.Meta.SessionID != "" {
		if _, merr := os.Stat(filepath.Join(cwd, "history", rep.Meta.SessionID+".json")); merr != nil {
			t.Errorf("no session manifest for the phase's session: %v", merr)
		}
	}
}
