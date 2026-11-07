package subprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/runctl"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/transition"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// Q04b end-to-end on the fake poller: a REAL driven session (the
// self-exec test worker, hermetic L1) observed by a ParkingObserver
// whose source is a Go function. These tests prove the plumbing -
// Start/Window/Close tie the fold to the measured idle spans, the
// evidence reaches the outcome AND the journal with its counts intact,
// and the three statuses come from the source's real behavior - without
// ever depending on this machine's actual parking state.

func TestRunSessionBurstsParkingObserved(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-pk1")

	sched := transition.BuildSchedule(3, 80*time.Millisecond, 150*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-pk1", append([]string{"--workload", "ram-constant"}, ramGrantArgs()...)...)

	// Fake source: this CPU is parked the whole time (the honest
	// meaning of observed: the SOURCE said so; the test makes the
	// source say so on purpose).
	var polls int
	park := NewParkingObserver(0, "fake-cpuset", 25*time.Millisecond, func(target int) (bool, error) {
		polls++
		return true, nil
	})
	out := RunSessionBursts(context.Background(), argv, "rid-pk1-c0", 0, sched, transition.DefaultTimeouts(), jw, park)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr != "" || out.Fault != "" {
		t.Fatalf("dirty session: %+v", out)
	}
	if out.Parking == nil {
		t.Fatal("no parking evidence despite an active observer")
	}
	ev := out.Parking
	if ev.Status != transition.ParkingObserved {
		t.Fatalf("status = %q, want observed (reason %q)", ev.Status, ev.Reason)
	}
	if ev.IdleWindows != 3 {
		t.Errorf("measured idle windows = %d, want 3 (one per commanded burst)", ev.IdleWindows)
	}
	if ev.SamplesInIdle < 3 || ev.ParkedInIdle < 3 {
		t.Errorf("fold lost coverage: inIdle=%d parked=%d polls=%d, want >=3 each", ev.SamplesInIdle, ev.ParkedInIdle, polls)
	}
	if ev.UnsampledWindows != 0 {
		t.Errorf("a 25ms poller inside 80ms gaps must cover every window, got %d unsampled (reason %q)", ev.UnsampledWindows, ev.Reason)
	}
	if ev.FirstParked.IsZero() || ev.LastParked.Before(ev.FirstParked) {
		t.Errorf("freshness stamps wrong: %v..%v", ev.FirstParked, ev.LastParked)
	}

	// The journal must carry the SAME evidence as its own record -
	// recovered, not echoed.
	recs, iss := journal.ReadAll(jp)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := journal.RecoverRuns(recs)
	if len(runs) != 1 || len(runs[0].Cores) != 1 {
		t.Fatalf("recovery shape wrong: %+v", runs)
	}
	c := runs[0].Cores[0]
	if len(c.Parking) != 1 {
		t.Fatalf("parking_obs records = %d, want 1", len(c.Parking))
	}
	j := c.Parking[0]
	if j.Status != string(transition.ParkingObserved) || j.Source != "fake-cpuset" || j.IntervalNS != int64(25*time.Millisecond) {
		t.Errorf("parking_obs record lost identity: %+v", j)
	}
	if j.ParkedInIdle != ev.ParkedInIdle || j.IdleWindows != ev.IdleWindows || j.UnsampledWindows != ev.UnsampledWindows {
		t.Errorf("outcome and journal disagree (fold %d/%d/%d uns vs record %d/%d/%d uns)",
			ev.ParkedInIdle, ev.SamplesInIdle, ev.IdleWindows, j.ParkedInIdle, j.SamplesInIdle, j.IdleWindows)
	}
	if j.FirstParked == "" || j.LastParked == "" {
		t.Errorf("observed record must carry stamps: %+v", j)
	}
}

func TestRunSessionBurstsParkingUnavailable(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-pk2")

	sched := transition.BuildSchedule(2, 60*time.Millisecond, 150*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-pk2", append([]string{"--workload", "ram-constant"}, ramGrantArgs()...)...)

	// Fake source that only ever refuses: the session must still run
	// (observation never slows or changes the commanded story), and
	// the evidence must say UNAVAILABLE - not unconfirmed, not parked.
	boom := errors.New("fake cpuset died")
	park := NewParkingObserver(0, "fake-cpuset", 25*time.Millisecond, func(int) (bool, error) {
		return false, boom
	})
	out := RunSessionBursts(context.Background(), argv, "rid-pk2-c0", 0, sched, transition.DefaultTimeouts(), jw, park)
	jw.RunEnd(out.Exit)
	jw.Close()

	if out.SetupErr != "" || out.Fault != "" || out.BurstsDone != 2 {
		t.Fatalf("parking observation must not touch the session itself: %+v", out)
	}
	if out.Parking == nil || out.Parking.Status != transition.ParkingUnavailable {
		t.Fatalf("status = %+v, want unavailable", out.Parking)
	}
	if out.Parking.SamplesInIdle != 0 || out.Parking.ParkedInIdle != 0 {
		t.Errorf("failed polls must not read as samples: %+v", out.Parking)
	}
	recs, _ := journal.ReadAll(jp)
	runs := journal.RecoverRuns(recs)
	if c := runs[0].Cores[0]; len(c.Parking) != 1 || c.Parking[0].Status != "unavailable" {
		t.Errorf("journal must carry the unavailable record too: %+v", c.Parking)
	}
}

// A nil observer (policy off) leaves zero trace: no parking_obs
// record exists at all - the pre-Q04 journal and a policy-off run
// must be indistinguishable, because absence means "never asked".
func TestRunSessionBurstsParkingNilLeavesNoRecord(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	jp := filepath.Join(dir, "j.jsonl")
	jw := openParentJournal(t, jp, "rid-pk3")
	sched := transition.BuildSchedule(2, 60*time.Millisecond, 150*time.Millisecond, 0, 0)
	argv := sessionRunArgv(jp, "rid-pk3", append([]string{"--workload", "ram-constant"}, ramGrantArgs()...)...)
	out := RunSessionBursts(context.Background(), argv, "rid-pk3-c0", 0, sched, transition.DefaultTimeouts(), jw, nil)
	jw.RunEnd(out.Exit)
	jw.Close()
	if out.Parking != nil {
		t.Fatalf("nil observer produced evidence: %+v", out.Parking)
	}
	recs, _ := journal.ReadAll(jp)
	runs := journal.RecoverRuns(recs)
	if c := runs[0].Cores[0]; len(c.Parking) != 0 {
		t.Errorf("policy-off run wrote parking records: %+v", c.Parking)
	}
}

// Observer plumbing guard: a window whose far bound is not after its
// start is refused (half-known is not measured).
func TestParkingObserverRejectsBadWindows(t *testing.T) {
	o := NewParkingObserver(3, "fake", time.Second, func(int) (bool, error) { return true, nil })
	now := time.Now()
	o.Window(now, now)                   // zero span: not a window
	o.Window(now, now.Add(-time.Second)) // backwards: not a window
	ev := o.Close()                      // never Started: no samples either
	if ev.IdleWindows != 0 {
		t.Fatalf("bad windows accepted: %d", ev.IdleWindows)
	}
	if ev.Status != transition.ParkingUnavailable {
		t.Errorf("never polled source must be unavailable, got %q", ev.Status)
	}
}

// The verdict fold buckets each core by ITS evidence state, and a nil
// fold (the question was never asked) is its own bucket - unevidenced
// under required, but never conflated with "not parked".
func TestFoldParkingOutcomes(t *testing.T) {
	obs := func(target int) *transition.ParkingEvidence {
		return &transition.ParkingEvidence{Target: target, Status: transition.ParkingObserved, SamplesInIdle: 5, ParkedInIdle: 2}
	}
	unconf := &transition.ParkingEvidence{Target: 3, Status: transition.ParkingUnconfirmed, SamplesInIdle: 5}
	unavail := &transition.ParkingEvidence{Target: 7, Status: transition.ParkingUnavailable}
	foreign := &transition.ParkingEvidence{Target: 9, Status: transition.ParkingStatus("future_state")}

	f := FoldParkingOutcomes([]int{0, 3, 7, 9, 11}, []SessionOutcome{
		{Parking: obs(0)}, {Parking: unconf}, {Parking: unavail}, {Parking: foreign}, {Parking: nil},
	})
	if len(f.Observed) != 1 || f.Observed[0] != 0 {
		t.Errorf("observed bucket wrong: %v", f.Observed)
	}
	if len(f.Unconfirmed) != 1 || f.Unconfirmed[0] != 3 || len(f.Unavailable) != 2 ||
		f.Unavailable[0] != 7 || f.Unavailable[1] != 9 {
		t.Errorf("status buckets wrong: unconf=%v unavail=%v (unknown status must land worst-case)", f.Unconfirmed, f.Unavailable)
	}
	if len(f.NeverAsked) != 1 || f.NeverAsked[0] != 11 {
		t.Errorf("nil evidence must be NeverAsked, never unavailable: %v", f.NeverAsked)
	}
	if !f.Unevidenced() {
		t.Error("any bucket beyond Observed must block a required claim")
	}
	if f.SamplesInIdle != 10 || f.ParkedInIdle != 2 {
		t.Errorf("sample totals = %d/%d, want 10/2", f.SamplesInIdle, f.ParkedInIdle)
	}
	msg := f.Message()
	for _, want := range []string{"observed 1 of 5 core(s)", "never saw parked on cores [3]",
		"unavailable on cores [7 9]", "never asked", "[11]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fold message must name every honest bucket (%q missing): %s", want, msg)
		}
	}

	// All observed: the only evidenced shape.
	all := FoldParkingOutcomes([]int{0, 1}, []SessionOutcome{{Parking: obs(0)}, {Parking: obs(1)}})
	if all.Unevidenced() {
		t.Errorf("every core observed must satisfy the required gate: %s", all.Message())
	}
	// Empty fold from an all-nil run: unevidenced through NeverAsked.
	if !FoldParkingOutcomes([]int{0}, []SessionOutcome{{}}).Unevidenced() {
		t.Error("never-asked must count as unevidenced for required")
	}
}

// ---- executeSession level: the policy must be REAL end to end ----
//
// These drive a full ExecuteSessionResult (self-exec children, hermetic)
// with the source itself scripted through the parkingProbe seam - the
// machine's actual parking state never enters the expectation. WHEA is
// silenced to a quiet monitor so the expectation cannot be polluted by
// this machine's event log.

func swapParkingProbe(t *testing.T, fn func() ([]sysinfo.CpuSetState, error)) {
	t.Helper()
	prev := parkingProbe
	parkingProbe = fn
	t.Cleanup(func() { parkingProbe = prev })
}

func silentWheaForParking(t *testing.T) {
	t.Helper()
	prev := wheaFactoryForRun
	wheaFactoryForRun = func(int) runctl.WheaFactory {
		return func(func(whea.Event) error) runctl.WheaMonitor {
			return &fakeSessionWhea{ch: make(chan whea.Event), state: whea.State{Available: true}}
		}
	}
	t.Cleanup(func() { wheaFactoryForRun = prev })
}

func parkingSessionOpt(dir string, policy string) plan.Options {
	return plan.Options{
		TestName: "general", A: "int", Cores: []int{0}, Workers: 1, PerProcess: true,
		ControlCore: -1, BeatEvery: 50 * time.Millisecond,
		LogFile: filepath.Join(dir, "run.log"),
		// Generous idle gaps: every window comfortably outlives the
		// 200ms sampling cadence, so coverage never decides these
		// verdict expectations (timing-safe by design).
		Bursts: 2, BurstGap: 600 * time.Millisecond, BurstRun: 200 * time.Millisecond,
		Parking: policy,
	}
}

func cpusetSource(parked bool) func() ([]sysinfo.CpuSetState, error) {
	return func() ([]sysinfo.CpuSetState, error) {
		return []sysinfo.CpuSetState{{LogicalCPU: 0, Parked: parked}}, nil
	}
}

func parkingFinding(res plan.Result) (plan.Finding, bool) {
	for _, f := range res.Findings {
		if f.Kind == plan.FindParking {
			return f, true
		}
	}
	return plan.Finding{}, false
}

// required + a source that cannot answer = setup refusal BEFORE any
// journal or worker exists: exit 2 naming the fix, no run records.
func TestExecuteSessionRequiredRefusesDeadSource(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	swapParkingProbe(t, func() ([]sysinfo.CpuSetState, error) {
		return nil, errors.New("fake cpuset died before the run")
	})
	opt := parkingSessionOpt(dir, "required")
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 2 || res.Execution != plan.ExecSetupError {
		t.Fatalf("required with a dead source must refuse as setup: %+v", res)
	}
	for _, want := range []string{"cannot be queried", "\"try\"", "\"off\""} {
		if !strings.Contains(res.AbortMsg, want) {
			t.Errorf("refusal must name the fix (%q missing): %q", want, res.AbortMsg)
		}
	}
	if _, err := os.Stat(journal.Path(opt.LogFile)); !os.IsNotExist(err) {
		t.Errorf("the refusal ran before journal OpenFile: a journal file exists (err=%v)", err)
	}
}

// try + a dead source: the run still runs (observation never blocks a
// try run), and the journal says so as unavailable - not unconfirmed,
// not absent, not "not parked".
func TestExecuteSessionTryWithDeadSourceRecordsUnavailable(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, func() ([]sysinfo.CpuSetState, error) {
		return nil, errors.New("fake cpuset died mid-run")
	})
	opt := parkingSessionOpt(dir, "try")
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("a dead parking source must not change a try run's exit: %+v", res)
	}
	if res.Validation != plan.ValVerified {
		t.Errorf("try never moves the verdict: validation = %q", res.Validation)
	}
	f, ok := parkingFinding(res)
	if !ok || !strings.Contains(f.Message, "unavailable on cores [0]") {
		t.Errorf("try must carry the honest finding: %+v", res.Findings)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	c := rep.Cores[0]
	if len(c.Parking) != 1 || c.Parking[0].Status != "unavailable" {
		t.Errorf("journal must carry the unavailable fold: %+v", c.Parking)
	}
}

// required + observed evidence: the gate passes and the verified claim
// survives UNTOUCHED - the contract never downgrades honest evidence.
func TestExecuteSessionRequiredObservedKeepsVerified(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, cpusetSource(true))
	opt := parkingSessionOpt(dir, "required")
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 || res.Validation != plan.ValVerified {
		t.Fatalf("required + observed must end verified: %+v", res)
	}
	f, ok := parkingFinding(res)
	if !ok || !strings.Contains(f.Message, "observed 1 of 1 core(s)") {
		t.Errorf("observed evidence must be stated in the finding: %+v", res.Findings)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	c := rep.Cores[0]
	if len(c.Parking) != 1 || c.Parking[0].Status != "observed" || c.Parking[0].ParkedInIdle < 1 {
		t.Errorf("journal fold must show parked idle samples: %+v", c.Parking)
	}
	if rep.Validation != "verified" {
		t.Errorf("journal run_end claim must match: %q", rep.Validation)
	}
}

// required + a live source that never showed parked: exit 0 (nothing
// failed), but the verification claim drops to partial and the finding
// says WHY in the policy's own words. No upgrade, no silent pass.
func TestExecuteSessionRequiredUnconfirmedDropsToPartial(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, cpusetSource(false))
	opt := parkingSessionOpt(dir, "required")
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 {
		t.Fatalf("an unevidenced but clean required run still ends 0: %+v", res)
	}
	if res.Validation != plan.ValPartial {
		t.Fatalf("required without evidence must never claim full verification: validation = %q", res.Validation)
	}
	f, ok := parkingFinding(res)
	if !ok {
		t.Fatal("no parking finding")
	}
	for _, want := range []string{"never saw parked on cores [0]", "full verification withheld"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("finding must name the honest branch and the withheld clause (%q missing): %q", want, f.Message)
		}
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	c := rep.Cores[0]
	if len(c.Parking) != 1 || c.Parking[0].Status != "unconfirmed" {
		t.Errorf("journal fold must be unconfirmed: %+v", c.Parking)
	}
	if rep.Validation != "partial" {
		t.Errorf("journal run_end must carry the downgraded claim too: %q", rep.Validation)
	}
}

// try + the same unevidenced shape: the SAME honest note, zero verdict
// power - the difference between try and required is the promise, not
// the honesty.
func TestExecuteSessionTryUnconfirmedKeepsVerdict(t *testing.T) {
	useTestBinaryAsWorker(t)
	dir := chdirTemp(t)
	silentWheaForParking(t)
	swapParkingProbe(t, cpusetSource(false))
	opt := parkingSessionOpt(dir, "try")
	res := ExecuteSessionResult(opt, nil)
	if res.Exit != 0 || res.Validation != plan.ValVerified {
		t.Fatalf("try never changes the verdict: %+v", res)
	}
	f, ok := parkingFinding(res)
	if !ok {
		t.Fatal("try must still record the observation honestly")
	}
	if !strings.Contains(f.Message, "never saw parked on cores [0]") {
		t.Errorf("try note missing: %q", f.Message)
	}
	if strings.Contains(f.Message, "withheld") {
		t.Errorf("try never withholds anything: %q", f.Message)
	}
}
