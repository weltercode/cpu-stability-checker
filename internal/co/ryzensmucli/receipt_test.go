package ryzensmucli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// identityAdapter has a validated build and a complete fingerprint
// (caps + topoID + mapping) already established, and an isolated
// receipt path, so receipt behaviour can be exercised without running
// the tool.
func identityAdapter(t *testing.T) *Adapter {
	t.Helper()
	a := adapter(t, &fakeRunner{})
	a.caps = co.Capabilities{Tool: "ryzen-smu-cli", Version: "0.1.3", SHA256: "exedigest", PayloadSHA256: "payloaddigest",
		Supported: co.Yes("ok"), BuildValidated: true, SyntaxKnown: true, SyntaxMin: -99, SyntaxMax: 99}
	a.mapping = &co.Mapping{Quality: "partial", Slots: []co.CoreSlot{
		{ToolID: "core0", SMUIndex: intp(0)}, {ToolID: "core1", SMUIndex: intp(1)}}}
	a.topoID = "f:11;e:11;k:2"
	a.ReceiptPath = filepath.Join(t.TempDir(), "receipt.json")
	return a
}

func TestReceiptRoundTripAndApply(t *testing.T) {
	a := identityAdapter(t)
	if err := a.WriteVerifiedReceipt("tx1", "core1", -12, -13); err != nil {
		t.Fatal(err)
	}
	st, err := a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found || !st.Applies {
		t.Fatalf("receipt should apply: found=%v applies=%v reasons=%v", st.Found, st.Applies, st.Reasons)
	}
	r := st.Receipt
	if r.Core != "core1" || r.FromValue != -12 || r.ToValue != -13 || r.TxID != "tx1" {
		t.Errorf("receipt content wrong: %+v", r)
	}
}

// A receipt is bound to the exact build. If the exe digest drifts
// (a different or updated binary), the receipt is still found but must
// not apply - it is not evidence about a build it was not recorded for.
func TestReceiptIdentityDrift(t *testing.T) {
	a := identityAdapter(t)
	if err := a.WriteVerifiedReceipt("tx1", "core1", -12, -13); err != nil {
		t.Fatal(err)
	}
	a.caps.SHA256 = "a-different-build"
	st, err := a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found {
		t.Fatal("receipt should still be found")
	}
	if st.Applies {
		t.Fatal("a drifted build must not have the receipt apply")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "exe digest") {
		t.Errorf("drift reason missing: %v", st.Reasons)
	}
}

// Audit C02 regressions: the payload is a first-class identity axis of
// the receipt. A receipt written for one payload must not apply after
// a payload swap (even with the launcher untouched); and a receipt
// written before the payload axis existed cannot vouch for the code
// that ran - found, but honestly does not apply.
func TestReceiptPayloadDriftAndLegacy(t *testing.T) {
	a := identityAdapter(t)
	if err := a.WriteVerifiedReceipt("tx1", "core1", -12, -13); err != nil {
		t.Fatal(err)
	}
	a.caps.PayloadSHA256 = "swapped-managed-payload"
	st, err := a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found {
		t.Fatal("receipt should still be found")
	}
	if st.Applies {
		t.Fatal("a payload-swapped build must not have the receipt apply")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "payload digest") {
		t.Errorf("payload drift reason missing: %v", st.Reasons)
	}

	// Legacy doc: same v1 envelope, payload field simply absent.
	if err := os.WriteFile(a.ReceiptPath,
		[]byte(`{"doc_version":1,"txid":"tx-old","tool":"ryzen-smu-cli","version":"0.1.3","exe_sha256":"exedigest","host":"x","topo_id":"f:11;e:11;k:2","core":"core1","from_value":-12,"to_value":-13,"verified_at":"2026-10-18T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Found {
		t.Fatal("legacy receipt should still be found")
	}
	if st.Applies {
		t.Fatal("a receipt that never recorded the payload must not apply")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "predates the managed-payload identity") {
		t.Errorf("legacy payload reason missing: %v", st.Reasons)
	}
}

func TestReceiptCoreNotAddressable(t *testing.T) {
	a := identityAdapter(t)
	if err := a.WriteVerifiedReceipt("tx1", "core9", -12, -13); err != nil {
		t.Fatal(err)
	}
	st, _ := a.CheckVerifiedReceipt()
	if st.Applies {
		t.Fatal("a receipt for a non-addressable core must not apply")
	}
	if !strings.Contains(strings.Join(st.Reasons, " "), "not addressable") {
		t.Errorf("reason missing: %v", st.Reasons)
	}
}

// An unvalidated build must not mint a receipt: it would be evidence
// for a binary that was never confirmed to be the pinned one.
func TestReceiptUnvalidatedBuildRefused(t *testing.T) {
	a := identityAdapter(t)
	a.caps.BuildValidated = false
	if err := a.WriteVerifiedReceipt("tx1", "core1", -12, -13); !co.IsKind(err, co.KindNotValidated) {
		t.Errorf("receipt on an unvalidated build not refused: %v", err)
	}
}

// Reading history never rewrites it: a corrupt receipt errors and the
// bytes stay exactly as they were (never repaired, never deleted).
func TestReceiptCorruptNotRepaired(t *testing.T) {
	a := identityAdapter(t)
	if err := os.WriteFile(a.ReceiptPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CheckVerifiedReceipt(); err == nil {
		t.Fatal("a corrupt receipt must error")
	}
	b, _ := os.ReadFile(a.ReceiptPath)
	if string(b) != "{not json" {
		t.Errorf("reading the receipt mutated it: %q", b)
	}
}

func TestReceiptMissing(t *testing.T) {
	a := identityAdapter(t)
	st, err := a.CheckVerifiedReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if st.Found {
		t.Fatal("no receipt should be found")
	}
}
