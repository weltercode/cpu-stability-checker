// Package lockfile provides OS-enforced single-owner locks. The
// kernel - not a pid heuristic - decides whether a holder is alive:
// the lock is a whole-file advisory lock (Windows LockFileEx, POSIX
// flock) held on a file handle that stays open for the whole
// operation. A crashed holder's lock vanishes with its process; a
// live holder is never evicted, whatever the file's metadata claims.
//
// The file itself is persistent: it is never deleted. There is no
// stale-file race to win because there is no file replacement at
// all, and no release can remove an owner it does not have. The
// contents are diagnostics only: they name the holder for humans,
// liveness never trusts them.
package lockfile

import (
	"fmt"
	"os"
	"strings"
)

// Lock is one held OS lock. Keep it (and its handle) alive for the
// whole operation being protected: Release, process exit or a crash
// all give up the lock at the OS level.
type Lock struct {
	path string
	f    *os.File
}

// HeldError reports that a live owner holds the lock. Metadata is
// the file's current content, best-effort: it may be stale, empty
// or garbage, and it is for humans only. The proof of liveness is
// the failed OS lock, never this text.
type HeldError struct {
	Path     string
	Metadata string
	Err      error
}

func (e *HeldError) Error() string {
	meta := strings.Join(strings.Fields(e.Metadata), " ")
	if meta == "" {
		meta = "no metadata recorded"
	}
	return fmt.Sprintf("%s is locked by a live owner (%s)", e.Path, meta)
}

// Acquire takes the OS lock on path (creating the file when
// missing) and keeps the handle open: the kernel tracks liveness
// through the handle. A live holder yields *HeldError; any other
// error means the lock state could not be established, and unknown
// is never treated as free. metadata is written after the lock is
// ours, diagnostics only: a metadata write failure never
// invalidates the lock we hold.
func Acquire(path, metadata string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if lerr := lockHandle(f); lerr != nil {
		existing, _ := os.ReadFile(path) // diagnostic text, best-effort
		f.Close()
		return nil, &HeldError{Path: path, Metadata: string(existing), Err: lerr}
	}
	if metadata != "" {
		if _, werr := f.WriteAt([]byte(metadata), 0); werr == nil {
			_ = f.Truncate(int64(len(metadata)))
			_ = f.Sync()
		}
	}
	return &Lock{path: path, f: f}, nil
}

// Path reports the file this lock was taken on.
func (l *Lock) Path() string { return l.path }

// Release gives up the lock. The file stays on disk: its existence
// proves nothing, only the lock does. Releasing a nil or already
// released lock is a no-op.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	uerr := unlockHandle(l.f)
	if cerr := l.f.Close(); uerr == nil {
		uerr = cerr
	}
	l.f = nil
	return uerr
}
