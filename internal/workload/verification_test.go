package workload

import (
	"context"
	"testing"
)

// A06: the fault class is explicit registry data, never name
// guessing: ram-* kernels are memory evidence, the prober is
// synthetic, everything else (including custom and external
// workloads without a class) keeps the established compute contract.
func TestFaultClassOf(t *testing.T) {
	cases := map[string]string{
		"ram-constant": FaultMemory,
		"ram-address":  FaultMemory,
		"ram-moving":   FaultMemory,
		"ram-random":   FaultMemory,
		"ram-walking":  FaultMemory,
		"ram-reverse":  FaultMemory,
		"ram-perm":     FaultMemory,
		"ram-stride":   FaultMemory,
		"ram-copy":     FaultMemory,
		"ram-dwell":    FaultMemory,
		"fault":        FaultSynthetic,
		"int":          FaultCompute,
		"mem":          FaultCompute,
		"mem-bw":       FaultCompute,
		"ghost":        FaultCompute, // unknown name: the conservative default
	}
	for name, want := range cases {
		if got := FaultClassOf(name); got != want {
			t.Errorf("FaultClassOf(%q) = %q, want %q", name, got, want)
		}
	}
	Register(&Workload{Name: "r01-custom", Run: func(context.Context, Reporter) (uint64, error) { return 0, nil }})
	defer Delete("r01-custom")
	if got := FaultClassOf("r01-custom"); got != FaultCompute {
		t.Errorf("registered workload without a class: %q, want %q (never silently no-blame)", got, FaultCompute)
	}
}

// The verification class decides what may be counted as a confirmed
// check; a wrong class here would inflate coverage claims.
func TestVerificationClasses(t *testing.T) {
	cases := []struct {
		name       string
		wantClass  Verification
		wantChecks bool
	}{
		{"int", VerifyIndependent, true},
		{"float", VerifyRepeated, true},
		// sha256 moved from KAT-probes-only to VerifyIndependent in
		// P04-C2: every main-work digest is now compared against the
		// scalar reference (itself anchored to published vectors), so
		// the main work itself is verified, not just sampled.
		{"sha256", VerifyIndependent, true},
		// float-exact compares the FPU path bitwise against an
		// independent integer reference on exactly representable
		// values: a genuine independent comparison, not a repeat.
		{"float-exact", VerifyIndependent, true},
		{"mem", VerifyIndependent, true},
		{"mem-bw", VerifyNone, false},
		{"fault", VerifyNone, false},
		{"no-such-workload", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if w, ok := Get(tt.name); ok && w.Verification != tt.wantClass {
				t.Errorf("class = %q, want %q", w.Verification, tt.wantClass)
			}
			if got := CountsIterationsAsChecks(tt.name); got != tt.wantChecks {
				t.Errorf("CountsIterationsAsChecks(%q) = %v, want %v", tt.name, got, tt.wantChecks)
			}
		})
	}
}

// Externally registered tools are never self-checking from our side:
// their real verification protocol is P03 work, so until then every
// run with them must report validation=unknown, not a pass.
func TestExternalRegisteredAsExternal(t *testing.T) {
	RegisterExternal(map[string]ExternalSpec{"class-probe": {Command: "cmd", Description: "probe"}})
	t.Cleanup(func() { Delete("class-probe") })
	w, ok := Get("class-probe")
	if !ok {
		t.Fatal("not registered")
	}
	if w.Verification != VerifyExternal {
		t.Errorf("class = %q, want %q", w.Verification, VerifyExternal)
	}
	if CountsIterationsAsChecks("class-probe") {
		t.Error("external beats must never count as verified checks")
	}
}
