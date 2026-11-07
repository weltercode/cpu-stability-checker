package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/logging"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// The watchdog keeps the journal beating and returns cleanly when the
// run ends. Temperature cutoff is no longer its job: the shared
// runctl Guard owns the thermal watch on every engine (covered by
// this package's temperature integration tests and the Guard's own
// unit tests), so the watchdog only sees the abort through the run
// status ledger.
func TestWatchdogStopsOnCancelWithoutCauses(t *testing.T) {
	lg, err := logging.New(filepath.Join(t.TempDir(), "wd.log"))
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	defer lg.Close()
	jw := journal.NewWriter(&strings.Builder{}, "run-wd")

	rs := newRunStatus(1, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	opt := plan.Options{TestName: "general"}

	done := make(chan struct{})
	go func() { watchdog(ctx, rs, opt, lg, cancel, jw, nil, nil); close(done) }()

	time.Sleep(1500 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not return after cancel")
	}
	if aborted, msg := rs.abortInfo(); aborted {
		t.Fatalf("abortInfo() = (true, %q), want no abort without a cause", msg)
	}
}
