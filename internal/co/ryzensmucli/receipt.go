package ryzensmucli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// A verifiedReceipt is the durable, identity-bound record that the CO
// write path was verified end-to-end (apply -> independent readback ->
// restore -> confirm) on THIS machine with THIS validated build, via a
// specific transaction. It is EVIDENCE, not permission: it never
// establishes a writable range (RangeKnown stays false) and never
// authorizes a write by itself. Its purpose is to let a later read say
// "the write path has already been verified here" and to let P11 gate
// its first tuning write on that recorded fact instead of bootstrapping
// blind. If any identity component drifts (a different build, host or
// topology) the receipt is found but does not apply.

type verifiedReceipt struct {
	DocVersion int    `json:"doc_version"`
	TxID       string `json:"txid"`
	Tool       string `json:"tool"`
	Version    string `json:"version"`
	ExeSHA256  string `json:"exe_sha256"`
	// PayloadSHA256 binds the measured managed-payload digest at
	// receipt time (audit C02). It is additive inside doc_version 1 -
	// absent means "written before the payload axis existed", an
	// honest unknown: such a receipt no longer APPLIES, because it
	// cannot vouch for which code really ran.
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
	Host          string `json:"host"`
	TopoID        string `json:"topo_id"`
	Core          string `json:"core"`
	FromValue     int    `json:"from_value"`
	ToValue       int    `json:"to_value"`
	VerifiedAt    string `json:"verified_at"` // RFC3339
}

const receiptDocVersion = 1

// DefaultReceiptPath is the absolute, cwd-independent location of the
// verified-write-path receipt, in the same base as the transaction
// document so two clients from different directories see the same one.
func DefaultReceiptPath() string {
	return filepath.Join(os.TempDir(), "cpu-stability-co-receipt.json")
}

// WriteVerifiedReceipt records that a bootstrap round-trip on `core`
// (from -> to, then restored to from) completed cleanly under
// transaction txid, bound to the currently established identity. It
// requires a build validated against the pinned provenance and a
// complete fingerprint: a receipt minted by an unvalidated build or an
// unconfirmed machine would be evidence for nothing. Atomic replace
// (temp + Sync + rename); a failure leaves any prior receipt exactly
// as it was.
func (a *Adapter) WriteVerifiedReceipt(txid, core string, from, to int) error {
	if !a.caps.BuildValidated {
		return &co.Error{Kind: co.KindNotValidated, Op: "receipt",
			Err: fmt.Errorf("cannot record a verified-write receipt for an unvalidated build")}
	}
	fp, missing := a.fingerprint()
	if fp == "" {
		return &co.Error{Kind: co.KindFingerprint, Op: "receipt",
			Err: fmt.Errorf("identity incomplete (missing: %v): cannot bind a receipt to an unconfirmed machine", missing)}
	}
	host, _ := os.Hostname()
	r := verifiedReceipt{
		DocVersion: receiptDocVersion,
		TxID:       txid,
		Tool:       a.caps.Tool,
		Version:    a.caps.Version,
		ExeSHA256:  a.caps.SHA256,
		// BuildValidated (required above) now entails a measured
		// payload digest, so this binds the code, not just the launcher.
		PayloadSHA256: a.caps.PayloadSHA256,
		Host:          host,
		TopoID:        a.topoID,
		Core:          core,
		FromValue:     from,
		ToValue:       to,
		VerifiedAt:    time.Now().Format(time.RFC3339),
	}
	return writeReceipt(a.receiptPath(), r)
}

// readReceipt reads the receipt. No receipt at all is not an error: it
// returns (nil, nil). Anything unparseable or of the wrong version is
// an error - never repaired, never deleted (reading history never
// rewrites it).
func readReceipt(path string) (*verifiedReceipt, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("co receipt: cannot read %s: %w", path, err)
	}
	var r verifiedReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("co receipt: cannot parse %s: %w (never repaired or deleted)", path, err)
	}
	if r.DocVersion != receiptDocVersion {
		return nil, fmt.Errorf("co receipt: %s has doc_version %d, want %d", path, r.DocVersion, receiptDocVersion)
	}
	return &r, nil
}

// writeReceipt replaces the receipt atomically: unique temp in the
// target directory, full write, Sync, Close, single Rename. Any failure
// removes the temp and leaves the previous receipt (if any) untouched.
// Process-crash durable; power-loss limits match the transaction store.
func writeReceipt(path string, r verifiedReceipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir, base := filepath.Split(path)
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("co receipt: cannot create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("co receipt: write failed: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("co receipt: Sync failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("co receipt: close failed: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("co receipt: rename failed: %w", err)
	}
	return nil
}

// ReceiptStatus reports what the receipt says and whether it still
// applies to the current machine, build and topology.
type ReceiptStatus struct {
	Found   bool
	Receipt verifiedReceipt
	Applies bool
	Reasons []string // why it does NOT apply (empty when it does)
}

// CheckVerifiedReceipt reads the receipt and validates it against the
// adapter's CURRENTLY established identity. Call it after Probe and
// Topology so caps, topoID and mapping are known. A receipt recorded
// for a different build, host or topology is found but does not apply;
// a core no longer addressable is reported. It never authorizes
// anything - it only reports the recorded, verified fact.
func (a *Adapter) CheckVerifiedReceipt() (ReceiptStatus, error) {
	r, err := readReceipt(a.receiptPath())
	if err != nil {
		return ReceiptStatus{}, err
	}
	if r == nil {
		return ReceiptStatus{Found: false}, nil
	}
	st := ReceiptStatus{Found: true, Receipt: *r, Applies: true}
	host, _ := os.Hostname()
	if r.Tool != a.caps.Tool {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("tool %q != current %q", r.Tool, a.caps.Tool))
	}
	if r.Version != a.caps.Version {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("version %q != current %q", r.Version, a.caps.Version))
	}
	if r.ExeSHA256 != a.caps.SHA256 {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("exe digest %s != current %s", sh(r.ExeSHA256, 12), sh(a.caps.SHA256, 12)))
	}
	// Audit C02: the launcher check alone would still trust a
	// payload-swapped build. A receipt written before the payload axis
	// existed cannot identify the code it vouched for - found, but it
	// honestly no longer applies.
	if r.PayloadSHA256 == "" {
		st.Applies = false
		st.Reasons = append(st.Reasons, "receipt predates the managed-payload identity: the code it vouched for cannot be re-identified")
	} else if r.PayloadSHA256 != a.caps.PayloadSHA256 {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("payload digest %s != current %s", sh(r.PayloadSHA256, 12), sh(a.caps.PayloadSHA256, 12)))
	}
	if r.Host != host {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("host %q != current %q", r.Host, host))
	}
	if r.TopoID != a.topoID {
		st.Applies = false
		st.Reasons = append(st.Reasons, fmt.Sprintf("topology %q != current %q", r.TopoID, a.topoID))
	}
	if a.mapping != nil {
		found := false
		for _, s := range a.mapping.Slots {
			if s.ToolID == r.Core {
				found = true
				break
			}
		}
		if !found {
			st.Applies = false
			st.Reasons = append(st.Reasons, fmt.Sprintf("core %s is not addressable in the current topology", r.Core))
		}
	}
	return st, nil
}
