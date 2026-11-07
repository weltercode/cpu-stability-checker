package cotx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
	"github.com/weltercode/cpu-stability-checker/internal/lockfile"
)

// --- fixtures -----------------------------------------------------

var (
	profA = co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -10}, {Core: "core1", Value: -20}}}
	profB = co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -30}, {Core: "core1", Value: -40}}}
	profX = co.Profile{Offsets: []co.Offset{{Core: "core0", Value: 5}, {Core: "core1", Value: 7}}} // foreign
)

func ip(i int) *int { return &i }

func snap(p co.Profile, fp string) co.Snapshot {
	return co.Snapshot{Profile: p, Quality: "exact", HardwareID: fp}
}

// goodFake scripts a backend that supports everything; reads are the
// scenario's hardware sequence (the last one repeats forever).
func goodFake(reads ...co.Snapshot) *co.Fake {
	return &co.Fake{
		Caps: co.Capabilities{
			Supported: co.Yes("ok"), Read: co.Yes("ok"), Readback: co.Yes("ok"),
			Apply: co.Unknown("ok"), RangeKnown: true, RangeMin: -99, RangeMax: 99,
		},
		Map: co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
			{ToolID: "core0", SMUIndex: ip(0)}, {ToolID: "core1", SMUIndex: ip(1)},
		}},
		Reads:       reads,
		ApplyResult: co.WriteResult{Mutated: co.MutatedCompleted},
		VerifyRes:   co.Verification{Match: co.Yes("match")},
		RestoreRes:  co.Verification{Match: co.Yes("match")},
	}
}

func newEngine(t *testing.T, f *co.Fake) *Engine {
	t.Helper()
	dir := t.TempDir()
	return &Engine{
		Backend:   f,
		LockPath:  filepath.Join(dir, "co.lock"),
		StorePath: filepath.Join(dir, "co-tx.json"),
	}
}

func callsStr(calls []string) string { return strings.Join(calls, ",") }

func hasCall(calls []string, op string) bool {
	for _, c := range calls {
		if c == op {
			return true
		}
	}
	return false
}

func countCall(calls []string, op string) int {
	n := 0
	for _, c := range calls {
		if c == op {
			n++
		}
	}
	return n
}

var errCrash = errors.New("simulated process death")

// --- happy path ----------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	ran := false
	res, err := e.Run(context.Background(), profB, func(context.Context) error { ran = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("phase hook did not run")
	}
	if res.Status != StatusRestored {
		t.Fatalf("status = %s, want %s (notes %v)", res.Status, StatusRestored, res.Notes)
	}
	want := "probe,topology,read,validate,apply,read,verify,read,restore,read"
	if got := callsStr(f.Calls); got != want {
		t.Errorf("call order:\n got %s\nwant %s", got, want)
	}
	doc, err := readDoc(e.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusRestored || !terminal(doc.Status) {
		t.Errorf("durable status = %s", doc.Status)
	}
	if doc.TxID == "" || doc.Observed == nil || !profileEqual(doc.Observed.Profile, profB) {
		t.Errorf("document lost the observed apply: %+v", doc.Observed)
	}
	if doc.State.Before.HardwareID != "fp1" || !profileEqual(doc.State.Desired, profB) {
		t.Errorf("document lost identity/request: %+v", doc.State)
	}
	if len(doc.Ops) < 5 {
		t.Errorf("operation ledger too thin: %+v", doc.Ops)
	}
}

// --- bootstrap single-core round-trip ------------------------------

// BootstrapCore derives the desired profile from its OWN before-read
// (one core moved by the delta), runs the full durable round-trip,
// and restores the touched core to its exact original.
func TestBootstrapCoreRoundTrip(t *testing.T) {
	desired := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: -11}, {Core: "core1", Value: -20}}}
	f := goodFake(snap(profA, "fp1"), snap(desired, "fp1"), snap(desired, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	res, err := e.BootstrapCore(context.Background(), "core0", -1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRestored {
		t.Fatalf("status = %s, want %s (notes %v)", res.Status, StatusRestored, res.Notes)
	}
	want := "probe,topology,read,validate,apply,read,verify,read,restore,read"
	if got := callsStr(f.Calls); got != want {
		t.Errorf("call order:\n got %s\nwant %s", got, want)
	}
	doc, err := readDoc(e.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	// The desired the engine DERIVED must be exactly one core moved
	// by the delta - not a hand-passed profile.
	if !profileEqual(doc.State.Desired, desired) {
		t.Errorf("engine derived the wrong desired profile: %+v", doc.State.Desired)
	}
	if doc.Observed == nil || !profileEqual(doc.Observed.Profile, desired) {
		t.Errorf("observed apply not the bootstrapped value: %+v", doc.Observed)
	}
}

func TestBootstrapCoreGuards(t *testing.T) {
	// delta 0 leaves the value unchanged: a bootstrap that rewrites
	// the same value proves nothing, so it is refused before any call.
	f := goodFake(snap(profA, "fp1"))
	e := newEngine(t, f)
	if _, err := e.BootstrapCore(context.Background(), "core0", 0); !co.IsKind(err, co.KindBadProfile) {
		t.Errorf("zero delta not refused: %v", err)
	}
	if hasCall(f.Calls, "apply") {
		t.Errorf("zero-delta bootstrap reached apply: %v", f.Calls)
	}
	// a core the machine does not report cannot be bootstrapped.
	f = goodFake(snap(profA, "fp1"))
	e = newEngine(t, f)
	if _, err := e.BootstrapCore(context.Background(), "core9", -1); !co.IsKind(err, co.KindBadProfile) {
		t.Errorf("unknown core not refused: %v", err)
	}
	if hasCall(f.Calls, "apply") {
		t.Errorf("unknown-core bootstrap reached apply: %v", f.Calls)
	}
}

// --- storage failures gate hardware calls --------------------------

func TestStorageFailureAtApplyPendingMeansZeroHardwareCalls(t *testing.T) {
	f := goodFake(snap(profA, "fp1"))
	e := newEngine(t, f)
	e.storeFault = func(step string) error {
		if step == "apply_pending" {
			return errors.New("disk on fire")
		}
		return nil
	}
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindStorage) {
		t.Fatalf("want storage kind, got %v", err)
	}
	for _, c := range f.Calls {
		if c == "apply" || c == "restore" {
			t.Fatalf("hardware call (%s) despite storage failure: %s", c, callsStr(f.Calls))
		}
	}
	if _, err := os.Stat(e.StorePath); !os.IsNotExist(err) {
		t.Error("a failed intent write left a document behind")
	}
}

func TestStorageFailureAtAppliedVerifiedLeavesRecoverableState(t *testing.T) {
	// Apply ran (hardware B), but the applied_verified record fails:
	// disk stays apply_pending; recovery must re-derive from the
	// hardware and restore.
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	e.storeFault = func(step string) error {
		if step == "applied_verified" {
			return errors.New("disk full")
		}
		return nil
	}
	res, err := e.Run(context.Background(), profB, nil)
	if !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("want recovery_required, got %v", err)
	}
	if res.Status != StatusApplyPending {
		t.Errorf("in-memory status %s must match the on-disk apply_pending", res.Status)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("engine auto-restored past a storage failure: %s", callsStr(f.Calls))
	}
	e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
	res2, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusRestored {
		t.Fatalf("recovery status = %s, want restored", res2.Status)
	}
}

func TestStorageFailureAtRestorePendingBlocksRestoreAttempt(t *testing.T) {
	// No durable restore intent => the restore is NOT attempted.
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	e.storeFault = func(step string) error {
		if step == "restore_pending" {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("want recovery_required, got %v", err)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("restore attempted without durable intent: %s", callsStr(f.Calls))
	}
	doc, _ := readDoc(e.StorePath)
	if doc.Status != StatusAppliedVerified {
		t.Fatalf("disk status = %s, want applied_verified", doc.Status)
	}
	e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
	res2, err := e2.Recover(context.Background())
	if err != nil || res2.Status != StatusRestored {
		t.Fatalf("recovery: status=%s err=%v", res2.Status, err)
	}
}

func TestStorageFailureAtTerminalRecordStaysUnresolved(t *testing.T) {
	// The restore landed (hardware A) but the terminal record fails:
	// disk stays restore_pending; recovery closes it BY OBSERVATION.
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	e.storeFault = func(step string) error {
		if step == "terminal" {
			return errors.New("disk full")
		}
		return nil
	}
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("want recovery_required, got %v", err)
	}
	e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
	res2, err := e2.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusRestored {
		t.Fatalf("recovery status = %s, want restored (by observation)", res2.Status)
	}
	if countCall(f.Calls, "restore") != 1 {
		t.Errorf("recovery re-ran the restore: %s", callsStr(f.Calls))
	}
}

// --- crash before/after every durable boundary ---------------------

func TestCrashAtEveryBoundaryRecovers(t *testing.T) {
	cases := []struct {
		name       string
		crashAt    string
		reads      []co.Snapshot // hardware sequence across crash + recovery
		wantStatus string
	}{
		// crash before any hardware call: hardware still A.
		{"before apply", "apply_pending",
			[]co.Snapshot{snap(profA, "fp1"), snap(profA, "fp1")},
			StatusClosedNoChange},
		// crash after apply landed (hardware B), before readback:
		// recovery must restore.
		{"after apply", "apply_result",
			[]co.Snapshot{snap(profA, "fp1"), snap(profB, "fp1"), snap(profA, "fp1")},
			StatusRestored},
		// crash after applied_verified on disk.
		{"after verified", "applied_verified",
			[]co.Snapshot{snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1")},
			StatusRestored},
		// crash after restore intent, before the restore call.
		{"after restore intent", "restore_pending",
			[]co.Snapshot{snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1")},
			StatusRestored},
		// crash after the restore landed (hardware A), before the
		// terminal record: recovery closes by observation.
		{"after restore call", "restore_called",
			[]co.Snapshot{snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"), snap(profA, "fp1")},
			StatusRestored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := goodFake(tc.reads...)
			e := newEngine(t, f)
			e.abortAfter = func(step string) error {
				if step == tc.crashAt {
					return errCrash
				}
				return nil
			}
			if _, err := e.Run(context.Background(), profB, nil); !errors.Is(err, errCrash) {
				t.Fatalf("expected the crash sentinel, got %v", err)
			}
			// A fresh Engine over the same paths = the next process.
			e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
			res, err := e2.Recover(context.Background())
			if err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			if res.Status != tc.wantStatus {
				t.Fatalf("recovered to %s, want %s (notes %v)", res.Status, tc.wantStatus, res.Notes)
			}
			// The terminal decision is durable.
			doc, derr := readDoc(e2.StorePath)
			if derr != nil || !terminal(doc.Status) || doc.Status != tc.wantStatus {
				t.Fatalf("durable terminal state wrong: %+v err=%v", doc, derr)
			}
			// Recovery retry never re-executes: no extra restore.
			before := countCall(f.Calls, "restore")
			res2, err := e2.Recover(context.Background())
			if err != nil || !strings.Contains(strings.Join(res2.Notes, " "), "never re-executed") {
				t.Fatalf("retry of a terminal transaction misbehaved: %+v err=%v", res2, err)
			}
			if countCall(f.Calls, "restore") != before {
				t.Errorf("retry re-ran hardware writes: %s", callsStr(f.Calls))
			}
		})
	}
}

// --- partial/unknown outcomes stop the automatic sequence ----------

func TestPartialApplyStopsForRecoveryWithoutAutoRestore(t *testing.T) {
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"))
	f.ApplyResult = co.WriteResult{Mutated: co.MutatedPartial, Notes: []string{"half the cores"}}
	f.ApplyErr = &co.Error{Kind: co.KindPartialApply, Op: "apply"}
	e := newEngine(t, f)
	_, err := e.Run(context.Background(), profB, nil)
	if !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("want recovery_required, got %v", err)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("auto-restored after a partial apply: %s", callsStr(f.Calls))
	}
	doc, _ := readDoc(e.StorePath)
	if doc.Status != StatusApplyPending || doc.State.Result.Mutated != co.MutatedPartial {
		t.Errorf("partial outcome not stored separately on disk: %+v", doc)
	}
	var found bool
	for _, op := range doc.Ops {
		if op.Op == "apply" && op.Kind == co.KindPartialApply {
			found = true
		}
	}
	if !found {
		t.Errorf("operation ledger lost the failure kind: %+v", doc.Ops)
	}
}

func TestReadbackMismatchStopsWithoutAutoRestore(t *testing.T) {
	// The backend VERDICT says match, but the engine's own observed
	// read disagrees: the engine trusts its read, not the verdict.
	f := goodFake(snap(profA, "fp1"), snap(profX, "fp1"))
	e := newEngine(t, f)
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("want recovery_required, got %v", err)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("auto-restored past a mismatched readback: %s", callsStr(f.Calls))
	}
}

// --- untrustworthy preconditions refuse before any write -----------

func TestUntrustworthyPreconditionsRefuseWrites(t *testing.T) {
	t.Run("before read partial", func(t *testing.T) {
		f := goodFake(co.Snapshot{Profile: profA, Quality: "partial"})
		e := newEngine(t, f)
		if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindNotValidated) {
			t.Fatalf("got %v", err)
		}
		if hasCall(f.Calls, "apply") {
			t.Error("apply ran on an untrustworthy before-state")
		}
	})
	t.Run("read path unconfirmed", func(t *testing.T) {
		f := goodFake(snap(profA, "fp1"))
		f.Caps.Read = co.Unknown("terse answer did not parse")
		e := newEngine(t, f)
		if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindNotValidated) {
			t.Fatalf("got %v", err)
		}
		if hasCall(f.Calls, "apply") {
			t.Error("apply ran with an unconfirmed read path")
		}
	})
	t.Run("empty profile", func(t *testing.T) {
		f := goodFake(snap(profA, "fp1"))
		e := newEngine(t, f)
		if _, err := e.Run(context.Background(), co.Profile{}, nil); !co.IsKind(err, co.KindBadProfile) {
			t.Fatalf("got %v", err)
		}
		if hasCall(f.Calls, "apply") {
			t.Error("empty profile reached the backend")
		}
	})
}

// --- lock, gate and document integrity -----------------------------

func TestSecondClientIsBlockedRegardlessOfCwd(t *testing.T) {
	f := goodFake(snap(profA, "fp1"))
	e := newEngine(t, f)
	lk, err := lockfile.Acquire(e.LockPath, "other client")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil { // different cwd
		t.Fatal(err)
	}
	defer os.Chdir(old)
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindLockHeld) {
		t.Fatalf("second client got: %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("blocked client touched the backend: %s", callsStr(f.Calls))
	}
	if !filepath.IsAbs(e.LockPath) || !filepath.IsAbs(e.StorePath) {
		t.Error("default collision points must be absolute")
	}
	lk.Release()
}

func TestNonTerminalDocumentGatesNewRuns(t *testing.T) {
	e := newEngine(t, goodFake(snap(profA, "fp1")))
	stuck := TxDoc{
		TxID: "stuck-1", Status: StatusAppliedVerified, MappingFP: "x",
		State: co.NewState(co.Capabilities{}, snap(profA, "fp1"), profB, co.WriteResult{}, co.Verification{}),
	}
	if err := writeDoc(e.StorePath, stuck); err != nil {
		t.Fatal(err)
	}
	f := e.Backend.(*co.Fake)
	if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("got %v", err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("gated run touched the backend: %s", callsStr(f.Calls))
	}
	// A terminal document does NOT gate: the old transaction is closed.
	done := stuck
	done.Status = StatusRestored
	if err := writeDoc(e.StorePath, done); err != nil {
		t.Fatal(err)
	}
	f2 := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e2 := &Engine{Backend: f2, LockPath: e.LockPath, StorePath: e.StorePath}
	if res, err := e2.Run(context.Background(), profB, nil); err != nil || res.Status != StatusRestored {
		t.Fatalf("terminal document gated a new run: %+v err=%v", res, err)
	}
}

func TestCorruptDocumentsBlockAndSurvive(t *testing.T) {
	cases := map[string]string{
		"garbage":         `not json at all`,
		"truncated tail":  `{"doc_version":1,"tx_id":"t","stat`,
		"future version":  `{"doc_version":2,"tx_id":"t","status":"apply_pending"}`,
		"missing version": `{"tx_id":"t","status":"apply_pending"}`,
		"unknown status":  `{"doc_version":1,"tx_id":"t","status":"wat"}`,
		"unknown field":   `{"doc_version":1,"tx_id":"t","status":"apply_pending","hacked":true}`,
		"empty id":        `{"doc_version":1,"tx_id":"","status":"apply_pending"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := goodFake(snap(profA, "fp1"))
			e := newEngine(t, f)
			if err := os.WriteFile(e.StorePath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Run(context.Background(), profB, nil); !co.IsKind(err, co.KindStateCorrupt) {
				t.Errorf("Run accepted a %s document: %v", name, err)
			}
			if _, err := e.Recover(context.Background()); !co.IsKind(err, co.KindStateCorrupt) {
				t.Errorf("Recover accepted a %s document: %v", name, err)
			}
			got, _ := os.ReadFile(e.StorePath)
			if string(got) != body {
				t.Errorf("the engine modified or deleted the damaged document (B: reading history never rewrites)")
			}
			if len(f.Calls) != 0 {
				t.Errorf("backend touched despite corrupt state: %s", callsStr(f.Calls))
			}
		})
	}
}

// --- external changes and identity drift ---------------------------

func TestExternalChangeDuringPhaseStopsRestore(t *testing.T) {
	// The third read (pre-restore recheck) shows a foreign profile:
	// restoring the original would silently overwrite it.
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profX, "fp1"))
	e := newEngine(t, f)
	_, err := e.Run(context.Background(), profB, nil)
	if !co.IsKind(err, co.KindExternalChange) {
		t.Fatalf("got %v", err)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("restore ran over an external profile: %s", callsStr(f.Calls))
	}
	doc, _ := readDoc(e.StorePath)
	if doc.Status != StatusAppliedVerified {
		t.Errorf("document should stay applied_verified for recovery, got %s", doc.Status)
	}
	// Recovery sees the same foreign state and also refuses.
	e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
	if _, err := e2.Recover(context.Background()); !co.IsKind(err, co.KindExternalChange) {
		t.Fatalf("recovery overwrote or accepted an external profile: %v", err)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("recovery wrote over an external profile: %s", callsStr(f.Calls))
	}
}

func TestExternalRevertClosesWithoutRestore(t *testing.T) {
	// The recheck already shows the original (someone reverted it):
	// close honestly, write nothing.
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	res, err := e.Run(context.Background(), profB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusClosedNoChange {
		t.Fatalf("status = %s, want closed_no_change", res.Status)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("restore ran although the machine was already back: %s", callsStr(f.Calls))
	}
}

func TestRecoveryRefusesIdentityDrift(t *testing.T) {
	t.Run("hardware fingerprint moved", func(t *testing.T) {
		f := goodFake(snap(profA, "fp1"), snap(profA, "fp2")) // current read: same profile, different machine identity
		e := newEngine(t, f)
		e.abortAfter = func(step string) error {
			if step == "apply_result" {
				return errCrash
			}
			return nil
		}
		if _, err := e.Run(context.Background(), profB, nil); !errors.Is(err, errCrash) {
			t.Fatalf("got %v", err)
		}
		e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
		if _, err := e2.Recover(context.Background()); !co.IsKind(err, co.KindFingerprint) {
			t.Fatalf("recovery acted across a fingerprint change: %v", err)
		}
		if hasCall(f.Calls, "restore") {
			t.Errorf("restore across identity drift: %s", callsStr(f.Calls))
		}
	})
	t.Run("mapping moved", func(t *testing.T) {
		f := goodFake(snap(profA, "fp1"), snap(profA, "fp1"))
		e := newEngine(t, f)
		e.abortAfter = func(step string) error {
			if step == "apply_pending" {
				return errCrash
			}
			return nil
		}
		if _, err := e.Run(context.Background(), profB, nil); !errors.Is(err, errCrash) {
			t.Fatalf("got %v", err)
		}
		f.Map.Slots = append(f.Map.Slots, co.CoreSlot{ToolID: "core2", SMUIndex: ip(2)})
		e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
		if _, err := e2.Recover(context.Background()); !co.IsKind(err, co.KindFingerprint) {
			t.Fatalf("recovery acted across a mapping change: %v", err)
		}
	})
}

func TestRecoveryStopsOnUntrustworthyRead(t *testing.T) {
	f := goodFake(snap(profA, "fp1"), co.Snapshot{Profile: profA, Quality: "unknown"})
	e := newEngine(t, f)
	e.abortAfter = func(step string) error {
		if step == "apply_pending" {
			return errCrash
		}
		return nil
	}
	if _, err := e.Run(context.Background(), profB, nil); !errors.Is(err, errCrash) {
		t.Fatalf("got %v", err)
	}
	e2 := &Engine{Backend: f, LockPath: e.LockPath, StorePath: e.StorePath}
	if _, err := e2.Recover(context.Background()); !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("recovery wrote on an untrustworthy read: %v", err)
	}
	if hasCall(f.Calls, "restore") || hasCall(f.Calls, "apply") {
		t.Errorf("automatic writes despite untrustworthy read: %s", callsStr(f.Calls))
	}
}

// --- cancellation and cleanup context ------------------------------

func TestCancelledPhaseStillRestoresOnCleanupContext(t *testing.T) {
	// The working context dies inside the phase; the restore must
	// still run on a fresh bounded context (the fake returns
	// cancelled for ANY call on a dead context, so a successful
	// restore proves the context was independent).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := goodFake(snap(profA, "fp1"), snap(profB, "fp1"), snap(profB, "fp1"), snap(profA, "fp1"))
	e := newEngine(t, f)
	e.CleanupTimeout = 5 * time.Second // explicit: the field is honored
	res, err := e.Run(ctx, profB, func(context.Context) error {
		cancel()
		return errors.New("the test phase aborted")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != StatusRestored {
		t.Fatalf("status = %s, want restored (notes %v)", res.Status, res.Notes)
	}
	if !hasCall(f.Calls, "restore") {
		t.Fatalf("no restore after a cancelled phase: %s", callsStr(f.Calls))
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "phase error") {
		t.Errorf("phase failure hidden: %v", res.Notes)
	}
}

func TestPreCancelledContextTouchesNothing(t *testing.T) {
	// A dead working context fails at the first backend call
	// (probe): nothing is documented, nothing is applied.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := goodFake(snap(profA, "fp1"))
	e := newEngine(t, f)
	if _, err := e.Run(ctx, profB, nil); !co.IsKind(err, co.KindCancelled) {
		t.Fatalf("got %v", err)
	}
	if len(f.Calls) != 1 || f.Calls[0] != "probe" {
		t.Errorf("calls after a pre-cancelled run: %s", callsStr(f.Calls))
	}
	if _, serr := os.Stat(e.StorePath); !os.IsNotExist(serr) {
		t.Error("a cancelled run created a document")
	}
}

func TestCancelledApplyNeverBlindRestores(t *testing.T) {
	// A cancelled apply means UNKNOWN mutation: the automatic
	// sequence stops at recovery_required, it does not blind-restore
	// a state we never observed.
	f := goodFake(snap(profA, "fp1"))
	f.ApplyErr = &co.Error{Kind: co.KindCancelled, Op: "apply"}
	f.ApplyResult = co.WriteResult{Mutated: co.MutatedUnknown}
	e := newEngine(t, f)
	res, err := e.Run(context.Background(), profB, nil)
	if !co.IsKind(err, co.KindRecoveryRequired) {
		t.Fatalf("cancelled apply must stop the sequence, got %v", err)
	}
	if res.Status != StatusApplyPending {
		t.Errorf("status = %s, want apply_pending", res.Status)
	}
	if hasCall(f.Calls, "restore") {
		t.Errorf("blind restore after a cancelled apply: %s", callsStr(f.Calls))
	}
}

func TestMissingDocumentRecoveryIsHonest(t *testing.T) {
	f := goodFake(snap(profA, "fp1"))
	e := newEngine(t, f)
	res, err := e.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("recovery touched the backend with no document: %s", callsStr(f.Calls))
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "nothing to recover") {
		t.Errorf("missing note: %v", res.Notes)
	}
}

// A failed replacement must preserve the prior valid document and
// leave no temp debris: recovery state is never half-updated.
func TestFailedReplacementPreservesPriorDocument(t *testing.T) {
	e := newEngine(t, goodFake(snap(profA, "fp1")))
	good := TxDoc{
		TxID: "keepme", Status: StatusRestored, MappingFP: "m",
		State: co.NewState(co.Capabilities{}, snap(profA, "fp1"), profB, co.WriteResult{}, co.Verification{}),
	}
	if err := writeDoc(e.StorePath, good); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(e.StorePath)
	if err := os.Chmod(e.StorePath, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(e.StorePath, 0o644)
	bad := good
	bad.TxID = "overwrite-attempt"
	bad.Status = StatusApplyPending
	err := writeDoc(e.StorePath, bad)
	tmps, _ := filepath.Glob(filepath.Join(filepath.Dir(e.StorePath), "*.tmp-*"))
	if len(tmps) != 0 {
		t.Errorf("temp files left behind after replacement: %v", tmps)
	}
	if err == nil {
		// POSIX happily replaces read-only targets; the invariant
		// that must hold everywhere is: no temp debris (checked
		// above). On Windows the replacement fails and the prior
		// document must be byte-identical:
		return
	}
	after, _ := os.ReadFile(e.StorePath)
	if string(after) != string(before) {
		t.Errorf("failed replacement damaged the prior document")
	}
}

func TestParseDocRoundTripAndStrictness(t *testing.T) {
	d := TxDoc{
		TxID: "abc", Status: StatusApplyPending, MappingFP: "m",
		State: co.NewState(
			co.Capabilities{Supported: co.Unknown("why not")},
			snap(profA, "fp"), profB,
			co.WriteResult{Mutated: co.MutatedUnknown, Notes: []string{"call log"}},
			co.Verification{Match: co.No("differs")}),
	}
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocVersion != DocVersion || got.TxID != "abc" {
		t.Errorf("envelope drifted: %+v", got)
	}
	if got.State.Caps.Supported.Known || got.State.Caps.Supported.Why != "why not" {
		t.Errorf("unknown reason lost: %+v", got.State.Caps.Supported)
	}
	if got.State.Result.Mutated != co.MutatedUnknown || got.State.Verify.Match.Yes {
		t.Errorf("outcome details lost: %+v", got.State)
	}
	// profileEqual is order-insensitive set equality.
	p1 := co.Profile{Offsets: []co.Offset{{Core: "core1", Value: 1}, {Core: "core0", Value: 2}}}
	p2 := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: 2}, {Core: "core1", Value: 1}}}
	if !profileEqual(p1, p2) {
		t.Error("reordered profile compared unequal")
	}
	dup := co.Profile{Offsets: []co.Offset{{Core: "core0", Value: 2}, {Core: "core0", Value: 2}}}
	if profileEqual(dup, p2) {
		t.Error("duplicate cores compared equal")
	}
}
