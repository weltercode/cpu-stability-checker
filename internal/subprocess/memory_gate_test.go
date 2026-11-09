package subprocess

import (
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// requireMemoryAdmission preflights the live machine with the PRODUCT'S
// OWN budget rule - the same sysinfo snapshot and the same
// memtest.ComputeBudget the run under test applies - before a
// memory-backed test starts (audit R12).
//
// Admission is a machine capability, not a code path: when available
// physical cannot cover the request plus the OS reserve, the run under
// test honestly refuses (exit 2, exact byte counts). A test that then
// demands exit 0 fails the machine's moment, not the code - and the
// machine never got to run the code at all. The honest outcome is the
// capability skip naming the constraint, exactly like the WMI/HWiNFO
// level skips: a restricted machine reports instead of failing, while a
// machine that HAS the memory and still misbehaves fails as it must.
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

// TestMemoryGateRuleIsLive pins that the preflight rule the helper uses
// is live code, not a wish: an absurd fixed request must be refused on
// any machine of this class, which is the exact condition that drives
// the skip branch. (The refusal message itself, and the budget rule
// underneath, are pinned in internal/memtest/budget_test.go.)
func TestMemoryGateRuleIsLive(t *testing.T) {
	snap := sysinfo.MemorySnapshotNow()
	if !snap.Known {
		t.Skip("memory is not observed on this platform: the budget layer's honest no-observation path applies")
	}
	if _, err := memtest.ComputeBudget(memtest.Request{Bytes: 60 << 30}, snap, memtest.DefaultTrialParams()); err == nil {
		t.Errorf("a 60 GiB fixed request must be refused on this machine (available physical %d B): the skip branch would be dead code", snap.AvailPhys)
	}
}
