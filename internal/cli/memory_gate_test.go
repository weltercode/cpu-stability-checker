package cli

import (
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// requireMemoryAdmission preflights the live machine with the PRODUCT'S
// OWN budget rule - the same sysinfo snapshot and the same
// memtest.ComputeBudget the child run applies - before a
// memory-backed CLI test starts (audit R12). The CLI child would
// honestly refuse with exit 2 and exact byte counts; the parent test
// asserting exit 0 would then be failing the machine's moment, not the
// code. A restricted machine reports a capability skip naming the
// constraint (like the documented level skips); a machine that HAS the
// memory and still misbehaves fails as it must.
//
// upperBoundBytes must be the documented upper bound of what the test
// requests, so the skip never hides a refusal the test would have hit.
func requireMemoryAdmission(t *testing.T, upperBoundBytes uint64) {
	t.Helper()
	snap := sysinfo.MemorySnapshotNow()
	if _, err := memtest.ComputeBudget(memtest.Request{Bytes: upperBoundBytes}, snap, memtest.DefaultTrialParams()); err != nil {
		t.Skipf("capability skip (audit R12): this machine cannot admit the test's memory budget right now: %v", err)
	}
}

// TestMemoryGateRuleIsLive pins that the preflight rule the helper
// uses is live code: an absurd fixed request must be refused on any
// machine of this class - the exact condition that drives the skip
// branch (see internal/memtest/budget_test.go for the rule itself).
func TestMemoryGateRuleIsLive(t *testing.T) {
	snap := sysinfo.MemorySnapshotNow()
	if !snap.Known {
		t.Skip("memory is not observed on this platform: the budget layer's honest no-observation path applies")
	}
	if _, err := memtest.ComputeBudget(memtest.Request{Bytes: 60 << 30}, snap, memtest.DefaultTrialParams()); err == nil {
		t.Errorf("a 60 GiB fixed request must be refused on this machine (available physical %d B): the skip branch would be dead code", snap.AvailPhys)
	}
}
