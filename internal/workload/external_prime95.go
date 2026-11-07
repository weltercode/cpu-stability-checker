package workload

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// p95Adapter is the version-specific protocol adapter for the pinned
// Prime95 v30.19 torture test. Protocol evidence, from primary sources
// measured against the pinned build (30.19b20, SHA-256 enforced at
// install):
//
//   - the build's own readme.txt: "prime95.exe -t" runs the torture
//     test, and results.txt is where "Prime95.exe writes results";
//   - a live short run on this machine (fresh directory; prime.txt
//     with StressTester=1, UsePrimenet=0, Min/MaxTortureFFT=8):
//     results.txt grows timestamp blocks "[Fri Oct 16 03:43:21 2026]"
//     plus "Self-test 8K (thread N of M) passed!" lines - each one a
//     self-checked FFT run. results.json.txt is NOT written for the
//     torture test (the JSON stream covers PrimeNet work types), so
//     the P01-era assumption of a JSON torture protocol was wrong:
//     this adapter reads results.txt;
//   - CoreCycler's long field-tested parser for the fault side:
//     "Hardware failure detected running 10752K FFT size, consult
//     stress.txt file."
//
// The honest ceiling: a torture test never completes by itself - it
// runs until stopped - and Prime95 writes no completion line (killing
// it produced none). This adapter therefore never reports
// confirmed-complete: a started, progressing run is partial evidence
// at best; only the tool's own hardware-failure line can say more
// (failed). "External validation complete" is unreachable through
// Prime95 torture by protocol, not by implementation gap. Version
// binding is the install's enforced SHA-256: the GUI exe prints no
// version banner into any stream we can read, and the adapter says so
// rather than inventing a banner check.
type p95Adapter struct {
	spec ExternalSpec

	reStamp    *regexp.Regexp // "[Fri Oct 16 03:43:21 2026]"
	reSelfPass *regexp.Regexp // "Self-test 8K (thread 1 of 2) passed!"
	reSelfFail *regexp.Regexp // "Self-test ... failed..."
	reHW       *regexp.Regexp // "Hardware failure detected ..."
	reToolErr  *regexp.Regexp // tool-level errors that are not hardware

	mu          sync.Mutex
	started     bool
	faultSeen   bool
	toolErrSeen bool
	toolErrLine string
}

func newP95Adapter(spec ExternalSpec) (runAdapter, error) {
	a := &p95Adapter{spec: spec}
	for _, f := range []struct {
		re  **regexp.Regexp
		pat string
	}{
		{&a.reStamp, `^\[\w{3} \w{3} +\d+ \d{2}:\d{2}:\d{2} \d{4}\]`},
		{&a.reSelfPass, `^Self-test \d+K \(thread \d+ of \d+\) passed!`},
		{&a.reSelfFail, `^Self-test .* failed`},
		{&a.reHW, `Hardware failure detected`},
		{&a.reToolErr, `^Error\b|Unable to `},
	} {
		re, err := regexp.Compile(f.pat)
		if err != nil {
			return nil, fmt.Errorf("prime95 adapter: pattern %q: %w", f.pat, err)
		}
		*f.re = re
	}
	return a, nil
}

func init() {
	registerAdapter("prime95-v30.19", newP95Adapter)
}

// watchesOutput is false: prime95.exe is a GUI process with nothing on
// our pipes; its protocol lives entirely in the results file.
func (a *p95Adapter) watchesOutput() bool { return false }
func (a *p95Adapter) watchesFile() bool   { return true }
func (a *p95Adapter) line(string) adapterAction {
	return adapterAction{} // not called: no output pipe
}

// launchArgs guarantees the torture-test flag: without -t prime95.exe
// opens its window and does nothing, which must not be mistaken for a
// running test.
func (a *p95Adapter) launchArgs() []string {
	for _, arg := range a.spec.Args {
		if arg == "-t" {
			return a.spec.Args
		}
	}
	return append([]string{"-t"}, a.spec.Args...)
}

// fileLine reads the results.txt protocol. The start position was
// fixed before launch (see tailFaultFile), so every line here belongs
// to this run; old results never re-trigger.
func (a *p95Adapter) fileLine(l string) adapterAction {
	act := adapterAction{beat: true} // any findings line is liveness
	l = strings.TrimSpace(l)
	if l == "" {
		return adapterAction{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.reHW.MatchString(l):
		a.faultSeen = true
		act.fault = fmt.Sprintf("Prime95 reported a hardware failure: %q", l)
	case a.reSelfFail.MatchString(l):
		a.faultSeen = true
		act.fault = fmt.Sprintf("Prime95 self-test failed: %q", l)
	case a.reSelfPass.MatchString(l):
		a.started = true
		act.status = "prime95: " + l
	case a.reToolErr.MatchString(l):
		if !a.toolErrSeen {
			a.toolErrSeen = true
			a.toolErrLine = l
			fmt.Fprintf(os.Stderr, "warning: Prime95 reported an error that is not a hardware fault: %q\n", l)
		}
		// timestamp blocks and anything unrecognized: liveness only,
		// never a claim about what was verified.
	}
	return act
}

func (a *p95Adapter) exit(waitErr error, ourStop bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ourStop {
		return nil // our deadline/Ctrl+C: the tool's ending says nothing
	}
	if a.faultSeen {
		// On a hardware failure Prime95 stops the test but keeps its
		// window open: the results line is the truth, the still-alive
		// process is not a pass.
		return externalFault{fmt.Errorf("Prime95 reported a hardware failure (exit %v): %v", exitCodeOf(waitErr), waitErr)}
	}
	if a.toolErrSeen {
		return NewSetupError(fmt.Errorf("external command %s: Prime95 reported an error: %q", a.spec.Command, a.toolErrLine))
	}
	if waitErr == nil {
		// A clean early exit is not a verdict: the torture test does
		// not self-complete, so exiting before our deadline proves
		// nothing beyond "it could run".
		return classifyExit(0, a.spec)
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return classifyExit(exitErr.ExitCode(), a.spec)
	}
	return NewSetupError(fmt.Errorf("external command %s: %w", a.spec.Command, waitErr))
}

// evidence: the honest ceiling described on the type comment - this
// function has no EvConfirmed branch on purpose.
func (a *p95Adapter) evidence() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.faultSeen:
		return EvFailed
	case a.started:
		return EvPartial // real self-tests ran, but completion has no format
	default:
		return "" // never wrote a self-test line: the engine keeps it unknown
	}
}
