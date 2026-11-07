package journal

import (
	"path/filepath"
	"testing"
	"time"
)

// mem_done carries the engine's real numbers and recovery attaches
// them to the core's result. Old journals without the record must
// read as unknown (nil), never as zeros.
func TestMemDoneRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-mem")
	cores := []int{0, 1}
	jw.RunStart("general", "", cores, 1, "5s", true)
	for _, c := range cores {
		jw.CoreStart(c, "mem")
	}
	seed := uint64(0xC0FFEE)
	jw.MemDone(0, MemAccounting{
		Requested: 1 << 20, Allocated: 1<<20 + 4096, Touched: 1 << 20,
		FirstTouchMS: 1.5, Verified: 1 << 20,
		LockMode: "try", Locked: 1 << 20, LockFailure: "",
		Seed: &seed,
	})
	// Core 1 gets no mem_done on purpose: its numbers must stay
	// unknown after recovery, never zero-filled.
	jw.CoreDoneChecks(0, OK, "", 5*time.Second, nil, "", "mem-regions-v1")
	jw.CoreDoneChecks(1, OK, "", 5*time.Second, nil, "", "mem-regions-v1")
	jw.RunEnd(0)
	jw.Close()

	records, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := RecoverRuns(records)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	byCore := map[int]*CoreOutcome{}
	for i := range runs[0].Cores {
		c := runs[0].Cores[i]
		byCore[c.Core] = &c
	}
	c0 := byCore[0]
	if c0 == nil || c0.Mem == nil {
		t.Fatalf("core 0 lost its mem accounting: %+v", c0)
	}
	m := *c0.Mem
	if m.Requested != 1<<20 || m.Allocated != 1<<20+4096 || m.Touched != 1<<20 || m.Verified != 1<<20 {
		t.Errorf("accounting numbers drifted: %+v", m)
	}
	if m.FirstTouchMS != 1.5 || m.LockMode != "try" || m.Locked != 1<<20 {
		t.Errorf("lock/timing fields drifted: %+v", m)
	}
	if m.Seed == nil || *m.Seed != 0xC0FFEE {
		t.Errorf("seed drifted: %v", m.Seed)
	}
	c1 := byCore[1]
	if c1 == nil {
		t.Fatal("core 1 missing")
	}
	if c1.Mem != nil {
		t.Errorf("core 1 has mem accounting it never wrote: %+v", c1.Mem)
	}
}
