package subprocess

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Audit C01 closed as a permanent regression (evolved from the audit's
// address probe): every RAM-worker THREAD inside one memory child owns
// its own disjoint grant set, and its kernel view excludes every other
// thread's memory. Two threads at 64 KiB must see two region sets that
// do not merely differ in one address - no region of one may overlap
// any region of the other.
func TestC01ThreadGrantOwnership(t *testing.T) {
	restoreProcessState(t)
	var mu sync.Mutex
	var views [][]memtest.AllocatedRegion
	workload.Register(&workload.Workload{Name: "c01-owner-probe", RequiresMemory: true,
		Run: func(_ context.Context, rep workload.Reporter) (uint64, error) {
			mu.Lock()
			defer mu.Unlock()
			views = append(views, rep.(workload.RegionsReporter).MemRegions())
			return 0, nil
		}})
	t.Cleanup(func() { workload.Delete("c01-owner-probe") })

	code := WorkerCmd([]string{"--core", "0", "--workers", "2",
		"--workload", "c01-owner-probe", "--duration", "200ms",
		"--journal", filepath.Join(t.TempDir(), "own.jsonl"), "--run-id", "own",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:32768", "--mem-grant", "0:1:destination:32768:32768",
		"--mem-grant", "1:0:source:65536:32768", "--mem-grant", "1:1:destination:98304:32768"})
	if code != 0 {
		t.Fatalf("WorkerCmd exit = %d, want 0", code)
	}
	if len(views) != 2 {
		t.Fatalf("owner views = %d, want 2 (one per thread)", len(views))
	}
	for i, v := range views {
		if len(v) != 2 {
			t.Errorf("thread %d sees %d regions, want exactly its own 2 (a full-process view would be 4)", i, len(v))
		}
	}
	// No region of view 0 may overlap any region of view 1.
	for _, a := range views[0] {
		aLo, aHi := a.Block.Addr, a.Block.Addr+uintptr(len(a.Block.Bytes()))
		for _, b := range views[1] {
			bLo, bHi := b.Block.Addr, b.Block.Addr+uintptr(len(b.Block.Bytes()))
			if aLo < bHi && bLo < aHi {
				t.Fatalf("threads own overlapping memory: %#x..%#x vs %#x..%#x", aLo, aHi, bLo, bHi)
			}
		}
	}
}

// The end-to-end point of C01: with per-thread grants, two real
// ram-moving threads on 64 KiB of disjoint memory COMPLETE CLEANLY -
// the exact shape whose 16/16 mismatches were this software defect.
// Coverage rides the journal per slot as the SUM of each thread's own
// work: unique bytes = the full data once (never counted per thread),
// total = each thread's completed passes over its own disjoint set.
func TestC01RamMovingTwoThreadsClean(t *testing.T) {
	restoreProcessState(t)
	jp := filepath.Join(t.TempDir(), "ram2.jsonl")
	code := WorkerCmd([]string{"--core", "0", "--workers", "2",
		"--workload", "ram-moving", "--duration", "250ms", "--ram-seed", "7",
		"--journal", jp, "--run-id", "ram2",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:32768", "--mem-grant", "0:1:destination:32768:32768",
		"--mem-grant", "1:0:source:65536:32768", "--mem-grant", "1:1:destination:98304:32768"})
	if code != 0 {
		t.Fatalf("WorkerCmd exit = %d, want 0 (a clean two-thread ram pass)", code)
	}

	recs := readFragment(t, jp, "ram2", 0)
	var done *journal.Record
	var mem *journal.Record
	for i, r := range recs {
		switch r.Type {
		case "core_done":
			done = &recs[i]
		case "mem_done":
			mem = &recs[i]
		}
	}
	if done == nil || done.Result != journal.OK {
		t.Fatalf("core_done = %+v, want result ok", done)
	}
	if done.Ram == nil || done.Ram.Total != 0 || len(done.Ram.Findings) != 0 {
		t.Errorf("ram evidence = %+v, want zero findings (the mismatch was C01's own doing)", done.Ram)
	}
	if mem == nil || mem.Mem == nil {
		t.Fatalf("mem_done missing: coverage numbers would be unknown, not verifiable")
	}
	const data = 131072
	// R15b: a starved machine moment gets its own honest shape, but
	// only when the accounting is CONSISTENTLY empty. The worker
	// records verified accounting ONLY when at least one whole pass
	// closed (session_worker: passes > 0), so a nil verified_unique
	// alongside a clean exit can legitimately mean "zero whole
	// passes over 64 KiB in 250 ms" - the pinned thread never got a
	// usable slice of CPU (the exact observed gate signature), and
	// the invariant below was NEVER EXERCISED. A CONTRADICTION
	// (unique absent while verified_total or the last-pass sum
	// reports passes) is never a moment: it is an evidence defect
	// and stays red, so this skip cannot mask a reporting
	// regression. Real C01 damage also lands BEFORE this skip: a
	// data mismatch makes the exit or the core_done findings red.
	if mem.Mem.VerifiedUnique == nil {
		if mem.Mem.VerifiedTotal != nil || mem.Mem.Verified != 0 {
			t.Fatalf("verified accounting contradicts itself: verified_unique absent while verified_total=%v and verified=%d - an evidence defect is never a machine moment", mem.Mem.VerifiedTotal, mem.Mem.Verified)
		}
		t.Skipf("C01 machine moment (named skip, R15b): exit 0 and core_done ok, but consistently ZERO completed passes over %d KiB in 250 ms (unique/total/last-pass all empty, no findings): the worker was descheduled, the invariant below was never exercised", data/1024)
	}
	// Each thread reports its OWN whole region set (data/2) once per
	// completed pass, and the two threads' pass counters advance
	// independently - so the honest invariant is a whole multiple of
	// the per-thread pass quantum, never a whole number of full
	// pass CYCLES across both threads.
	const threadPass = data / 2
	if *mem.Mem.VerifiedUnique != data {
		t.Errorf("verified_unique = %v, want %d: every byte of the full data exactly once", *mem.Mem.VerifiedUnique, data)
	}
	if total := mem.Mem.VerifiedTotal; total == nil {
		t.Errorf("verified_total absent, want at least %d and a whole multiple of %d", data, threadPass)
	} else if *total < data || *total%threadPass != 0 {
		t.Errorf("verified_total = %d, want at least %d and a whole multiple of %d (every completed pass reports its thread's whole set exactly once, no double-counted regions)",
			*total, data, threadPass)
	}
	if mem.Mem.Verified != data {
		t.Errorf("verified = %d, want %d (sum of both threads' last completed pass)", mem.Mem.Verified, data)
	}
}

// labSnapshot swaps memSnapshotNow for an obviously roomy SYNTHETIC
// machine (never a copy of the host: the point is that the arithmetic
// is machine-independent, and no test hardcodes the box it runs on).
// t.Cleanup restores the live default, and the swap is only sound for
// tests that map nothing - anything spawning workers keeps the live
// snapshot and pays real admission.
func labSnapshot(t *testing.T) {
	t.Helper()
	old := memSnapshotNow
	memSnapshotNow = func() sysinfo.MemorySnapshot {
		const GiB = 1 << 30
		return sysinfo.MemorySnapshot{
			Known:         true,
			CommitKnown:   true,
			TotalPhys:     64 * GiB,
			AvailPhys:     32 * GiB,
			TotalPageFile: 64 * GiB,
			AvailPageFile: 32 * GiB,
		}
	}
	t.Cleanup(func() { memSnapshotNow = old })
}

// Parent side: the two-stage partition. Stage one stays the golden
// process-level division (allowance per PROCESS, unchanged budgets
// everywhere else); stage two re-splits each child's own envelope at
// thread granularity, local numbering, rebased offsets keeping global
// disjointness. A budget that cannot give EVERY thread a complete set
// is a setup error for verifying workloads - never a partial map.
//
// The test pins partition ARITHMETIC and maps nothing, so it runs
// against a synthetic admitting machine (labSnapshot): the live view
// made the golden vectors admission hostages - 272 MiB plus the 2 GiB
// commit reserve sits exactly on this machine's resident baseline,
// and the very same arithmetic flipped between pass and setup-error
// refusal depending on what the desktop committed this minute. The
// live path keeps its honest admission everywhere a plan actually
// maps (E2E sessions, and the handoff section's real CreateSharedMapping).
func TestC01BuildMemPlanThreadSplit(t *testing.T) {
	labSnapshot(t)
	const MiB = 1 << 20
	opt := plan.Options{Memory: "272MiB", Workers: 2, A: "ram-moving", MemLock: "off", RamSeed: 7}
	mp, err := buildMemPlan(opt, 2)
	if err != nil {
		t.Fatalf("buildMemPlan: %v", err)
	}
	if mp.allowance != 128*MiB {
		t.Errorf("allowance = %d, want 128MiB (per PROCESS: stage one unchanged)", mp.allowance)
	}
	if mp.skipped != 0 {
		t.Errorf("skipped = %d, want 0", mp.skipped)
	}
	var all []memtest.Grant
	for c := 0; c < 2; c++ {
		base := uint64(c*72) * MiB
		want := []memtest.Grant{
			{Worker: 0, Region: 0, Role: memtest.RoleSource, Offset: base, Requested: 18 * MiB},
			{Worker: 0, Region: 1, Role: memtest.RoleDest, Offset: base + 18*MiB, Requested: 18 * MiB},
			{Worker: 1, Region: 0, Role: memtest.RoleSource, Offset: base + 36*MiB, Requested: 18 * MiB},
			{Worker: 1, Region: 1, Role: memtest.RoleDest, Offset: base + 54*MiB, Requested: 18 * MiB},
		}
		if len(mp.grants[c]) != 4 {
			t.Fatalf("child %d grants = %+v, want 4 (2 threads x 2 roles)", c, mp.grants[c])
		}
		seen := map[memtest.Grant]int{}
		for _, g := range mp.grants[c] {
			seen[g]++
			all = append(all, g)
		}
		for _, g := range want {
			if seen[g] != 1 {
				t.Errorf("child %d: grant %+v present %d times, want exactly once (grants: %+v)", c, g, seen[g], mp.grants[c])
			}
		}
	}
	if err := memtest.ValidateGrants(memtest.Budget{Bytes: 272 * MiB}, memtest.PartitionResult{Grants: all}); err != nil {
		t.Errorf("global rebased grants must stay disjoint: %v", err)
	}

	// Completeness refusal: 8 KiB of data cannot give 2 threads each a
	// page pair - the second thread would own nothing.
	small := plan.Options{Memory: "134225920", Workers: 2, A: "ram-moving", MemLock: "off"}
	if _, err := buildMemPlan(small, 2); err == nil {
		t.Error("buildMemPlan with a thread-starved budget: want a completeness refusal, got nil")
	}
	// Load-only workloads tolerate the explicit partial shape.
	load := plan.Options{Memory: "134225920", Workers: 2, A: "mem-bw", MemLock: "off"}
	if mp, err := buildMemPlan(load, 2); err != nil {
		t.Errorf("mem-bw with a thread-starved budget: %v, want tolerance with explicit skips", err)
	} else if len(mp.grants[1]) != 0 {
		t.Errorf("child 1 grants = %+v, want none (stage one skipped it explicitly)", mp.grants[1])
	}
}
