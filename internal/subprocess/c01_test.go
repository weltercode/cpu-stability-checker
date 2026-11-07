package subprocess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// Audit C01, child-side refusal and ownership validation. The one-shot
// path is made safe by per-thread grant ownership (parent splits the
// child envelope at thread granularity; the child re-validates the map
// from argv before the measured window). Session mode is refused by
// CONTRACT, not as an unproven shape: it runs exactly one prepared
// engine per pinned core (P08 bursts run one at a time; P07e ownership
// is the per-block grant plus the parent arbiter, proven end to end
// with the corruption probe), so --workers > 1 in a session would be
// parsed and silently never run - the quiet reduction this project
// forbids. Also refused: a thread map with a missing or foreign owner.
// Setup error exit 2 in every case - never the 1 that would pose a
// software defect as a memory fault.
func TestC01OwnershipAndRefusals(t *testing.T) {
	restoreProcessState(t)

	cases := []struct {
		name     string
		workload string
		session  bool
		extra    []string
		grants   []string
		wantMsg  string
	}{
		{
			name:     "missing thread owner",
			workload: "ram-moving",
			extra:    []string{"--workers", "2"},
			// both grants name thread 0: thread 1 would own nothing
			grants:  []string{"0:0:source:0:32768", "0:1:destination:32768:32768"},
			wantMsg: "thread 1 owns no grant",
		},
		{
			name:     "foreign thread index",
			workload: "mem",
			extra:    []string{"--workers", "2"},
			grants:   []string{"2:0:source:0:32768", "2:1:destination:32768:32768"},
			wantMsg:  "names thread 2 outside 0..1",
		},
		{
			name:     "session refuses workers>1 with engine regions",
			workload: "ram-moving",
			session:  true,
			extra:    []string{"--workers", "2"},
			grants:   []string{"0:0:source:0:32768", "0:1:destination:32768:32768", "1:0:source:65536:32768", "1:1:destination:98304:32768"},
			wantMsg:  "one prepared engine per pinned core",
		},
		{
			// The contract is the SHAPE, not the memory: even a CPU-only
			// session with threads would be parsed-and-ignored, so it is
			// refused before anything runs (audit C01 closeout).
			name:     "session refuses workers>1 even without memory workloads",
			workload: "int",
			session:  true,
			extra:    []string{"--workers", "2"},
			wantMsg:  "one prepared engine per pinned core",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jp := filepath.Join(t.TempDir(), "c01.jsonl")
			args := []string{"--core", "0",
				"--workload", tc.workload,
				"--journal", jp, "--run-id", "c01run",
				"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off"}
			if tc.session {
				args = append(args, "--session") // session takes no -duration
			} else {
				args = append(args, "--duration", "200ms")
			}
			args = append(args, tc.extra...)
			for _, g := range tc.grants {
				args = append(args, "--mem-grant", g)
			}
			code := WorkerCmd(args)
			if code != 2 {
				t.Fatalf("WorkerCmd exit = %d, want 2 (setup error)", code)
			}
			recs := readFragment(t, jp, "c01run", 0)
			done := 0
			for _, r := range recs {
				if r.Type == "core_done" && r.Result == journal.SetupError && strings.Contains(r.Msg, tc.wantMsg) {
					done++
				}
			}
			if done != 1 {
				t.Errorf("journal core_done setup_error containing %q: got %d of %d records: %+v", tc.wantMsg, done, len(recs), recs)
			}
		})
	}

	// Scoped exactly: odd maps without verification stay legal - int
	// threads share nothing mutable in the engine, and the odd map is
	// then only a bookkeeping oddity, not a data race on verified bytes.
	t.Run("int with an odd map and a budget stays legal", func(t *testing.T) {
		jp := filepath.Join(t.TempDir(), "c01int.jsonl")
		code := WorkerCmd([]string{"--core", "0", "--workers", "2",
			"--workload", "int", "--duration", "100ms",
			"--journal", jp, "--run-id", "c01int",
			"--mem-budget", "65536", "--mem-allowance", "0", "--mem-lock", "off",
			"--mem-grant", "0:0:source:0:32768", "--mem-grant", "0:1:destination:32768:32768"})
		if code != 0 {
			t.Fatalf("WorkerCmd exit = %d, want 0 (no shared mutable data involved)", code)
		}
	})
}

// readFragment returns the child's own journal records for one slot.
func readFragment(t *testing.T, mainPath, runID string, core int) []journal.Record {
	t.Helper()
	f, err := os.Open(journal.FragmentPath(mainPath, runID, core))
	if err != nil {
		t.Fatalf("open fragment: %v", err)
	}
	defer f.Close()
	recs, iss := journal.ParseAll(f, "c01-fragment")
	if !iss.Empty() {
		t.Fatalf("parse fragment issues: %v", iss)
	}
	return recs
}
