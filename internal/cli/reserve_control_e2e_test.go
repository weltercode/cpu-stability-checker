package cli

// Real-run acceptance for the reserve_control headroom policy: a
// saturated general run from a config that lists EVERY logical CPU
// must hand the highest-numbered one to the helpers, say so loudly
// before the first load, and journal the reduced worker set. This is
// the live-process proof the resolver unit tests cannot give: the
// pinning path (no affinity warning allowed), the journal's actual
// core set and the loud reproduce line all execute for real. The run
// is short and half-idle on purpose: the acceptance is the shape, not
// the burn-in (that lives in the hardware protocols, on purpose).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func TestE2EReserveControlRun(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e builds the exe and runs a saturated all-cores general run")
	}
	num := runtime.NumCPU()
	if num < 4 {
		t.Skipf("test assumes at least 4 logical CPUs, machine has %d", num)
	}
	exe := e2eExe(t)
	cwd := t.TempDir()

	parts := make([]string, num)
	for i := range parts {
		parts[i] = strconv.Itoa(i)
	}
	cfg := fmt.Sprintf(`{
  "test": "general", "workload_a": "int", "workload_b": "idle",
  "cores": [%s], "workers": 1, "duration": "1s", "stop_on_fault": true,
  "log_file": "rsv.log", "reserve_control": "logical"
}`, strings.Join(parts, ", "))
	if err := os.WriteFile(filepath.Join(cwd, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	run := exec.Command(exe, "run", "-test", "general")
	run.Dir = cwd
	var out, outErr strings.Builder
	run.Stdout, run.Stderr = &out, &outErr
	if err := run.Run(); err != nil {
		t.Fatalf("reserve run exit: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), outErr.String())
	}
	o := out.String()
	if !strings.Contains(o, fmt.Sprintf("cpu %d removed from the worker set", num-1)) {
		t.Errorf("the coverage loss was not said out loud before load:\n%s", o)
	}
	if !strings.Contains(o, fmt.Sprintf("control=core%d (reserved from workers: %d)", num-1, num-1)) {
		t.Errorf("the reproduce line does not name the reservation:\n%s", o)
	}
	if strings.Contains(outErr.String(), "warning: control core") {
		t.Errorf("helpers could not be pinned to the reserved cpu:\n%s", outErr.String())
	}

	// The journal IS the coverage truth: run_start names the reduced
	// set, and the reserved cpu never loaded a worker.
	recs, iss := journal.ReadAll(journal.Path(filepath.Join(cwd, "rsv.log")))
	if iss.ReadErr != "" {
		t.Fatalf("journal unreadable: %s", iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("journaled runs = %d, want 1", len(reports))
	}
	rep := reports[0]
	if !rep.Ended || rep.Exit != 0 {
		t.Fatalf("journaled outcome wrong: ended=%v exit=%d", rep.Ended, rep.Exit)
	}
	if len(rep.Selected) != num-1 {
		t.Fatalf("journal run_start selected %d cpus (%v), want %d (the reserved one left the worker set)",
			len(rep.Selected), rep.Selected, num-1)
	}
	for _, c := range rep.Selected {
		if c == num-1 {
			t.Errorf("the reserved cpu %d still carries a worker in the journal: %v", num-1, rep.Selected)
		}
	}
	if len(rep.Cores) != num-1 {
		t.Errorf("core records = %d, want %d (coverage)", len(rep.Cores), num-1)
	}
}
