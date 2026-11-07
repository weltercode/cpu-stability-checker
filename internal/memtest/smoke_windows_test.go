//go:build windows

package memtest

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// TestMultiGBSmokeOptIn is the plan's readiness proof that the SAME
// API serves the small prototype and the multi-gigabyte run: it
// partitions a real multi-GiB budget, commits it all-or-nothing,
// first-touches every byte and releases everything. It commits real
// memory, so it only runs when explicitly opted in:
//
//	CPU_TEST_SMOKE_GB=4 go test -count=1 -run TestMultiGBSmokeOptIn ./internal/memtest/
//
// The fixed request is honored or refused - never quietly reduced -
// and the refusal message says which constraint bound it.
func TestMultiGBSmokeOptIn(t *testing.T) {
	gb := os.Getenv("CPU_TEST_SMOKE_GB")
	if gb == "" {
		t.Skip("test level 4 (destructive/long, opt-in only): set CPU_TEST_SMOKE_GB=<GiB> to run the multi-gigabyte smoke (it commits real memory)")
	}
	n, err := strconv.Atoi(gb)
	if err != nil || n < 1 || n > 16 {
		t.Fatalf("CPU_TEST_SMOKE_GB=%q: want an integer number of GiB between 1 and 16", gb)
	}

	snap := sysinfo.MemorySnapshotNow()
	params := DefaultTrialParams()
	req, err := ParseRequest(fmt.Sprintf("%dGiB", n))
	if err != nil {
		t.Fatal(err)
	}
	budget, err := ComputeBudget(req, snap, params)
	if err != nil {
		t.Fatalf("fixed %d GiB refused (this machine: avail phys %d, free commit %d): %v",
			n, snap.AvailPhys, snap.AvailPageFile, err)
	}
	res, err := Partition(budget,
		Layout{Workers: 2, Roles: []Role{RoleSource, RoleDest}}, params, 4096)
	if err != nil {
		t.Fatalf("partition %d bytes over 2 workers: %v", budget.Bytes, err)
	}
	if len(res.Skipped) > 0 {
		t.Fatalf("multi-GB budget skipped workers: %+v", res)
	}

	a, err := NewAllocator()
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(res.Grants, a)
	if err != nil {
		t.Fatalf("open %d bytes: %v", res.DataTotal, err)
	}
	defer func() {
		if err := e.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	prep, err := e.Prepare(context.Background(), opaqueFill())
	if err != nil {
		t.Fatalf("first touch: %v", err)
	}
	requested, allocated := e.Totals()
	if requested != res.DataTotal {
		t.Errorf("requested %d does not match the partition's data total %d", requested, res.DataTotal)
	}
	if allocated < requested {
		t.Errorf("allocated %d below requested %d", allocated, requested)
	}
	if prep.Touched != requested {
		t.Errorf("touched %d, want every requested byte (%d)", prep.Touched, requested)
	}
	for _, r := range e.Regions() {
		b := r.Block.Bytes()
		if byteAt(b, 0) != wantFill() || byteAt(b, len(b)-1) != wantFill() {
			t.Errorf("region worker=%d region=%d was not fully touched", r.Worker, r.Region)
		}
	}
	t.Logf("multi-GB smoke: budget=%d rule=%q", budget.Bytes, budget.Rule)
	t.Logf("requested=%d allocated=%d touched=%d first_touch=%v (%.2f GiB/s)",
		requested, allocated, prep.Touched, prep.Elapsed,
		float64(prep.Touched)/float64(1<<30)/prep.Elapsed.Seconds())
}
