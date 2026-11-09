// Package runctl is the shared safety lifecycle of one run: signal
// cancel, temperature preflight and cutoff, WHEA monitoring with a
// bounded final drain, stop-on-fault, and the run-level findings those
// observers produce. All three executors (threaded, per-process,
// transition sessions) run their workers inside one Guard, so the
// same MaxTempC / StopOnFault / WHEA settings mean the same thing on
// every path - the audit's A01 was exactly this drift: the transition
// engine carried none of it.
//
// The Guard owns the STOP decision; engines own their workers and the
// panel. It never paints the screen: notices route through the
// caller's Notice function (the panel's writer when one is live).
package runctl

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/whea"
)

// WheaMonitor is the observation surface the Guard needs. The real
// *whea.Monitor implements it; tests inject a scripted fake so the
// WHEA policy can be proven without the Windows event log.
type WheaMonitor interface {
	Events() <-chan whea.Event
	State() whea.State
	Close()
}

// WheaFactory starts the observation for one run (persist is the
// durability gate: an event is acknowledged only after the journal
// kept it). A nil monitor means the platform has no WHEA at all -
// that reads as unavailable, never as "no errors".
type WheaFactory func(persist func(whea.Event) error) WheaMonitor

// Deps are the injected observers. Every field is optional: an
// absent observer is recorded as unavailable, never silently skipped.
type Deps struct {
	Temp    *temperature.Monitor // nil = no thermal control possible
	Whea    WheaFactory          // nil = no WHEA observation on this platform
	Journal *journal.Writer      // nil-safe: state records simply cannot exist
	Notice  func(string)         // nil = stderr
	// OnAbort lets an engine mirror the first recorded cause into its
	// own bookkeeping (the threaded engine's run status feeds its
	// watchdog, progress UI and summary from one ledger).
	OnAbort func(kind plan.Execution, msg string)
	// Helper, when set, launches the Guard's observer goroutines so
	// the engine can confine them to its control core (the threaded
	// engine pins every helper thread off the loaded cores).
	Helper func(name string, fn func())
}

// Guard is one run's shared lifecycle. Create it with New, call
// Preflight before any worker spawns, Start once workers may run,
// and Finish before assembling the verdict.
type Guard struct {
	opt  plan.Options
	deps Deps

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	aborted     bool
	abortMsg    string
	abortKind   plan.Execution
	findings    []plan.Finding
	faults      map[int]string
	wheaEnd     time.Time // when the workload part ended: later events are "late"
	wheaCount   int64
	wheaState   string // last journaled state label (for the panel)
	wheaReason  string
	tempDone    chan struct{}
	wheaDone    chan struct{}
	sigDone     chan struct{}
	started     bool
	finished    bool
	wheaMonitor WheaMonitor

	// WHEA state-record dedup: only transitions are journaled.
	wheaWritten string
}

// --- G01: the process's single run, reachable from outside the engine ---
//
// The G01 v1 policy forbids concurrent run sessions in one process,
// so one pointer is the whole truth about which run is live. The
// pending request closes the timing gap: a GUI cancel can arrive
// after the "started" event but before the engine reached New (or
// between child runs), and an arrived cancel must never be lost. The
// slot is scoped to ONE run session though: when that session's control
// reader stops, it clears whatever its own channel produced - past the
// session's end such a request is a dead GUI's ghost waiting to strike
// the next unrelated run in a long-lived process (audit A01's shuffle
// repro: a menu host or test binary runs many).
var (
	currentGuard  atomic.Pointer[Guard]
	pendingCancel atomic.Pointer[cancelRequest]
)

type cancelRequest struct {
	kind plan.Execution
	msg  string
}

// Current returns the Guard of the run this process is executing
// (nil before the engine creates one and after the process ends).
func Current() *Guard { return currentGuard.Load() }

// RequestCancel cancels the live run - or, when no live Guard
// exists (the engine has not reached New, or the last child run
// finished and the next has not begun), records the request so the
// next Guard this process creates cancels itself the moment it
// exists. An arrived cancel is never lost to timing - and a cancel
// already applied to a live Guard never lingers to strike the next
// run by accident. Idempotent at the Guard level: first cause wins.
func RequestCancel(kind plan.Execution, msg string) {
	if g := currentGuard.Load(); g != nil && !g.isDone() {
		g.Cancel(kind, msg)
		return
	}
	pendingCancel.Store(&cancelRequest{kind: kind, msg: msg})
	// A Guard may have been created between the check and the
	// store: hand it the request directly (New's own Swap may have
	// already consumed it - then this finds nothing, which is the
	// correct outcome).
	if g := currentGuard.Load(); g != nil && !g.isDone() {
		if p := pendingCancel.Swap(nil); p != nil {
			g.Cancel(p.kind, p.msg)
		}
	}
}

// ClearPendingCancel discards a pending cancel no live Guard took, and
// reports whether one was there. A managed session's control channel
// calls it as its reader stops: inside one session the pending slot is
// the G01c contract (a GUI cancel must survive the gap between phases
// and child runs); past the session's end the same slot is a ghost -
// the next Guard this process creates would cancel itself for a reason
// a GUI that is already gone sent to a run that is already over.
func ClearPendingCancel() bool {
	return pendingCancel.Swap(nil) != nil
}

// isDone reports whether this Guard's run has ended: a finished
// Guard cannot be cancelled into doing anything anymore.
func (g *Guard) isDone() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.finished
}

// New wraps parent (the engine's own cancel path - a GUI cancel
// arrives through it later) with the run's safety observers. The
// signal handler starts immediately: a run must be interruptible
// from the moment the Guard exists, including during the preflight
// wait (a blocked sensor wait must not swallow Ctrl+C).
func New(parent context.Context, opt plan.Options, deps Deps) *Guard {
	ctx, cancel := context.WithCancel(parent)
	notice := deps.Notice
	if notice == nil {
		notice = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	}
	deps.Notice = notice
	g := &Guard{
		opt: opt, deps: deps, ctx: ctx, cancel: cancel,
		faults: map[int]string{},
	}
	// G01: this process now has one run, reachable from outside the
	// engine (the managed stdin reader). A cancel that arrived
	// before any Guard existed - the gap between "started" and the
	// engine's runctl.New, or between child runs - is applied the
	// moment this Guard exists: an arrived cancel is never lost to
	// timing.
	currentGuard.Store(g)
	if p := pendingCancel.Swap(nil); p != nil {
		g.Cancel(p.kind, p.msg)
	}
	g.sigDone = make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	g.goObs("signal", func() {
		defer close(g.sigDone)
		defer signal.Stop(sigCh)
		interrupts := 0
		for {
			select {
			case <-g.ctx.Done():
				return
			case s := <-sigCh:
				interrupts++
				if interrupts >= 2 {
					// The clean stop did not land in time: the second
					// press force-quits. 130 is the signal-path exit
					// contract on every engine.
					fmt.Fprintln(os.Stderr, "second interrupt — force quit")
					os.Exit(130)
				}
				g.deps.Notice("interrupted — stopping workers (Ctrl+C again to force quit)")
				g.Cancel(plan.ExecCancelled, fmt.Sprintf("user interrupt (%v)", s))
			}
		}
	})
	return g
}

// goObs launches one observer goroutine, through the engine's Helper
// when it wants them pinned off the loaded cores.
func (g *Guard) goObs(name string, fn func()) {
	if g.deps.Helper != nil {
		g.deps.Helper(name, fn)
		return
	}
	go fn()
}

// Ctx is what every worker must watch: cancel means stop at the
// honest boundary.
func (g *Guard) Ctx() context.Context { return g.ctx }

// Abort records what ended the run WITHOUT stopping anything yet
// (first cause wins, so a fault that started the stop keeps its
// classification even when the user also pressed Ctrl+C). Engines
// use it when the fault must shape the verdict but the load policy
// says keep running (continue-on-fault).
func (g *Guard) Abort(kind plan.Execution, msg string) {
	g.mu.Lock()
	first := !g.aborted
	if first {
		g.aborted = true
		g.abortMsg = msg
		g.abortKind = kind
	}
	g.mu.Unlock()
	if first && g.deps.OnAbort != nil {
		g.deps.OnAbort(kind, msg)
	}
}

// Cancel records the cause and stops the run: every worker watching
// Ctx must stop at its honest boundary.
func (g *Guard) Cancel(kind plan.Execution, msg string) {
	g.Abort(kind, msg)
	g.cancel()
}

// Stop ends the run's work WITHOUT recording a cause: the engine
// chose to end (the schedule ran out, the children are done). The
// verdict then stays "completed" unless an observer already aborted
// it. Idempotent.
func (g *Guard) Stop() { g.cancel() }

// AbortInfo reports how the run ended: completed when nothing aborted
// it, otherwise the first recorded cause.
func (g *Guard) AbortInfo() (aborted bool, msg string, kind plan.Execution) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.aborted {
		return false, "", plan.ExecCompleted
	}
	if g.abortKind == "" {
		return true, g.abortMsg, plan.ExecRuntimeError
	}
	return true, g.abortMsg, g.abortKind
}

// Findings collects the run-level observations (WHEA records, thermal
// stops, sensor loss, reported faults) for Finalize.
func (g *Guard) Findings() []plan.Finding {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]plan.Finding(nil), g.findings...)
}

// AddFinding records one engine-side observation (a worker fault, a
// journal hole) so every report form sees the same facts.
func (g *Guard) AddFinding(f plan.Finding) {
	g.mu.Lock()
	g.findings = append(g.findings, f)
	g.mu.Unlock()
}

// ReportFault records a worker fault and applies the run's
// stop-on-fault policy: with it on, the whole run stops applying load
// instead of letting the machine ride out to a thermal shutdown. The
// fault itself is always recorded, policy or not.
func (g *Guard) ReportFault(core int, msg string) {
	g.mu.Lock()
	if _, seen := g.faults[core]; !seen {
		g.faults[core] = msg
	}
	stop := g.opt.StopOnFault
	g.mu.Unlock()
	if stop {
		g.Cancel(plan.ExecRuntimeError, fmt.Sprintf("fault on core %d: %s", core, msg))
	}
}

// Preflight runs the temperature preflight BEFORE any worker spawns.
// A missing sensor blocks a safe run (setup error); an already
// tripped limit is a runtime condition. Neither is a CPU fault. ok is
// true when the run may start; otherwise the engine finalizes the
// returned execution/finding pair.
func (g *Guard) Preflight() (ok bool, exec plan.Execution, finding *plan.Finding) {
	if g.deps.Temp == nil || g.opt.MaxTempC <= 0 {
		return true, "", nil
	}
	state := g.deps.Temp.Prepare(g.ctx, g.opt.MaxTempC)
	g.deps.Notice(state.Line())
	if state.Abort == "" {
		return true, "", nil
	}
	exec, kind := plan.ExecRuntimeError, plan.FindThermalStop
	if state.State == temperature.Lost {
		kind = plan.FindSensorLoss
	}
	if !state.Known {
		exec = plan.ExecSetupError
	}
	g.deps.Notice(state.Abort)
	return false, exec, &plan.Finding{Kind: kind, Source: "temperature", Message: state.Abort,
		Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}}
}

// Start launches the observers: the WHEA consumer (with its
// journal-gated delivery) and the thermal cutoff ticker. The signal
// handler already watches from New on; the temperature monitor itself
// must already be armed (the panel or the engine owns it) and
// Preflight prepared it.
func (g *Guard) Start() {
	if g.started {
		return
	}
	g.started = true

	// WHEA: an uncorrected hardware error record during the run is a
	// fault even if every worker exits cleanly. The policy keys on the
	// classified severity, never on a localized level string; the
	// journal is the durability gate, and the observation state
	// travels as whea_state records so "0 WHEA" stays interpretable.
	g.wheaDone = make(chan struct{})
	var wm WheaMonitor
	if g.deps.Whea != nil {
		wm = g.deps.Whea(func(ev whea.Event) error {
			if g.deps.Journal == nil {
				return nil // no journal: live delivery continues; state records cannot exist
			}
			return g.deps.Journal.Whea(ev.Provider, ev.EventID, ev.RecordID, ev.Severity, ev.Nature, ev.Summary(), ev.Time)
		})
	}
	g.wheaMonitor = wm
	if wm == nil {
		g.wheaRecord("unavailable", "WHEA is not available on this machine")
		close(g.wheaDone)
	} else {
		// The armed state, not a promise: "available" is only claimed
		// once a full query cycle has actually completed (P13 - a
		// green WHEA line from a window whose cursor query never once
		// succeeded is exactly the lie the observation states exist
		// to prevent).
		g.wheaRecord("initializing", "WHEA monitoring armed: first query cycle not completed yet")
		ch := wm.Events()
		g.goObs("whea", func() {
			defer close(g.wheaDone)
			// Publish by LABEL, not by state struct: the label is what
			// the journal carries, and the journal's dedup works on it.
			// A monitor that was already broken when the run started
			// must have that fact published, not waited on a change
			// that never happens.
			lastLabel := "initializing"
			publish := func() {
				st := wm.State()
				if label := WheaWindowState(st); label != lastLabel {
					g.wheaRecord(label, st.WindowReason)
					lastLabel = label
				}
			}
			handle := func(ev whea.Event) {
				g.mu.Lock()
				g.wheaCount++
				end := g.wheaEnd
				g.mu.Unlock()
				msg := ev.Summary()
				if !end.IsZero() && ev.ReceivedAt.After(end) {
					// Surfaced after the workload ended: the event
					// time may still sit inside the run, so the
					// phase stays ambiguous - both times are in
					// the journal and the message says which is
					// which.
					msg += fmt.Sprintf(" (late: received %s, after the run ended)",
						ev.ReceivedAt.Format(time.RFC3339))
				}
				g.deps.Notice(msg)
				publish()
				// Every new record becomes a visible finding; the
				// kind decides the verdict effect (plan.WHEAFinding).
				// Only uncorrected stops the workers: the OS names
				// the error source, not our CPU (a CPER APIC ID is
				// not our logical CPU numbering), so no finding
				// blacklists anyone.
				g.AddFinding(plan.WHEAFinding(ev.Severity, ev.Nature, msg))
				if ev.Severity == whea.SevUncorrected {
					g.Cancel(plan.ExecRuntimeError, msg)
				}
			}
			const publishEvery = time.Second
			tick := time.NewTicker(publishEvery)
			defer tick.Stop()
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						return // monitor closed: the final cycle already delivered all it had
					}
					handle(ev)
				case <-tick.C:
					publish()
				case <-g.ctx.Done():
					// Workload over: stop publishing, keep draining
					// until the monitor closes its channel - the
					// final drain must not lose late records.
					tick.Stop()
					for ev := range ch {
						handle(ev)
					}
					return
				}
			}
		})
	}

	// Thermal cutoff: sensor loss or an over-limit reading cancels
	// every worker, at any stop-on-fault setting.
	g.tempDone = make(chan struct{})
	if g.deps.Temp != nil && g.opt.MaxTempC > 0 {
		g.goObs("thermal", func() {
			defer close(g.tempDone)
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-g.ctx.Done():
					return
				case <-tick.C:
					st := g.deps.Temp.Snapshot()
					if st.Abort == "" {
						continue
					}
					g.deps.Notice(st.Abort + " — stopping workers")
					kind := plan.FindThermalStop
					if st.State == temperature.Lost {
						kind = plan.FindSensorLoss
					}
					g.AddFinding(plan.Finding{Kind: kind, Source: "temperature", Message: st.Abort,
						Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}})
					g.Cancel(plan.ExecRuntimeError, st.Abort)
					return
				}
			}
		})
	} else {
		close(g.tempDone)
	}
}

// Finish ends the run's observation window: it cancels the context
// (the workers are done or already stopped; context.CancelFunc is
// idempotent), marks the workload end, stops the WHEA poller and
// drains its bounded final cycle before the verdict, then records the
// window's final observation state. It must run BEFORE the engine
// assembles its findings: the consumer still appends late records.
func (g *Guard) Finish() {
	g.cancel()

	g.mu.Lock()
	if g.finished || !g.started {
		// Never started: there are no observers to drain; the cancel
		// above is all the cleanup there is.
		g.finished = true
		g.mu.Unlock()
		return
	}
	g.finished = true
	g.wheaEnd = time.Now().UTC()
	g.mu.Unlock()

	<-g.tempDone

	if g.wheaMonitor != nil {
		g.wheaMonitor.Close()
	}
	select {
	case <-g.wheaDone:
	case <-time.After(15 * time.Second):
		g.deps.Notice("warning: WHEA final drain did not finish in 15s: late records may be missing from the verdict")
	}
	if g.wheaMonitor != nil {
		st := g.wheaMonitor.State()
		g.wheaRecord(WheaWindowState(st), st.WindowReason)
	}
}

// WheaStateString is the current observation label for the panel
// ("" = never watched). The panel shows it even when there is no
// journal.
func (g *Guard) WheaStateString() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wheaState
}

// WheaCount is every WHEA record seen during this run (panel display).
func (g *Guard) WheaCount() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wheaCount
}

// WheaWindow snapshots the observation state and the reason recorded
// with it as ONE atomic pair under the lock. Callers at the engine's
// return boundary (the end-of-run evidence block, R19) need both
// together: separate reads could pair one window's label with another
// window's reason. State "" means this run never watched WHEA at all.
func (g *Guard) WheaWindow() (state, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.wheaState, g.wheaReason
}

// PlanHash identifies the plan this Guard wraps: it is set on the
// options before the engine runs and journaled into run_meta. The
// evidence capture compares it against the phase being closed, so a
// leftover Guard from an earlier run (the slot survives the run)
// never speaks for a phase that never had it. opt is immutable after
// New, so this needs no lock.
func (g *Guard) PlanHash() string { return g.opt.PlanHash }

// wheaRecord journals an observation-state transition (deduped: only
// changes are written).
func (g *Guard) wheaRecord(state, reason string) {
	g.mu.Lock()
	g.wheaState = state
	g.wheaReason = reason
	dup := state+"\x00"+reason == g.wheaWritten
	if !dup {
		g.wheaWritten = state + "\x00" + reason
	}
	g.mu.Unlock()
	if !dup && g.deps.Journal != nil {
		_ = g.deps.Journal.WheaState(state, reason)
	}
}

// WheaWindowState maps the monitor's state onto the journal
// vocabulary. It reports the window verdict, not the last-cycle
// snapshot: one bad moment makes the whole window suspect. Engines
// that journal WHEA state outside a Guard use the same mapping.
// "initializing" means armed but unproven: no full query cycle has
// completed yet, so no coverage claim is possible either way.
// "degraded" requires an actual failure or gap - it is what a closed
// short window (a transition child that ended before its first cycle
// finished) must NOT claim: nothing went wrong there, nothing was
// proven. A failing cycle always marks WindowDegraded sticky, so a
// state that is neither proven nor degraded has simply never
// completed a cycle: initializing, in the window as in the run.
func WheaWindowState(st whea.State) string {
	switch {
	case st.WindowDegraded:
		return "degraded"
	case st.Proven:
		return "available"
	default:
		return "initializing"
	}
}
