package workload

// External workloads: the CoreCycler idea where a third-party stress program
// does the heavy lifting and self-verification, and we judge it by its exit
// code and, optionally, by its output.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/procjob"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// ExternalSpec describes a third-party stress program used as a workload.
//
// Exit-code classification:
//   - OKExit    codes mean the program completed and verified itself (default [0]);
//   - FaultExit codes mean the program reported a computation fault;
//   - anything else is treated as an unexpected failure.
//
// FaultPattern is a regular expression checked against every output line.
// A match means the program reported a fault in its output (e.g. mprime's
// "A hardware failure detected") even if it keeps running: the process is
// stopped immediately and the workload is marked as a fault. Setting it also
// captures output in timer mode.
//
// FaultFile names a file (relative to Dir) the tool writes its findings to,
// for GUI tools without console output — Prime95 on Windows appends its
// torture-test findings (self-test lines, hardware failures) to results.txt
// in plain text; results.json.txt exists but carries only PrimeNet work
// types, measured against the pinned build. The file is tailed from a
// position fixed before launch, so only lines appended during this run are
// checked against FaultPattern; old results never re-trigger.
//
// Beat selects how progress is reported to the watchdog: "timer" beats every
// second while the process lives (default), "stdout" treats each output line
// as progress (useful for chatty stress programs).
//
// Dir is the working directory of the process (empty = inherit ours). Real
// stress tools (mprime, y-cruncher) read their config and write their logs
// into their own working directory, so pointing Dir at the tool's install
// folder is what makes them usable.
//
// SingleInstance is for tools that must be one process per run: Prime95's
// GUI refuses a second instance (extra launches just exit), and y-cruncher
// already spreads across the whole machine by itself. The first slot of a
// run starts the process; every other slot follows that process (beats for
// the watchdog, same outcome) instead of starting its own.
type ExternalSpec struct {
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Description    string            `json:"description,omitempty"`
	OKExit         []int             `json:"ok_exit,omitempty"`
	FaultExit      []int             `json:"fault_exit,omitempty"`
	FaultPattern   string            `json:"fault_pattern,omitempty"`
	FaultFile      string            `json:"fault_file,omitempty"`
	Beat           string            `json:"beat,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Dir            string            `json:"dir,omitempty"`
	SingleInstance bool              `json:"single_instance,omitempty"`
	// WriteFiles writes (overwrites) files relative to Dir before the
	// process starts. Prime95 reads prime.txt and y-cruncher reads a
	// stress config file: neither takes the test selection on the
	// command line, so this is what makes presets one config entry
	// each instead of copied tool directories.
	WriteFiles map[string]string `json:"write_files,omitempty"`
	// StatusPattern is a regex matched against every output line; its
	// first capture group names what the tool just finished (y-cruncher
	// prints "Running BKT: Passed" after each algorithm). With
	// StatusSequence the panel shows the NEXT test in that list instead:
	// the tool runs its tests in exactly that order, so a completion
	// report honestly names what is running now.
	StatusPattern  string   `json:"status_pattern,omitempty"`
	StatusSequence []string `json:"status_sequence,omitempty"`
	// Adapter selects the protocol parser for this tool. Empty (or
	// "legacy") is the universal regex adapter every old config entry
	// already uses: it proves liveness and faults, never
	// verification. Version-specific adapters (y-cruncher, Prime95)
	// register by name; an unknown name is a setup error, never a
	// silent fallback that might misread the tool's output.
	Adapter string `json:"adapter,omitempty"`
}

const (
	externalBeatStdout   = "stdout"
	externalBeatInterval = time.Second
)

// externalTeardownGrace is how long an external process gets to end
// its own teardown AFTER it declared its protocol complete (the
// adapter's done action). Waiting past the claim buys nothing: the
// verification evidence is the claim itself, and stalling past it can
// outlast the phase deadline and erase the claim (measured live with
// y-cruncher mode binaries: minutes of "Waiting for threads to
// terminate..." after the completion banner). 30 s comfortably covers
// measured normal teardowns (sub-second after the banner) and cuts
// the observed stalls down to the grace itself. Tests shorten it.
const externalTeardownGraceDefault = 30 * time.Second

var externalTeardownGrace = externalTeardownGraceDefault

// classifyExit maps an external program's exit code to nil (ok) or an error
// (fault / unexpected). It is pure so it can be tested without any process.
func classifyExit(code int, spec ExternalSpec) error {
	okCodes := spec.OKExit
	if len(okCodes) == 0 {
		okCodes = []int{0}
	}
	for _, c := range okCodes {
		if c == code {
			return nil
		}
	}
	for _, c := range spec.FaultExit {
		if c == code {
			return externalFault{fmt.Errorf("external command %s reported a fault (exit code %d)", spec.Command, code)}
		}
	}
	return NewSetupError(fmt.Errorf("external command %s exited with unexpected code %d", spec.Command, code))
}

// externalOutcome interprets the result of cmd.Start/Wait. It is separate from
// run so the interpretation rules are testable on their own.
//
// The run ending (duration elapsed, Ctrl+C, stop-on-fault) cancels ctx and
// kills the process: that is never the external program's fault.
func externalOutcome(ctx context.Context, waitErr error, spec ExternalSpec) error {
	if ctx.Err() != nil {
		return nil
	}
	if waitErr == nil {
		return classifyExit(0, spec)
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return classifyExit(exitErr.ExitCode(), spec)
	}
	return NewSetupError(fmt.Errorf("external command %s: %w", spec.Command, waitErr))
}

// newCommand builds the process with the given arguments (the adapter
// decides them) and the configured working directory and environment.
func (spec ExternalSpec) newCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, spec.Command, args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		env := os.Environ()
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	return cmd
}

// run starts the external program and behaves like a built-in workload: it
// returns when ctx is done (normal stop), when the process exits early
// (classified via classifyExit), or when FaultPattern matches a line of the
// process output or of the watched FaultFile.
func (spec ExternalSpec) run(ctx context.Context, rep Reporter) (n uint64, err error) {
	defer func() {
		var fault externalFault
		if err != nil && !errors.As(err, &fault) {
			err = NewSetupError(err)
		}
	}()
	if spec.Command == "" {
		return 0, fmt.Errorf("external workload: command is empty")
	}

	if spec.SingleInstance {
		return spec.runSharedInstance(ctx, rep)
	}
	return spec.runOwnProcess(ctx, rep)
}

// sharedRun is the one process a SingleInstance workload has per run: the
// first slot to arrive owns it, later slots follow its outcome.
type sharedRun struct {
	done chan struct{}
	err  error
}

var sharedRuns = struct {
	mu sync.Mutex
	m  map[string]*sharedRun
}{m: map[string]*sharedRun{}}

func (spec ExternalSpec) runSharedInstance(ctx context.Context, rep Reporter) (uint64, error) {
	key := strings.ToLower(spec.Command) + "|" + strings.ToLower(spec.Dir)

	sharedRuns.mu.Lock()
	sh, follower := sharedRuns.m[key], false
	if existing, ok := sharedRuns.m[key]; ok {
		sh, follower = existing, true
	} else {
		sh = &sharedRun{done: make(chan struct{})}
		sharedRuns.m[key] = sh
	}
	sharedRuns.mu.Unlock()

	if follower {
		return followSharedRun(ctx, sh, rep)
	}

	// Owner: start the one process, publish the outcome, then let the next
	// run claim the key again.
	defer func() {
		sharedRuns.mu.Lock()
		delete(sharedRuns.m, key)
		sharedRuns.mu.Unlock()
	}()
	lines, err := spec.runOwnProcess(ctx, rep)
	if err != nil {
		// One process, one fact: wrap it so every follower slot reports
		// the same run-level finding instead of faulting all CPUs.
		err = &SharedRunError{ID: key, Err: err}
	}
	sh.err = err
	close(sh.done)
	return lines, err
}

// followSharedRun keeps the slot alive for the watchdog while the owner's
// process runs, and adopts the owner's outcome: a fault in the one shared
// process faults every slot, exactly like a fault in a per-slot process.
func followSharedRun(ctx context.Context, sh *sharedRun, rep Reporter) (uint64, error) {
	tick := time.NewTicker(externalBeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, nil
		case <-sh.done:
			return 0, sh.err
		case <-tick.C:
			rep.Beat()
		}
	}
}

// writeFiles lays down the tool's config files (relative to Dir) before
// launch. Contents come straight from config.json: presets differ only
// by these bytes.
func (spec ExternalSpec) writeFiles() error {
	for name, content := range spec.WriteFiles {
		path := name
		if spec.Dir != "" {
			path = filepath.Join(spec.Dir, name)
		}
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("external workload: write %q: %w", name, err)
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("external workload: write %q: %w", name, err)
		}
	}
	return nil
}

// statusText turns a status_pattern match into the panel string: with a
// configured sequence it names the NEXT test after the completed one -
// the tool runs its tests in exactly that list order, so the completion
// report honestly identifies what is running now. Without a sequence the
// captured text is shown as the tool reported it.
func (spec ExternalSpec) statusText(match []string) string {
	tag := match[0]
	if len(match) > 1 && match[1] != "" {
		tag = match[1]
	}
	if len(spec.StatusSequence) > 0 {
		for i, t := range spec.StatusSequence {
			if strings.EqualFold(t, tag) {
				return spec.StatusSequence[(i+1)%len(spec.StatusSequence)]
			}
		}
	}
	return tag
}

func (spec ExternalSpec) runOwnProcess(ctx context.Context, rep Reporter) (uint64, error) {
	if err := spec.writeFiles(); err != nil {
		return 0, err
	}
	// The adapter is the protocol side of this run: the plumbing
	// below only starts, watches and kills the process, and asks the
	// adapter what every observation means.
	ad, err := spec.adapterFor()
	if err != nil {
		return 0, err
	}

	// A private cancel lets a matched fault (output line or watched-file
	// line) stop the process without touching the caller's ctx (which would
	// look like our own stop).
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var faultMu sync.Mutex
	faultMsg := ""
	fail := func(msg string) {
		faultMu.Lock()
		if faultMsg == "" {
			faultMsg = msg
			cancel()
		}
		faultMu.Unlock()
	}

	// Fix the watched file's starting position BEFORE the process can
	// write anything: old content must not re-trigger, and this run's
	// first appended lines must not be skipped because a tailing
	// goroutine got scheduled late.
	var tailOff int64
	tailExisted := false
	if spec.FaultFile != "" && ad.watchesFile() {
		tailPath := spec.FaultFile
		if spec.Dir != "" {
			tailPath = filepath.Join(spec.Dir, spec.FaultFile)
		}
		if fh, oerr := os.Open(tailPath); oerr == nil {
			if end, serr := fh.Seek(0, io.SeekEnd); serr == nil {
				tailOff, tailExisted = end, true
			}
			fh.Close()
		}
	}

	tailDone := make(chan struct{})
	if spec.FaultFile != "" && ad.watchesFile() {
		go func() {
			defer close(tailDone)
			spec.tailFaultFile(cctx, ad, fail, tailOff, tailExisted)
		}()
	} else {
		close(tailDone)
	}

	var lines uint64
	args := ad.launchArgs()
	if ad.watchesOutput() {
		lines, err = spec.runPiped(ctx, cctx, cancel, spec.newCommand(cctx, args), spec.newJob(), rep, ad, fail)
	} else {
		lines, err = spec.runWithTimer(ctx, spec.newCommand(cctx, args), spec.newJob(), rep, ad)
	}
	// The process is done: end the tailer's wait so its final drain
	// runs and it returns. runPiped already cancels before returning;
	// runWithTimer does not, and waiting for a tailer that only exits
	// on ctx.Done without this cancel would deadlock the run.
	// cancel is idempotent.
	cancel()
	<-tailDone

	// What the adapter's protocol proved for this slot ("" = nothing
	// proved: the engine keeps the evidence unknown). Reported before
	// the fault return so a protocol-confirmed fault is also visible
	// as evidence, not only as a finding.
	if ev := ad.evidence(); ev != "" {
		if er, ok := rep.(EvidenceReporter); ok {
			er.ReportEvidence(ev)
		}
	}

	faultMu.Lock()
	fm := faultMsg
	faultMu.Unlock()
	if fm != "" {
		return lines, externalFault{errors.New(fm)}
	}
	return lines, err
}

// tailFaultFile watches a file the tool writes its findings to (Prime95's
// results.json.txt on Windows, where there is no console output to scan).
// The starting position was fixed synchronously before the process
// launched: only what this run appends matters, old results must not
// re-trigger, and this run's first appended lines are never skipped by
// a goroutine that got scheduled late.
//
// Only COMPLETE lines go to the adapter: a partial trailing line stays
// buffered until it finishes, so a fault split across writes is not
// missed and half a line never falsely matches. When the run ends, one
// final drain reads whatever is left - including a final line without
// a newline - so a last-gasp fault still reaches the verdict.
// Truncation, replacement (tools rotate by renaming), oversized lines
// and read errors are reported to stderr, never swallowed: observation
// that was lost says so.
func (spec ExternalSpec) tailFaultFile(ctx context.Context, ad runAdapter, fail func(string), startOff int64, existed bool) {
	path := spec.FaultFile
	if spec.Dir != "" {
		path = filepath.Join(spec.Dir, spec.FaultFile)
	}

	const (
		maxBlock = 64 * 1024 // bounded reads: an overnight log is never slurped at once
		maxCarry = 1 << 20   // a "line" longer than this is matched as-is, with a warning
	)

	var f *os.File
	var carry []byte
	offset := startOff
	defer func() {
		// Whatever handle is open at exit gets closed: a leaked handle
		// locks the file on Windows and hides it from cleanup and from
		// the tool's own rotation.
		if f != nil {
			f.Close()
		}
	}()
	warned := map[string]bool{}
	warn := func(what string, err error) {
		if warned[what] {
			return
		}
		warned[what] = true
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: external %s watches %s: %s: %v (observation degraded)\n", spec.Command, spec.FaultFile, what, err)
			return
		}
		fmt.Fprintf(os.Stderr, "warning: external %s watches %s: %s (observation degraded)\n", spec.Command, spec.FaultFile, what)
	}

	feed := func(line string) {
		if act := ad.fileLine(line); act.fault != "" {
			fail(act.fault)
		}
	}

	consume := func(chunk []byte, final bool) {
		carry = append(carry, chunk...)
		for {
			i := bytes.IndexByte(carry, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSpace(string(carry[:i]))
			carry = carry[i+1:]
			if line != "" {
				feed(line)
			}
		}
		if len(carry) > maxCarry {
			// A line that never ends is not a line; match it as read
			// (with a warning) rather than hide what the tool wrote.
			line := strings.TrimSpace(string(carry))
			carry = nil
			if line != "" {
				warn(fmt.Sprintf("watched line exceeded %d bytes; matched as read", maxCarry), nil)
				feed(line)
			}
		}
		if final && len(carry) > 0 {
			// Final tail: the file ended mid-line, without a newline.
			// A last-gasp fault still deserves its chance at the verdict.
			line := strings.TrimSpace(string(carry))
			carry = nil
			if line != "" {
				feed(line)
			}
		}
	}

	readPass := func(final bool) {
		if f == nil {
			nf, err := os.Open(path)
			if err != nil {
				if final || !os.IsNotExist(err) {
					warn("cannot open the watched file", err)
				}
				return // the tool has not created it yet
			}
			f = nf
			if !existed {
				offset = 0 // created after we fixed the position: all of it is this run's
			}
		}
		hfi, err := f.Stat()
		if err != nil {
			warn("cannot stat the watched file", err)
			return
		}

		// Replacement: the path may now name a different file (tools
		// rotate by renaming the old one). Our handle keeps reading
		// the old file; compare a checkpoint byte through the path and
		// switch over when it diverged. Appends never move the
		// checkpoint, so this cannot false-alarm on growth.
		if offset > 0 {
			var ours, atPath [1]byte
			_, r1 := f.ReadAt(ours[:], offset-1)
			pf, perr := os.Open(path)
			if perr != nil {
				if !os.IsNotExist(perr) {
					warn("cannot open the watched file path for the identity check", perr)
				} else {
					warn("watched file path was renamed away; still reading the open handle", nil)
				}
			} else {
				_, r2 := pf.ReadAt(atPath[:], offset-1)
				pf.Close()
				if r1 != nil || r2 != nil || ours[0] != atPath[0] {
					// The path no longer continues our file: adopt it.
					f.Close()
					nf, oerr := os.Open(path)
					if oerr != nil {
						warn("watched file was replaced but could not be reopened", oerr)
						f = nil
						return
					}
					f = nf
					offset = 0
					carry = nil
					warn("watched file was replaced mid-run; reading the new file from its start", nil)
					if hfi, err = f.Stat(); err != nil {
						warn("cannot stat the replaced watched file", err)
						return
					}
				}
			}
		}

		if hfi.Size() < offset { // truncated in place: start over, loudly
			warn("watched file was truncated; content between the old end and the truncation may have been skipped", nil)
			offset = 0
			carry = nil
		}
		for hfi.Size() > offset {
			want := min(hfi.Size()-offset, maxBlock)
			buf := make([]byte, want)
			n, rerr := f.ReadAt(buf, offset)
			if n > 0 {
				offset += int64(n)
				consume(buf[:n], final)
			}
			if rerr != nil {
				warn(fmt.Sprintf("read error at offset %d", offset), rerr)
				return
			}
			if n < int(want) {
				break
			}
		}
	}

	tick := time.NewTicker(externalBeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// The run is over: one final bounded drain so a fault in
			// the last line - newline or not - still lands before the
			// verdict is decided.
			readPass(true)
			return
		case <-tick.C:
			readPass(false)
		}
	}
}

// newJob creates the process group that keeps the whole tool tree killable.
// Best-effort: on failure the direct process is still killed by the context,
// only the descendants would survive.
func (spec ExternalSpec) newJob() *procjob.Job {
	job, err := procjob.New()
	if err != nil {
		return nil
	}
	return job
}

// noteAppliedAffinity reads back - from the OS, not from hope - what
// affinity the external process actually has, and states every gap
// between that fact and any per-CPU claim we might otherwise make.
// An allowed mask is never proof that a CPU was used: it only bounds
// where the tool could run. Unsupported processor groups, a tool that
// reset its own affinity, and an unconfined shared process are all
// said out loud, never papered over.
func (spec ExternalSpec) noteAppliedAffinity(pid int) {
	parentMask, parentOK := sysinfo.ProcessAffinity()
	childMask, err := sysinfo.ProcessAffinityOf(pid)
	maxGroups, activeCPUs, groupsOK := sysinfo.ProcessorGroups()
	for _, msg := range affinityObservations(spec, parentMask, parentOK, childMask, err, groupsOK, maxGroups, activeCPUs) {
		fmt.Fprintf(os.Stderr, "warning: %s\n", msg)
	}
}

// affinityObservations is the pure decision half of the note above, so
// the honesty rules are testable without spawning anything.
func affinityObservations(spec ExternalSpec, parentMask uint64, parentOK bool, childMask uint64, readErr error, groupsOK bool, maxGroups, activeCPUs int) []string {
	var notes []string
	// Machine-level fact first: our mask-based pinning speaks group-0
	// 64-bit masks. Beyond that boundary we say "outside pinning's
	// reach", we never truncate the mask into a false claim.
	if groupsOK && (maxGroups > 1 || activeCPUs > 64) {
		notes = append(notes, fmt.Sprintf("this machine has %d processor groups / %d active CPUs: mask-based pinning covers only group 0 (cores 0-63); anything beyond is outside what pinning can express", maxGroups, activeCPUs))
	}
	if readErr != nil {
		return append(notes, fmt.Sprintf("cannot read back the affinity of external process %s: %v: its CPU coverage is unconfirmed", spec.Command, readErr))
	}
	if childMask == 0 {
		// No group-0 affinity at all: the process may live in another
		// processor group (per-process group membership cannot be read
		// reliably, see sysinfo.ProcessorGroups) - unconfirmed, never
		// assumed covered.
		return append(notes, fmt.Sprintf("external process %s has no group-0 CPU affinity (it may live in another processor group): its CPU coverage is unconfirmed", spec.Command))
	}
	if spec.SingleInstance {
		// One process serving the whole machine is the design of these
		// tools: the mask says which CPUs it MAY use, nothing more.
		return append(notes, fmt.Sprintf("shared external process %s may use CPUs mask %#x: worker count and per-CPU coverage are not confirmed by this adapter", spec.Command, childMask))
	}
	if !parentOK {
		return append(notes, fmt.Sprintf("cannot read our own affinity to compare with external process %s: its CPU coverage is unconfirmed", spec.Command))
	}
	if childMask != parentMask {
		return append(notes, fmt.Sprintf("external process %s reset its own affinity: applied mask %#x, ours %#x: per-CPU coverage not confirmed", spec.Command, childMask, parentMask))
	}
	if !isSingleCoreMask(parentMask) {
		return append(notes, fmt.Sprintf("external process %s shares our broad CPU mask %#x: it is not confined to one core; per-CPU coverage not confirmed", spec.Command, childMask))
	}
	return notes // confined exactly to our core: still an allowed set, not usage proof, but nothing to admit
}

func isSingleCoreMask(mask uint64) bool { return mask != 0 && mask&(mask-1) == 0 }

// runWithTimer keeps the process's output discarded and reports a beat every
// second while it lives, so the watchdog sees liveness even from silent
// tools. Timer beats are liveness only - they are never iterations.
func (spec ExternalSpec) runWithTimer(ctx context.Context, cmd *exec.Cmd, job *procjob.Job, rep Reporter, ad runAdapter) (uint64, error) {
	defer job.Close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(externalBeatInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-tick.C:
				rep.Beat()
			}
		}
	}()

	if err := cmd.Start(); err != nil {
		close(stop)
		<-done
		return 0, ad.exit(err, ctx.Err() != nil)
	}
	_ = job.Attach(cmd.Process) // best-effort; the direct kill still works
	spec.noteAppliedAffinity(cmd.Process.Pid)
	err := cmd.Wait()
	close(stop)
	<-done
	return 0, ad.exit(err, ctx.Err() != nil)
}

// runPiped merges the process's stdout+stderr into a line stream and feeds
// every line to the adapter. The adapter decides what a line means: a fault
// that must stop the process now, a status update for the panel, or simple
// liveness. Lines are NEVER counted as iterations - an output line is not a
// verified check, and pretending otherwise inflated coverage with beats.
func (spec ExternalSpec) runPiped(ctx context.Context, cctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, job *procjob.Job, rep Reporter, ad runAdapter, fail func(string)) (uint64, error) {
	// One actual CloseHandle for this job, no matter who reaches it first:
	// the teardown-grace timer below and the normal path both end the tree,
	// and racing double closes could hand a recycled handle value to the OS.
	var closeOnce sync.Once
	closeJob := func() { closeOnce.Do(func() { _ = job.Close() }) }
	defer closeJob()

	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("external command %s: %w", spec.Command, err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return 0, fmt.Errorf("external command %s: %w", spec.Command, err)
	}
	_ = job.Attach(cmd.Process) // best-effort; the direct kill still works
	spec.noteAppliedAffinity(cmd.Process.Pid)
	// The parent's copy of the write end must go, otherwise the reader never
	// sees EOF when the child exits.
	pw.Close()

	// In timer beat mode liveness comes from the ticker, not from lines.
	tickerDone := make(chan struct{})
	if spec.Beat != externalBeatStdout {
		go func() {
			defer close(tickerDone)
			tick := time.NewTicker(externalBeatInterval)
			defer tick.Stop()
			for {
				select {
				case <-cctx.Done():
					return
				case <-tick.C:
					rep.Beat()
				}
			}
		}()
	} else {
		close(tickerDone)
	}

	var lines uint64
	scanDone := make(chan struct{})
	// Teardown grace: once the adapter learns the tool itself declared
	// its protocol complete, waiting for that process to die can
	// outlast the phase deadline and erase the completion claim
	// (measured live with y-cruncher). Arm a one-shot grace; when it
	// fires while the process still lives, end the tree ourselves.
	// teardownArm is written only by the scan goroutine until the scan
	// joins, then read below - no further race.
	var teardownArm *time.Timer
	var procDone, forceKilled atomic.Bool
	go func() {
		defer close(scanDone)
		defer pr.Close()
		sc := bufio.NewScanner(pr)
		// A chatty tool can emit long lines; the default 64KiB token
		// cap would stop the scan and silently lose the rest of the
		// output. Raise the cap and admit it if even that is exceeded.
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			lines++
			act := ad.line(line)
			if act.beat {
				rep.Beat()
			}
			if act.fault != "" {
				// stop applying load now, the program told us it failed
				fail(act.fault)
			}
			if act.status != "" {
				if sr, ok := rep.(StatusReporter); ok {
					sr.Status(act.status)
				}
			}
			if act.done && teardownArm == nil {
				teardownArm = time.AfterFunc(externalTeardownGrace, func() {
					if !procDone.Load() {
						forceKilled.Store(true)
						closeJob() // kill-on-close takes the whole tree
						// Where the job object is a no-op stub
						// (non-Windows), the direct kill still ends
						// the direct process; both are best-effort.
						if cmd.Process != nil {
							_ = cmd.Process.Kill()
						}
					}
				})
			}
		}
		if serr := sc.Err(); serr != nil {
			fmt.Fprintf(os.Stderr, "warning: external %s output scan stopped: %v (further output may be unread)\n", spec.Command, serr)
		}
	}()

	err = cmd.Wait()
	// The direct process is gone, but a grandchild may still hold the write
	// end of our pipe (y-cruncher's launcher spawns the real binary as a
	// child). Closing the job now terminates the whole tree, which releases
	// the pipe so the scanner can see EOF; waiting for the scanner before
	// that would deadlock behind the grandchild.
	procDone.Store(true)
	closeJob()
	// The process is gone: release the ticker goroutine before joining it,
	// otherwise runPiped would wait for a goroutine that only ends when
	// run's own deferred cancel runs.
	cancel()
	<-scanDone
	<-tickerDone
	// Safe to read now: the scan goroutine only assigned teardownArm before
	// closing scanDone (happens-before), and the timer callback never writes
	// it. Stopping a grace that already fired costs nothing.
	if teardownArm != nil {
		teardownArm.Stop()
	}
	// If the grace ended the tree, our terminate is what made the process
	// stop - and after the tool's own completion banner the exit route says
	// nothing about the verification: the same honesty rule as our own
	// deadline (ourStop). The claim stays; disclose our role, never silently
	// rewrite what the tool did before the banner.
	waitErr := err
	var exitErr *exec.ExitError
	if forceKilled.Load() && ctx.Err() == nil && errors.As(err, &exitErr) {
		fmt.Fprintf(os.Stderr, "note: external %s: teardown ended by us %s after the tool's own completion banner; the exit route after the banner is not counted\n",
			spec.Command, externalTeardownGrace)
		waitErr = nil
	}
	return lines, ad.exit(waitErr, ctx.Err() != nil)
}

// RegisterExternal turns the config's "external" section into normal registry
// entries, so external names work everywhere built-ins do:
// '-test general -a ycruncher', the menu, 'list' and the cyclic test.
// Re-registering a name replaces it; built-ins are only shadowed on purpose.
// Entries with an uncompilable fault pattern are skipped with a warning —
// a typo must not silently disable fault detection.
func RegisterExternal(specs map[string]ExternalSpec) {
	for name, spec := range specs {
		if spec.Command == "" {
			continue
		}
		// A watched findings file needs a pattern only in legacy mode:
		// a version-specific adapter knows its own protocol's fault
		// lines and does not need a hand-written regex.
		if spec.Adapter == "" && spec.FaultFile != "" && spec.FaultPattern == "" {
			fmt.Fprintf(os.Stderr, "external workload %q: fault_file needs fault_pattern; entry skipped\n", name)
			continue
		}
		// Building the adapter validates the protocol selection too:
		// a bad pattern or an unknown adapter name must not silently
		// disable fault detection or guess a protocol.
		if _, aerr := spec.adapterFor(); aerr != nil {
			fmt.Fprintf(os.Stderr, "external workload %q: %v; entry skipped\n", name, aerr)
			continue
		}
		spec := spec
		description := spec.Description
		if description == "" {
			description = fmt.Sprintf("external command: %s", spec.Command)
		}
		Register(&Workload{
			Name:         name,
			Description:  description,
			Run:          spec.run,
			Verification: VerifyExternal,
			// The adapter name is the kernel identity for external
			// engines; legacy entries carry "" (unknown protocol
			// implementation) rather than pretending to be versioned.
			KernelVersion:  spec.Adapter,
			SingleInstance: spec.SingleInstance,
		})
	}
}
