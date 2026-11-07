package cotx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/lockfile"
)

// DefaultCleanupTimeout bounds the restore phase when the working
// context is already cancelled or dead. Provisional (no measured
// hardware evidence yet); configurable per Engine.
const DefaultCleanupTimeout = 30 * time.Second

// Engine runs one CO transaction at a time against a Backend, with
// durable state and OS-backed exclusivity. Zero-value paths default
// to the absolute machine locations; tests point them at a temp dir.
type Engine struct {
	Backend   co.Backend
	LockPath  string // "" -> DefaultLockPath()
	StorePath string // "" -> DefaultStorePath()

	// CleanupTimeout bounds the restore phase, which always rides a
	// fresh context independent of the (possibly cancelled) working
	// context. 0 -> DefaultCleanupTimeout.
	CleanupTimeout time.Duration

	// storeFault, when set, is consulted before each durable write
	// with the step name; a non-nil return makes that write fail
	// (the document stays at its previous step). Test seam.
	storeFault func(step string) error

	// abortAfter, when set, is consulted right after each successful
	// durable write with the step name; a non-nil return aborts the
	// operation as if the process died there (the document is now at
	// that step on disk). Test seam for crash-before/after-each-
	// boundary: a later Engine.Recover (in this or a new process)
	// must recover from exactly that durable state.
	abortAfter func(step string) error
}

// SetAbortAfter installs a crash seam: fn is consulted right after
// each successful durable write with the step name, and a non-nil
// return aborts the operation as if the process died there (the
// document is left at that step on disk). It lets a caller in another
// package exercise the exact crash-then-Recover boundary the durability
// contract promises. Test/diagnostic use.
func (e *Engine) SetAbortAfter(fn func(step string) error) { e.abortAfter = fn }

func (e *Engine) lockPath() string {
	if e.LockPath != "" {
		return e.LockPath
	}
	return DefaultLockPath()
}
func (e *Engine) storePath() string {
	if e.StorePath != "" {
		return e.StorePath
	}
	return DefaultStorePath()
}
func (e *Engine) cleanupTimeout() time.Duration {
	if e.CleanupTimeout != 0 {
		return e.CleanupTimeout
	}
	return DefaultCleanupTimeout
}

// Result reports the final durable status of a Run or Recover and
// the document that produced it.
type Result struct {
	TxID   string
	Status string
	Notes  []string
	Doc    TxDoc
}

func resultOf(d *TxDoc) *Result { return &Result{TxID: d.TxID, Status: d.Status, Doc: *d} }

func coErr(kind, op string, err error) *co.Error { return &co.Error{Kind: kind, Op: op, Err: err} }

// lockErr maps both a live holder and an unestablishable lock state
// to lock_held: unknown is never treated as free.
func lockErr(err error) *co.Error {
	var he *lockfile.HeldError
	if errors.As(err, &he) {
		return coErr(co.KindLockHeld, "lock", err)
	}
	return coErr(co.KindLockHeld, "lock", fmt.Errorf("cannot establish CO lock state: %w", err))
}

func opRec(op string, err error) OpRec {
	r := OpRec{Op: op, At: time.Now()}
	if err != nil {
		var ce *co.Error
		if errors.As(err, &ce) {
			r.Kind = ce.Kind
		} else {
			r.Kind = "error"
		}
		r.Detail = err.Error()
	}
	return r
}

// persist writes the document (bumping UpdatedAt), honoring the
// storeFault seam.
func (e *Engine) persist(d *TxDoc, step string) error {
	if e.storeFault != nil {
		if err := e.storeFault(step); err != nil {
			return err
		}
	}
	d.UpdatedAt = time.Now()
	return writeDoc(e.storePath(), *d)
}

// maybeAbort honors the abortAfter seam after a successful durable
// write, simulating a process death at that boundary.
func (e *Engine) maybeAbort(d *TxDoc, step string) (*Result, bool, error) {
	if e.abortAfter == nil {
		return nil, false, nil
	}
	if err := e.abortAfter(step); err != nil {
		return resultOf(d), true, err
	}
	return nil, false, nil
}

// Run performs one manual-apply transaction: read the original,
// durably record apply_pending BEFORE any hardware call, apply,
// re-read and verify, and only on a clean match run the optional
// phase then restore the original and confirm by read. A partial or
// unknown apply outcome stops the automatic sequence (recovery
// required) rather than blindly restoring. phase may be nil.
func (e *Engine) Run(ctx context.Context, profile co.Profile, phase func(context.Context) error) (*Result, error) {
	lk, err := lockfile.Acquire(e.lockPath(), "co-tx")
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Release()
	if err := e.checkGate(); err != nil {
		return nil, err
	}
	caps, mapping, before, err := e.establish(ctx)
	if err != nil {
		return nil, err
	}
	return e.runBody(ctx, caps, mapping, before, profile, phase)
}

// BootstrapCore runs the minimal single-core bootstrap round-trip:
// capture the exact original, move exactly one core by delta (the
// adapter's bootstrap authority bounds the magnitude), apply, verify
// by independent readback, then restore the touched core to its exact
// original and confirm by read. The desired profile is derived from
// the engine's OWN before-read, so the write is bounded against the
// same exact state the transaction captured - not a second, possibly
// drifted read.
func (e *Engine) BootstrapCore(ctx context.Context, core string, delta int) (*Result, error) {
	lk, err := lockfile.Acquire(e.lockPath(), "co-bootstrap")
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Release()
	if err := e.checkGate(); err != nil {
		return nil, err
	}
	caps, mapping, before, err := e.establish(ctx)
	if err != nil {
		return nil, err
	}
	desired, err := bootstrapProfile(before, core, delta)
	if err != nil {
		return nil, err
	}
	return e.runBody(ctx, caps, mapping, before, desired, nil)
}

// checkGate refuses new hardware work while a non-terminal document
// from a previous crash is still unrecovered.
func (e *Engine) checkGate() error {
	if d, derr := readDoc(e.storePath()); derr != nil {
		return coErr(co.KindStateCorrupt, "gate", derr)
	} else if d != nil && !terminal(d.Status) {
		return coErr(co.KindRecoveryRequired, "gate",
			fmt.Errorf("transaction %s is %s; recover it before starting a new one", d.TxID, d.Status))
	}
	return nil
}

// establish probes, confirms the read AND readback path, maps the
// topology and captures the exact, fingerprinted original snapshot.
func (e *Engine) establish(ctx context.Context) (co.Capabilities, co.Mapping, co.Snapshot, error) {
	caps, err := e.Backend.Probe(ctx)
	if err != nil {
		return caps, co.Mapping{}, co.Snapshot{}, err
	}
	if !caps.Supported.Known || !caps.Supported.Yes {
		return caps, co.Mapping{}, co.Snapshot{}, coErr(co.KindUnsupportedCPU, "probe", fmt.Errorf("tool/CPU support not confirmed"))
	}
	// Automatic apply/restore needs a confirmed read AND readback
	// path; otherwise only capability diagnostics are honest.
	if !caps.Read.Known || !caps.Read.Yes || !caps.Readback.Known || !caps.Readback.Yes {
		return caps, co.Mapping{}, co.Snapshot{}, coErr(co.KindNotValidated, "probe",
			fmt.Errorf("read/readback not confirmed: automatic apply refused"))
	}
	mapping, err := e.Backend.Topology(ctx)
	if err != nil {
		return caps, mapping, co.Snapshot{}, err
	}
	before, err := e.Backend.Read(ctx)
	if err != nil {
		return caps, mapping, before, err
	}
	if before.Quality != "exact" || before.HardwareID == "" {
		return caps, mapping, before, coErr(co.KindNotValidated, "read",
			fmt.Errorf("original snapshot not exact/fingerprinted (quality %q): cannot guarantee a safe restore", before.Quality))
	}
	return caps, mapping, before, nil
}

// bootstrapProfile copies the original profile and moves exactly one
// core by delta. The delta must actually change the value (a
// bootstrap that rewrites the same value proves nothing); the
// adapter's Validate still bounds the magnitude.
func bootstrapProfile(before co.Snapshot, core string, delta int) (co.Profile, error) {
	prof := co.Profile{Offsets: append([]co.Offset(nil), before.Profile.Offsets...)}
	for i := range prof.Offsets {
		if prof.Offsets[i].Core == core {
			nv := prof.Offsets[i].Value + delta
			if nv == prof.Offsets[i].Value {
				return co.Profile{}, coErr(co.KindBadProfile, "bootstrap",
					fmt.Errorf("delta %d leaves %s unchanged: a bootstrap that rewrites the same value proves nothing", delta, core))
			}
			prof.Offsets[i].Value = nv
			return prof, nil
		}
	}
	return co.Profile{}, coErr(co.KindBadProfile, "bootstrap",
		fmt.Errorf("%s is not a core the machine reports: cannot bootstrap a write against it", core))
}

// runBody is the durable transaction body shared by Run and
// BootstrapCore: validate, durably record apply_pending BEFORE any
// hardware call, apply, re-read and verify, and only on a clean match
// run the optional phase then restore the original and confirm by
// read. A partial or unknown apply outcome stops the automatic
// sequence (recovery required) rather than blindly restoring.
func (e *Engine) runBody(ctx context.Context, caps co.Capabilities, mapping co.Mapping, before co.Snapshot, profile co.Profile, phase func(context.Context) error) (*Result, error) {
	if len(profile.Offsets) == 0 {
		return nil, coErr(co.KindBadProfile, "validate", fmt.Errorf("empty profile: nothing to apply"))
	}
	if err := e.Backend.Validate(profile); err != nil {
		return nil, err
	}

	doc := &TxDoc{
		TxID:      newTxID(),
		Status:    StatusApplyPending,
		MappingFP: mappingFP(mapping),
		State:     co.NewState(caps, before, profile, co.WriteResult{}, co.Verification{}),
	}
	// The durable intent MUST be on disk (written + Synced) before
	// the first SMU change. A storage failure here means zero
	// hardware calls happened.
	if err := e.persist(doc, "apply_pending"); err != nil {
		return nil, coErr(co.KindStorage, "apply_pending",
			fmt.Errorf("intent not durable, no hardware call made: %w", err))
	}
	if r, abort, err := e.maybeAbort(doc, "apply_pending"); abort {
		return r, err
	}

	wr, applyErr := e.Backend.Apply(ctx, profile)
	doc.Ops = append(doc.Ops, opRec("apply", applyErr))
	doc.State = co.NewState(caps, before, profile, wr, co.Verification{})
	// Record the outcome (still apply_pending until proven clean).
	if err := e.persist(doc, "apply_result"); err != nil {
		return resultOf(doc), coErr(co.KindRecoveryRequired, "apply_result",
			fmt.Errorf("apply happened but outcome not durable: %w", err))
	}
	if r, abort, err := e.maybeAbort(doc, "apply_result"); abort {
		return r, err
	}
	// Stop the automatic sequence only when the apply CALL itself was
	// ambiguous (errored / cancelled / timed out) or only PART of the
	// cores were written: then even a readback could race a still-in-
	// flight write, and recovery must re-derive from a fresh read.
	// A clean apply call (no error) that reports UNKNOWN mutation is
	// the normal shape for a backend that refuses to self-confirm a
	// write from its own call log: the write call finished, and the
	// independent readback below - never the log - is what decides.
	// So proceed to the readback instead of stopping (the restore is
	// still gated by the pre-restore recheck, never blind).
	if applyErr != nil || wr.Mutated == co.MutatedPartial {
		return resultOf(doc), coErr(co.KindRecoveryRequired, "apply",
			fmt.Errorf("apply call outcome %s (err=%v): automatic sequence stopped, recovery must read the hardware", wr.Mutated, applyErr))
	}

	observed, obsErr := e.Backend.Read(ctx)
	doc.Observed = &observed
	doc.Ops = append(doc.Ops, opRec("read_observed", obsErr))
	ver, verErr := e.Backend.Verify(ctx, profile)
	doc.Ops = append(doc.Ops, opRec("verify", verErr))
	doc.State = co.NewState(caps, before, profile, wr, ver)

	clean := obsErr == nil && verErr == nil && observed.Quality == "exact" &&
		observed.HardwareID == before.HardwareID &&
		profileEqual(observed.Profile, profile) && ver.Match.Known && ver.Match.Yes
	if clean {
		doc.Status = StatusAppliedVerified
	}
	if err := e.persist(doc, "applied_verified"); err != nil {
		// Report the DISK truth, not the in-memory intent: the
		// record never made it, so the durable state is still
		// apply_pending and recovery decides from the hardware.
		doc.Status = StatusApplyPending
		return resultOf(doc), coErr(co.KindRecoveryRequired, "applied_verified",
			fmt.Errorf("applied state not durable: %w", err))
	}
	if r, abort, err := e.maybeAbort(doc, "applied_verified"); abort {
		return r, err
	}
	if !clean {
		return resultOf(doc), coErr(co.KindRecoveryRequired, "verify",
			fmt.Errorf("readback not a clean match (observed quality %q, match known=%v yes=%v): recovery required",
				observed.Quality, ver.Match.Known, ver.Match.Yes))
	}

	// Verified apply: run the optional phase, then ALWAYS restore
	// the original (even if the phase failed or cancelled), on a
	// fresh bounded cleanup context independent of the working one.
	var phaseErr error
	if phase != nil {
		phaseErr = phase(ctx)
	}

	restoreCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
	defer cancel()

	// Recheck immediately before restoring: no hardware compare-
	// and-swap exists and foreign tools ignore this lock, so a
	// foreign tool (or the phase itself) may have moved the profile.
	// Blindly restoring the original over an unexpected profile
	// would silently overwrite it - the sequence stops instead and
	// recovery re-derives.
	recheck, recheckErr := e.Backend.Read(restoreCtx)
	if recheckErr != nil || recheck.Quality != "exact" || recheck.HardwareID != before.HardwareID {
		return resultOf(doc), coErr(co.KindRecoveryRequired, "pre_restore_recheck",
			fmt.Errorf("trustworthy recheck impossible before restore (err=%v, quality %q): automatic restore stopped, recovery will re-derive",
				recheckErr, recheck.Quality))
	}
	if !profileEqual(recheck.Profile, profile) {
		if profileEqual(recheck.Profile, before.Profile) {
			// Already back at the saved original (the phase or a
			// foreign tool reverted it): close honestly with no
			// write of ours.
			doc.Status = StatusClosedNoChange
			if err := e.persist(doc, "closed"); err != nil {
				doc.Status = StatusAppliedVerified // disk truth
				return resultOf(doc), coErr(co.KindRecoveryRequired, "closed",
					fmt.Errorf("close not durable: %w", err))
			}
			res := resultOf(doc)
			res.Notes = append(res.Notes,
				"profile already back at the saved original before restore: no restore write performed (who reverted it is unknown)")
			if phaseErr != nil {
				res.Notes = append(res.Notes, "phase error: "+phaseErr.Error())
			}
			return res, nil
		}
		return resultOf(doc), coErr(co.KindExternalChange, "pre_restore_recheck",
			fmt.Errorf("profile changed under the transaction (neither the applied desired state nor the saved original): automatic restore stopped, recovery required"))
	}

	doc.Status = StatusRestorePending
	if err := e.persist(doc, "restore_pending"); err != nil {
		doc.Status = StatusAppliedVerified // disk truth: intent never landed
		return resultOf(doc), coErr(co.KindRecoveryRequired, "restore_pending",
			fmt.Errorf("restore intent not durable: %w", err))
	}
	if r, abort, err := e.maybeAbort(doc, "restore_pending"); abort {
		return r, err
	}
	_, restoreErr := e.Backend.Restore(restoreCtx, before)
	doc.Ops = append(doc.Ops, opRec("restore", restoreErr))
	final, finalErr := e.Backend.Read(restoreCtx)
	doc.Ops = append(doc.Ops, opRec("read_final", finalErr))
	// Crash AFTER the restore ran but before its terminal record:
	// the disk still says restore_pending and recovery must close
	// the transaction by observation, never re-blind-apply.
	if r, abort, err := e.maybeAbort(doc, "restore_called"); abort {
		return r, err
	}
	restoredOK := restoreErr == nil && finalErr == nil && final.Quality == "exact" &&
		final.HardwareID == before.HardwareID && profileEqual(final.Profile, before.Profile)
	if restoredOK {
		doc.Status = StatusRestored
	} else {
		doc.Status = StatusRestoredFailed
	}
	if err := e.persist(doc, "terminal"); err != nil {
		doc.Status = StatusRestorePending // disk truth: recovery will close by observation
		return resultOf(doc), coErr(co.KindRecoveryRequired, "terminal",
			fmt.Errorf("terminal state not durable: %w", err))
	}
	if r, abort, err := e.maybeAbort(doc, "terminal"); abort {
		return r, err
	}
	res := resultOf(doc)
	if phaseErr != nil {
		res.Notes = append(res.Notes, "phase error: "+phaseErr.Error())
	}
	if !restoredOK {
		res.Notes = append(res.Notes, fmt.Sprintf("restore not confirmed (restore err=%v, final read err=%v quality %q)",
			restoreErr, finalErr, final.Quality))
	}
	return res, nil
}

// Recover re-derives the truth from a fresh read and reconciles a
// non-terminal transaction. It never re-executes a terminal
// transaction, never re-applies the requested profile, and refuses
// to act across identity/topology drift or an external profile.
func (e *Engine) Recover(ctx context.Context) (*Result, error) {
	lk, err := lockfile.Acquire(e.lockPath(), "co-recover")
	if err != nil {
		return nil, lockErr(err)
	}
	defer lk.Release()

	d, err := readDoc(e.storePath())
	if err != nil {
		return nil, coErr(co.KindStateCorrupt, "recover", err)
	}
	if d == nil {
		return &Result{Notes: []string{"no transaction document; nothing to recover"}}, nil
	}
	if terminal(d.Status) {
		res := resultOf(d)
		res.Notes = append(res.Notes, "terminal transaction: never re-executed")
		return res, nil
	}

	if _, err := e.Backend.Probe(ctx); err != nil {
		return resultOf(d), err
	}
	mapping, err := e.Backend.Topology(ctx)
	if err != nil {
		return resultOf(d), err
	}
	if mappingFP(mapping) != d.MappingFP {
		return resultOf(d), coErr(co.KindFingerprint, "recover",
			fmt.Errorf("tool/topology mapping changed since the transaction: refusing automatic restore"))
	}
	cur, err := e.Backend.Read(ctx)
	if err != nil || cur.Quality != "exact" || cur.HardwareID == "" {
		return resultOf(d), coErr(co.KindRecoveryRequired, "recover",
			fmt.Errorf("current read not trustworthy (quality %q, err=%v): automatic writes stopped", cur.Quality, err))
	}
	if cur.HardwareID != d.State.Before.HardwareID {
		return resultOf(d), coErr(co.KindFingerprint, "recover",
			fmt.Errorf("hardware fingerprint changed since the transaction: refusing to overwrite"))
	}

	desired := d.State.Desired
	before := d.State.Before
	orig := d.Status // what the disk actually holds before we move it
	switch {
	case profileEqual(cur.Profile, before.Profile):
		// The machine already shows the original. After a RECORDED
		// restore intent that closes as restored-by-observation
		// (the restore call may have landed before the process
		// died); without a restore intent it closes as
		// closed_no_change - either way we never claim the apply
		// did not happen.
		note := "current profile already equals the saved original; NOT claiming the apply never happened"
		if d.Status == StatusRestorePending || d.Status == StatusRestoredFailed {
			d.Status = StatusRestored
			note = "restored by observation: current profile equals the saved original after a recorded restore intent"
		} else {
			d.Status = StatusClosedNoChange
		}
		if err := e.persist(d, "closed"); err != nil {
			d.Status = orig // disk truth
			return resultOf(d), coErr(co.KindRecoveryRequired, "closed", fmt.Errorf("close not durable: %w", err))
		}
		res := resultOf(d)
		res.Notes = append(res.Notes, note)
		return res, nil
	case profileEqual(cur.Profile, desired):
		restoreCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		d.Status = StatusRestorePending
		if err := e.persist(d, "restore_pending"); err != nil {
			d.Status = orig // disk truth: no durable restore intent
			return resultOf(d), coErr(co.KindRecoveryRequired, "restore_pending", fmt.Errorf("restore intent not durable: %w", err))
		}
		if r, abort, err := e.maybeAbort(d, "restore_pending"); abort {
			return r, err
		}
		_, restoreErr := e.Backend.Restore(restoreCtx, before)
		d.Ops = append(d.Ops, opRec("restore", restoreErr))
		final, finalErr := e.Backend.Read(restoreCtx)
		d.Ops = append(d.Ops, opRec("read_final", finalErr))
		if r, abort, err := e.maybeAbort(d, "restore_called"); abort {
			return r, err
		}
		if restoreErr == nil && finalErr == nil && final.Quality == "exact" &&
			final.HardwareID == before.HardwareID && profileEqual(final.Profile, before.Profile) {
			d.Status = StatusRestored
		} else {
			d.Status = StatusRestoredFailed
		}
		if err := e.persist(d, "terminal"); err != nil {
			d.Status = StatusRestorePending // disk truth: close by observation later
			return resultOf(d), coErr(co.KindRecoveryRequired, "terminal", fmt.Errorf("terminal state not durable: %w", err))
		}
		return resultOf(d), nil
	default:
		return resultOf(d), coErr(co.KindExternalChange, "recover",
			fmt.Errorf("current profile matches neither the saved original nor the requested profile: refusing to overwrite external changes"))
	}
}
