package runctl

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// fakeSensor is a scripted temperature reader: a value, or a failure,
// or a value that flips after a signal - no hardware heating required.
type fakeSensor struct {
	mu      sync.Mutex
	value   float64
	fail    bool
	flipAt  <-chan struct{} // start failing once this fires
	riseAt  <-chan struct{} // jump to hot once this fires (mid-run overheat)
	hot     float64
	flipped bool
	rose    bool
}

func (f *fakeSensor) Read() (temperature.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flipAt != nil && !f.flipped {
		select {
		case <-f.flipAt:
			f.flipped = true
		default:
		}
	}
	if f.riseAt != nil && !f.rose {
		select {
		case <-f.riseAt:
			f.rose = true
		default:
		}
	}
	if f.fail || f.flipped {
		return temperature.Reading{}, errors.New("sensor disconnected")
	}
	v := f.value
	if f.rose {
		v = f.hot
	}
	return temperature.Reading{Celsius: v, Source: "fake", Sensor: "CPU Package", At: time.Now(), CPU: true}, nil
}
func (f *fakeSensor) Close() {}

func newSensor(value float64, fail bool) *fakeSensor {
	return &fakeSensor{value: value, fail: fail}
}

// fakeWhea is a scripted WHEA observation: events pushed by the test
// pass the journal durability gate first (exactly like the real
// monitor: an event is delivered only after persist succeeded), and
// Close closes the channel like the real final drain does.
type fakeWhea struct {
	ch      chan whea.Event
	state   whea.State
	persist func(whea.Event) error
	closed  sync.Once
}

func (f *fakeWhea) Events() <-chan whea.Event { return f.ch }
func (f *fakeWhea) State() whea.State         { return f.state }
func (f *fakeWhea) Close()                    { f.closed.Do(func() { close(f.ch) }) }

// push journals the record, then delivers it to the consumer.
func (f *fakeWhea) push(ev whea.Event) {
	if f.persist != nil {
		if err := f.persist(ev); err != nil {
			return // the cursor would stay put in the real monitor; the test does not retry
		}
	}
	f.ch <- ev
}

func wheaEvent(severity, nature string, recID int) whea.Event {
	return whea.Event{
		Provider: "Microsoft-Windows-WHEA-Logger", EventID: 1, RecordID: recID,
		Severity: severity, Nature: nature, Time: time.Now().UTC(), ReceivedAt: time.Now().UTC(),
	}
}

func newGuard(t *testing.T, opt plan.Options, sensor *fakeSensor, wm WheaFactory) (*Guard, *bytes.Buffer) {
	t.Helper()
	jbuf := &bytes.Buffer{}
	jw := journal.NewWriter(jbuf, "run-test")
	deps := Deps{Journal: jw, Notice: func(string) {}}
	if sensor != nil {
		deps.Temp = temperature.NewWithReaders(200*time.Millisecond, 10*time.Millisecond, sensor)
	}
	if wm != nil {
		deps.Whea = wm
	}
	g := New(context.Background(), opt, deps)
	t.Cleanup(func() { g.cancel() })
	return g, jbuf
}

// A01 acceptance: the temperature preflight runs BEFORE workers spawn.
// A hot sensor is a runtime condition; a missing one blocks the run
// as a setup error; neither is a CPU fault.
func TestPreflightHotAndMissing(t *testing.T) {
	cases := []struct {
		name     string
		sensor   *fakeSensor
		wantExec plan.Execution
		wantKind plan.FindingKind
	}{
		{"hot sensor", newSensor(95, false), plan.ExecRuntimeError, plan.FindThermalStop},
		{"missing sensor", newSensor(60, true), plan.ExecSetupError, plan.FindSensorLoss},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, _ := newGuard(t, plan.Options{MaxTempC: 85}, c.sensor, nil)
			ok, exec, f := g.Preflight()
			if ok {
				t.Fatalf("preflight accepted an unsafe run: %+v", f)
			}
			if exec != c.wantExec {
				t.Errorf("exec = %q, want %q", exec, c.wantExec)
			}
			if f == nil || f.Kind != c.wantKind || f.Attribution.Kind != plan.AttrRun {
				t.Errorf("finding = %+v, want %q run-level", f, c.wantKind)
			}
		})
	}
}

// A limit that is not set needs no sensor at all (the honest default:
// no control claimed, no refusal).
func TestPreflightDisabledNeedsNoSensor(t *testing.T) {
	g, _ := newGuard(t, plan.Options{}, nil, nil)
	if ok, exec, f := g.Preflight(); !ok || exec != "" || f != nil {
		t.Errorf("disabled limit must not refuse: ok=%v exec=%q finding=%+v", ok, exec, f)
	}
}

// A01 acceptance: sensor loss and an over-limit reading cancel the run
// (ctx done) at ANY stop-on-fault setting, and land as findings.
func TestThermalCutoffCancelsWorkers(t *testing.T) {
	cases := []struct {
		name     string
		opt      plan.Options
		sensor   func() *fakeSensor
		wantKind plan.FindingKind
	}{
		{"overheat mid-run, stop-on-fault off", plan.Options{MaxTempC: 85}, func() *fakeSensor {
			return &fakeSensor{value: 60, riseAt: afterChan(300 * time.Millisecond), hot: 95}
		}, plan.FindThermalStop},
		{"sensor loss mid-run", plan.Options{MaxTempC: 85, StopOnFault: true}, func() *fakeSensor {
			return &fakeSensor{value: 60, flipAt: afterChan(100 * time.Millisecond)}
		}, plan.FindSensorLoss},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, _ := newGuard(t, c.opt, c.sensor(), nil)
			if ok, _, f := g.Preflight(); !ok {
				t.Fatalf("preflight refused a safe start: %+v", f)
			}
			g.Start()
			select {
			case <-g.Ctx().Done():
			case <-time.After(5 * time.Second):
				t.Fatal("thermal condition did not cancel the run")
			}
			g.Finish()
			aborted, msg, kind := g.AbortInfo()
			if !aborted || kind != plan.ExecRuntimeError || msg == "" {
				t.Errorf("abort = (%v, %q, %q), want runtime cause", aborted, msg, kind)
			}
			var found bool
			for _, f := range g.Findings() {
				if f.Kind == c.wantKind && f.Source == "temperature" {
					found = true
				}
			}
			if !found {
				t.Errorf("no %q finding: %+v", c.wantKind, g.Findings())
			}
		})
	}
}

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func afterChan(d time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(d)
		close(ch)
	}()
	return ch
}

// A01 acceptance: WHEA findings, state records and the final drain
// reach the transition path too - proven here at the shared layer.
// Only uncorrected records stop the workers; corrected PCIe is
// recorded without blame; the window state is journaled so "0 WHEA"
// stays interpretable.
func TestWheaPolicyAndDrain(t *testing.T) {
	fw := &fakeWhea{ch: make(chan whea.Event, 4), state: whea.State{Available: true, Proven: true}}
	g, jbuf := newGuard(t, plan.Options{}, nil, func(persist func(whea.Event) error) WheaMonitor {
		fw.persist = persist
		return fw
	})
	g.Start()
	fw.push(wheaEvent(whea.SevCorrected, whea.NaturePCIe, 1))
	fw.push(wheaEvent(whea.SevCorrected, whea.NatureMemory, 2))
	// give the consumer a moment before the uncorrected one cancels
	time.Sleep(50 * time.Millisecond)
	select {
	case <-g.Ctx().Done():
		t.Fatal("corrected records must not cancel the run")
	default:
	}
	fw.push(wheaEvent(whea.SevUncorrected, whea.NatureCPU, 3))
	select {
	case <-g.Ctx().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("uncorrected record did not cancel the run")
	}
	g.Finish() // Close() ends the fake's channel; the drain sees it

	var pcie, mem, cpu bool
	for _, f := range g.Findings() {
		switch f.Kind {
		case plan.FindPCIeObs:
			pcie = true
			if f.Attribution.Kind != plan.AttrRun {
				t.Errorf("PCIe finding must not blame a component: %+v", f.Attribution)
			}
		case plan.FindWHEA:
			if strings.Contains(f.Message, "pcie") {
				continue
			}
			if strings.Contains(f.Message, "memory") {
				mem = true
			} else {
				cpu = true
			}
		}
	}
	if !pcie || !mem || !cpu {
		t.Errorf("findings = %+v, want pcie-obs + corrected-memory + uncorrected-cpu", g.Findings())
	}
	aborted, _, kind := g.AbortInfo()
	if !aborted || kind != plan.ExecRuntimeError {
		t.Errorf("abort = (%v, %q), want runtime", aborted, kind)
	}
	if g.WheaCount() != 3 {
		t.Errorf("WheaCount = %d, want 3", g.WheaCount())
	}
	text := jbuf.String()
	for _, want := range []string{`"type":"whea"`, `"type":"whea_state"`, `"result":"available"`} {
		if !strings.Contains(text, want) {
			t.Errorf("journal missing %s:\n%s", want, text)
		}
	}
}

// No WHEA observation on this platform: the state record must say
// unavailable - "0 events" from a blind watcher is never "clean".
func TestWheaUnavailableIsRecorded(t *testing.T) {
	g, jbuf := newGuard(t, plan.Options{}, nil, nil)
	g.Start()
	g.Finish()
	if !strings.Contains(jbuf.String(), `"result":"unavailable"`) {
		t.Errorf("no unavailable state record:\n%s", jbuf.String())
	}
	if g.WheaStateString() != "unavailable" {
		t.Errorf("panel state = %q, want unavailable", g.WheaStateString())
	}
}

// P13 (mandatory tests 4+5): "WHEA available" may only be claimed once
// the monitor has COMPLETED a full successful query cycle. At Start the
// journal says only that monitoring is armed ("initializing") - a green
// line before the first query is exactly the lie this prevents. And a
// window whose queries never once succeeded ends degraded, carrying
// the monitor's own reason: coverage unavailable, never proof that no
// events exist.
func TestWheaNeverClaimsUnprovenAvailable(t *testing.T) {
	t.Run("unproven window never reads available", func(t *testing.T) {
		fw := &fakeWhea{ch: make(chan whea.Event, 1), state: whea.State{Available: true}} // baseline probe answered; no full cycle
		g, jbuf := newGuard(t, plan.Options{}, nil, func(persist func(whea.Event) error) WheaMonitor {
			fw.persist = persist
			return fw
		})
		g.Start()
		if text := jbuf.String(); strings.Contains(text, `"result":"available"`) {
			t.Fatalf("journal claimed available before any full query cycle:\n%s", text)
		} else if !strings.Contains(text, `"result":"initializing"`) {
			t.Fatalf("journal must state the armed-but-unproven state:\n%s", text)
		}
		g.Finish()
		if text := jbuf.String(); strings.Contains(text, `"result":"available"`) {
			t.Errorf("Finish still claimed available from an unproven window:\n%s", text)
		}
		if st := g.WheaStateString(); st != "initializing" {
			t.Errorf("panel state = %q, want initializing", st)
		}
	})
	t.Run("failed queries publish degraded without any event", func(t *testing.T) {
		// A real monitor marks every failure sticky (noteFailure):
		// Failures counted, WindowDegraded set, Available false. The
		// fake carries the same shape - "never claimed available" and
		// "closed before any cycle" are DIFFERENT states (see
		// TestWheaWindowStateMapping) and this window failed, so it
		// is degraded.
		fw := &fakeWhea{ch: make(chan whea.Event, 1), state: whea.State{
			Failures:       1,
			WindowReason:   "cursor query: exit status 15001: wevtutil: The specified query is invalid.",
			WindowDegraded: true,
			Reason:         "cursor query: exit status 15001: wevtutil: The specified query is invalid.",
		}}
		g, jbuf := newGuard(t, plan.Options{}, nil, func(persist func(whea.Event) error) WheaMonitor {
			fw.persist = persist
			return fw
		})
		g.Start()
		// No WHEA event ever arrives (the common case): the STATE must
		// still reach the journal by itself, within one publish tick -
		// not only at Finish, so a report can tell when coverage broke.
		deadline := time.Now().Add(3 * time.Second)
		for !strings.Contains(jbuf.String(), `"result":"degraded"`) {
			if time.Now().After(deadline) {
				t.Fatalf("degraded state never published without an event:\n%s", jbuf.String())
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !strings.Contains(jbuf.String(), "15001") {
			t.Errorf("journal must carry the monitor's own reason: %s", jbuf.String())
		}
		g.Finish()
	})
}

// The label mapping itself, pinned as a table. The incident that
// matters: a transition child session can be shorter than the first
// WHEA query cycle - the Guard starts, no cycle completes, Finish
// closes the monitor. The old mapping read "degraded" for that child:
// a window with NOTHING failed claiming a coverage failure it never
// had. Neither proven nor broken is initializing - in the console
// line and in the journal.
func TestWheaWindowStateMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   whea.State
		want string
	}{
		{"never cycled (short child)", whea.State{}, "initializing"},
		{"baseline answered only", whea.State{Available: true}, "initializing"},
		{"full cycle done", whea.State{Available: true, Proven: true}, "available"},
		{"proven then a gap", whea.State{Proven: true, Available: false, WindowDegraded: true, WindowReason: "suspected rollover"}, "degraded"},
		{"proven then recovered still suspect", whea.State{Proven: true, Available: true, WindowDegraded: true, WindowReason: "log epoch change"}, "degraded"},
		{"platform without WHEA at all", whea.State{WindowDegraded: true, WindowReason: "WHEA is not supported on this platform"}, "degraded"},
	} {
		if got := WheaWindowState(tc.st); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}

	// End-to-end through the Guard: a window closed before any cycle
	// completes must journal initializing, never degraded.
	t.Run("short window journals initializing, not degraded", func(t *testing.T) {
		fw := &fakeWhea{ch: make(chan whea.Event, 1), state: whea.State{}} // nothing ever ran a cycle
		g, jbuf := newGuard(t, plan.Options{}, nil, func(persist func(whea.Event) error) WheaMonitor {
			fw.persist = persist
			return fw
		})
		g.Start()
		g.Finish()
		if text := jbuf.String(); strings.Contains(text, `"result":"degraded"`) {
			t.Errorf("an unproven-but-clean window was called degraded:\n%s", text)
		}
		if st := g.WheaStateString(); st != "initializing" {
			t.Errorf("panel state = %q, want initializing", st)
		}
	})
}

// reported fault cancels the run; with it off, the fault is still
// recorded but the run continues.
func TestStopOnFaultPolicy(t *testing.T) {
	t.Run("on cancels peers", func(t *testing.T) {
		g, _ := newGuard(t, plan.Options{StopOnFault: true}, nil, nil)
		g.Start()
		g.ReportFault(3, "mismatch")
		select {
		case <-g.Ctx().Done():
		case <-time.After(2 * time.Second):
			t.Fatal("stop-on-fault did not cancel the run")
		}
		_, msg, kind := g.AbortInfo()
		if kind != plan.ExecRuntimeError || !strings.Contains(msg, "core 3") {
			t.Errorf("abort = (%q, %q), want runtime fault on core 3", msg, kind)
		}
	})
	t.Run("off records without cancelling", func(t *testing.T) {
		g, _ := newGuard(t, plan.Options{}, nil, nil)
		g.Start()
		g.ReportFault(3, "mismatch")
		select {
		case <-g.Ctx().Done():
			t.Fatal("stop-on-fault off must not cancel")
		default:
		}
		if _, msg, _ := g.AbortInfo(); msg != "" {
			t.Errorf("abort msg = %q, want none", msg)
		}
	})
}

// The first cause wins: a fault that started the stop keeps its
// classification even when the user also cancels.
func TestCancelFirstCauseWins(t *testing.T) {
	g, _ := newGuard(t, plan.Options{}, nil, nil)
	g.Cancel(plan.ExecCancelled, "user interrupt")
	g.Cancel(plan.ExecRuntimeError, "fault on core 0")
	_, msg, kind := g.AbortInfo()
	if kind != plan.ExecCancelled || msg != "user interrupt" {
		t.Errorf("abort = (%q, %q), want the first cause kept", msg, kind)
	}
}

// TestRequestCancelPendingAppliesToNextGuard pins the G01 cancel
// contract at the lifecycle level: a cancel that arrives while no
// live Guard exists must strike the next Guard the moment it is
// created, and a cancel applied to a live Guard must leave nothing
// pending that could strike a later run by accident.
func TestRequestCancelPendingAppliesToNextGuard(t *testing.T) {
	gone := New(context.Background(), plan.Options{}, Deps{})
	gone.Finish() // never started: Finish is all cleanup

	RequestCancel(plan.ExecCancelled, "early gui cancel")
	if Current() != gone {
		t.Error("New did not register the process's guard")
	}

	live := New(context.Background(), plan.Options{}, Deps{})
	defer live.Finish()
	if Current() != live {
		t.Error("the newest guard is not the current one")
	}
	ab, msg, kind := live.AbortInfo()
	if !ab || kind != plan.ExecCancelled || msg != "early gui cancel" {
		t.Errorf("the pending cancel did not reach the new guard: aborted=%v kind=%q msg=%q", ab, msg, kind)
	}

	RequestCancel(plan.ExecCancelled, "cancel while live")
	if p := pendingCancel.Load(); p != nil {
		t.Error("a cancel applied to a live guard lingered for the next run")
	}
}

// The pending slot is scoped to ONE run session (audit A01's shuffle
// repro): a cancel that found only finished Guards arms the slot -
// that is the G01 contract inside a session - and the session's close
// must discard its own residue, or the NEXT run in a long-lived
// process (menu host, test binary) cancels itself with a reason a dead
// GUI sent to a run that already ended.
func TestCancelResidueIsScopedToItsSession(t *testing.T) {
	gone := New(context.Background(), plan.Options{}, Deps{})
	gone.Finish()
	RequestCancel(plan.ExecCancelled, "dead gui eof")
	if p := pendingCancel.Load(); p == nil {
		t.Fatal("a cancel with only finished Guards must arm the pending slot (the G01 contract)")
	}
	if !ClearPendingCancel() {
		t.Error("the session close found no residue to clear")
	}
	if p := pendingCancel.Load(); p != nil {
		t.Error("ClearPendingCancel left a request armed")
	}
	if ClearPendingCancel() {
		t.Error("ClearPendingCancel reported residue that was not there")
	}
	next := New(context.Background(), plan.Options{}, Deps{})
	defer next.Finish()
	if ab, msg, _ := next.AbortInfo(); ab {
		t.Errorf("a closed session's ghost struck the next guard: %q", msg)
	}
}
