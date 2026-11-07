package plan

// P07e handoff phase (S5): the shape and the budget of a
// parent-arbitrated block-ownership phase live in ONE place, so the
// scenario compile, the up-front resource gate and the executor all
// answer from the same rule - a plan that "succeeds" on paper while
// execution would refuse it is exactly what this tool promises never
// to produce.

import (
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// HandoffKernel names the only workload a handoff phase runs: the
// parent-arbitrated lease kernel. The registry refuses it as a
// standalone workload run (no shared section in sight); as a phase it
// is driven by the coordinator, never by the slot loop.
const HandoffKernel = "ram-handoff"

// DefaultHandoffBlockSize is one source of truth for the phase
// default, shared by compile and display.
const DefaultHandoffBlockSize = "64KiB"

// ValidateHandoff checks the handoff-phase shape on resolved Options
// (mirroring ValidateTransition's role). Geometry divisibility across
// the SELECTED cores is judged by the coordinator at execution (the
// core set may still resolve from "all" here); everything decidable
// from the plan alone is decided now, named.
func ValidateHandoff(opt Options) error {
	if opt.HandoffBlocks <= 0 {
		return nil
	}
	if opt.A != HandoffKernel {
		return fmt.Errorf("handoff phase must name workload a=%q: the coordinator drives parent-arbitrated block leases for that kernel, and handoff_* on any other workload would pose as a test this shape never runs", HandoffKernel)
	}
	if !opt.PerProcess {
		return fmt.Errorf("handoff needs per-process mode: the leases arbitrate ownership between pinned worker processes over one inherited shared section")
	}
	if opt.Rotate {
		return fmt.Errorf("handoff and rotate do not mix: the arbiter commands the leases, rotate freezes whole windows")
	}
	if opt.Bursts > 0 {
		return fmt.Errorf("a phase is either a prepared-burst transition or a handoff lease schedule: handoff_* with bursts=%d defines two phases at once", opt.Bursts)
	}
	if opt.Workers > 1 {
		return fmt.Errorf("workers=%d with a handoff phase: a handoff worker is one prepared engine per pinned core serving the lease schedule - extra threads would be parsed but never run (audit C01 shape: silently ignored concurrency)", opt.Workers)
	}
	if opt.HandoffRounds < 1 {
		return fmt.Errorf("handoff phase needs at least one round (every block published and independently re-read)")
	}
	if opt.HandoffBlockBytes == 0 || opt.HandoffBlockBytes%4 != 0 {
		return fmt.Errorf("handoff_block_size %d must be a positive multiple of the 4-byte word the kernel verifies", opt.HandoffBlockBytes)
	}
	if uint64(opt.HandoffBlocks) > ^uint64(0)/opt.HandoffBlockBytes {
		return fmt.Errorf("handoff section %d x %d overflows 64-bit bytes", opt.HandoffBlocks, opt.HandoffBlockBytes)
	}
	if _, err := memtest.ParseLockMode(opt.MemLock); err != nil {
		return fmt.Errorf("handoff phase memory lock: %w", err)
	}
	return nil
}

// HandoffAdmission answers "could THIS machine map this section right
// now?" with the same budget rule the memory engine uses (physical
// headroom, commit headroom, checked arithmetic - B09), BEFORE any
// save or load: the geometry is the phase's budget, so the budget
// knob "memory" must not be consulted for it - and skipping the
// admission in the dry-run while execution performs it would be the
// paper plan this tool forbids. Cheap and side-effect free: a memory
// snapshot and arithmetic.
func HandoffAdmission(opt Options) error {
	if opt.HandoffBlocks <= 0 {
		return nil
	}
	if err := ValidateHandoff(opt); err != nil {
		return err
	}
	section := uint64(opt.HandoffBlocks) * opt.HandoffBlockBytes
	if _, err := memtest.ComputeBudget(memtest.Request{Bytes: section}, sysinfo.MemorySnapshotNow(), memtest.DefaultTrialParams()); err != nil {
		return fmt.Errorf("handoff shared section admission (%d x %d bytes): %w", opt.HandoffBlocks, opt.HandoffBlockBytes, err)
	}
	return nil
}
