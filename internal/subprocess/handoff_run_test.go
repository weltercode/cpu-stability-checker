//go:build windows

package subprocess

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// The real two-process proof for P07e: the coordinator spawns one
// handoff worker per core (test-binary helpers, exactly like every
// other subprocess e2e here), passes the shared section through
// AdditionalInheritedHandles, and drives every block from its owner
// to a reader on another core. Small section, small blocks: the
// weight is the process choreography, not the memory.

func handoffSeams(t *testing.T) {
	t.Helper()
	prevExe, prevPrefix := selfExecutable, workerArgsPrefix
	t.Cleanup(func() { selfExecutable, workerArgsPrefix = prevExe, prevPrefix })
	selfExecutable = func() (string, error) { return os.Args[0], nil }
	workerArgsPrefix = []string{"-test.run=^TestWorkerHelperProcess$", "--"}
}

func handoffSpec(t *testing.T) (HandoffSpec, *journal.Writer) {
	t.Helper()
	jpath := journal.Path(filepath.Join(t.TempDir(), "handoff.log"))
	jf, err := journal.OpenFile(jpath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	jw := journal.NewWriter(jf, "handoff-e2e")
	t.Cleanup(func() { jw.Close() })
	return HandoffSpec{
		Cores:       []int{0, 1},
		BlockBytes:  64 * 1024,
		Blocks:      4,
		Rounds:      1,
		Seed:        42,
		Lock:        "off",
		JournalPath: jpath,
		RunID:       "handoff-e2e",
		Opt:         plan.Options{TestName: "handoff", ControlCore: -1, StopOnFault: true},
	}, jw
}

func TestHandoffE2EClean(t *testing.T) {
	handoffSeams(t)
	spec, jw := handoffSpec(t)
	var stream []leaseEvent
	spec.OnLease = func(core int, obs journal.LeaseObservation) {
		stream = append(stream, leaseEvent{core, obs})
	}

	out := RunHandoff(context.Background(), spec, jw)
	if out.SetupErr != "" {
		t.Fatalf("setup error: %s", out.SetupErr)
	}
	if out.Fault != "" {
		t.Fatalf("unexpected fault: %s", out.Fault)
	}
	if out.Interrupted {
		t.Fatal("run reported interrupted")
	}
	if out.HandoffsTried != 4 || out.HandoffsDone != 4 {
		t.Errorf("handoffs tried/done = %d/%d, want 4/4", out.HandoffsTried, out.HandoffsDone)
	}
	if !out.ChecksKnown || out.Checks != 4*16384 {
		t.Errorf("checks = %d (known=%v), want 65536 known", out.Checks, out.ChecksKnown)
	}
	if len(out.Exits) != 2 {
		t.Fatalf("worker exits = %v, want 2 entries", out.Exits)
	}
	for i, e := range out.Exits {
		if e != 0 {
			t.Errorf("worker %d exit = %d, want 0", i, e)
		}
	}
	t.Logf("cache domains: %s", out.LLCInfo)

	// Every lease step journaled: 4 writes + 4 reads, each with its
	// block/gen identity and the peer core.
	recs, iss := journal.ReadAll(spec.JournalPath)
	if iss.ReadErr != "" {
		t.Fatalf("journal read: %s", iss.ReadErr)
	}
	var leases, acked int
	seen := map[[2]uint64]bool{}
	for _, r := range recs {
		if r.Type != journal.SessionLease || r.SessionLease == nil {
			continue
		}
		leases++
		if r.SessionLease.Acked {
			acked++
		}
		if r.SessionLease.PeerCore == nil {
			t.Errorf("lease record without peer core: %+v", r.SessionLease)
		}
		seen[[2]uint64{r.SessionLease.Block, r.SessionLease.Gen}] = true
	}
	if leases != 8 || acked != 8 {
		t.Errorf("session_lease records = %d (acked %d), want 8/8", leases, acked)
	}
	if len(seen) != 4 {
		t.Errorf("distinct (block,gen) steps = %d, want 4", len(seen))
	}

	// S4: the stream sees the SAME observation the journal got, in
	// the same order (the seam fires at the journaling moment): one
	// source per step means the live view can never drift from the
	// durable artifact.
	if len(stream) != 8 {
		t.Fatalf("OnLease events = %d, want 8", len(stream))
	}
	var journaled []struct {
		core int
		obs  journal.LeaseObservation
	}
	for _, r := range recs {
		if r.Type != journal.SessionLease || r.SessionLease == nil {
			continue
		}
		core := -1
		if r.Core != nil {
			core = *r.Core
		}
		journaled = append(journaled, struct {
			core int
			obs  journal.LeaseObservation
		}{core, *r.SessionLease})
	}
	for i := range journaled {
		if stream[i].core != journaled[i].core || !reflect.DeepEqual(stream[i].obs, journaled[i].obs) {
			t.Errorf("stream event %d = %+v core %d, want the journaled %+v core %d (same value, same moment)",
				i, stream[i].obs, stream[i].core, journaled[i].obs, journaled[i].core)
		}
	}
}

// leaseEvent is one OnLease delivery: the core plus the journaled
// observation it mirrors.
type leaseEvent struct {
	core int
	obs  journal.LeaseObservation
}

// Damage injected by the PARENT between publication and verification
// must surface as a kernel fault (exit 1 worker, ERROR ack, phase
// stopped): the arbiter serialized the ordering, so the only thing
// left to explain changed bytes is the memory path itself. This is
// the "a race in the tester can never look like a hardware error"
// claim, tested from the other side.
func TestHandoffE2ETamperIsKernelFault(t *testing.T) {
	handoffSeams(t)
	spec, jw := handoffSpec(t)
	spec.tamper = func(block uint64, view []byte) {
		if block == 1 {
			view[8] ^= 0xFF
		}
	}
	var stream []leaseEvent
	spec.OnLease = func(core int, obs journal.LeaseObservation) {
		stream = append(stream, leaseEvent{core, obs})
	}

	out := RunHandoff(context.Background(), spec, jw)
	if out.Fault == "" {
		t.Fatalf("injected damage did not surface as a fault (setup=%q done=%d)", out.SetupErr, out.HandoffsDone)
	}
	if !strings.Contains(out.Fault, "lease-read fault") {
		t.Errorf("fault = %q, want the lease-read fault naming the mismatch", out.Fault)
	}
	if out.HandoffsDone >= 4 {
		t.Errorf("phase ran to completion despite the fault: done=%d", out.HandoffsDone)
	}
	sawFaultExit := false
	for _, e := range out.Exits {
		if e == 1 {
			sawFaultExit = true
		}
	}
	if !sawFaultExit {
		t.Errorf("worker exits = %v, want one worker to exit 1 (kernel fault)", out.Exits)
	}

	// S4: the live stream sees the failed step too - the loop stops
	// at the fault, so the LAST event is the broken lease, named
	// (mode, error, unacked): a GUI watching the stream never has to
	// guess whether the run stopped or stalled.
	if len(stream) == 0 {
		t.Fatal("no lease events reached the seam")
	}
	last := stream[len(stream)-1]
	if last.obs.Acked || last.obs.Mode != "lease-read" || !strings.Contains(last.obs.Error, "lease-read fault") {
		t.Errorf("last stream event = %+v, want the unacked lease-read naming the fault", last.obs)
	}
	for _, ev := range stream[:len(stream)-1] {
		if !ev.obs.Acked {
			t.Errorf("an earlier event failed too: %+v (the fault must stop the loop at its own step)", ev.obs)
		}
	}
}

// Spec refusals never spawn anything: the contracts that would
// silently change what each worker holds are refused up front.
func TestHandoffSpecRefusals(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*HandoffSpec)
		match string
	}{
		{"one core proves nothing", func(s *HandoffSpec) { s.Cores = []int{0} }, "at least two cores"},
		{"duplicate cpu proves nothing across cores", func(s *HandoffSpec) { s.Cores = []int{0, 0} }, "selected twice"},
		{"cpu that does not exist here", func(s *HandoffSpec) { s.Cores = []int{0, 9999} }, "does not exist"},
		{"ragged blocks", func(s *HandoffSpec) { s.Blocks = 5 }, "divide evenly"},
		{"fewer blocks than cores", func(s *HandoffSpec) { s.Cores = []int{0, 1, 2, 3}; s.Blocks = 2 }, "divide evenly"},
		{"non-word block size", func(s *HandoffSpec) { s.BlockBytes = 4095 }, "multiple of the 4-byte word"},
		{"zero rounds", func(s *HandoffSpec) { s.Rounds = 0 }, "at least one round"},
		{"garbage lock mode", func(s *HandoffSpec) { s.Lock = "maybe" }, "memory lock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, jw := handoffSpec(t)
			c.mut(&spec)
			out := RunHandoff(context.Background(), spec, jw)
			if out.SetupErr == "" || !strings.Contains(out.SetupErr, c.match) {
				t.Errorf("setup error = %q, want it to contain %q", out.SetupErr, c.match)
			}
			if len(out.Exits) != 0 {
				t.Errorf("refusal spawned %d workers", len(out.Exits))
			}
			_ = jw
		})
	}
}

// ExecuteHandoff is the top-level contract: the shared exit
// vocabulary (clean 0), a run_end carrying the honest validation
// claim, and a journal that recovers into the full story - parent
// lease records AND the workers' own fragments (core_done) merged by
// ReadAll. A clean handoff verified every word of every block
// independently, so "verified" is earned, not decorative.
func TestHandoffExecuteClean(t *testing.T) {
	handoffSeams(t)
	spec, _ := handoffSpec(t)

	if exit := ExecuteHandoff(spec, nil); exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	recs, iss := journal.ReadAll(spec.JournalPath)
	if iss.ReadErr != "" {
		t.Fatalf("journal read: %s", iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("runs = %d, want 1", len(reports))
	}
	rep := reports[0]
	if rep.Validation != "verified" {
		t.Errorf("validation = %q, want verified (every read was an independent full-block verify)", rep.Validation)
	}
	var leases, acked, okCores int
	for _, c := range rep.Cores {
		leases += len(c.LeaseObs)
		for _, o := range c.LeaseObs {
			if o.Acked {
				acked++
			}
		}
		if c.Status == journal.OK {
			okCores++
		}
	}
	if leases != 8 || acked != 8 {
		t.Errorf("recovered leases = %d (acked %d), want 8/8", leases, acked)
	}
	if okCores != 2 {
		t.Errorf("worker core_done OK = %d, want 2 (fragments must merge into the story)", okCores)
	}
}

// The fault path keeps the exit contract too: injected damage ->
// worker exit 1 -> run exit 1, and the validation claim is NOT
// "verified" (a mismatch happened; no claim may paper over it).
func TestHandoffExecuteFault(t *testing.T) {
	handoffSeams(t)
	spec, _ := handoffSpec(t)
	spec.tamper = func(block uint64, view []byte) {
		if block == 1 {
			view[8] ^= 0xFF
		}
	}

	if exit := ExecuteHandoff(spec, nil); exit != 1 {
		t.Fatalf("exit = %d, want 1 (kernel fault)", exit)
	}

	recs, iss := journal.ReadAll(spec.JournalPath)
	if iss.ReadErr != "" {
		t.Fatalf("journal read: %s", iss.ReadErr)
	}
	reports := journal.RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("runs = %d, want 1", len(reports))
	}
	if reports[0].Validation == "verified" {
		t.Error("a run with a kernel fault claims verified")
	}
	var failed bool
	for _, c := range reports[0].Cores {
		for _, o := range c.LeaseObs {
			if !o.Acked && strings.Contains(o.Error, "lease-read fault") {
				failed = true
			}
		}
	}
	if !failed {
		t.Error("the failed lease is not in the journal story")
	}
}

// Cancel MID-schedule (audit B09: cancellation honored during the
// lease loop): the workers were spawned and prepared, the schedule
// stops at the cancelled context instead of riding out, and every
// worker is reaped through the STOP path (exit 0, no orphan holding
// a mapping). The other half of the contract - a PRE-cancelled
// context touching nothing at all, zero spawns - is
// TestHandoffPreCancelledTouchesNothing (the audit's B09 regression
// that replaced this test's old "they were already commanded"
// expectation).
func TestHandoffInterruptedReapsAll(t *testing.T) {
	handoffSeams(t)
	spec, jw := handoffSpec(t)
	spec.Rounds = 200 // long enough that the cancel lands mid-schedule
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(500 * time.Millisecond) // after prepare, well before the schedule ends
		cancel()
	}()

	out := RunHandoff(ctx, spec, jw)
	if !out.Interrupted {
		t.Errorf("outcome = %+v, want interrupted", out)
	}
	if out.HandoffsTried >= spec.Blocks*spec.Rounds {
		t.Errorf("the schedule rode out despite the cancel: tried %d of %d", out.HandoffsTried, spec.Blocks*spec.Rounds)
	}
	if len(out.Exits) != 2 {
		t.Fatalf("worker exits = %v, want 2 reaped", out.Exits)
	}
	for i, e := range out.Exits {
		if e != 0 {
			t.Errorf("worker %d exit = %d, want 0 (STOP path, not a kill)", i, e)
		}
	}
}
