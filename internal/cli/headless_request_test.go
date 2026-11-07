package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// R03a (audit B06) permanent regressions: the headless run executes
// the request it loaded and gated - never whatever the repeat slot
// happens to hold. Before the fix a failed slot save left the OLD
// recipe on disk, repeatLastRunWithUI re-read it, and the tool ran a
// different experiment than the caller asked for with exit 0. The
// executed request is now the in-memory object; the slot is only the
// durable promise that the next crash can be repeated.

// r03aFake registers a millisecond fake workload and returns the
// flag that flips the moment its Run starts - so a test can prove
// not only that the new request ran, but that the old one never did.
func r03aFake(t *testing.T, name string) *bool {
	t.Helper()
	ran := new(bool)
	workload.Register(&workload.Workload{Name: name,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			*ran = true
			rep.Add(1)
			rep.Beat()
			<-ctx.Done()
			return 1, nil
		}})
	t.Cleanup(func() { workload.Delete(name) })
	return ran
}

// r03aRestoreAffinity mirrors the session pipeline's affinity dance
// the way TestRunCmdRequestRunsThroughRepeatPipeline does: whatever
// the run pinned to core 0, the process mask is back afterwards.
func r03aRestoreAffinity(t *testing.T) {
	t.Helper()
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})
}

func r03aRequest(workloadName, logName string) lastRun {
	req := sessionRequest()
	req.Session.Workload = workloadName
	req.Session.LogFile = logName
	return req
}

// The exact B06 scenario: a valid OLD recipe sits in the slot, the
// NEW request's save fails, and the run must still execute the NEW
// request (with the repeat promise loudly given up) - never the old
// one. The save fails portably because the slot path is a directory:
// the atomic rename can never replace a directory, no
// handle-sharing tricks needed.
func TestHeadlessSaveFailureRunsNewRequest(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	r03aRestoreAffinity(t)
	oldRan := r03aFake(t, "r03a-old-fake")
	newRan := r03aFake(t, "r03a-new-fake")

	dir := t.TempDir()
	prev := r03aRequest("r03a-old-fake", "r03a-old.log")
	if err := saveLastRun(prev); err != nil {
		t.Fatalf("seed old slot: %v", err)
	}

	orig := lastrunPath
	lastrunPath = filepath.Join(dir, "lastrun.json")
	t.Cleanup(func() { lastrunPath = orig })
	if err := os.Mkdir(lastrunPath, 0o755); err != nil {
		t.Fatal(err)
	}

	req := r03aRequest("r03a-new-fake", "r03a-new.log")
	code := runCmd([]string{"-request", writeRequestFile(t, dir, req)})
	if code != 0 {
		t.Fatalf("headless run exit = %d, want 0: a failed repeat-save must not block the run itself", code)
	}
	if *oldRan {
		t.Error("B06: the OLD slot recipe ran after the new request's save failed")
	}
	if !*newRan {
		t.Error("the new request did not execute")
	}
	if m := onlyManifest(t); m.State != "completed" {
		t.Errorf("manifest state = %q, want completed", m.State)
	}
}

// A stale slot (the "replaced between save and start" shape: once
// execution never re-reads the slot, any tampering state is
// indistinguishable and equally harmless): the in-memory request
// runs, and afterwards the slot holds the NEW recipe.
func TestHeadlessStaleSlotStillRunsNewRequest(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	r03aRestoreAffinity(t)
	oldRan := r03aFake(t, "r03a-old-fake")
	newRan := r03aFake(t, "r03a-new-fake")

	dir := t.TempDir()
	prev := r03aRequest("r03a-old-fake", "r03a-old.log")
	if err := saveLastRun(prev); err != nil {
		t.Fatalf("seed old slot: %v", err)
	}

	req := r03aRequest("r03a-new-fake", "r03a-new.log")
	if code := runCmd([]string{"-request", writeRequestFile(t, dir, req)}); code != 0 {
		t.Fatalf("headless run exit = %d, want 0", code)
	}
	if *oldRan {
		t.Error("B06: the stale slot recipe ran instead of the loaded request")
	}
	if !*newRan {
		t.Error("the new request did not execute")
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.Session == nil || lr.Session.Workload != "r03a-new-fake" {
		t.Errorf("slot did not take the new recipe: %+v", lr)
	}
}

// A damaged slot must not disturb the run either: the request file
// is the whole question, the slot is only a promise.
func TestHeadlessDamagedSlotStillRunsNewRequest(t *testing.T) {
	chdirTemp(t)
	useScratchReceipt(t)
	r03aRestoreAffinity(t)
	newRan := r03aFake(t, "r03a-new-fake")

	dir := t.TempDir()
	if err := os.WriteFile("lastrun.json", []byte("{ damaged recipe"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := r03aRequest("r03a-new-fake", "r03a-new.log")
	if code := runCmd([]string{"-request", writeRequestFile(t, dir, req)}); code != 0 {
		t.Fatalf("headless run exit = %d, want 0 despite the damaged slot", code)
	}
	if !*newRan {
		t.Error("the new request did not execute over a damaged slot")
	}
}
