package subprocess

import (
	"path/filepath"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// R31 end-to-end: the REAL ram-block4 kernel through the real child
// (engine grants, journal, evidence). A 300 ms window on two 64 KiB
// regions is the whole point of the block kernel: a whole-region
// kernel with a cancelled partial pass credits nothing, while this
// one closes whole verified blocks - so core_done must carry real
// confirmed checks, mem_done must tell the truth on every channel,
// and the record must carry the kernel version that produced it.
func TestWorkerCmdRamBlock4ShortWindow(t *testing.T) {
	restoreProcessState(t)
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "ram-block4", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-block4-short",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--ram-seed", "9",
		"--mem-grant", "0:0:source:0:65536",
		"--mem-grant", "0:1:destination:65536:65536",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v", rep.Cores)
	}
	c := rep.Cores[0]

	// The confirmed-checks channel: this kernel's checks ARE verified
	// blocks, and a short window still closed whole ones. Zero here
	// would mean the block kernel saved nothing over a whole-region
	// kernel at this scale.
	if c.Checks == nil {
		t.Fatal("checks unknown for a self-checking kernel")
	}
	if *c.Checks == 0 {
		t.Error("checks = 0: the short window credited no verified block")
	}

	// Kernel identity rode into the record.
	if c.KernelVersion != "ram-block4-v1" {
		t.Errorf("kernel version = %q, want ram-block4-v1", c.KernelVersion)
	}

	// Evidence: a clean block run carries it (Total 0 is a result).
	if c.Ram == nil || c.Ram.Total != 0 || len(c.Ram.Findings) != 0 {
		t.Errorf("clean ram evidence wrong: %+v", c.Ram)
	}

	m := c.Mem
	if m == nil {
		t.Fatal("mem_done missing")
	}
	// Verified (MemVerified): the distinct bytes the kernel verified
	// at least once - the union of both regions, exactly once, never
	// doubled across sweeps.
	if m.Verified != 131072 {
		t.Errorf("verified = %d, want 131072 (region union, unique)", 131072)
	}
	// The pass channels: whole-sweep declarations only. A 300 ms
	// window on 128 KiB completes many sweeps, so both must be known
	// and consistent: unique = one sweep's volume, total = a whole
	// multiple of it.
	if m.VerifiedUnique == nil || *m.VerifiedUnique != 131072 {
		t.Errorf("verified_unique = %v, want 131072 (one sweep covers the union exactly)", m.VerifiedUnique)
	}
	if m.VerifiedTotal == nil || *m.VerifiedTotal == 0 || *m.VerifiedTotal%131072 != 0 {
		t.Errorf("verified_total = %v, want a whole multiple of 131072", m.VerifiedTotal)
	}
	if m.Seed == nil || *m.Seed != 9 {
		t.Errorf("seed = %v, want the consumed seed 9 journaled", m.Seed)
	}
}
