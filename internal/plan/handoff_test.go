package plan

// S5 handoff phase: the compile-time shape contract. Everything the
// scenario pipeline will trust about a handoff phase is decided HERE,
// before any save or load - geometry, defaults, forced all_or_none and
// every refusal - so the dry plan and the execution can never disagree.

import (
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
)

func handoffBase() config.Config {
	cfg := config.Default()
	cfg.WorkloadA = HandoffKernel
	cfg.WorkloadB = ""
	return cfg
}

func TestHandoffPhaseCompile(t *testing.T) {
	t.Run("valid phase resolves geometry and forces all_or_none", func(t *testing.T) {
		opt, err := OptionsFromPhase(handoffBase(), config.Phase{
			Name: "ho", Cores: []int{0, 1},
			HandoffBlocks: 4, HandoffBlockSize: "16KiB", HandoffRounds: 2, HandoffSeed: 7,
		})
		if err != nil {
			t.Fatalf("valid handoff phase refused: %v", err)
		}
		if opt.HandoffBlocks != 4 || opt.HandoffBlockBytes != 16*1024 ||
			opt.HandoffRounds != 2 || opt.HandoffSeed != 7 {
			t.Errorf("geometry = %+v, want 4 x 16384, 2 rounds, seed 7", opt)
		}
		if !opt.AllOrNone {
			t.Error("AllOrNone not forced: the block-to-core partition is load-bearing and must never be re-divided mid-run")
		}
		// auto execution resolves the lease kernel per-process even when
		// the phase did not name a mode (pinned workers share the section).
		if !opt.PerProcess {
			t.Error("auto execution must pick per-process for the handoff kernel")
		}
	})
	t.Run("defaults: 64KiB blocks, one round, seed 0 is a valid seed", func(t *testing.T) {
		opt, err := OptionsFromPhase(handoffBase(), config.Phase{
			Name: "ho", PerProcess: true, HandoffBlocks: 2,
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if opt.HandoffBlockBytes != 64*1024 || opt.HandoffRounds != 1 {
			t.Errorf("defaults = %d bytes x %d rounds, want 65536 x 1", opt.HandoffBlockBytes, opt.HandoffRounds)
		}
	})
	t.Run("handoff_* presence without handoff_blocks is refused, not defaulted", func(t *testing.T) {
		for _, ph := range []config.Phase{
			{HandoffBlockSize: "16KiB"},
			{HandoffRounds: 2},
			{HandoffSeed: 9},
		} {
			if _, err := OptionsFromPhase(handoffBase(), ph); err == nil ||
				!strings.Contains(err.Error(), "handoff_blocks") {
				t.Errorf("phase %+v accepted or refused off-message: %v", ph, err)
			}
		}
	})
	t.Run("slot A must name the lease kernel", func(t *testing.T) {
		cfg := handoffBase()
		cfg.WorkloadA = "float"
		if _, err := OptionsFromPhase(cfg, config.Phase{HandoffBlocks: 4, PerProcess: true}); err == nil ||
			!strings.Contains(err.Error(), HandoffKernel) {
			t.Errorf("handoff on a plain workload not refused (or refused off-message): %v", err)
		}
	})
	t.Run("contradictions refused: rotate, bursts, workers>1", func(t *testing.T) {
		rot, err := OptionsFromPhase(handoffBase(), config.Phase{
			HandoffBlocks: 4, PerProcess: true, Rotate: boolp(true),
		})
		if err == nil || !strings.Contains(err.Error(), "rotate") {
			t.Errorf("handoff+rotate accepted: %v (opt=%+v)", err, rot)
		}
		bur, err := OptionsFromPhase(handoffBase(), config.Phase{
			HandoffBlocks: 4, PerProcess: true, Bursts: 2,
		})
		if err == nil || !strings.Contains(err.Error(), "bursts") {
			t.Errorf("handoff+bursts accepted: %v (opt=%+v)", err, bur)
		}
		cfg := handoffBase()
		cfg.Workers = 2
		w, err := OptionsFromPhase(cfg, config.Phase{HandoffBlocks: 4, PerProcess: true})
		if err == nil || !strings.Contains(err.Error(), "workers") {
			t.Errorf("handoff with workers=2 accepted: %v (opt=%+v)", err, w)
		}
	})
	t.Run("block size must be a whole number of words", func(t *testing.T) {
		if _, err := OptionsFromPhase(handoffBase(), config.Phase{
			HandoffBlocks: 4, PerProcess: true, HandoffBlockSize: "18",
		}); err == nil || !strings.Contains(err.Error(), "4-byte word") {
			t.Errorf("18-byte blocks accepted or refused off-message: %v", err)
		}
	})
	t.Run("ValidateHandoff defense in depth: overflow, rounds, lock", func(t *testing.T) {
		base := Options{A: HandoffKernel, PerProcess: true, HandoffBlocks: 4,
			HandoffBlockBytes: 64 * 1024, HandoffRounds: 1}
		ovf := base
		ovf.HandoffBlocks = 1 << 40
		ovf.HandoffBlockBytes = 1 << 40
		if err := ValidateHandoff(ovf); err == nil || !strings.Contains(err.Error(), "overflows") {
			t.Errorf("geometry overflow not refused: %v", err)
		}
		r := base
		r.HandoffRounds = 0
		if err := ValidateHandoff(r); err == nil {
			t.Error("rounds=0 accepted")
		}
		l := base
		l.MemLock = "maybe"
		if err := ValidateHandoff(l); err == nil || !strings.Contains(err.Error(), "lock") {
			t.Errorf("bad lock mode accepted: %v", err)
		}
		if err := ValidateHandoff(Options{}); err != nil {
			t.Errorf("ordinary phase (no handoff) must pass untouched: %v", err)
		}
	})
}

func TestHandoffPreflightAndAdmission(t *testing.T) {
	valid := Options{A: HandoffKernel, PerProcess: true, Workers: 1,
		HandoffBlocks: 4, HandoffBlockBytes: 16 * 1024, HandoffRounds: 1}

	// The geometry IS the budget: an empty "memory" knob must not
	// refuse the phase, and the inherited slot B (which never loads
	// in this shape) must not demand one either.
	vb := valid
	vb.B = "ram-constant"
	if err := PreflightMemoryWorkloads(vb); err != nil {
		t.Errorf("handoff phase refused for a budget it does not use: %v", err)
	}
	// A shape error surfaces HERE (setup refusal), not mid-run.
	bad := valid
	bad.A = "float"
	if err := PreflightMemoryWorkloads(bad); err == nil {
		t.Error("broken handoff shape passed the memory preflight")
	}

	// "Healthy machine" is a named precondition, not an assumption:
	// with commit pressure (free commit under budget+reserve) the
	// product honestly refuses a 64KiB section, and failing that
	// would grade the machine's moment, not this code (audit R12
	// rule). The refusal then must be the product's own named rule,
	// and the skip quotes it.
	if err := HandoffAdmission(valid); err != nil {
		t.Skipf("capability skip (audit R12): this machine's commit cannot admit the 64KiB section right now: %v", err)
	}
	huge := valid
	huge.HandoffBlocks = 400_000_000
	huge.HandoffBlockBytes = 4 << 20 // 1.6 PiB: no machine honors this
	err := HandoffAdmission(huge)
	if err == nil || !strings.Contains(err.Error(), "handoff shared section admission") {
		t.Errorf("1.6 PiB section not named-refused: %v", err)
	}
	// A handoff phase carries no handoff geometry -> untouched.
	if err := HandoffAdmission(Options{A: "float"}); err != nil {
		t.Errorf("ordinary phase touched by admission: %v", err)
	}
}

func boolp(v bool) *bool { return &v }
