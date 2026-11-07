package cotx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Default absolute locations, cwd-independent on purpose: two
// clients starting from different directories must collide on the
// same lock and the same document, or the exclusivity is a lie.
// Tests point both at a t.TempDir().

func coTempBase() string { return os.TempDir() }

// DefaultLockPath is the OS-backed CO ownership file (internal/
// lockfile whole-file lock). It is never deleted; only the held
// lock proves anything.
func DefaultLockPath() string { return filepath.Join(coTempBase(), "cpu-stability-co.lock") }

// DefaultStorePath is the single durable transaction document.
func DefaultStorePath() string { return filepath.Join(coTempBase(), "cpu-stability-co-tx.json") }

// readDoc reads the current document. No document at all is not an
// error: it returns (nil, nil) and callers must remember that after
// power loss a missing document does NOT prove no transaction ever
// ran (see the durability contract in the package doc). Anything
// unparseable is returned as an error - never repaired, never
// deleted.
func readDoc(path string) (*TxDoc, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("co tx: cannot read %s: %w", path, err)
	}
	d, perr := ParseDoc(data)
	if perr != nil {
		return nil, perr
	}
	return &d, nil
}

// writeDoc replaces the document atomically: unique temp in the
// target directory, full write, Sync, Close, single Rename. Any
// failure removes the temp and leaves the previous document (if any)
// exactly as it was - a failed replacement never half-updates the
// recovery state. (Process-crash durable; power-loss limits are in
// the package doc.)
func writeDoc(path string, d TxDoc) error {
	b, err := d.MarshalJSON()
	if err != nil {
		return err
	}
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("co tx: cannot create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("co tx: write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("co tx: Sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("co tx: close failed: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("co tx: rename failed: %w", err)
	}
	return nil
}
