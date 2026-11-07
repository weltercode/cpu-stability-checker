package ryzensmucli

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/cotx"
)

// This file wires the REAL adapter to the REAL cotx engine against a
// stateful fake machine. It is the exact composition the first real
// SMU write will use: the engine derives the desired profile from its
// own before-read, the adapter's bootstrap authority accepts a single-
// core delta only on a build validated against the pinned provenance,
// the write touches ONLY the changed core, and the restore puts that
// one core back to its exact original. No real SMU is touched here;
// the fake machine's state survives across every read the engine
// makes, so the readbacks are genuinely produced by the writes rather
// than scripted.

// stateRunner is a stateful fake machine: --get-offsets-terse returns
// the current offsets, --offset mutates them.
type stateRunner struct {
	offsets []int
	calls   [][]string
}

func (s *stateRunner) Run(ctx context.Context, exe string, args ...string) (string, int, error) {
	s.calls = append(s.calls, append([]string{exe}, args...))
	switch args[0] {
	case "--version":
		return verOK, 0, nil
	case "--get-physical-cores":
		return phys16, 0, nil
	case "--get-enabled-cores":
		return enabled3, 0, nil
	case "--get-offsets-terse":
		return "Current PBO offsets:\n" + joinInts(s.offsets) + "\n", 0, nil
	case "--offset":
		for _, pair := range strings.Split(args[1], ",") {
			n, v, ok := parseOffsetPair(pair)
			if !ok || n < 0 || n >= len(s.offsets) {
				return "Error: bad offset\n", 1, nil
			}
			s.offsets[n] = v
		}
		return "Set logical core offset applied.\n", 0, nil
	}
	return "unknown option\n", 1, nil
}

// offsetCalls returns just the --offset argument strings, in order.
func (s *stateRunner) offsetCalls() []string {
	var out []string
	for _, c := range s.calls {
		if len(c) >= 3 && c[1] == "--offset" {
			out = append(out, c[2])
		}
	}
	return out
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

func parseOffsetPair(pair string) (int, int, bool) {
	i := strings.IndexByte(pair, ':')
	if i < 0 {
		return 0, 0, false
	}
	n, err1 := strconv.Atoi(strings.TrimSpace(pair[:i]))
	v, err2 := strconv.Atoi(strings.TrimSpace(pair[i+1:]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return n, v, true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBootstrapEndToEndAgainstFakeHardware(t *testing.T) {
	orig := []int{-10, -20, -30} // all negative, like the user's live tuning
	sr := &stateRunner{offsets: append([]int(nil), orig...)}
	a := New(tempExe(t), sr)
	// Present the pinned launcher AND payload digests so Probe marks
	// the build VALIDATED (version+commit come from verOK); the
	// bootstrap authority requires it, and every write re-checks both.
	pinBuild(a)
	dir := t.TempDir()
	e := &cotx.Engine{
		Backend:   a,
		LockPath:  filepath.Join(dir, "co.lock"),
		StorePath: filepath.Join(dir, "co-tx.json"),
	}
	res, err := e.BootstrapCore(context.Background(), "core1", -1)
	if err != nil {
		t.Fatalf("bootstrap round-trip failed: %v", err)
	}
	if res.Status != cotx.StatusRestored {
		t.Fatalf("status = %s, want %s (notes %v)", res.Status, cotx.StatusRestored, res.Notes)
	}
	// The fake machine is back at the exact original profile.
	if !equalInts(sr.offsets, orig) {
		t.Fatalf("hardware not restored: got %v, want %v", sr.offsets, orig)
	}
	// Exactly two writes happened (apply + restore), each touching
	// ONLY core1: 1:-21 then 1:-20. No other core was ever written.
	got := sr.offsetCalls()
	want := []string{"1:-21", "1:-20"}
	if !equalStrs(got, want) {
		t.Fatalf("offset writes = %v, want %v (only core1 may be touched)", got, want)
	}
}

// An unvalidated build must never bootstrap-write, even for a minimal
// delta: the digest axes (launcher and managed payload) are what stop
// tampered or payload-swapped binaries.
func TestBootstrapEndToEndUnvalidatedBuildRefused(t *testing.T) {
	orig := []int{-10, -20, -30}
	sr := &stateRunner{offsets: append([]int(nil), orig...)}
	a := New(tempExe(t), sr)
	// exeSHA returns a digest that does NOT match the pinned build.
	a.exeSHA = func(string) (string, error) { return strings.Repeat("0", 64), nil }
	dir := t.TempDir()
	e := &cotx.Engine{
		Backend:   a,
		LockPath:  filepath.Join(dir, "co.lock"),
		StorePath: filepath.Join(dir, "co-tx.json"),
	}
	if _, err := e.BootstrapCore(context.Background(), "core1", -1); !co.IsKind(err, co.KindNotValidated) {
		t.Fatalf("bootstrap on an unvalidated build not refused: %v", err)
	}
	if len(sr.offsetCalls()) != 0 {
		t.Fatalf("unvalidated build reached the SMU: %v", sr.offsetCalls())
	}
	if !equalInts(sr.offsets, orig) {
		t.Fatalf("unvalidated build changed hardware: %v", sr.offsets)
	}
}

// The single most important safety property for the first real write:
// if the process dies AFTER the write landed but BEFORE the restore,
// a fresh process (new adapter + engine, same durable paths) must put
// the touched core back to its exact original - authorized by the
// saved transaction, NOT by a range or a receipt (none exists yet),
// and writing ONLY that core.
func TestBootstrapCrashRecoveryEndToEnd(t *testing.T) {
	orig := []int{-10, -20, -30}
	sr := &stateRunner{offsets: append([]int(nil), orig...)}
	a := New(tempExe(t), sr)
	pinBuild(a)
	dir := t.TempDir()
	lock := filepath.Join(dir, "co.lock")
	store := filepath.Join(dir, "co-tx.json")

	// Process 1: crash right after the apply outcome is durable. The
	// fake machine is left at the bootstrapped value; the document is
	// non-terminal (apply_pending with the apply result recorded).
	e1 := &cotx.Engine{Backend: a, LockPath: lock, StorePath: store}
	e1.SetAbortAfter(func(step string) error {
		if step == "apply_result" {
			return errSimulatedDeath
		}
		return nil
	})
	if _, err := e1.BootstrapCore(context.Background(), "core1", -1); err == nil {
		t.Fatal("expected the simulated crash to surface as an error")
	}
	if equalInts(sr.offsets, orig) || sr.offsets[1] != -21 {
		t.Fatalf("crash should leave core1 at -21, got %v", sr.offsets)
	}

	// Process 2: fresh adapter + engine over the SAME fake machine and
	// durable paths. Recovery must restore core1 to -20 and confirm.
	a2 := New(tempExe(t), sr)
	pinBuild(a2)
	e2 := &cotx.Engine{Backend: a2, LockPath: lock, StorePath: store}
	res, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if res.Status != cotx.StatusRestored {
		t.Fatalf("recovery status = %s, want %s (notes %v)", res.Status, cotx.StatusRestored, res.Notes)
	}
	if !equalInts(sr.offsets, orig) {
		t.Fatalf("recovery did not restore the touched core: got %v, want %v", sr.offsets, orig)
	}
	// Across crash + recovery only core1 was ever written: 1:-21 (the
	// crashed apply) then 1:-20 (the recovery restore). No other core.
	if got, want := sr.offsetCalls(), []string{"1:-21", "1:-20"}; !equalStrs(got, want) {
		t.Fatalf("offset writes across crash+recovery = %v, want %v", got, want)
	}
}

// After a verified round-trip, the CLI records an identity-bound
// receipt; CheckVerifiedReceipt then reports it as applying to this
// machine/build/topology. (The receipt is evidence, not permission.)
func TestBootstrapEndToEndWritesReceipt(t *testing.T) {
	orig := []int{-10, -20, -30}
	sr := &stateRunner{offsets: append([]int(nil), orig...)}
	a := New(tempExe(t), sr)
	pinBuild(a)
	a.ReceiptPath = filepath.Join(t.TempDir(), "receipt.json")
	dir := t.TempDir()
	e := &cotx.Engine{Backend: a, LockPath: filepath.Join(dir, "co.lock"), StorePath: filepath.Join(dir, "co-tx.json")}
	res, err := e.BootstrapCore(context.Background(), "core1", -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != cotx.StatusRestored {
		t.Fatalf("status = %s, want restored", res.Status)
	}
	// Mirror what `co verify-hardware` does on a confirmed restore.
	if err := a.WriteVerifiedReceipt(res.TxID, "core1", -20, -21); err != nil {
		t.Fatal(err)
	}
	st, err := a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found || !st.Applies {
		t.Fatalf("receipt should apply after a verified round-trip: found=%v applies=%v reasons=%v", st.Found, st.Applies, st.Reasons)
	}
	if st.Receipt.Core != "core1" || st.Receipt.FromValue != -20 || st.Receipt.ToValue != -21 {
		t.Errorf("receipt content wrong: %+v", st.Receipt)
	}
}

var errSimulatedDeath = errors.New("simulated process death")
