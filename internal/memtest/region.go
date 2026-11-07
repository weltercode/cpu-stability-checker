package memtest

import (
	"fmt"
	"sort"
)

// Role identifies what a region is for. Source and destination are
// the plan's two buffer roles (pattern engines write/read one and
// compare against the other); Control is for service structures.
type Role string

const (
	RoleSource  Role = "source"
	RoleDest    Role = "destination"
	RoleControl Role = "control"
)

// Grant is an immutable region assignment: which worker owns it, its
// role, its identity offset, and its exact size. The parent computes
// grants once from the phase budget and hands them to children
// verbatim (C4); a child never re-derives an auto rule, because its
// view of the machine could differ and silently multiply the budget.
type Grant struct {
	Worker    int
	Region    int // region index within the worker (0..len(roles)-1)
	Role      Role
	Offset    uint64 // global identity offset; ranges must not overlap
	Requested uint64
}

// Layout says how many workers run and which regions each gets.
type Layout struct {
	Workers int
	Roles   []Role
}

// PartitionResult is the full account of how the budget was divided:
// every byte is in exactly one of Allowance, DataTotal or Unused, and
// Skipped lists workers that got no region - explicitly, never a
// silent share of someone else's memory.
type PartitionResult struct {
	Grants    []Grant
	Skipped   []int
	Allowance uint64 // service/process overhead carved out for every worker
	DataTotal uint64 // sum of all grant sizes
	Unused    uint64 // rounding leftovers, reported not hidden
	Rule      string
}

// Partition divides the phase budget among workers and roles with
// checked arithmetic:
//
//	allowance = PerWorkerAllowance * workers   (overflow-checked)
//	data      = budget - allowance
//	region    = floor(data / workers / roles, rounded DOWN to roundTo)
//
// Workers that cannot fit even one roundTo-sized region per role are
// listed in Skipped instead of sharing or silently dropping. The
// budget is one pool for the phase: nothing here multiplies it by
// worker count, and the totals are re-derivable from the result.
func Partition(budget Budget, layout Layout, p TrialParams, roundTo uint64) (PartitionResult, error) {
	var res PartitionResult
	if layout.Workers <= 0 {
		return res, fmt.Errorf("memtest: layout needs at least one worker, got %d", layout.Workers)
	}
	if len(layout.Roles) == 0 {
		return res, fmt.Errorf("memtest: layout needs at least one role")
	}
	if roundTo == 0 {
		return res, fmt.Errorf("memtest: roundTo must be > 0")
	}

	allowance, ok := mulChecked(p.PerWorkerAllowance, uint64(layout.Workers))
	if !ok {
		return res, fmt.Errorf("memtest: service allowance %d x %d workers overflows 64-bit bytes",
			p.PerWorkerAllowance, layout.Workers)
	}
	if allowance >= budget.Bytes {
		return res, fmt.Errorf("memtest: budget %d cannot cover the service allowance %d for %d workers",
			budget.Bytes, allowance, layout.Workers)
	}
	data := budget.Bytes - allowance

	perWorker := data / uint64(layout.Workers)
	perRole := perWorker / uint64(len(layout.Roles))
	region := perRole - perRole%roundTo
	fittable := layout.Workers
	if region == 0 {
		// Equal division cannot even give one page per role: fall
		// back to page-sized regions for as many workers as fit and
		// skip the rest explicitly - never sharing, never silently
		// dropping (the plan's "workers more than blocks" case).
		region = roundTo
		cost := region * uint64(len(layout.Roles))
		if cost == 0 || cost > data {
			return res, fmt.Errorf("memtest: budget data share %d cannot give one %d-byte region per role to any of %d workers",
				data, roundTo, layout.Workers)
		}
		fittable = int(data / cost)
	}

	var offset uint64
	for w := 0; w < fittable; w++ {
		for r, role := range layout.Roles {
			res.Grants = append(res.Grants, Grant{
				Worker:    w,
				Region:    r,
				Role:      role,
				Offset:    offset,
				Requested: region,
			})
			offset += region
		}
	}
	for w := fittable; w < layout.Workers; w++ {
		res.Skipped = append(res.Skipped, w)
	}

	res.Allowance = allowance
	res.DataTotal = uint64(len(res.Grants)) * region
	res.Unused = budget.Bytes - allowance - res.DataTotal
	res.Rule = fmt.Sprintf("partition: budget=%d allowance=%d workers=%d served=%d roles=%d region=%d data=%d unused=%d",
		budget.Bytes, allowance, layout.Workers, fittable, len(layout.Roles), region, res.DataTotal, res.Unused)
	return res, nil
}

// ValidateGrants re-checks a grant set against the budget: totals
// must fit, offsets must not overlap, sizes must be real. The child
// calls this on grants received over argv BEFORE trusting them -
// malformed or tampered assignments are refused, not allocated.
func ValidateGrants(budget Budget, res PartitionResult) error {
	var total uint64
	for _, g := range res.Grants {
		if g.Requested == 0 {
			return fmt.Errorf("memtest: grant worker=%d region=%d has zero size", g.Worker, g.Region)
		}
		if g.Requested > budget.Bytes {
			return fmt.Errorf("memtest: grant worker=%d region=%d size %d exceeds the budget %d",
				g.Worker, g.Region, g.Requested, budget.Bytes)
		}
		s, ok := addChecked(total, g.Requested)
		if !ok || s > budget.Bytes-res.Allowance {
			return fmt.Errorf("memtest: grant sizes exceed the budget's data share (budget %d allowance %d)",
				budget.Bytes, res.Allowance)
		}
		total = s
	}
	if res.Allowance+total > budget.Bytes {
		return fmt.Errorf("memtest: allowance %d + data %d exceeds budget %d", res.Allowance, total, budget.Bytes)
	}
	// Disjointness of [Offset, Offset+Requested) ranges.
	byOffset := make([]Grant, len(res.Grants))
	copy(byOffset, res.Grants)
	sort.Slice(byOffset, func(i, j int) bool { return byOffset[i].Offset < byOffset[j].Offset })
	for i := 1; i < len(byOffset); i++ {
		prev := byOffset[i-1]
		end, ok := addChecked(prev.Offset, prev.Requested)
		if !ok || end > byOffset[i].Offset {
			return fmt.Errorf("memtest: grant ranges overlap at offset %d (worker=%d region=%d)",
				byOffset[i].Offset, byOffset[i].Worker, byOffset[i].Region)
		}
	}
	return nil
}

// AllocatedRegion is a grant plus the real block the OS gave for it.
type AllocatedRegion struct {
	Grant
	Block Block
}

// Engine owns the committed regions for one process. Open is all-or-
// nothing: if any commit fails, everything committed so far is
// released before the error returns - a partial allocation never
// leaks, and the failure is the allocator's honest error (resource
// condition, not a CPU fault).
type Engine struct {
	alloc   Allocator
	regions []AllocatedRegion
}

func Open(grants []Grant, alloc Allocator) (*Engine, error) {
	e := &Engine{alloc: alloc}
	for _, g := range grants {
		b, err := alloc.Commit(g.Requested)
		if err != nil {
			if cerr := alloc.Close(); cerr != nil {
				return nil, fmt.Errorf("memtest: commit worker=%d region=%d failed (%v); cleanup also failed: %w",
					g.Worker, g.Region, err, cerr)
			}
			return nil, fmt.Errorf("memtest: commit worker=%d region=%d (%d bytes): %w",
				g.Worker, g.Region, g.Requested, err)
		}
		e.regions = append(e.regions, AllocatedRegion{Grant: g, Block: b})
	}
	return e, nil
}

// Regions exposes the allocated regions (read-only view). Ownership
// stays with the Engine until Close.
func (e *Engine) Regions() []AllocatedRegion {
	return e.regions
}

// Totals sums requested and actually-committed bytes over every
// region, for honest accounting lines (and the journal fields that
// C5 adds). The numbers are what the allocator reported, not what
// was asked for: they differ exactly when the OS rounded a region,
// and the report must show the difference, not hide it.
func (e *Engine) Totals() (requested, allocated uint64) {
	for _, r := range e.regions {
		requested += r.Grant.Requested
		allocated += r.Block.Allocated
	}
	return requested, allocated
}

// Close releases every region. Safe twice, safe after Open failed
// partially (the failed Engine never exists, its allocator was
// already closed), safe with zero regions.
func (e *Engine) Close() error {
	if e == nil || e.alloc == nil {
		return nil
	}
	err := e.alloc.Close()
	e.alloc = nil
	e.regions = nil
	return err
}

func mulChecked(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > ^uint64(0)/b {
		return 0, false
	}
	return a * b, true
}
