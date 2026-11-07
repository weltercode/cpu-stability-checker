package plan

// Q05 prepared pair-switch: the shape of the mode lives in ONE place
// (the role ValidateHandoff/ValidateTransition play), so the scenario
// compile, the dry-run display and the executor all answer from the
// same rule. The execution machinery (turn ledger, driver) is
// internal/transition's pure model; this file answers "is this a
// legal pair-switch experiment?" BEFORE any load - and answers with
// the problem's name, never a silent narrowing.

import (
	"fmt"
	"runtime"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/transition"
)

// Default switch windows: one source of truth for the entry paths.
// They sit deliberately wider than the P08 burst defaults: a switch
// window's idle gap IS the observation window (the Q04 parking
// instrument samples exactly there), and an interference transition
// worth measuring needs room to be seen - 600ms of observation per
// gap is the shape the owner already benched.
const (
	DefaultSwitchGap = 600 * time.Millisecond
	DefaultSwitchRun = 200 * time.Millisecond
)

// SwitchActive reports whether a resolved Options carries the
// pair-switch mode. Presence is ALL-OR-NOTHING by contract below: a
// half-specified switch shape is refused, never defaulted around.
func SwitchActive(opt Options) bool {
	return len(opt.SwitchPairs) > 0 || opt.SwitchRounds > 0 || opt.SwitchOrder != "" ||
		opt.SwitchGap != 0 || opt.SwitchRun != 0 || opt.SwitchJitter != 0 || opt.SwitchSeed != 0
}

// ValidateSwitch checks the pair-switch shape on resolved Options.
// Absent is legal (and invisible to the hash); present must be the
// WHOLE honest shape. "Exactly two groups" was the agreed minimal
// increment (A1), and A1 named the matrix widening as a SEPARATE
// deliberate increment - that increment landed 2026-11-02: at least
// two groups now, whether named by hand (switch_pairs) or paired off
// the detected topology by switch_scope. The refusal below remains the
// anti-silent-widening tripwire: what is banned is an UNNAMED shape,
// not a big one.
func ValidateSwitch(opt Options) error {
	if !SwitchActive(opt) {
		return nil
	}
	if opt.SwitchRounds < 1 {
		return fmt.Errorf("pair-switch phase needs switch_rounds: the phase's end criterion is rounds x groups - switch_* fields without it define nothing (the handoff geometry rule again)")
	}
	if len(opt.SwitchPairs) < 2 {
		return fmt.Errorf("pair-switch needs at least two prepared groups, got %d: turns alternate BETWEEN groups, so a lone group has nothing to switch against; the ccx/smt/all scope matrix pairs off as many groups as the detected topology yields - one pair against one pair is just its minimal case", len(opt.SwitchPairs))
	}
	seen := map[int]string{} // core -> "group%d:role" for the named refusal
	for gi, group := range opt.SwitchPairs {
		role := func(slot int) string {
			if slot == 0 {
				return "aggressor"
			}
			return "observer"
		}
		if len(group) != 2 {
			return fmt.Errorf("switch group %d must list exactly two cores [aggressor, observer], got %d (slot order mirrors abab: the aggressor workload loads first slot)", gi, len(group))
		}
		for slot, core := range group {
			if core < 0 {
				return fmt.Errorf("switch group %d %s core %d is negative", gi, role(slot), core)
			}
			if core >= runtime.NumCPU() {
				return fmt.Errorf("switch group %d %s core %d is outside the available range 0..%d", gi, role(slot), core, runtime.NumCPU()-1)
			}
			if other, dup := seen[core]; dup {
				return fmt.Errorf("logical CPU %d sits in two switch groups at once (%s and group %d:%s): a prepared participant belongs to exactly one group, or a turn would load it under two schedules", core, other, gi, role(slot))
			}
			seen[core] = fmt.Sprintf("group %d:%s", gi, role(slot))
		}
	}
	if opt.SwitchRun <= 0 {
		return fmt.Errorf("pair-switch run window must be positive (switch_run)")
	}
	if opt.SwitchGap < 0 || opt.SwitchJitter < 0 {
		return fmt.Errorf("pair-switch gap and jitter must not be negative (gap=%s jitter=%s)", opt.SwitchGap, opt.SwitchJitter)
	}
	if !validSwitchWord(opt.SwitchOrder) {
		return fmt.Errorf(`unknown switch_order %q: the turn order word is the default fixed alternation or "seeded"`, opt.SwitchOrder)
	}
	// Shape exclusivity (ValidateHandoff's pattern): one phase, one
	// schedule. Two shapes at once define two experiments and pick
	// neither out loud.
	if opt.Bursts > 0 {
		return fmt.Errorf("a phase is either a prepared-burst schedule or a pair-switch schedule: switch_* with bursts=%d defines two phases at once", opt.Bursts)
	}
	if opt.HandoffBlocks > 0 {
		return fmt.Errorf("a phase carries one parent-arbiter schedule: switch_* with handoff_* poses turn-taking and block-ownership over the same cores at once")
	}
	if !opt.PerProcess {
		return fmt.Errorf("pair-switch needs per-process mode: every participant is a prepared worker process pinned to its group core (the P08 session contract)")
	}
	if opt.Rotate {
		return fmt.Errorf("pair-switch and rotate do not mix: the turn ledger commands the switches, rotate freezes whole windows")
	}
	if opt.Workers > 1 {
		return fmt.Errorf("workers=%d with a pair-switch phase: a switch participant is one prepared engine per pinned core - extra threads would be parsed but never run (audit C01 shape: silently ignored concurrency)", opt.Workers)
	}
	// The geometry must build: transition's builder refuses what a
	// plan line must never promise (this also pins that plan and
	// executor compute from one geometry, not two readings of it).
	if _, err := transition.BuildSwitchSchedule(len(opt.SwitchPairs), opt.SwitchRounds, opt.SwitchGap, opt.SwitchRun, opt.SwitchJitter, opt.SwitchOrder, opt.SwitchSeed); err != nil {
		return err
	}
	return nil
}

func validSwitchWord(word string) bool {
	return word == transition.SwitchOrderFixed || word == transition.SwitchOrderSeeded
}
