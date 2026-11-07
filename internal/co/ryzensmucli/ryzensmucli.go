// Package ryzensmucli is the ryzen-smu-cli backend for the CO
// contract (P10). It drives the external binary directly with
// explicit args (never through a shell), bounded by ctx deadlines and
// output caps, and parses only formats pinned to the source of the
// probed version (github.com/sp00n/ryzen-smu-cli, Program.cs):
// unknown output is reported as malformed, never guessed.
//
// The mapping trap the plan warns about is real and measured from
// that source: the tool's "logical core" numbering is its OWN
// enumeration of enabled PHYSICAL cores (CO is per physical core),
// zero-indexed in SMU topology order - NOT Windows logical CPU ids.
// The adapter therefore keeps tool ids, SMU physical indices and OS
// logical ids apart and only claims a confirmed match when evidence
// (counts + enabled/disabled pattern against our own topology)
// supports it; otherwise the mapping quality stays partial with the
// reason in notes.
//
// Tool exit codes (from Program.cs) map to typed kinds:
//
//	1 -> no_privilege      ("must be run as an administrator")
//	2 -> unsupported_cpu   (ZenStates-Core failed to initialize:
//	                        unsupported CPU or missing driver - the
//	                        tool cannot tell them apart, so neither do we)
//	3 -> unsupported_op    (no PBO offset register on this SKU)
//	4 -> bad_profile       (tool rejected the offset argument format)
//	5 -> offset_count      (more offsets than active cores)
//	6 -> bad_profile       (core index beyond the active mapping)
//	8 -> offset_count      (tool's own core mapping failed)
//
// Core disablement, PBO scalar and arbitrary SMU commands exist in
// the tool but are NOT exposed: the CO contract is offsets only.
package ryzensmucli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/procjob"
)

// Runner executes the tool. Injected in tests; the real one attaches
// the child to a kill-on-close job object so a canceled or killed
// parent never leaves a half-run SMU client behind.
type Runner interface {
	// Run returns combined stdout (stdout only, strictly) plus the
	// process exit code. A ctx deadline produces kind timeout, a ctx
	// cancel kind cancelled (B02: the two are never conflated);
	// output beyond the cap is a hard error, not a truncation.
	Run(ctx context.Context, exe string, args ...string) (stdout string, exitCode int, err error)
}

// Adapter is the ryzen-smu-cli Backend. Exe is the explicit path the
// user configured; nothing is searched in PATH silently.
type Adapter struct {
	Exe string
	Run Runner

	// MaxOutput bounds one tool answer; the pinned outputs are tiny,
	// so anything beyond this is either a different tool or a bug.
	// New hands it to the default production runner, which enforces
	// it DURING the read (B02) - an injected Runner enforces its own.
	MaxOutput int

	// exeSHA computes the launcher digest used for build validation.
	// It defaults to fileSHA256; the seam lets a test present the
	// pinned digest without shipping the real 138 KB apphost, so the
	// VALIDATED branch (version + embedded commit + exe digest all
	// matching the manifest) is testable deterministically.
	exeSHA func(path string) (string, error)

	// dllSHA computes the managed-PAYLOAD digest (audit C02). The
	// apphost exe is only the launcher: a same-version swap of the
	// paired DLL would otherwise inherit every launcher check and keep
	// BuildValidated. Same default and seam rationale as exeSHA.
	dllSHA func(path string) (string, error)

	// lastExact is the last EXACT, fingerprinted read. It is the
	// reference for the bootstrap write authority (a single-core
	// delta bounded against what the machine actually holds right
	// now) and for differential writes (only cores whose value
	// actually changes are written). Reset on every Probe.
	lastExact *co.Snapshot

	// ReceiptPath overrides the verified-write receipt location (test
	// isolation); "" -> DefaultReceiptPath().
	ReceiptPath string

	caps    co.Capabilities // recorded by Probe (reset on every probe)
	mapping *co.Mapping     // recorded by Topology (nil = never established)
	topoID  string          // canonical topology pattern of the last good Topology ("" = unknown)
}

func (a *Adapter) receiptPath() string {
	if a.ReceiptPath != "" {
		return a.ReceiptPath
	}
	return DefaultReceiptPath()
}

func New(exe string, r Runner) *Adapter {
	a := &Adapter{Exe: exe, MaxOutput: defaultMaxOutput, exeSHA: fileSHA256, dllSHA: fileSHA256}
	if r == nil {
		r = execRunner{MaxOutput: a.MaxOutput, MaxDuration: defaultMaxDuration}
	}
	a.Run = r
	return a
}

var _ co.Backend = (*Adapter)(nil)

// Process-execution bounds (B02): the cap on one answer, the command
// budget applied when the caller's context carries no deadline, and
// the grace for our own reap after we killed the child. None of these
// is optional: a caller without a deadline still gets a bounded run.
const (
	defaultMaxOutput   = 1 << 20
	defaultMaxDuration = 30 * time.Second
	reapGrace          = 5 * time.Second
)

// bootstrapMaxDelta bounds the single-core write allowed while no
// hardware-validated range exists: at most this many Curve Optimizer
// counts away from the value the machine holds right now, on exactly
// one core. It is the minimal-risk bootstrap authority (audit B04
// still refuses every other write while RangeKnown is false).
const bootstrapMaxDelta = 2

// jobObject is the kill-on-close containment the production runner
// needs; procjob.Job implements it. The seam lets tests force an
// Attach failure without touching real job objects from a fake path.
type jobObject interface {
	Attach(p *os.Process) error
	Close() error
}

var newJobObject = func() (jobObject, error) { return procjob.New() }

// execRunner is the production Runner: direct exec, no shell, bounded
// by ctx AND by an explicit command budget, with output caps enforced
// during the read. B01/B02 fixed the old Start+Output double-start:
// pipes are wired before Start, there is exactly one Start and, after
// a successful Start, exactly one Wait on every path - attach
// failure, cancel, timeout and overflow all end through that same
// bounded reap.
type execRunner struct {
	MaxOutput   int           // stdout/stderr accumulation cap; 0 = defaultMaxOutput
	MaxDuration time.Duration // applied when ctx has no deadline; 0 = defaultMaxDuration
}

// cappedWriter accumulates at most max bytes and flags overflow
// instead of buffering an unbounded answer (B02: the limit is
// enforced while reading, not after allocating the whole answer).
// The first overflow fires onOver - the runner uses it to finish off
// and reap its own process, never to truncate.
type cappedWriter struct {
	mu     sync.Mutex
	buf    []byte
	max    int
	over   bool
	onOver func()
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.buf)+len(p) > c.max {
		if keep := c.max - len(c.buf); keep > 0 {
			c.buf = append(c.buf, p[:keep]...)
		}
		first := !c.over
		c.over = true
		cb := c.onOver
		c.mu.Unlock()
		if first && cb != nil {
			cb() // B02: exceeding the cap finishes off OUR process
		}
		return len(p), nil // swallow the rest: the answer is already refused
	}
	c.buf = append(c.buf, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *cappedWriter) state() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf, c.over
}

func (e execRunner) Run(ctx context.Context, exe string, args ...string) (string, int, error) {
	maxOut := e.MaxOutput
	if maxOut <= 0 {
		maxOut = defaultMaxOutput
	}
	maxDur := e.MaxDuration
	if maxDur <= 0 {
		maxDur = defaultMaxDuration
	}
	// B02: an explicit command budget even for a caller whose context
	// has no deadline. When the caller DOES carry one, its cause is
	// preserved so cancel and timeout stay distinguishable below.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxDur)
		defer cancel()
	}

	job, err := newJobObject()
	if err != nil {
		return "", -1, fmt.Errorf("job object: %w", err)
	}
	defer job.Close() // kill-on-close: descendants never outlive this Run

	// B01: pipes exist before Start. cmd.Stdout/Stderr as *os.File
	// means exec spawns no internal copy goroutine and Wait returns
	// at process exit - the readers below are ours to bound.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return "", -1, fmt.Errorf("stdout pipe: %w", err)
	}
	defer stdoutR.Close()
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		return "", -1, fmt.Errorf("stderr pipe: %w", err)
	}
	defer stderrR.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if err := cmd.Start(); err != nil {
		stdoutW.Close()
		stderrW.Close()
		return "", -1, err // never started: no Wait to run (B01)
	}
	// Our copies of the write ends close now: EOF from here means
	// "the child and its descendants closed them", not "we stopped
	// reading". Keeping them open would deadlock the readers on EOF.
	stdoutW.Close()
	stderrW.Close()

	// Exactly one Wait after a successful Start - attach failure
	// included (the old code killed without ever waiting).
	attachErr := error(nil)
	if err := job.Attach(cmd.Process); err != nil {
		attachErr = fmt.Errorf("job attach: %w", err)
		_ = cmd.Process.Kill() // reaped by the single Wait below
	}

	var outBuf, errBuf cappedWriter
	outBuf.max, errBuf.max = maxOut, maxOut
	// B02: exceeding the cap does not truncate - it finishes off and
	// reaps our own process through the single Wait below.
	outBuf.onOver = func() { _ = cmd.Process.Kill() }
	errBuf.onOver = outBuf.onOver
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, stdoutR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, stderrR) }()

	// Watchdog: cancel or deadline kills OUR process (the job object
	// takes descendants with it on Close); the kill unblocks Wait.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()

	waitErr := cmd.Wait() // the one and only Wait

	// Unblock the readers even if a descendant inherited the write
	// end and is still holding it (B02 regression): closing OUR read
	// ends makes the pending reads return.
	stdoutR.Close()
	stderrR.Close()
	readersDone := make(chan struct{})
	go func() { wg.Wait(); close(readersDone) }()
	readersOK := false
	select {
	case <-readersDone:
		readersOK = true
	case <-time.After(reapGrace):
		// readers wedged despite closed handles: bounded, but the
		// answer is then incomplete and must not be trusted.
	}

	code := cmd.ProcessState.ExitCode()

	// Honest cause order (B02): the caller's cancel is cancelled, a
	// deadline - ours or theirs - is timeout; neither is the other.
	if kerr := ctx.Err(); kerr != nil {
		kind := co.KindCancelled
		if errors.Is(kerr, context.DeadlineExceeded) {
			kind = co.KindTimeout
		}
		return "", code, &co.Error{Kind: kind, Op: "exec", Err: kerr}
	}
	if attachErr != nil {
		return "", code, attachErr // killed and waited above
	}
	if !readersOK {
		return "", code, &co.Error{Kind: co.KindMalformedOutput, Op: "exec",
			Err: fmt.Errorf("output readers wedged %v after the process ended and our handles closed: the answer is incomplete", reapGrace)}
	}
	out, outOver := outBuf.state()
	_, errOver := errBuf.state()
	if outOver || errOver {
		// B02: beyond the cap is a hard error, never a truncation;
		// the oversized answer was finished off and reaped above.
		which := "stdout"
		if !outOver {
			which = "stderr"
		}
		return "", code, &co.Error{Kind: co.KindMalformedOutput, Op: "exec",
			Err: fmt.Errorf("%s exceeded the %d byte output cap: refusing to buffer an answer this big", which, maxOut)}
	}
	if waitErr != nil && code == -1 {
		return "", code, waitErr // could not run at all
	}
	return string(out), code, nil
}

// Probe establishes tool identity (version + exe hash), asks
// --get-physical-cores whether ZenStates-Core initialized at all,
// and proves the READ PATH by actually running the terse offset read
// (B12: "available command" must not be inferred from a different
// command succeeding). The tool cannot distinguish "unsupported CPU"
// from "missing driver" (both are exit 2), so Probe reports
// unsupported_cpu with the honest note instead of guessing which one
// it was. Every previous probe fact is dropped first: stale support,
// mapping or fingerprint must never survive into a new probe.
func (a *Adapter) Probe(ctx context.Context) (co.Capabilities, error) {
	a.caps = co.Capabilities{}
	a.mapping = nil
	a.topoID = ""
	a.lastExact = nil
	caps := co.Capabilities{Tool: "ryzen-smu-cli"}
	sha := a.exeSHA
	if sha == nil {
		sha = fileSHA256 // adapters built as literals (tests) still digest
	}
	if sum, err := sha(a.Exe); err != nil {
		return co.Capabilities{}, &co.Error{Kind: co.KindMissingTool, Op: "probe", Err: err}
	} else {
		caps.SHA256 = sum
	}
	// C02: the launcher alone says nothing about the logic that
	// actually runs - for a .NET apphost that lives in the paired
	// managed payload. Measure it too. A read failure here is NOT a
	// probe error (the exe demonstrably runs; the payload may just
	// live in a nonstandard layout): the digest stays unknown (""),
	// which keeps BuildValidated false, and the identity notes below
	// state what was measured and what was not.
	dsha := a.dllSHA
	if dsha == nil {
		dsha = fileSHA256 // adapters built as literals (tests) still digest
	}
	if sum, derr := dsha(payloadPath(a.Exe)); derr == nil {
		caps.PayloadSHA256 = sum
	}
	out, code, err := a.Run.Run(ctx, a.Exe, "--version")
	if err != nil {
		return co.Capabilities{}, err
	}
	verCommit := ""
	if code == 0 {
		caps.Version = parseVersion(out) // "" stays unknown when unrecognized
		verCommit = parseVersionCommit(out)
	}
	// Support probe: physical core list must initialize and print.
	out, code, err = a.Run.Run(ctx, a.Exe, "--get-physical-cores")
	if err != nil {
		return co.Capabilities{}, err
	}
	switch code {
	case 0:
		if _, perr := parseCoreList(out); perr != nil {
			return co.Capabilities{}, &co.Error{Kind: co.KindMalformedOutput, Op: "probe", Err: perr}
		}
		caps.Supported = co.Yes("ZenStates-Core initialized")
	case 1:
		return co.Capabilities{}, &co.Error{Kind: co.KindNoPrivilege, Op: "probe",
			Err: fmt.Errorf("ryzen-smu-cli requires elevation (exit 1)")}
	case 2:
		return co.Capabilities{}, &co.Error{Kind: co.KindUnsupportedCPU, Op: "probe",
			Err: fmt.Errorf("ZenStates-Core failed to initialize (exit 2): unsupported CPU or missing driver - the tool cannot tell them apart")}
	default:
		return co.Capabilities{}, &co.Error{Kind: co.KindMalformedOutput, Op: "probe",
			Err: fmt.Errorf("unexpected exit %d from --get-physical-cores", code)}
	}
	// Read capability is proven by the read path itself: the terse
	// offset read must answer AND parse (Sol: exit 0 alone is not a
	// usable readback). A partial answer (disabled cores) still
	// proves the path exists, and says what caps it.
	out, code, err = a.Run.Run(ctx, a.Exe, "--get-offsets-terse")
	if err != nil {
		return co.Capabilities{}, err
	}
	switch {
	case code == 0:
		if vals, partial, note, perr := parseOffsetsTerse(out); perr == nil {
			caps.Read = co.Yes(fmt.Sprintf("--get-offsets-terse answered and parsed (%d values)", len(vals)))
			if partial {
				caps.Notes = append(caps.Notes, "terse read is partial while cores are disabled: "+note)
			}
		} else {
			caps.Read = co.Unknown(fmt.Sprintf("terse answer did not parse: %v", perr))
		}
	default:
		caps.Read = co.Unknown(fmt.Sprintf("terse read answered exit %d", code))
	}
	if caps.Read.Known && caps.Read.Yes {
		caps.Readback = co.Yes("readback rides the same terse read path, proven above")
	} else {
		caps.Readback = co.Unknown("readback rides the read path, which is not confirmed")
	}
	// Apply support is only certain when the offset register exists;
	// the tool reports that itself (exit 3) at apply time, so before
	// any write the honest answer is unknown.
	caps.Apply = co.Unknown("offset register only proven by a real apply (exit 3 otherwise)")
	// B12: the tool's syntax bound and the hardware-validated span
	// are different facts and live in different fields. The bound
	// says what the tool PARSES; it never authorizes a write.
	caps.SyntaxKnown = true
	caps.SyntaxMin, caps.SyntaxMax = pinnedProvenance.SyntaxMin, pinnedProvenance.SyntaxMax
	caps.RangeKnown = false
	caps.Notes = append(caps.Notes,
		fmt.Sprintf("syntax bound [%d..%d] is the pinned %s argument regex: what the tool parses, not what this CPU accepts",
			caps.SyntaxMin, caps.SyntaxMax, pinnedProvenance.Tag),
		"hardware-validated CO span unknown: writes stay refused until a verified read-only smoke on the installed build (P10d)")
	// B12 + P10d + C02: the build is validated on four independent
	// axes - the reported version, the SourceLink commit the build
	// embeds in its own informational version, the measured launcher
	// digest, and the measured managed-payload digest (the code that
	// really runs, audit C02). All four matching the pinned manifest
	// means the installed build IS the pinned 0.1.3 build; any
	// mismatch keeps it UNVALIDATED - a different or tampered binary
	// never inherits trust just because it runs, prints a plausible
	// version and carries an untouched launcher. The archive hash
	// pins the whole release package; these per-file digests pin the
	// installed files an archive hash alone cannot follow after install.
	switch {
	case caps.Version == pinnedProvenance.AssemblyVer &&
		verCommit == pinnedProvenance.Commit &&
		caps.SHA256 == pinnedProvenance.ExeSHA256 &&
		caps.PayloadSHA256 == pinnedProvenance.DllSHA256:
		caps.BuildValidated = true
		caps.Notes = append(caps.Notes, fmt.Sprintf(
			"build VALIDATED: version %s, its embedded SourceLink commit %s, the exe digest and the managed payload digest all match the pinned %s build (source %s tag %s commit %s; archive %s sha256 %s; dll payload sha256 %s)",
			caps.Version, sh(verCommit, 12), pinnedProvenance.Tag, pinnedProvenance.Repo, pinnedProvenance.Tag,
			sh(pinnedProvenance.Commit, 12), pinnedProvenance.Asset, sh(pinnedProvenance.AssetSHA256, 12), sh(pinnedProvenance.DllSHA256, 12)))
	case caps.Version == pinnedProvenance.AssemblyVer:
		caps.Notes = append(caps.Notes, fmt.Sprintf(
			"version %s matches pinned release %s but identity does NOT: embedded commit %s / exe digest %s / payload digest %s vs pinned commit %s / exe %s / payload %s: UNVALIDATED build (different, tampered or payload-swapped binary)",
			caps.Version, pinnedProvenance.Tag, orUnknownQ(sh(verCommit, 12)), orUnknownQ(sh(caps.SHA256, 12)), orUnknownQ(sh(caps.PayloadSHA256, 12)),
			sh(pinnedProvenance.Commit, 12), sh(pinnedProvenance.ExeSHA256, 12), sh(pinnedProvenance.DllSHA256, 12)))
	default:
		caps.Notes = append(caps.Notes, fmt.Sprintf(
			"version %q does not match pinned release %s: UNVALIDATED build",
			orUnknownQ(caps.Version), pinnedProvenance.Tag))
	}
	a.caps = caps
	return caps, nil
}

func orUnknownQ(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// sh shortens a hex string for a note, never pasting a full digest
// into prose and never panicking on a short/empty value.
func sh(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Topology builds the tool-side slot table from --get-physical-cores
// (fused status) and --get-enabled-cores (SMU-readable = offset
// addressable), and cross-checks counts against our own topology when
// a checker is supplied. OS logical ids are only filled by the
// checker's confirmed match; otherwise they stay nil.
func (a *Adapter) Topology(ctx context.Context) (co.Mapping, error) {
	// Any previous mapping is dropped before re-deriving: a failed
	// or changed topology must never leave a stale one behind.
	a.mapping = nil
	a.topoID = ""
	fusedOut, code, err := a.Run.Run(ctx, a.Exe, "--get-physical-cores")
	if err != nil {
		return co.Mapping{}, err
	}
	if code != 0 {
		return co.Mapping{}, exitError("topology", code, fusedOut)
	}
	fused, err := parseCoreList(fusedOut)
	if err != nil {
		return co.Mapping{}, &co.Error{Kind: co.KindMalformedOutput, Op: "topology", Err: err}
	}
	enabledOut, code, err := a.Run.Run(ctx, a.Exe, "--get-enabled-cores")
	if err != nil {
		return co.Mapping{}, err
	}
	if code != 0 {
		return co.Mapping{}, exitError("topology", code, enabledOut)
	}
	enabled, err := parseCoreList(enabledOut)
	if err != nil {
		return co.Mapping{}, &co.Error{Kind: co.KindMalformedOutput, Op: "topology", Err: err}
	}
	if len(fused) != len(enabled) {
		return co.Mapping{}, &co.Error{Kind: co.KindOffsetCount, Op: "topology",
			Err: fmt.Errorf("physical %d vs enabled %d core lists disagree", len(fused), len(enabled))}
	}
	// The tool numbers its addressable "logical cores" 0..k-1 over
	// the SMU-readable (enabled) physical cores in physical order.
	m := co.Mapping{Quality: "partial"}
	k := 0
	for phys, en := range enabled {
		if !en {
			m.Notes = append(m.Notes, fmt.Sprintf("physical core %d disabled: offsets not addressable while disabled", phys))
			continue
		}
		slot := co.CoreSlot{
			ToolID:   fmt.Sprintf("core%d", k),
			SMUIndex: intp(phys),
		}
		if !fused[phys] {
			slot.Notes = append(slot.Notes, "fused disabled but SMU-readable: unusual, reported not guessed")
		}
		m.Slots = append(m.Slots, slot)
		k++
	}
	if len(m.Slots) == 0 {
		return co.Mapping{}, &co.Error{Kind: co.KindOffsetCount, Op: "topology",
			Err: fmt.Errorf("no SMU-addressable cores found")}
	}
	a.mapping = &m
	a.topoID = topoPattern(fused, enabled, len(m.Slots))
	return m, nil
}

// topoPattern is the canonical topology identity part of the
// fingerprint: fused pattern, SMU-enabled pattern and the resulting
// addressable-slot count, in one comparable string.
func topoPattern(fused, enabled []bool, slots int) string {
	pat := func(list []bool) string {
		s := make([]byte, len(list))
		for i, e := range list {
			if e {
				s[i] = '1'
			} else {
				s[i] = '0'
			}
		}
		return string(s)
	}
	return "f:" + pat(fused) + ";e:" + pat(enabled) + ";k:" + strconv.Itoa(slots)
}

// fingerprint is the snapshot's machine+tool identity binding
// (audit B03): tool name, reported version, installed exe digest,
// measured managed-payload digest (C02 - the launcher alone does not
// identify the code that runs), host name and the topology pattern
// of the last successful Topology. ANY missing component makes the
// whole fingerprint unknown ("") - a partial fingerprint must never
// masquerade as an identity. Honest limits, stated once here because
// consumers ask: identical machines can collide (no serial/UUID
// source is read here), and a fingerprint never detects another tool
// having changed offsets - the read/compare contract stays mandatory.
func (a *Adapter) fingerprint() (string, []string) {
	var missing []string
	host, herr := os.Hostname()
	if herr != nil || host == "" {
		missing = append(missing, "host name")
	}
	if a.caps.Tool == "" {
		missing = append(missing, "tool name")
	}
	if a.caps.Version == "" {
		missing = append(missing, "tool version")
	}
	if a.caps.SHA256 == "" {
		missing = append(missing, "exe digest")
	}
	if a.caps.PayloadSHA256 == "" {
		missing = append(missing, "payload digest")
	}
	if a.topoID == "" {
		missing = append(missing, "topology pattern")
	}
	if len(missing) > 0 {
		return "", missing
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"tool=" + a.caps.Tool,
		"version=" + a.caps.Version,
		"exe=" + a.caps.SHA256,
		"payload=" + a.caps.PayloadSHA256,
		"host=" + host,
		"topo=" + a.topoID,
	}, "\x00")))
	return hex.EncodeToString(sum[:]), nil
}

// Read takes the terse offset list and ties it to the established
// topology and identity (audit B03). Upstream SKIPS per-core reads
// that fail and only adds a general note: after such a loss the
// remaining values cannot be attributed to core0/core1/... - the
// position of the skip is unknown. So a short CSV, an empty CSV or
// the disabled-cores note yields quality unknown with an EMPTY
// profile: the raw values stay in the notes as evidence, never as
// addressable offsets. A full CSV becomes exact only when the
// hardware fingerprint is complete; otherwise partial with the
// missing components named.
func (a *Adapter) Read(ctx context.Context) (co.Snapshot, error) {
	if a.mapping == nil {
		if _, err := a.Topology(ctx); err != nil {
			return co.Snapshot{}, err
		}
	}
	out, code, err := a.Run.Run(ctx, a.Exe, "--get-offsets-terse")
	if err != nil {
		return co.Snapshot{}, err
	}
	if code != 0 {
		return co.Snapshot{}, exitError("read", code, out)
	}
	vals, partial, note, err := parseOffsetsTerse(out)
	if err != nil {
		return co.Snapshot{}, &co.Error{Kind: co.KindMalformedOutput, Op: "read", Err: err}
	}
	snap := co.Snapshot{ReadAt: time.Now()}
	n := len(a.mapping.Slots)
	if partial || len(vals) != n {
		why := fmt.Sprintf("tool returned %d values for %d addressable slots", len(vals), n)
		if partial {
			why = note
		}
		snap.Quality = "unknown"
		snap.Notes = append(snap.Notes,
			why+": value-to-core attribution unknown (upstream skips failed reads without position markers)",
			fmt.Sprintf("raw values kept, not attributed: %v", vals))
		return snap, nil
	}
	for i, v := range vals {
		snap.Profile.Offsets = append(snap.Profile.Offsets,
			co.Offset{Core: a.mapping.Slots[i].ToolID, Value: v})
	}
	fp, missing := a.fingerprint()
	if fp == "" {
		snap.Quality = "partial"
		snap.Notes = append(snap.Notes,
			fmt.Sprintf("hardware fingerprint incomplete (missing: %s): identity not confirmed", strings.Join(missing, ", ")))
	} else {
		snap.Quality = "exact"
		snap.HardwareID = fp
		// Keep an independent copy as the write-authority reference:
		// a later read must not mutate the snapshot a pending write
		// was bounded against.
		cp := snap
		cp.Profile.Offsets = append([]co.Offset(nil), snap.Profile.Offsets...)
		a.lastExact = &cp
	}
	return snap, nil
}

// Validate enforces the public write boundary HERE, not in a
// well-behaved caller (audit B04). Structure is always required:
// probe-known support, a live mapping, addressable unique cores, and
// values inside the tool's SYNTAX bound (what it parses). On top of
// that, the write needs an authority:
//   - a hardware-VALIDATED range (RangeKnown) -> values inside it; or
//   - the minimal bootstrap authority -> exactly one core moved by at
//     most bootstrapMaxDelta from the value the machine holds right
//     now, every other addressed core unchanged.
//
// Anything else stays refused while no hardware-validated range
// exists. Behind this function zero runner calls happen on a refusal.
func (a *Adapter) Validate(p co.Profile) error {
	if !a.caps.Supported.Known || !a.caps.Supported.Yes {
		return &co.Error{Kind: co.KindUnsupportedCPU, Op: "validate",
			Err: fmt.Errorf("tool/CPU support not established by a successful probe")}
	}
	if a.mapping == nil {
		return &co.Error{Kind: co.KindNotValidated, Op: "validate",
			Err: fmt.Errorf("topology not established: no mapping to address cores by")}
	}
	if len(p.Offsets) == 0 {
		return &co.Error{Kind: co.KindBadProfile, Op: "validate",
			Err: fmt.Errorf("empty profile: nothing to write")}
	}
	if err := a.checkStructure(p.Offsets); err != nil {
		return err
	}
	if a.caps.RangeKnown {
		for _, o := range p.Offsets {
			if o.Value < a.caps.RangeMin || o.Value > a.caps.RangeMax {
				return &co.Error{Kind: co.KindBadProfile, Op: "validate",
					Err: fmt.Errorf("offset %d for %s outside validated range [%d,%d]", o.Value, o.Core, a.caps.RangeMin, a.caps.RangeMax)}
			}
		}
		return nil
	}
	return a.bootstrapAuthority(p)
}

// checkStructure validates core addressing, uniqueness and the tool
// syntax bound - the facts that hold regardless of what this CPU
// accepts. It never asserts a hardware-validated writable range.
func (a *Adapter) checkStructure(offs []co.Offset) error {
	seen := map[string]bool{}
	for _, o := range offs {
		idx, ok := toolIDX(o.Core)
		if !ok || idx >= len(a.mapping.Slots) {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate",
				Err: fmt.Errorf("%q is not an addressable tool core (valid: core0..core%d)", o.Core, len(a.mapping.Slots)-1)}
		}
		if seen[o.Core] {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate",
				Err: fmt.Errorf("%s appears twice: one core cannot take two offsets in one write", o.Core)}
		}
		seen[o.Core] = true
		if o.Value < a.caps.SyntaxMin || o.Value > a.caps.SyntaxMax {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate",
				Err: fmt.Errorf("offset %d for %s outside the tool syntax bound [%d,%d]", o.Value, o.Core, a.caps.SyntaxMin, a.caps.SyntaxMax)}
		}
	}
	return nil
}

// bootstrapAuthority is the only write allowed while no hardware-
// validated range exists: a single core nudged by at most
// bootstrapMaxDelta counts from the value the machine reports right
// now, with every other addressed core left exactly as found. It is
// bounded against a live exact read, so it cannot drive a core to a
// wild value and cannot touch cores the operator did not choose.
func (a *Adapter) bootstrapAuthority(p co.Profile) error {
	if !a.caps.BuildValidated {
		return &co.Error{Kind: co.KindNotValidated, Op: "validate",
			Err: fmt.Errorf("installed build is not validated against the pinned provenance (version/embedded commit/exe digest): a different or tampered binary must never reach the SMU, at any delta size; writes stay refused")}
	}
	if a.lastExact == nil {
		return &co.Error{Kind: co.KindNotValidated, Op: "validate",
			Err: fmt.Errorf("hardware-validated CO range unknown and no exact current read to bound a minimal write against: writes stay refused")}
	}
	cur := map[string]int{}
	for _, o := range a.lastExact.Profile.Offsets {
		cur[o.Core] = o.Value
	}
	if len(p.Offsets) != len(cur) {
		return &co.Error{Kind: co.KindNotValidated, Op: "validate",
			Err: fmt.Errorf("bootstrap write must address exactly the %d cores the machine reports, got %d", len(cur), len(p.Offsets))}
	}
	changed := 0
	for _, o := range p.Offsets {
		have, ok := cur[o.Core]
		if !ok {
			return &co.Error{Kind: co.KindBadProfile, Op: "validate",
				Err: fmt.Errorf("%s is not a core the machine currently reports", o.Core)}
		}
		if have == o.Value {
			continue
		}
		d := o.Value - have
		if d < 0 {
			d = -d
		}
		if d > bootstrapMaxDelta {
			return &co.Error{Kind: co.KindNotValidated, Op: "validate",
				Err: fmt.Errorf("bootstrap write moves %s by %d counts (> %d) from its current value %d: refused", o.Core, d, bootstrapMaxDelta, have)}
		}
		changed++
	}
	if changed != 1 {
		return &co.Error{Kind: co.KindNotValidated, Op: "validate",
			Err: fmt.Errorf("bootstrap write must change exactly one core, it changes %d: refused", changed)}
	}
	return nil
}

// ValidateRestore is the restore gate. Re-applying values the machine
// already held needs only structure and the tool syntax bound - NOT a
// hardware-validated writable range, because the saved originals are
// routinely outside any minimal bootstrap span (e.g. -48 against a
// verified -13). Restoring them is authorized by the transaction that
// captured them, not by a range.
func (a *Adapter) ValidateRestore(s co.Snapshot) error {
	if a.mapping == nil {
		return &co.Error{Kind: co.KindNotValidated, Op: "restore",
			Err: fmt.Errorf("topology not established: no mapping to address cores by")}
	}
	if len(s.Profile.Offsets) == 0 {
		return &co.Error{Kind: co.KindBadProfile, Op: "restore",
			Err: fmt.Errorf("empty snapshot: nothing to restore")}
	}
	return a.checkStructure(s.Profile.Offsets)
}

// Apply writes via --offset core:value pairs after Validate. The
// tool's "Set logical core ..." lines are a CALL LOG: Program.cs
// prints them after SetPsmMarginSingleCore WITHOUT checking its
// result (audit B05), so this backend never self-confirms a
// mutation: Mutated stays unknown whatever the log says, the log is
// cross-read for disagreements, and only an independent readback
// (Verify) can state the observed state. A refused profile never
// reaches the runner at all.
func (a *Adapter) Apply(ctx context.Context, p co.Profile) (co.WriteResult, error) {
	if len(p.Offsets) == 0 {
		return co.WriteResult{Mutated: co.MutatedNone}, nil
	}
	if err := a.Validate(p); err != nil {
		return co.WriteResult{Mutated: co.MutatedNone}, err // zero runner calls (B04)
	}
	// Write only the cores whose value actually changes against the
	// last exact read: untouched cores are never rewritten, so a
	// single-core bootstrap touches exactly one SMU register and a
	// restore of a one-core change rewrites one register, not sixteen.
	target := diffAgainst(p.Offsets, a.lastExact)
	if len(target) == 0 {
		return co.WriteResult{Mutated: co.MutatedNone,
			Notes: []string{"every addressed core already holds the requested value: no write needed"}}, nil
	}
	return a.applyOffsets(ctx, target)
}

// applyOffsets performs the raw --offset write for the given cores
// (already authorized by the caller's gate) and returns the honest
// unknown-mutation result with the call log cross-read for
// disagreements only. It performs no value validation of its own -
// but it re-identifies the CURRENT install (audit C02), because the
// caller's gate decided against the build that was on disk at Probe
// time, not necessarily the one on disk right now.
func (a *Adapter) applyOffsets(ctx context.Context, offs []co.Offset) (co.WriteResult, error) {
	if err := a.installCurrent("apply"); err != nil {
		return co.WriteResult{Mutated: co.MutatedNone}, err // zero runner calls (B04 pattern, C02)
	}
	arg := ""
	for i, o := range offs {
		if i > 0 {
			arg += ","
		}
		arg += fmt.Sprintf("%s:%d", coreArg(o.Core), o.Value)
	}
	out, code, err := a.Run.Run(ctx, a.Exe, "--offset", arg)
	res := co.WriteResult{Mutated: co.MutatedUnknown}
	if err != nil {
		// A timeout or cancel mid-apply: the call may have reached
		// the SMU and the output was lost - unknown, never none, and
		// no call-log claims are read out of an answer we never saw.
		res.Notes = []string{"runner error before the answer: the write call may or may not have reached the SMU; outcome unknown until a readback"}
		return res, err
	}
	logged, notes := checkApplyLog(out, co.Profile{Offsets: offs}, a.mapping.Slots)
	res.Notes = append([]string{fmt.Sprintf(
		"tool logged %d Set call(s) for %d requested offset(s); the tool prints these lines after calling SetPsmMarginSingleCore without checking its result: a call log, not hardware proof; the SMU outcome stays %s until an independent readback (Verify) says otherwise",
		logged, len(offs), co.MutatedUnknown)}, notes...)
	if code != 0 {
		return res, exitError("apply", code, out)
	}
	return res, nil
}

// diffAgainst keeps only the requested cores whose value differs from
// the reference snapshot. With no reference (nil) it writes everything
// requested (the caller already validated it).
func diffAgainst(offs []co.Offset, ref *co.Snapshot) []co.Offset {
	if ref == nil {
		return offs
	}
	cur := map[string]int{}
	for _, o := range ref.Profile.Offsets {
		cur[o.Core] = o.Value
	}
	var out []co.Offset
	for _, o := range offs {
		if v, ok := cur[o.Core]; ok && v == o.Value {
			continue
		}
		out = append(out, o)
	}
	return out
}

// Verify re-reads and compares exactly. Readback quality of the read
// caps the verdict: a partial read can never confirm a full match.
func (a *Adapter) Verify(ctx context.Context, p co.Profile) (co.Verification, error) {
	snap, err := a.Read(ctx)
	if err != nil {
		return co.Verification{}, err
	}
	if snap.Quality != "exact" {
		return co.Verification{Match: co.Unknown("readback read was " + snap.Quality),
			Notes: snap.Notes}, nil
	}
	cur := map[string]int{}
	for _, o := range snap.Profile.Offsets {
		cur[o.Core] = o.Value
	}
	var diff []co.Offset
	for _, want := range p.Offsets {
		if got, ok := cur[want.Core]; !ok || got != want.Value {
			diff = append(diff, co.Offset{Core: want.Core, Value: got})
		}
	}
	if len(diff) == 0 {
		return co.Verification{Match: co.Yes("readback equals requested profile")}, nil
	}
	return co.Verification{Match: co.No("readback differs"), Diff: diff}, nil
}

// Restore re-applies exactly the saved snapshot (not zeros) and
// confirms by readback. B03: an incomplete, identity-less or empty
// snapshot never gains write permission - refused before any runner
// call. B05: the write outcome is unknown by nature for this
// backend, so Restore never gates on self-confirmation: after a
// write that RAN, the honest recovery answer is the independent
// readback. A validation refusal stops the restore entirely. The
// caller (transaction coordinator) still owns the fingerprint
// compare that decides whether a restore is safe to attempt at all.
func (a *Adapter) Restore(ctx context.Context, s co.Snapshot) (co.Verification, error) {
	if s.Quality != "exact" || s.HardwareID == "" || len(s.Profile.Offsets) == 0 {
		return co.Verification{Match: co.Unknown("restore refused: snapshot quality " + s.Quality)},
			&co.Error{Kind: co.KindRestoreRefused, Op: "restore",
				Err: fmt.Errorf("snapshot quality %q, fingerprint present=%t, offsets=%d: only an exact, fingerprinted, non-empty snapshot may be restored",
					s.Quality, s.HardwareID != "", len(s.Profile.Offsets))}
	}
	// Validate the restore by its OWN gate: structure + syntax bound,
	// never a hardware-validated writable range (the saved originals
	// are routinely outside any minimal bootstrap span). A refused
	// snapshot stops the restore before any write, and the refusal is
	// not mistaken below for a write that ran and failed.
	if err := a.ValidateRestore(s); err != nil {
		return co.Verification{Match: co.Unknown("restore refused before any write")}, err
	}
	// Re-read to compute what actually differs, then write ONLY those
	// cores: restoring a single-core change rewrites one register, not
	// the whole profile, and a restore where nothing differs writes
	// nothing. An untrustworthy current read stops before any write.
	cur, rerr := a.Read(ctx)
	if rerr != nil || cur.Quality != "exact" {
		return co.Verification{Match: co.Unknown("restore not attempted: current read before restore is untrustworthy")},
			&co.Error{Kind: co.KindRecoveryRequired, Op: "restore",
				Err: fmt.Errorf("current read before restore untrustworthy (err=%v, quality %q): cannot safely compute the restore diff", rerr, cur.Quality)}
	}
	target := diffAgainst(s.Profile.Offsets, &cur)
	if len(target) == 0 {
		return co.Verification{Match: co.Yes("current state already equals the saved snapshot: no restore write needed")}, nil
	}
	res, applyErr := a.applyOffsets(ctx, target)
	// The write ran: whatever it reported (exit code, timeout,
	// cancel), the independent readback is the truth-teller. A
	// failing readback is recorded as unknown, never hidden.
	ver, verr := a.Verify(ctx, s.Profile)
	if verr != nil {
		ver = co.Verification{Match: co.Unknown("readback failed after the restore write attempt: " + verr.Error())}
	}
	ver.Notes = append(ver.Notes, fmt.Sprintf("restore re-applied %d differing core(s); write-call outcome %s", len(target), res.Mutated))
	if applyErr != nil {
		return ver, applyErr
	}
	return ver, verr
}

// coreArg maps our tool id ("coreN") back to the tool's numeric
// logical core index.
func coreArg(id string) string {
	if len(id) > 4 && id[:4] == "core" {
		return id[4:]
	}
	return id
}

func exitError(op string, code int, stderrHint string) error {
	kind := co.KindMalformedOutput
	switch code {
	case 1:
		kind = co.KindNoPrivilege
	case 2:
		kind = co.KindUnsupportedCPU
	case 3:
		kind = co.KindUnsupportedOp
	case 4, 6:
		kind = co.KindBadProfile
	case 5, 8:
		kind = co.KindOffsetCount
	}
	return &co.Error{Kind: kind, Op: op,
		Err: fmt.Errorf("ryzen-smu-cli exit %d", code)}
}

// payloadPath is the apphost convention: the managed payload sits
// beside the launcher under the same base name (ryzen-smu-cli.exe ->
// ryzen-smu-cli.dll). A nonstandard layout makes the payload simply
// unmeasurable, which keeps BuildValidated false - an honest unknown,
// never a silent pass.
func payloadPath(exe string) string {
	return strings.TrimSuffix(exe, ".exe") + ".dll"
}

// installCurrent re-digests the CURRENT install - launcher AND
// payload - against the pinned provenance, immediately before a
// hardware write (audit C02). Probe's verdict is a cached fact: the
// files on disk can change afterwards, and a payload swapped after a
// successful Probe must never reach the SMU. Any read failure or
// mismatch refuses the write BEFORE the tool is ever called (zero
// runner calls, the B04 pattern), and the mutation honestly stays none.
func (a *Adapter) installCurrent(op string) error {
	exeSHA := a.exeSHA
	if exeSHA == nil {
		exeSHA = fileSHA256
	}
	dllSHA := a.dllSHA
	if dllSHA == nil {
		dllSHA = fileSHA256
	}
	exeSum, xerr := exeSHA(a.Exe)
	if xerr != nil {
		return &co.Error{Kind: co.KindNotValidated, Op: op,
			Err: fmt.Errorf("cannot re-digest the installed launcher immediately before writing: %w", xerr)}
	}
	dllSum, derr := dllSHA(payloadPath(a.Exe))
	if derr != nil {
		return &co.Error{Kind: co.KindNotValidated, Op: op,
			Err: fmt.Errorf("cannot read the installed managed payload %s immediately before writing (the apphost convention pairs it beside the launcher): %w", payloadPath(a.Exe), derr)}
	}
	if exeSum != pinnedProvenance.ExeSHA256 {
		return &co.Error{Kind: co.KindNotValidated, Op: op,
			Err: fmt.Errorf("installed launcher digest %s no longer matches the pinned build %s: the launcher changed since the probe - write refused", sh(exeSum, 12), sh(pinnedProvenance.ExeSHA256, 12))}
	}
	if dllSum != pinnedProvenance.DllSHA256 {
		return &co.Error{Kind: co.KindNotValidated, Op: op,
			Err: fmt.Errorf("installed managed payload %s does not match the pinned build %s: the code that runs is not the validated build (swapped after the probe?) - write refused", sh(dllSum, 12), sh(pinnedProvenance.DllSHA256, 12))}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func intp(v int) *int { return &v }
