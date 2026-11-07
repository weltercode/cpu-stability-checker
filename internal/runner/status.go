package runner

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/human"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// coreStatus is the live, concurrency-safe state for one core under test.
// Workloads report into it via the workload.Reporter interface.
type coreStatus struct {
	core     int
	workload string
	iters    atomic.Uint64
	lastBeat atomic.Int64 // unix nano
	tid      atomic.Int64 // OS thread id of the slot's first worker (0 = unknown)
	status   atomic.Value // string: latest workload self-report (external tools)
	evidence atomic.Value // string: latest protocol evidence state (external adapters)
	loadMu   sync.Mutex
	loadFrom time.Time
	loadTo   time.Time
}

// Concurrent workers share a CPU slot: count the union of their load
// windows, not the sum of their durations.
func (c *coreStatus) recordLoad(from, to time.Time) {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	if c.loadFrom.IsZero() || from.Before(c.loadFrom) {
		c.loadFrom = from
	}
	if to.After(c.loadTo) {
		c.loadTo = to
	}
}

func (c *coreStatus) loadDuration() time.Duration {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	if c.loadFrom.IsZero() {
		return 0
	}
	return max(time.Duration(0), c.loadTo.Sub(c.loadFrom))
}

func (c *coreStatus) Add(n uint64)        { c.iters.Add(n) }
func (c *coreStatus) Beat()               { c.lastBeat.Store(time.Now().UnixNano()) }
func (c *coreStatus) itersNow() uint64    { return c.iters.Load() }
func (c *coreStatus) lastBeatNano() int64 { return c.lastBeat.Load() }

// ReportEvidence implements the optional workload.EvidenceReporter:
// external adapters report here what the tool's protocol proved
// (started / partial / confirmed-complete / failed). The last state
// reported wins; nothing is ever inferred from liveness alone.
func (c *coreStatus) ReportEvidence(state string) { c.evidence.Store(state) }

func (c *coreStatus) evidenceNow() string {
	if v, ok := c.evidence.Load().(string); ok {
		return v
	}
	return ""
}

// Status implements the optional workload.StatusReporter: external tools
// print what they are doing (y-cruncher names each test as it finishes);
// the live panel shows the latest report.
func (c *coreStatus) Status(text string) { c.status.Store(text) }

func (c *coreStatus) statusNow() string {
	if v, ok := c.status.Load().(string); ok {
		return v
	}
	return ""
}

// faultCount reports how many cores have faulted so far (live panel only;
// it never influences the run).
func (rs *runStatus) faultCount() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.faults)
}

// lastFault is the newest recorded core fault (core, message, moment):
// what the live panel's red context line names. The counter says how
// many; this says where and when.
func (rs *runStatus) lastFault() (core int, msg string, at time.Time, ok bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for c, f := range rs.faults {
		if !ok || f.at.After(at) {
			core, msg, at, ok = c, f.msg, f.at, true
		}
	}
	return
}

// faultInfo is a per-core first fault plus what kind of fact it was
// and WHEN it landed (the live panel states the moment on its red
// context line; the journal carries the authoritative timestamp).
type faultInfo struct {
	msg  string
	kind plan.FindingKind
	at   time.Time
}

// runStatus aggregates every core plus run-level state (start, duration,
// faults, abort, findings) that the watchdog, progress UI and logger all
// read.
type runStatus struct {
	mu          sync.Mutex
	faults      map[int]faultInfo // core -> first fault
	setupErrors map[int]string
	// sharedSeen deduplicates the outcome of one shared external
	// process: the fact is recorded once, run-level, no matter how
	// many slots followed it. sharedCores marks those slots so their
	// units end as interrupted instead of faulted.
	sharedSeen  map[string]bool
	sharedCores map[int]bool
	findings    []plan.Finding // run-level facts: WHEA, thermal, shared outcomes
	abortKind   plan.Execution // what ended the run (first cause wins)
	aborted     bool
	abortMsg    string
	start       time.Time
	duration    time.Duration
	cores       []*coreStatus
}

func newRunStatus(n int, duration time.Duration) *runStatus {
	rs := &runStatus{
		faults:      make(map[int]faultInfo),
		setupErrors: make(map[int]string),
		sharedSeen:  make(map[string]bool),
		sharedCores: make(map[int]bool),
		start:       time.Now(),
		duration:    duration,
		cores:       make([]*coreStatus, 0, n),
	}
	for i := 0; i < n; i++ {
		rs.cores = append(rs.cores, &coreStatus{core: i})
	}
	return rs
}

func (rs *runStatus) recordSetupError(core int, msg string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, exists := rs.setupErrors[core]; !exists {
		rs.setupErrors[core] = msg
	}
	if !rs.aborted {
		rs.aborted, rs.abortMsg = true, fmt.Sprintf("setup error on core %d: %s", core, msg)
		rs.abortKind = plan.ExecSetupError
	}
}

func (rs *runStatus) setupInfo(core int) (string, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	msg, ok := rs.setupErrors[core]
	return msg, ok
}

func (rs *runStatus) hasSetupErrors() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.setupErrors) > 0
}

// recordFault stores the first fault for a core and flags the run as
// aborted (so the watchdog can stop things cleanly instead of letting
// the system melt). The error's own class decides the finding kind: a
// tool-identified fault is not an arithmetic mismatch, but both are
// confirmed observations pinned to this core.
func (rs *runStatus) recordFault(core int, err error) {
	kind := plan.FindDataMismatch
	if workload.IsExternalFault(err) {
		kind = plan.FindToolFailure
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.faults[core]; ok {
		return
	}
	rs.faults[core] = faultInfo{msg: err.Error(), kind: kind, at: time.Now()}
	if !rs.aborted {
		rs.aborted = true
		rs.abortMsg = fmt.Sprintf("fault on core %d: %s", core, err.Error())
		rs.abortKind = plan.ExecRuntimeError
	}
}

// recordSharedOutcome folds the outcome of one shared external process
// into a single run-level finding. The slots that followed it are
// marked interrupted: one process faulting does not prove anything
// about each CPU it served, and a blacklist of all of them would be
// invented attribution.
func (rs *runStatus) recordSharedOutcome(core int, sh *workload.SharedRunError) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sharedCores[core] = true
	if rs.sharedSeen[sh.ID] {
		return
	}
	rs.sharedSeen[sh.ID] = true
	f := plan.Finding{Source: "tool", Message: sh.Error(),
		Attribution: plan.Attribution{Kind: plan.AttrRun, Confidence: plan.ConfConfirmed}}
	if workload.IsSetupError(sh.Err) {
		f.Kind, f.Source = plan.FindSetupError, "workload"
		if !rs.aborted {
			rs.aborted, rs.abortMsg = true, sh.Error()
			rs.abortKind = plan.ExecSetupError
		}
	} else {
		f.Kind = plan.FindToolFailure
		if !rs.aborted {
			rs.aborted, rs.abortMsg = true, sh.Error()
			rs.abortKind = plan.ExecRuntimeError
		}
	}
	rs.findings = append(rs.findings, f)
}

func (rs *runStatus) isSharedCore(core int) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.sharedCores[core]
}

// addFinding records a run-level observed fact (WHEA, thermal stop,
// sensor loss).
func (rs *runStatus) addFinding(f plan.Finding) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.findings = append(rs.findings, f)
}

func (rs *runStatus) collectedFindings() []plan.Finding {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]plan.Finding(nil), rs.findings...)
}

// requestAbort sets the abort flag if not already set; the first cause
// wins, so a fault that started the stop keeps its classification even
// when the user also pressed Ctrl+C.
func (rs *runStatus) requestAbort(kind plan.Execution, msg string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !rs.aborted {
		rs.aborted = true
		rs.abortMsg = msg
		rs.abortKind = kind
	}
}

func (rs *runStatus) abortInfo() (bool, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.aborted, rs.abortMsg
}

// abortExecution reports how the run ended: completed when nothing
// aborted it, otherwise the first recorded cause.
func (rs *runStatus) abortExecution() plan.Execution {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !rs.aborted {
		return plan.ExecCompleted
	}
	if rs.abortKind != "" {
		return rs.abortKind
	}
	return plan.ExecRuntimeError
}

func (rs *runStatus) firstFault(core int) (string, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	f, ok := rs.faults[core]
	return f.msg, ok
}

func (rs *runStatus) faultKind(core int) plan.FindingKind {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.faults[core].kind
}

// progressLine builds a single log line summarizing per-core progress at a
// point in time, e.g. "running test=general elapsed=3s/30s (10%) core0=float:1.2M ...".
func progressLine(rs *runStatus, test string) string {
	elapsed := time.Since(rs.start)
	pct := 0
	if rs.duration > 0 {
		pct = int(100 * float64(elapsed) / float64(rs.duration))
		if pct > 100 {
			pct = 100
		}
	}
	var parts []string
	for _, c := range rs.cores {
		parts = append(parts, fmt.Sprintf("core%d=%s:%s", c.core, c.workload, human.Count(c.iters.Load())))
	}
	return fmt.Sprintf("running test=%s elapsed=%s/%s (%d%%) %s",
		test, truncateDur(elapsed), truncateDur(rs.duration), pct, strings.Join(parts, " "))
}

func truncateDur(d time.Duration) string {
	s := d.Round(time.Millisecond).String()
	if len(s) > 8 {
		s = d.Truncate(time.Second).String()
	}
	return s
}
