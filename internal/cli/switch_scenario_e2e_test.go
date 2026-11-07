package cli

// Q05d end-to-end: a pair-switch phase inside an ordinary scenario
// config, run twice by the real executable through the scenario
// command. This is the "one execution path + honest replay" proof:
// the phase is a config.Phase like any other, the single dispatch
// runs the switch, the journal carries the PREPARED turns before the
// observed ones, and replaying the saved recipe reproduces the same
// plan hash and the same realized (seeded) turn order - no PRNG
// version between the two runs changes what was commanded.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

const switchScenarioConfig = `{
  "test": "general",
  "workload_a": "float",
  "workload_b": "float",
  "log_file": "swphase.log",
  "stop_on_fault": true,
  "phases": [
    {
      "name": "sw",
      "switch_pairs": [[0, 1], [2, 3]],
      "switch_rounds": 2,
      "switch_gap": "60ms",
      "switch_run": "120ms",
      "switch_order": "seeded",
      "switch_seed": 7
    }
  ]
}`

// Q05a1 pins the seeded order: seed 7 over two groups shuffles
// round-by-round to groups [1 0 | 0 1 | ...] - the first four turns
// of that golden vector are exactly what the run must command.
var switchGoldenGroups = []int{1, 0, 0, 1}

func TestE2EScenarioSwitchPhase(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs real four-process pair-switches")
	}
	exe := e2eExe(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "config.json"), []byte(switchScenarioConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dry plan first: groups, order word AND its seed are stated as the
	// promise, with a span built from turns, not a clock.
	dry := exec.Command(exe, "scenario", "-dry-run")
	dry.Dir = cwd
	var dryOut, dryErr strings.Builder
	dry.Stdout, dry.Stderr = &dryOut, &dryErr
	if err := dry.Run(); err != nil {
		t.Fatalf("scenario -dry-run: %v\n%s\n%s", err, dryOut.String(), dryErr.String())
	}
	for _, want := range []string{
		"mode=switch(g0[aggressor 0, observer 1] g1[aggressor 2, observer 3]",
		"order=seeded seed=7",
	} {
		if !strings.Contains(dryOut.String(), want) {
			t.Errorf("dry plan lacks %q:\n%s", want, dryOut.String())
		}
	}

	// Run the SAME recipe twice: replay must reproduce the experiment,
	// not merely rerun something similar.
	for run := 1; run <= 2; run++ {
		cmd := exec.Command(exe, "scenario")
		cmd.Dir = cwd
		var out, outErr strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &outErr
		if err := cmd.Run(); err != nil {
			t.Fatalf("scenario run %d: %v\nstdout:\n%s\nstderr:\n%s", run, err, out.String(), outErr.String())
		}
		if !strings.Contains(out.String(), "pair switch mode: 2 group(s) x 2 round(s)") {
			t.Errorf("run %d header did not state the switch shape:\n%s", run, out.String())
		}
	}

	jpath := journal.Path(filepath.Join(cwd, "swphase.log"))
	recs, iss := journal.ReadAll(jpath)
	if iss.ReadErr != "" {
		t.Fatalf("journal %s: %s", jpath, iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 2 {
		t.Fatalf("journaled runs = %d, want 2 (one per scenario invocation)", len(reports))
	}
	for i, rep := range reports {
		if !rep.Ended || rep.Exit != 0 {
			t.Errorf("run %d: ended=%v exit=%d, want ended/0", i, rep.Ended, rep.Exit)
		}
		if rep.Validation != "verified" {
			t.Errorf("run %d validation = %q, want verified", i, rep.Validation)
		}
		// Commanded: the PREPARED seeded timeline, journaled before
		// anything spawned, carries the golden vector - and it is the
		// reproduction artifact, so identical replays must show it.
		if rep.SwitchSchedule == nil || len(rep.SwitchSchedule.Turns) != 4 {
			t.Fatalf("run %d switch_schedule missing/wrong: %+v", i, rep.SwitchSchedule)
		}
		if rep.SwitchSchedule.Seed == nil || *rep.SwitchSchedule.Seed != 7 {
			t.Errorf("run %d schedule seed = %+v, want the provenance seed 7", i, rep.SwitchSchedule.Seed)
		}
		for k, wantGroup := range switchGoldenGroups {
			if got := rep.SwitchSchedule.Turns[k].Group; got != wantGroup {
				t.Errorf("run %d planned turn %d group = %d, want %d (seed 7 golden vector)", i, k, got, wantGroup)
			}
		}
		// Observed: four closed parent turn records in the commanded
		// order, plus both views of every turn for every participant.
		if len(rep.Switches) != 4 {
			t.Fatalf("run %d turn records = %d, want 4", i, len(rep.Switches))
		}
		for k, o := range rep.Switches {
			if !o.Complete || o.Group != switchGoldenGroups[k] {
				t.Errorf("run %d observed turn %d wrong: %+v", i, k, o)
			}
		}
		if len(rep.Cores) != 4 {
			t.Fatalf("run %d participants = %d, want 4", i, len(rep.Cores))
		}
		for _, c := range rep.Cores {
			if c.Status != journal.OK {
				t.Errorf("run %d core %d status = %s, want ok", i, c.Core, c.Status)
			}
			if len(c.SessionObs) != 2 || len(c.Bursts) != 2 {
				t.Errorf("run %d core %d views = parent %d worker %d, want 2/2", i, c.Core, len(c.SessionObs), len(c.Bursts))
			}
		}
		if rep.Meta == nil || rep.Meta.SessionID == "" || len(rep.Meta.PlanHash) != 64 {
			t.Fatalf("run %d run_meta attribution missing/malformed: %+v", i, rep.Meta)
		}
		if _, merr := os.Stat(filepath.Join(cwd, "history", rep.Meta.SessionID+".json")); merr != nil {
			t.Errorf("run %d has no session manifest: %v", i, merr)
		}
	}
	// Replay identity: same recipe, same plan hash, same realized
	// (seeded) turn sequence - the two runs are the same experiment.
	if reports[0].Meta.PlanHash != reports[1].Meta.PlanHash {
		t.Errorf("replaying the recipe changed the plan hash: %s vs %s",
			reports[0].Meta.PlanHash, reports[1].Meta.PlanHash)
	}
	for k := range switchGoldenGroups {
		a, b := reports[0].Switches[k], reports[1].Switches[k]
		if a.Group != b.Group || a.Round != b.Round {
			t.Errorf("replay commanded a different turn %d: g%d/r%d vs g%d/r%d", k, a.Group, a.Round, b.Group, b.Round)
		}
	}
}
