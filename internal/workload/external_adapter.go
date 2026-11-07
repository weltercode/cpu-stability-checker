package workload

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// This file separates WHAT an external tool's protocol means from HOW
// its process is started, watched and killed. The generic observation
// runner (external.go) hands every observation - output line, watched
// file line, process ending - to a runAdapter and only obeys its
// answers. That is what lets version-specific adapters (y-cruncher,
// Prime95) plug in later without touching process plumbing, and it
// keeps two facts apart that used to be conflated:
//
//   - the OS fact "the process started and stayed alive", and
//   - the protocol claim "the tool began / progressed / completed
//     its own verification".
//
// Protocol events an adapter can recognize (the reducer vocabulary):
// started, progress, validation progress, complete, failed,
// cancelled, tool error. An adapter only ever reports what its
// version-specific parser confirms.

// runAdapter is the protocol side of one external process run. A new
// adapter is created per process run; it is fed observations
// sequentially from the line reader and the file tailer (separate
// goroutines, so its own state must be concurrency-safe).
type runAdapter interface {
	// watchesOutput says whether output lines must be captured and
	// fed to line() (otherwise the runner discards them and keeps
	// the watchdog alive with timer beats).
	watchesOutput() bool
	// watchesFile says whether lines appended to the configured
	// FaultFile must be fed to fileLine().
	watchesFile() bool
	// launchArgs returns the process arguments to launch with. The
	// protocol side may need startup options of its own (y-cruncher
	// waits for ENTER without skip-warnings); the plumbing just obeys.
	// The legacy adapter passes the configured args through unchanged.
	launchArgs() []string
	// line interprets one merged stdout/stderr line.
	line(line string) adapterAction
	// fileLine interprets one line appended to the watched file.
	fileLine(line string) adapterAction
	// exit classifies the process ending. ourStop means WE ended the
	// run (deadline, Ctrl+C, stop-on-fault): the tool's exit code
	// then says nothing about it.
	exit(waitErr error, ourStop bool) error
	// evidence is the protocol evidence proved so far (Ev*
	// vocabulary; "" when the adapter proves nothing at all).
	evidence() string
}

// adapterAction is what the runner must do because of one observed
// line.
type adapterAction struct {
	fault  string // non-empty: the tool reported a hardware fault; stop it now
	status string // non-empty: update the panel's "what runs now"
	beat   bool   // the line shows the tool alive and working (liveness only)
	done   bool   // the tool declared its own protocol complete (completion
	// banner). The verification claim is now protocol history: what
	// happens to the process afterwards cannot add or erase it. The
	// plumbing may stop waiting for a stalled teardown (runPiped's
	// grace), and our terminate afterwards says nothing about the
	// tool - exactly like our own deadline.
}

// legacyAdapter is the universal, regex-driven adapter preserved for
// existing config entries. Its honesty rule: it never claims protocol
// start or completion, because regexes on localized output prove
// nothing about verification. Output lines keep the watchdog alive
// (Beat) but are NEVER counted as iterations - a line is not a
// verified check - and the evidence stays unknown unless a fault
// pattern or a fault exit code proves a failure (EvFailed). A clean
// legacy run is, and reports itself as, unknown.
type legacyAdapter struct {
	spec      ExternalSpec
	faultRe   *regexp.Regexp
	statusRe  *regexp.Regexp
	mu        sync.Mutex
	faultSeen bool
}

func newLegacyAdapter(spec ExternalSpec) (*legacyAdapter, error) {
	a := &legacyAdapter{spec: spec}
	if spec.FaultPattern != "" {
		re, err := regexp.Compile(spec.FaultPattern)
		if err != nil {
			return nil, fmt.Errorf("external workload %s: bad fault_pattern: %w", spec.Command, err)
		}
		a.faultRe = re
	}
	if spec.StatusPattern != "" {
		re, err := regexp.Compile(spec.StatusPattern)
		if err != nil {
			return nil, fmt.Errorf("external workload %s: bad status_pattern: %w", spec.Command, err)
		}
		a.statusRe = re
	}
	return a, nil
}

func (a *legacyAdapter) watchesOutput() bool {
	return a.spec.Beat == externalBeatStdout || a.faultRe != nil || a.statusRe != nil
}

func (a *legacyAdapter) watchesFile() bool { return a.faultRe != nil }

// launchArgs: the legacy adapter adds nothing to the launch - old
// config entries run exactly as configured.
func (a *legacyAdapter) launchArgs() []string { return a.spec.Args }

func (a *legacyAdapter) line(line string) adapterAction {
	act := adapterAction{beat: a.spec.Beat == externalBeatStdout}
	if a.faultRe != nil && a.faultRe.MatchString(line) {
		act.fault = fmt.Sprintf("external command %s output reported a fault: %q", a.spec.Command, line)
		a.noteFault()
	}
	if a.statusRe != nil {
		if m := a.statusRe.FindStringSubmatch(line); m != nil {
			act.status = a.spec.statusText(m)
		}
	}
	return act
}

func (a *legacyAdapter) fileLine(line string) adapterAction {
	var act adapterAction
	if a.faultRe != nil && a.faultRe.MatchString(line) {
		act.fault = fmt.Sprintf("external command %s reported a fault in %s: %q", a.spec.Command, a.spec.FaultFile, line)
		a.noteFault()
	}
	return act
}

func (a *legacyAdapter) noteFault() {
	a.mu.Lock()
	a.faultSeen = true
	a.mu.Unlock()
}

func (a *legacyAdapter) exit(waitErr error, ourStop bool) error {
	// Reuse the pure, tested classification: a cancelled ctx is how
	// externalOutcome recognizes "we stopped it, not the tool".
	ctx := context.Background()
	if ourStop {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		cancel()
	}
	err := externalOutcome(ctx, waitErr, a.spec)
	if err != nil {
		var fault externalFault
		if errors.As(err, &fault) {
			a.noteFault()
		}
	}
	return err
}

func (a *legacyAdapter) evidence() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.faultSeen {
		return EvFailed
	}
	return "" // a legacy adapter proved nothing: unknown, never success
}

// adapterFactories maps selectable adapter names (lower-cased) to
// constructors. Version-specific adapters register themselves here;
// the empty name is always the legacy universal adapter, so old
// config entries keep working unchanged.
var adapterFactories = map[string]func(ExternalSpec) (runAdapter, error){}

// registerAdapter makes an adapter selectable by name from config.
func registerAdapter(name string, f func(ExternalSpec) (runAdapter, error)) {
	adapterFactories[strings.ToLower(name)] = f
}

// adapterFor builds the protocol adapter for one process run. An
// empty Adapter name is the legacy adapter; an unknown name is a
// setup error - silently falling back to a protocol that might
// misread the tool is exactly the dishonesty this stage removes.
func (spec ExternalSpec) adapterFor() (runAdapter, error) {
	name := strings.ToLower(spec.Adapter)
	if name == "" || name == "legacy" {
		return newLegacyAdapter(spec)
	}
	f, ok := adapterFactories[name]
	if !ok {
		return nil, fmt.Errorf("external workload %s: unknown adapter %q: refusing to guess its protocol", spec.Command, spec.Adapter)
	}
	return f(spec)
}
