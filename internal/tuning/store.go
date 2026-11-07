package tuning

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultPlanPath is the absolute, cwd-independent location of the
// durable tuning plan, in the same base as the cotx transaction
// document and receipt so every client sees the same one.
func DefaultPlanPath() string {
	return filepath.Join(os.TempDir(), "cpu-stability-co-tune.json")
}

// DefaultTuneLockPath is the tuning-session lock. It is deliberately
// a DIFFERENT path from the cotx lock: the trial holds the tuning
// session while cotx.Run takes and releases the CO lock inside it,
// so one lock would self-deadlock.
func DefaultTuneLockPath() string {
	return filepath.Join(os.TempDir(), "cpu-stability-co-tune.lock")
}

// Store is the strict single-document plan store. Every Save is one
// atomic replacement (unique temp, full write, Sync, Close, Rename);
// the previous main document is first copied to <path>.bak so a
// truncated main has an explicit, user-promoted recovery path. A
// failed Save removes the temp and leaves main (and bak) exactly as
// they were. Process-crash durable; power-loss limits match the cotx
// store (no directory-flush primitive on Windows).
type Store struct {
	Path string // "" -> DefaultPlanPath()

	// storeFault, when set, is consulted before each durable write
	// with the step name; a non-nil return makes that write fail.
	// Test seam.
	storeFault func(step string) error

	// abortAfter, when set, is consulted right after each successful
	// durable write; a non-nil return aborts as if the process died
	// there (the document is now at that state on disk). Test seam
	// for crash-before/after-each durable trial transition.
	abortAfter func(step string) error
}

func (s *Store) path() string {
	if s.Path != "" {
		return s.Path
	}
	return DefaultPlanPath()
}
func (s *Store) bakPath() string { return s.path() + ".bak" }

// SetStoreFault installs a storage-failure seam consulted before
// each durable write (step name in, error out). Test/diagnostic use.
func (s *Store) SetStoreFault(fn func(step string) error) { s.storeFault = fn }

// SetAbortAfter installs a crash seam consulted right after each
// successful durable write; a non-nil return aborts the operation as
// if the process died there. Test use.
func (s *Store) SetAbortAfter(fn func(step string) error) { s.abortAfter = fn }

// Load reads the plan. No plan at all is not an error: it returns
// (nil, nil). A corrupt or wrong-version main is a *PlanCorruptError
// carrying what the backup (if any) holds - reading NEVER rewrites,
// repairs or deletes anything.
func (s *Store) Load() (*Plan, error) {
	b, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("co tune: cannot read %s: %w", s.path(), err)
	}
	p, perr := ParsePlan(b)
	if perr == nil {
		return &p, nil
	}
	ce := &PlanCorruptError{Path: s.path(), MainErr: perr}
	bb, berr := os.ReadFile(s.bakPath())
	if berr == nil {
		_, bperr := ParsePlan(bb)
		if bperr == nil {
			ce.BakGood = true
		} else {
			ce.BakErr = bperr
		}
	} else if !os.IsNotExist(berr) {
		ce.BakErr = berr
	}
	return nil, ce
}

// PlanCorruptError reports an unreadable main document and whether a
// parseable backup exists. The recovery is explicit (RecoverBackup),
// never automatic.
type PlanCorruptError struct {
	Path    string
	MainErr error
	BakGood bool
	BakErr  error
}

func (e *PlanCorruptError) Error() string {
	if e.BakGood {
		return fmt.Sprintf("co tune: plan %s is corrupt (%v); a parseable backup exists at %s - promote it explicitly, nothing was repaired", e.Path, e.MainErr, e.Path+".bak")
	}
	if e.BakErr != nil {
		return fmt.Sprintf("co tune: plan %s is corrupt (%v) and the backup is unreadable (%v): fail closed", e.Path, e.MainErr, e.BakErr)
	}
	return fmt.Sprintf("co tune: plan %s is corrupt (%v) and no backup exists: fail closed", e.Path, e.MainErr)
}

// RecoverBackup explicitly promotes the parseable backup over the
// corrupt main (atomic copy). It is a deliberate operation, never
// part of Load, and it refuses when the backup is missing or
// unparseable. The corrupt bytes are preserved at <path>.corrupt-*
// before replacement so the damaged history is never destroyed.
func (s *Store) RecoverBackup() (*Plan, error) {
	bb, err := os.ReadFile(s.bakPath())
	if err != nil {
		return nil, fmt.Errorf("co tune: no backup to promote at %s: %w", s.bakPath(), err)
	}
	p, perr := ParsePlan(bb)
	if perr != nil {
		return nil, fmt.Errorf("co tune: backup is not parseable: %w (nothing promoted)", perr)
	}
	if b, rerr := os.ReadFile(s.path()); rerr == nil {
		keep := fmt.Sprintf("%s.corrupt-%d", s.path(), time.Now().UnixNano())
		if werr := os.WriteFile(keep, b, 0o644); werr != nil {
			return nil, fmt.Errorf("co tune: cannot preserve corrupt plan: %w (nothing promoted)", werr)
		}
	}
	if err := writeFileAtomic(s.path(), bb); err != nil {
		return nil, err
	}
	return &p, nil
}

// Save durably replaces the plan document. The previous main is
// copied to <path>.bak first (best-effort copy of the previous
// bytes: a crash between the two renames leaves main at the old
// state and bak at the older one, never a half-written document).
// The storeFault/abortAfter seams honor the named step.
func (s *Store) Save(p Plan, step string) error {
	if s.storeFault != nil {
		if err := s.storeFault(step); err != nil {
			return fmt.Errorf("co tune: storage failed before %s: %w", step, err)
		}
	}
	p.UpdatedAt = time.Now()
	b, err := p.MarshalJSON()
	if err != nil {
		return err
	}
	if cur, rerr := os.ReadFile(s.path()); rerr == nil {
		// Preserve the previous main as the backup BEFORE replacing
		// it. A failure here is reported but does not block the
		// save: the backup is recovery convenience, the main write
		// is the contract.
		if werr := writeFileAtomic(s.bakPath(), cur); werr != nil {
			return fmt.Errorf("co tune: cannot write backup for %s: %w", step, werr)
		}
	}
	if err := writeFileAtomic(s.path(), b); err != nil {
		return fmt.Errorf("co tune: durable write failed at %s: %w", step, err)
	}
	if s.abortAfter != nil {
		if err := s.abortAfter(step); err != nil {
			return &CrashAfterError{Step: step, Err: err}
		}
	}
	return nil
}

// CrashAfterError reports that the durable write SUCCEEDED and the
// crash seam then aborted the operation: the document on disk is at
// that step. Callers must treat it as "the process died here", not
// as "the write failed".
type CrashAfterError struct {
	Step string
	Err  error
}

func (e *CrashAfterError) Error() string {
	return fmt.Sprintf("co tune: aborted after durable write at %s: %v", e.Step, e.Err)
}

func (e *CrashAfterError) Unwrap() error { return e.Err }

// MarshalJSON pins the schema version on the way out.
func (p Plan) MarshalJSON() ([]byte, error) {
	p.PlanVersion = PlanVersion
	type planNoMethod Plan
	return json.Marshal(planNoMethod(p))
}

// writeFileAtomic: unique temp in the target directory, full write,
// Sync, Close, single Rename; any failure removes the temp and
// leaves the target exactly as it was.
func writeFileAtomic(path string, data []byte) error {
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("Sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close failed: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename failed: %w", err)
	}
	return nil
}
