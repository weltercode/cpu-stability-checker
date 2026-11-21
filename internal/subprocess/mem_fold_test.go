package subprocess

// R34 (owner queue 2026-11-17): the session-shape executors now READ
// the mem_done facts their children already journaled and fold them
// into the live result with the single rule (plan.FoldRAM). These
// tests pin the reading: run-scoped (journal files are shared across
// runs), main AND fragments, last-writer-wins per slot (the merge
// rule scanCoreOutcomes uses), absence kept as absence (a killed
// worker or budget-skipped core becomes an unknown slot, never a
// zero), no block at all when nobody reported, and the live line
// riding the session summary inside the single-write block.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestScanMemDoneRunScopedAndFold(t *testing.T) {
	dir := t.TempDir()
	jp := filepath.Join(dir, "j.jsonl")

	// Run r1 writes two mem_done for the SAME slot in the main file:
	// the last one is the authoritative settle (same merge rule the
	// one-shot scan uses - an earlier record is never a second copy).
	jf, err := journal.OpenFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	w1 := journal.NewWriter(jf, "r1")
	w1.MemDone(3, journal.MemAccounting{Requested: 100, Allocated: 100, Touched: 100, Verified: 700})
	w1.MemDone(3, journal.MemAccounting{Requested: 100, Allocated: 100, Touched: 100, Verified: 800})
	w1.Close()

	// A second run in the SAME shared file must never leak into r1.
	jf2, err := journal.OpenFile(jp)
	if err != nil {
		t.Fatal(err)
	}
	w2 := journal.NewWriter(jf2, "r2")
	w2.MemDone(9, journal.MemAccounting{Requested: 5000, Allocated: 5000, Touched: 5000, Verified: 5000})
	w2.Close()

	// And the session-typical shape: a worker's OWN fragment file.
	fp := journal.FragmentPath(jp, "r1", 7)
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		t.Fatal(err)
	}
	ff, err := journal.OpenFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	w3 := journal.NewWriter(ff, "r1")
	uniq := uint64(50)
	w3.MemDone(7, journal.MemAccounting{Requested: 50, Allocated: 50, Touched: 50, Verified: 50, VerifiedUnique: &uniq, VerifiedTotal: &uniq})
	w3.Close()

	seen := scanMemDone(jp, "r1")
	if len(seen) != 2 {
		t.Fatalf("scanned slots = %v, want exactly 3 and 7 (r2's record and its fragment tree stay out)", seen)
	}
	if m := seen[3]; m == nil || m.Verified != 800 {
		t.Errorf("slot 3 accounting = %+v, want the LAST writer's (verified 800)", seen[3])
	}
	if _, leaked := seen[9]; leaked {
		t.Error("run r2's mem_done leaked into r1's scan (shared journal is run-scoped)")
	}
	if m := seen[7]; m == nil || m.Verified != 50 {
		t.Errorf("fragment slot 7 = %+v, want the worker's own fragment record", seen[7])
	}

	// The fold: 4 slots were planned, only 2 journaled. The two
	// missing ones (a killed worker, a budget-skipped core) are
	// unknown - stated, never zero-folded.
	fold := foldRunRAM(jp, "r1", 4)
	if fold == nil {
		t.Fatal("two reported slots folded to nil")
	}
	if fold.EngineSlots != 2 || fold.UnknownSlots != 2 {
		t.Errorf("slots = %d accounted / %d unknown, want 2/2", fold.EngineSlots, fold.UnknownSlots)
	}
	if fold.Requested != 150 || fold.Allocated != 150 || fold.Touched != 150 || fold.Verified != 850 {
		t.Errorf("sums wrong: %+v", fold)
	}
	if fold.LastPass != 50 {
		t.Errorf("last-pass = %d, want 50 (only the fragment slot proved a pass volume)", fold.LastPass)
	}

	// Nobody ever reported for this run: nil block, not a block of
	// zeros - the honesty contract every artifact keeps.
	if f := foldRunRAM(jp, "r-none", 3); f != nil {
		t.Errorf("unreported run folded into %+v, want nil (absence = not observed)", f)
	}

	// The live line rides the session summary INSIDE the single-write
	// block (the panel repaint rule) when the fold exists, and is
	// simply absent when it does not.
	var sb strings.Builder
	writeSessionReport(&sb, jp, []int{3}, []SessionOutcome{{BurstsDone: 1, BurstsTried: 1, Exit: 0}}, "", fold.RAMString())
	if !strings.Contains(sb.String(), "memory coverage (cumulative): 2 slot(s) accounted") {
		t.Errorf("fold line missing from the session summary block:\n%s", sb.String())
	}
	sb.Reset()
	writeSessionReport(&sb, jp, []int{3}, []SessionOutcome{{BurstsDone: 1, BurstsTried: 1, Exit: 0}}, "", "")
	if strings.Contains(sb.String(), "memory coverage") {
		t.Errorf("an unreported run grew a mem block out of nothing:\n%s", sb.String())
	}
}

// The parent-side end for transitions: a session that actually ran
// the memory engine must show the fold LIVE - the whole point of R34
// is that the facts were always journaled, the live summary just
// never read them. And the fold must not move the verdict: exit and
// validation are exactly what the clean session always ended with.
func TestExecuteSessionResultFoldsLiveMem(t *testing.T) {
	requireMemoryAdmission(t, 128<<20) // documented bound: the smallest fixed budget that clears the 64MiB service allowance for one worker
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t) // the WHEA seam only: this machine's event log never enters the expectation
	opt := plan.Options{
		TestName: "general", A: "ram-constant", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		// Generous windows (the parking fixtures' discipline): the
		// expectation never hinges on this machine's timing.
		Bursts: 2, BurstGap: 600 * time.Millisecond, BurstRun: 250 * time.Millisecond,
		Memory: "128MiB", MemLock: "off",
	}
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 || res.Validation != plan.ValVerified {
		t.Fatalf("a clean engine session must end verified, fold or no fold: %+v", res)
	}
	mem := res.Coverage.Mem
	if mem == nil {
		t.Fatal("the session worker journaled its own mem_done; a live fold that drops it turns a KNOWN fact into an absence")
	}
	if mem.EngineSlots != 1 || mem.UnknownSlots != 0 {
		t.Errorf("slots = %d/%d, want 1/0 (one planned slot, terminal path reached)", mem.EngineSlots, mem.UnknownSlots)
	}
	if mem.Verified == 0 || mem.LastPass == 0 {
		t.Errorf("verified=%d last-pass=%d, want the kernel's own proven counts", mem.Verified, mem.LastPass)
	}
	if !strings.Contains(mem.RAMString(), "memory coverage (cumulative)") {
		t.Errorf("live line wording: %q", mem.RAMString())
	}
	// The journal half tells the SAME story the live fold read - one
	// set of numbers, never a second arithmetic beside the report.
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if rep.Cores[0].Mem == nil || rep.Cores[0].Mem.Verified != mem.Verified {
		t.Errorf("journal mem_done story differs from the live fold: journal %+v live %+v", rep.Cores[0].Mem, mem)
	}
}
