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

// yCruncherAdapter is the version-specific protocol adapter for the
// pinned y-cruncher v0.8.x stress tester. Protocol evidence, from
// primary sources measured against the pinned build (v0.8.7.9547b):
//
//   - the build's own "Command Lines.txt": startup options must precede
//     the action (skip-warnings, pause:-2, colors:0, status:none), and
//     `config <file>` / `stress -D: -TL:` are the automation entries;
//   - a live short run on this machine: "Allocating Memory..." marks
//     the real start, "Iteration: N ..." plus "Running <TAG>: Passed"
//     are the tool's self-checked iterations, and "Test Finished.
//     Waiting for threads to terminate..." is the tool's own completion
//     claim, followed by exit 0;
//   - CoreCycler's long field-tested parser for the failure side:
//     "Running <TAG>: Failed", "Error(s) encountered on logical core N"
//     and "Errors encountered. Stopping test..." - after which
//     y-cruncher stops itself and still exits 0, so the exit code is
//     never the completion signal; "Exception Encountered:" marks a
//     tool exception, not a hardware verdict.
//
// The adapter never infers completion from liveness: only the tool's
// own completion banner, seen after a real start, confirms it - the
// banner is the protocol claim, and the process ending after it is
// never evidence that adds to it (y-cruncher stops on a fault and
// still exits 0; our own deadline says nothing about the tool).
// Measured live 2026-10-24: the mode binaries can linger in their own
// "Waiting for threads to terminate..." for minutes after the banner
// (their TBB workers spin, then sleep: the machine looks idle while
// the process lives), so the banner also emits a done action: the
// plumbing gives the teardown a short grace and then ends the tree
// itself, with the exit route after that terminate not counted
// against the tool. A version banner that contradicts v0.8
// downgrades the claim to partial - another version's protocol is not
// this protocol. The banner is printed only by the launcher exe; the
// pinned mode binaries print none, and there the install's SHA-256 pin
// is what ties the binary to this protocol - the absence is disclosed
// once, never hidden.
type yCruncherAdapter struct {
	spec ExternalSpec

	reVersion   *regexp.Regexp
	reStart     *regexp.Regexp
	rePass      *regexp.Regexp
	reFail      *regexp.Regexp
	reCoreErr   *regexp.Regexp
	reStop      *regexp.Regexp
	reException *regexp.Regexp
	reComplete  *regexp.Regexp

	mu              sync.Mutex
	versionMismatch bool
	started         bool
	completeSeen    bool
	faultSeen       bool
	exceptionSeen   bool
	exitedOK        bool
	versionWarned   bool
}

func newYCruncherAdapter(spec ExternalSpec) (runAdapter, error) {
	a := &yCruncherAdapter{spec: spec}
	for _, f := range []struct {
		re  **regexp.Regexp
		pat string
	}{
		{&a.reVersion, `y-cruncher v\d+\.\d+`},
		{&a.reStart, `^Allocating Memory\.\.\.$`},
		{&a.rePass, `Running \w+: Passed`},
		{&a.reFail, `Running \w+: Failed`},
		{&a.reCoreErr, `Error\(s\) encountered on logical core \d+`},
		{&a.reStop, `Errors encountered\. Stopping test`},
		{&a.reException, `Exception Encountered:`},
		{&a.reComplete, `Test Finished\. Waiting for threads to terminate`},
	} {
		re, err := regexp.Compile(f.pat)
		if err != nil {
			return nil, fmt.Errorf("y-cruncher adapter: pattern %q: %w", f.pat, err)
		}
		*f.re = re
	}
	return a, nil
}

func init() {
	registerAdapter("ycruncher-v0.8", newYCruncherAdapter)
}

func (a *yCruncherAdapter) watchesOutput() bool { return true }
func (a *yCruncherAdapter) watchesFile() bool   { return false }

// launchArgs adds the automation startup options the manual documents,
// before the action argument: without skip-warnings the tool waits for
// ENTER, and without pause:-2 it may pause at the end (or on errors) -
// both would look like a hang to us. Anything the config already set
// for the same option (any pause: value, any colors: value...) is left
// alone: the config author wins, and we never send the tool two
// conflicting options.
func (a *yCruncherAdapter) launchArgs() []string {
	need := []string{"skip-warnings", "pause:-2", "colors:0", "status:none"}
	out := make([]string, 0, len(a.spec.Args)+len(need))
	for _, n := range need {
		key := strings.ToLower(strings.SplitN(n, ":", 2)[0]) // "pause", "colors"...
		found := false
		for _, arg := range a.spec.Args {
			aLower := strings.ToLower(arg)
			if aLower == strings.ToLower(n) || strings.HasPrefix(aLower, key+":") {
				found = true
				break
			}
		}
		if !found {
			out = append(out, n)
		}
	}
	return append(out, a.spec.Args...)
}

func (a *yCruncherAdapter) line(l string) adapterAction {
	act := adapterAction{beat: true} // every protocol line is liveness
	l = strings.TrimSpace(l)
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.reVersion.MatchString(l):
		if !strings.Contains(l, " v0.8") && !a.versionMismatch {
			a.versionMismatch = true
			fmt.Fprintf(os.Stderr, "warning: ycruncher-v0.8 adapter: tool banner %q is not v0.8: completion will not be confirmed for this protocol\n", l)
		}
	case a.reStart.MatchString(l):
		a.started = true
	case a.reFail.MatchString(l), a.reCoreErr.MatchString(l), a.reStop.MatchString(l):
		a.faultSeen = true
		act.fault = fmt.Sprintf("y-cruncher reported a fault: %q", l)
	case a.rePass.MatchString(l):
		act.status = "y-cruncher: " + l
	case a.reException.MatchString(l):
		a.exceptionSeen = true
		if !a.versionWarned { // versionWarned doubles as "exception warned once"
			a.versionWarned = true
			fmt.Fprintf(os.Stderr, "warning: y-cruncher reported an exception: %q (tool error, not a hardware verdict)\n", l)
		}
	case a.reComplete.MatchString(l):
		a.completeSeen = true
		act.done = true // arms the plumbing's teardown grace: see runPiped
	}
	return act
}

func (a *yCruncherAdapter) fileLine(string) adapterAction { return adapterAction{} }

func (a *yCruncherAdapter) exit(waitErr error, ourStop bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ourStop {
		return nil // our deadline/Ctrl+C: the tool's ending says nothing about it
	}
	if a.faultSeen {
		// y-cruncher stops itself on error and can still exit 0: the
		// fault lines are the truth, the exit code is not.
		return externalFault{fmt.Errorf("y-cruncher reported a fault and stopped (exit %v): %v", exitCodeOf(waitErr), waitErr)}
	}
	if a.exceptionSeen {
		return NewSetupError(fmt.Errorf("external command %s: y-cruncher hit an exception: tool error, not a hardware verdict", a.spec.Command))
	}
	if waitErr == nil {
		a.exitedOK = true
		return classifyExit(0, a.spec)
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return classifyExit(exitErr.ExitCode(), a.spec)
	}
	return NewSetupError(fmt.Errorf("external command %s: %w", a.spec.Command, waitErr))
}

func (a *yCruncherAdapter) evidence() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.faultSeen:
		return EvFailed
	case a.exceptionSeen:
		return "" // tool error: what it verified before dying is unknown
	case a.completeSeen && a.exitedOK && !a.versionMismatch:
		return EvConfirmed
	case a.started:
		return EvPartial // real start, iterations, but no confirmed completion
	default:
		return "" // never even started its stress: the engine keeps it unknown
	}
}

// exitCodeOf digs the exit code out of a Wait error for messages; -1 when
// the process died by signal/kill.
func exitCodeOf(waitErr error) int {
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	if waitErr == nil {
		return 0
	}
	return -1
}
