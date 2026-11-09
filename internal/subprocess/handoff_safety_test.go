//go:build windows

package subprocess

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// B09/B10 (audit): the handoff coordinator runs the SAME shared
// safety lifecycle as every other engine, refuses unsafe geometry
// and oversized sections BEFORE any mapping or spawn, and labels
// what the cpu-set view can actually promise.

// A hot sensor refuses the run BEFORE any mapping or worker exists
// (runtime condition); a missing sensor blocks it as a setup error.
// Neither is a CPU fault, and the journal must show zero lease
// activity - the preflight decides before anything touches memory.
func TestHandoffUnsafePreflightBlocksEverything(t *testing.T) {
	handoffSeams(t)
	for _, tc := range []struct {
		name    string
		value   float64
		missing bool
		exit    int
		want    string // what the honest stop reason must name
	}{
		{"hot", 95, false, 1, "temperature"},
		{"missing", 60, true, 2, "sensor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, _ := handoffSpec(t)
			spec.Opt.MaxTempC = 85
			r := &childTemperature{value: tc.value, missing: tc.missing}
			panel := dashboard.NewWithTemperature(discardFile(t), temperature.NewWithReaders(20*time.Millisecond, time.Millisecond, r))
			defer panel.Stop()
			oldErr := os.Stderr
			errR, errW, _ := os.Pipe()
			os.Stderr = errW
			drained := make(chan string, 1)
			go func() { b, _ := io.ReadAll(errR); drained <- string(b) }()
			exit := ExecuteHandoff(spec, panel)
			os.Stderr = oldErr
			errW.Close()
			stderr := <-drained
			if exit != tc.exit {
				t.Fatalf("exit = %d, want %d\n%s", exit, tc.exit, stderr)
			}
			if !strings.Contains(strings.ToLower(stderr), tc.want) {
				t.Errorf("the %q stop reason was never stated:\n%s", tc.want, stderr)
			}
			recs, iss := journal.ReadAll(spec.JournalPath)
			if iss.ReadErr != "" {
				t.Fatalf("journal read: %s", iss.ReadErr)
			}
			var leases int
			for _, r := range recs {
				if r.Type == journal.SessionLease {
					leases++
				}
			}
			if leases != 0 {
				t.Errorf("%d lease records after an unsafe preflight: workers ran", leases)
			}
		})
	}
}

// discardFile gives the fake-sensor panel a stdout it will never
// paint on (the panel is never started; this only keeps stray
// writes out of test output).
func discardFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// An uncorrected WHEA record mid-handoff stops the run through the
// shared Guard: exit 1 with a WHEA finding even though no worker
// faulted, and the lease loop halts early instead of riding out the
// schedule.
func TestHandoffWheaUncorrectedStopsRun(t *testing.T) {
	requireMemoryAdmission(t, 1<<20) // small handoff geometry; documented upper bound
	handoffSeams(t)
	fw := &fakeSessionWhea{ch: make(chan whea.Event, 4), state: whea.State{Available: true}}
	prev := wheaFactoryForRun
	wheaFactoryForRun = func(controlCore int) runctl.WheaFactory {
		return func(persist func(whea.Event) error) runctl.WheaMonitor {
			fw.persist = persist
			return fw
		}
	}
	t.Cleanup(func() { wheaFactoryForRun = prev })

	spec, _ := handoffSpec(t)
	spec.Rounds = 100 // long enough that the event lands mid-run
	go func() {
		time.Sleep(300 * time.Millisecond)
		fw.push(whea.Event{
			Provider: "Microsoft-Windows-WHEA-Logger", EventID: 1, RecordID: 78,
			Severity: whea.SevUncorrected, Nature: whea.NatureCPU,
			Time: time.Now().UTC(), ReceivedAt: time.Now().UTC(),
		})
	}()
	start := time.Now()
	if exit := ExecuteHandoff(spec, nil); exit != 1 {
		t.Fatalf("exit = %d, want 1 (uncorrected WHEA)", exit)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("the WHEA cancel did not stop the lease loop")
	}
	recs, iss := journal.ReadAll(spec.JournalPath)
	if iss.ReadErr != "" {
		t.Fatalf("journal read: %s", iss.ReadErr)
	}
	var wheaRec, leases int
	for _, r := range recs {
		if r.Type == journal.Whea {
			wheaRec++
		}
		if r.Type == journal.SessionLease {
			leases++
		}
	}
	if wheaRec == 0 {
		t.Error("the WHEA record never reached the journal")
	}
	if leases >= 2*spec.Blocks*spec.Rounds {
		t.Errorf("the lease loop rode out the whole schedule (%d leases) despite the WHEA cancel", leases)
	}
}

// A pre-cancelled context touches NOTHING: no mapping, no worker,
// no lease record (audit B09: cancellation honored on prepare too).
func TestHandoffPreCancelledTouchesNothing(t *testing.T) {
	handoffSeams(t)
	spec, _ := handoffSpec(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := RunHandoff(ctx, spec, nil)
	if !out.Interrupted {
		t.Errorf("pre-cancelled run not reported interrupted: %+v", out)
	}
	if out.SetupErr != "" {
		t.Errorf("setup error on a pre-cancelled run: %q", out.SetupErr)
	}
	if len(out.Exits) != 0 {
		t.Errorf("pre-cancelled run spawned %d workers", len(out.Exits))
	}
}

// B09: the shared section passes the SAME budget rule (physical +
// commit headroom) before CreateSharedMapping: an absurd
// blocks x block-size is refused with the constraint named, and
// nothing is allocated or spawned.
func TestHandoffBudgetAdmissionRefusesOversized(t *testing.T) {
	handoffSeams(t)
	spec, _ := handoffSpec(t)
	spec.BlockBytes = 512 << 30 // 512 GiB per block: no machine here commits this quietly
	spec.Blocks = 2
	out := RunHandoff(context.Background(), spec, nil)
	if out.SetupErr == "" || !strings.Contains(out.SetupErr, "admission") {
		t.Errorf("setup error = %q, want the budget admission refusal", out.SetupErr)
	}
	if len(out.Exits) != 0 {
		t.Errorf("oversized section spawned %d workers", len(out.Exits))
	}
}

// B10: the pairing label states llc domains and physical cores as
// SEPARATE facts, never promotes an SMT-sibling pair to a
// cross-core pair, and keeps unknown unknown.
func TestHandoffPairingLabeling(t *testing.T) {
	st := func(cpu, core, llc int) sysinfo.CpuSetState {
		return sysinfo.CpuSetState{LogicalCPU: cpu, Core: core, LastLevelCache: llc}
	}
	// 0,1 are SMT siblings of physical core 0 in llc domain 7;
	// 2,3 are siblings of core 1, also domain 7; 4 stands alone in 8.
	states := []sysinfo.CpuSetState{st(0, 0, 7), st(1, 0, 7), st(2, 1, 7), st(3, 1, 7), st(4, 2, 8)}

	info, llc, known := handoffPairing([]int{0, 1}, states, nil)
	if !known {
		t.Error("a complete view must be known")
	}
	if len(llc) == 0 {
		t.Error("no llc map returned")
	}
	if !strings.Contains(info, "SMT siblings of ONE physical core") || !strings.Contains(info, "cross-thread only, never cross-core") {
		t.Errorf("SMT-sibling pair mislabeled: %q", info)
	}

	info, _, known = handoffPairing([]int{0, 2}, states, nil)
	if !known || !strings.Contains(info, "single llc domain") || !strings.Contains(info, "2 physical cores") {
		t.Errorf("cross-core same-domain pair mislabeled: %q", info)
	}
	if strings.Contains(info, "cross-llc pairing possible") && !strings.Contains(info, "no cross-llc pairing possible") {
		t.Errorf("same-domain pair promoted: %q", info)
	}

	info, _, _ = handoffPairing([]int{0, 1, 2}, states, nil)
	if !strings.Contains(info, "2 physical cores") || !strings.Contains(info, "SMT siblings") {
		t.Errorf("mixed sibling set mislabeled: %q", info)
	}

	info, _, _ = handoffPairing([]int{2, 4}, states, nil)
	if !strings.Contains(info, "2 llc domains") || !strings.Contains(info, "2 physical cores") {
		t.Errorf("cross-domain pair mislabeled: %q", info)
	}

	info, _, known = handoffPairing([]int{0, 1}, nil, context.DeadlineExceeded)
	if known || !strings.Contains(info, "unknown") {
		t.Errorf("a failed view must stay unknown: %q known=%v", info, known)
	}
	info, _, known = handoffPairing([]int{0, 99}, states, nil)
	if known || !strings.Contains(info, "unknown") {
		t.Errorf("a core missing from the view must stay unknown: %q known=%v", info, known)
	}
}
