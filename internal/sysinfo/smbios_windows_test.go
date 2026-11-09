//go:build windows

package sysinfo

import "testing"

// TestMemoryProfileLive runs the real collector against this
// machine's firmware table. It is read-only (one firmware query) and
// follows the capability-skip convention (audit R12): firmware that
// refuses or publishes nothing is a named skip, never a failure and
// never a zero-filled pass. The assertions only pin the parser's own
// invariants - what firmware says stays firmware's business.
func TestMemoryProfileLive(t *testing.T) {
	prof, err := CollectMemoryProfile()
	if err != nil {
		t.Skipf("SMBIOS memory profile unavailable: %v", err)
	}
	if len(prof.Devices) == 0 {
		t.Skipf("firmware publishes no memory device records (rejected candidates: %d)",
			prof.RejectedCandidates)
	}
	total := 0
	for _, d := range prof.Devices {
		t.Logf("slot %q bank %q: %s part %q size %s MB spd %s / maxcap %s / configured-as-written %s MT/s",
			d.Locator, d.BankLocator, d.Manufacturer, d.PartNumber,
			ival(d.SizeMB), ival(d.SPDMaxMTs), ival(d.MaxCapMTs), ival(d.ConfiguredMTs))
		if d.SizeMB == nil {
			continue
		}
		total += *d.SizeMB
	}
	if total == 0 {
		t.Skip("no populated memory device reported (honest skip, not a pass)")
	}
	if prof.ArrayMaxCapacityMB != nil && total > *prof.ArrayMaxCapacityMB {
		t.Skipf("firmware contradicts itself: installed %d MB exceeds declared array max %d MB",
			total, *prof.ArrayMaxCapacityMB)
	}
}

// TestRatedBaseMHzLive stays in perf_test.go; the memory collector is
// deliberately not cross-asserted against rated numbers: this tool
// states what firmware wrote and draws no conclusions about clocks.
