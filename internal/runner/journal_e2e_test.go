package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// TestExecuteWritesRecoverableJournal lives in the runner package because it
// drives a real Execute() and then recovers the story from the journal file.
func TestExecuteWritesRecoverableJournal(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "run.log")

	opt := plan.Options{
		TestName: "general",
		A:        "float",
		Workers:  1,
		Duration: 1200 * time.Millisecond,
		LogFile:  logPath,
	}
	if code := Execute(opt); code != 0 {
		t.Fatalf("Execute() exit code = %d, want 0", code)
	}

	f, err := os.Open(journal.Path(logPath))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()

	reports := journal.RecoverRuns(journal.Parse(f))
	if len(reports) != 1 {
		t.Fatalf("recovered %d runs, want 1", len(reports))
	}
	rep := reports[0]
	if !rep.Ended || rep.Exit != 0 {
		t.Fatalf("run report = ended:%v exit:%d, want ended:true exit:0", rep.Ended, rep.Exit)
	}
	if len(rep.Cores) != runtime.NumCPU() {
		t.Fatalf("report has %d cores, want %d", len(rep.Cores), runtime.NumCPU())
	}
	for _, c := range rep.Cores {
		if c.Status != journal.OK {
			t.Errorf("core %d status = %q, want %q", c.Core, c.Status, journal.OK)
		}
	}
}
