package subprocess

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// enginePageSize is the granularity the budget is divided at.
// Measured on this machine, VirtualAlloc(NULL) rounds regions to
// page granularity (not the 64 KiB GetSystemInfo advertises), and
// the allocator reports the true committed size via VirtualQuery -
// this constant only decides how evenly the budget is SPLIT, never
// what is actually committed.
const enginePageSize = 4096

// memPlanSpec is the parent's once-computed memory plan for a phase:
// the budget, the service allowance, the lock policy, and the exact
// grants each child slot receives. The parent computes it ONCE -
// children never re-derive an auto rule, because their view of the
// machine could differ and silently multiply the budget.
type memPlanSpec struct {
	budgetBytes uint64
	allowance   uint64
	lock        string // normalized lock mode ("off"/"try"/"required")
	ramSeed     uint64 // seed handed to seed-consuming workloads (0 is a valid seed)
	grants      [][]memtest.Grant
	skipped     int // child slots the budget could not give even one page set
	rule        string
}

// testedBytes is the total data bytes the run verifies: the sum of the
// grant sizes the parent computed ONCE (never budgetBytes minus a
// guessed allowance - it is the exact envelope handed to the children,
// the same numbers their mem_done evidence will report). A nil plan
// (no engine on this run) is 0: the panel omits the segment rather
// than claiming a tested region of zero bytes.
func (mp *memPlanSpec) testedBytes() uint64 {
	if mp == nil {
		return 0
	}
	var sum uint64
	for _, perWorker := range mp.grants {
		for _, g := range perWorker {
			sum += g.Requested
		}
	}
	return sum
}

// memSnapshotNow is the machine view fed to memory budget admission.
// Production reads the LIVE machine on purpose: every plan that
// reaches load passes real admission first (fixed requests are
// honored or refused by name, never quietly shrunk). The seam exists
// for plan-arithmetic tests only: they compute partitions and never
// map a byte, and inheriting the host's free commit made golden
// vectors admission hostages - a 272 MiB golden shape plus the 2 GiB
// commit reserve sits exactly on this machine's resident desktop
// baseline and the same test flips between pass and setup-error
// refusal within minutes. The partition math is what any admitting
// machine would compute; with a synthetic admitting snapshot it is
// pinned for the machine-independent part, while everything that
// actually maps (E2E sessions, real grants) keeps the live view and
// its honest refusal.
var memSnapshotNow = sysinfo.MemorySnapshotNow

// buildMemPlan snapshots the machine, applies the budget rule and
// partitions the result across the child processes. Every failure
// here is a setup error: the memory request was explicit, and
// quietly running without memory would be a silent reduction of it.
func buildMemPlan(opt plan.Options, children int) (*memPlanSpec, error) {
	req, err := memtest.ParseRequest(opt.Memory)
	if err != nil {
		return nil, err
	}
	lock, err := memtest.ParseLockMode(opt.MemLock)
	if err != nil {
		return nil, err
	}
	snap := memSnapshotNow()
	params := memtest.DefaultTrialParams()
	budget, err := memtest.ComputeBudget(req, snap, params)
	if err != nil {
		return nil, err
	}
	res, err := memtest.Partition(budget,
		memtest.Layout{Workers: children, Roles: []memtest.Role{memtest.RoleSource, memtest.RoleDest}},
		params, enginePageSize)
	if err != nil {
		return nil, err
	}
	if err := memtest.ValidateGrants(budget, res); err != nil {
		return nil, fmt.Errorf("internal grant partition rejected: %w", err)
	}
	per := make([][]memtest.Grant, children)
	for _, g := range res.Grants {
		per[g.Worker] = append(per[g.Worker], g)
	}

	// Audit C01 stage 2: every RAM-worker THREAD inside one child owns
	// its own complete grant set. The parent re-splits each child's
	// data envelope at thread granularity before launch - still using
	// the golden-tested Partition as the only division math, called
	// with a zero service allowance because the process-level allowance
	// was already carved in stage one. Offsets rebase onto the child's
	// stage-one window, so global disjointness survives, and the Grant
	// Worker field on the wire becomes the LOCAL thread index the child
	// maps to its own goroutines. No child ever re-derives anything.
	workers := opt.Workers
	if workers < 1 {
		workers = 1
	}
	tp := params
	tp.PerWorkerAllowance = 0
	rule := budget.Rule + "; " + res.Rule
	threadsSkipped := 0
	for c := 0; c < children; c++ {
		if len(per[c]) == 0 {
			continue // stage one already listed this child in Skipped
		}
		if workers == 1 {
			// Same division, one thread owns everything of its child:
			// only the local numbering normalizes, offsets and sizes
			// stay exactly as stage one set them.
			local := make([]memtest.Grant, len(per[c]))
			copy(local, per[c])
			for i := range local {
				local[i].Worker = 0
			}
			per[c] = local
			continue
		}
		env, start := uint64(0), ^uint64(0)
		for _, g := range per[c] {
			env += g.Requested
			if g.Offset < start {
				start = g.Offset
			}
		}
		res2, err := memtest.Partition(memtest.Budget{Bytes: env},
			memtest.Layout{Workers: workers, Roles: []memtest.Role{memtest.RoleSource, memtest.RoleDest}},
			tp, enginePageSize)
		if err != nil {
			return nil, fmt.Errorf("thread grant split for child %d over its %d-byte envelope: %w", c, env, err)
		}
		for i := range res2.Grants {
			res2.Grants[i].Offset += start
		}
		if err := memtest.ValidateGrants(budget, memtest.PartitionResult{Grants: res2.Grants}); err != nil {
			return nil, fmt.Errorf("internal thread grant split rejected: %w", err)
		}
		per[c] = res2.Grants
		threadsSkipped += len(res2.Skipped)
		if c == 0 {
			rule += fmt.Sprintf("; threads: workers=%d served=%d data=%d unused=%d",
				workers, workers-len(res2.Skipped), res2.DataTotal, res2.Unused)
		}
	}
	// A memory-verifying run whose budget cannot give EVERY thread a
	// complete grant set is a setup error: running with some threads
	// over the whole set or on nothing is the silent reduction this
	// project forbids. Load-only workloads tolerate the explicit skips
	// (loudly, as before).
	if threadsSkipped > 0 && memVerifyingWorkloads(opt) {
		return nil, fmt.Errorf("memory budget too small: %d worker thread(s) would get no complete grant set (audit C01 completeness): raise the memory budget or reduce workers", threadsSkipped)
	}

	return &memPlanSpec{
		budgetBytes: budget.Bytes,
		allowance:   res.Allowance,
		lock:        string(lock),
		ramSeed:     opt.RamSeed,
		grants:      per,
		skipped:     len(res.Skipped),
		rule:        rule,
	}, nil
}

// memVerifyingWorkloads reports whether either selected workload
// verifies data in engine regions (the ram-* family, and the built-in
// mem whenever an engine is configured - buildMemPlan runs only with
// an explicit budget).
func memVerifyingWorkloads(opt plan.Options) bool {
	for _, name := range []string{opt.A, opt.B} {
		if wl, ok := workload.Get(name); ok && (wl.RequiresMemory || name == "mem") {
			return true
		}
	}
	return false
}

// forSlot narrows the plan to one child: same budget/allowance/lock,
// only that slot's grants. A skipped slot gets none, and the child
// says so out loud rather than running memory-less in silence.
// Safe on a nil plan (engine off).
func (mp *memPlanSpec) forSlot(slot int) *memPlanSpec {
	if mp == nil {
		return nil
	}
	child := &memPlanSpec{budgetBytes: mp.budgetBytes, allowance: mp.allowance, lock: mp.lock, ramSeed: mp.ramSeed}
	if slot >= 0 && slot < len(mp.grants) {
		child.grants = [][]memtest.Grant{mp.grants[slot]}
	}
	return child
}

// memArgs appends the memory-engine flags to one child's command
// line. With the engine off nothing is appended, keeping old command
// lines byte-identical.
func memArgs(args []string, mp *memPlanSpec) []string {
	if mp == nil {
		return args
	}
	args = append(args,
		"--mem-budget", strconv.FormatUint(mp.budgetBytes, 10),
		"--mem-allowance", strconv.FormatUint(mp.allowance, 10),
		"--mem-lock", mp.lock,
		"--ram-seed", strconv.FormatUint(mp.ramSeed, 10),
	)
	for _, g := range mp.grants[0] {
		args = append(args, "--mem-grant", formatMemGrant(g))
	}
	return args
}

// formatMemGrant serializes one grant; parseMemGrant is its inverse,
// and the round-trip is pinned by tests so parent and child can
// never drift on the wire format.
func formatMemGrant(g memtest.Grant) string {
	return fmt.Sprintf("%d:%d:%s:%d:%d", g.Worker, g.Region, g.Role, g.Offset, g.Requested)
}

// parseMemGrant parses "worker:region:role:offset:len". Every field
// is checked: a malformed or unknown-role grant is a visible setup
// error, never a silently dropped argument.
func parseMemGrant(s string) (memtest.Grant, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 5 {
		return memtest.Grant{}, fmt.Errorf("grant %q wants worker:region:role:offset:len", s)
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil || w < 0 {
		return memtest.Grant{}, fmt.Errorf("grant %q: bad worker", s)
	}
	r, err := strconv.Atoi(parts[1])
	if err != nil || r < 0 {
		return memtest.Grant{}, fmt.Errorf("grant %q: bad region", s)
	}
	role := memtest.Role(parts[2])
	switch role {
	case memtest.RoleSource, memtest.RoleDest, memtest.RoleControl:
	default:
		return memtest.Grant{}, fmt.Errorf("grant %q: unknown role %q", s, parts[2])
	}
	off, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return memtest.Grant{}, fmt.Errorf("grant %q: bad offset", s)
	}
	size, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil || size == 0 {
		return memtest.Grant{}, fmt.Errorf("grant %q: bad length", s)
	}
	return memtest.Grant{Worker: w, Region: r, Role: role, Offset: off, Requested: size}, nil
}

// grantFlag collects repeatable --mem-grant values for flag.Var.
type grantFlag []memtest.Grant

func (g *grantFlag) String() string {
	if g == nil {
		return ""
	}
	parts := make([]string, len(*g))
	for i, gr := range *g {
		parts[i] = formatMemGrant(gr)
	}
	return strings.Join(parts, " ")
}

func (g *grantFlag) Set(s string) error {
	gr, err := parseMemGrant(s)
	if err != nil {
		return err
	}
	*g = append(*g, gr)
	return nil
}

// openMemEngine is the child side: it re-validates the grants
// RECEIVED on the command line BEFORE trusting them (a tampered or
// corrupted argument set is refused, never allocated), commits every
// region all-or-nothing, runs the cancellable first touch, and
// applies the lock policy. On success it returns the engine plus the
// accounting snapshot (verified bytes are filled in later by the
// reporter, when the workload's last completed pass reports them).
//
// Every error returned here is a resource or setup condition -
// ErrUnsupported, ErrNoCommit, ErrLockFailed, or a validation
// refusal - and the caller maps it to exit 2, never the 1 that
// means CPU fault. On any error every committed byte is released
// and no accounting is returned: the numbers are unknown, not the
// residue of a freed allocation.
func openMemEngine(ctx context.Context, budget, allowance uint64, grants []memtest.Grant, lock memtest.LockMode) (*memtest.Engine, *journal.MemAccounting, error) {
	alloc, err := memtest.NewAllocator()
	if err != nil {
		return nil, nil, fmt.Errorf("memory allocator: %w", err)
	}
	return openMemEngineWith(ctx, budget, allowance, grants, lock, alloc)
}

// openMemEngineWith is the same contract over an INJECTED allocator
// (P07e: the shared-mapping allocator hands out subviews of one
// section instead of fresh VirtualAlloc commits). On every failure -
// including validation refusal - the allocator is closed: the caller
// handed it over for this engine, and a partial story never leaks a
// mapping or a commit.
func openMemEngineWith(ctx context.Context, budget, allowance uint64, grants []memtest.Grant, lock memtest.LockMode, alloc memtest.Allocator) (*memtest.Engine, *journal.MemAccounting, error) {
	if err := memtest.ValidateGrants(memtest.Budget{Bytes: budget}, memtest.PartitionResult{Grants: grants}); err != nil {
		_ = alloc.Close()
		return nil, nil, fmt.Errorf("received grants failed validation: %w", err)
	}
	e, err := memtest.Open(grants, alloc)
	if err != nil {
		return nil, nil, fmt.Errorf("region commit: %w", err)
	}
	prep, err := e.Prepare(ctx, 0xA5)
	if err != nil {
		if cerr := e.Close(); cerr != nil {
			return nil, nil, fmt.Errorf("first touch after %d bytes: %w (release also failed: %v)", prep.Touched, err, cerr)
		}
		return nil, nil, fmt.Errorf("first touch after %d bytes: %w", prep.Touched, err)
	}
	lrep, err := e.Lock(lock)
	if err != nil {
		if cerr := e.Close(); cerr != nil {
			return nil, nil, fmt.Errorf("%w (release also failed: %v)", err, cerr)
		}
		return nil, nil, err
	}
	requested, allocated := e.Totals()
	acct := &journal.MemAccounting{
		Requested:    requested,
		Allocated:    allocated,
		Touched:      prep.Touched,
		FirstTouchMS: float64(prep.Elapsed) / float64(time.Millisecond),
		LockMode:     string(lrep.Mode),
		Locked:       lrep.LockedBytes,
		LockFailure:  lrep.Failure,
	}
	ws := "unknown"
	if lrep.WorkingSetKnown {
		ws = fmt.Sprintf("%d..%d", lrep.WorkingSetMin, lrep.WorkingSetMax)
	}
	fmt.Fprintf(os.Stderr,
		"worker: mem: budget=%d allowance=%d requested=%d allocated=%d touched=%d first_touch=%v lock=%s locked=%d lock_failed=%d lock_msg=%q working_set=%s large_pages=%q\n",
		budget, allowance, requested, allocated, prep.Touched, prep.Elapsed,
		lrep.Mode, lrep.LockedBytes, lrep.FailedRegions, lrep.Failure, ws, lrep.LargePages)
	return e, acct, nil
}
