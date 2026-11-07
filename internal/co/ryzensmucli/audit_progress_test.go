package ryzensmucli

// Audit C02 probes, kept as permanent regressions (R04b): the managed
// payload beside the apphost launcher is a first-class build-identity
// axis, not a note string.
//
//  1. an untouched launcher with a DIFFERENT payload must not keep
//     BuildValidated;
//  2. an unreadable/missing payload keeps the digest an honest unknown
//     and the build UNVALIDATED;
//  3. a payload swapped AFTER a validated Probe must never reach the
//     SMU: installCurrent re-digests launcher and payload immediately
//     before the only mutating call, with zero runner calls (B04).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

func TestAuditManagedPayloadIdentity(t *testing.T) {
	exe := tempExe(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(exe), "ryzen-smu-cli.dll"), []byte("different managed payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	a := New(exe, r)
	// Keep the launcher's identity unchanged; the payload is different.
	a.exeSHA = func(string) (string, error) { return pinnedProvenance.ExeSHA256, nil }
	caps, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.BuildValidated {
		t.Fatal("BuildValidated=true despite a different ryzen-smu-cli.dll")
	}
	notes := strings.Join(caps.Notes, " ")
	if !strings.Contains(notes, "payload digest") || !strings.Contains(notes, "UNVALIDATED") {
		t.Errorf("the payload axis is not stated in the identity notes: %v", caps.Notes)
	}

	// Missing payload: the digest stays an honest unknown (no probe
	// error just because the paired file is gone), and the build stays
	// UNVALIDATED.
	a2 := New(tempExe(t), &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}})
	a2.exeSHA = func(string) (string, error) { return pinnedProvenance.ExeSHA256, nil }
	if err := os.Remove(payloadPath(a2.Exe)); err != nil {
		t.Fatal(err)
	}
	caps2, err := a2.Probe(context.Background())
	if err != nil {
		t.Fatalf("missing payload must not fail the probe itself: %v", err)
	}
	if caps2.PayloadSHA256 != "" || caps2.BuildValidated {
		t.Errorf("unreadable payload must stay unknown and unvalidated: %+v", caps2)
	}
}

func TestAuditPayloadSwapAfterProbe(t *testing.T) {
	r := &fakeRunner{outs: []string{verOK, phys16, terse3}, codes: []int{0, 0, 0}}
	a := adapter(t, r)
	pinBuild(a)
	caps, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.BuildValidated {
		t.Fatalf("fixture build should validate: %v", caps.Notes)
	}
	// Swap only the payload after the validated Probe (same launcher,
	// same version string): every write must now refuse before the
	// tool is ever called.
	a.dllSHA = func(string) (string, error) { return strings.Repeat("f", 64), nil }
	_, err = a.applyOffsets(context.Background(), []co.Offset{{Core: "core0", Value: -12}})
	if !co.IsKind(err, co.KindNotValidated) {
		t.Fatalf("post-probe payload swap not refused: %v", err)
	}
	if len(r.calls) != 3 {
		t.Errorf("refusal reached the runner (%d calls, want the 3 probe calls only): %v", len(r.calls), r.calls)
	}
}
