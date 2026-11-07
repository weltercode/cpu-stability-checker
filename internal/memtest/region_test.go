package memtest

import (
	"errors"
	"testing"
)

func TestPartitionTables(t *testing.T) {
	def := DefaultTrialParams() // 64 MiB per-worker allowance
	cases := []struct {
		name        string
		budget      uint64
		workers     int
		roles       []Role
		roundTo     uint64
		allowance   uint64 // per-worker service allowance (default if nil-able; 0 used where noted)
		customAllow bool
		wantRegion  uint64
		wantServed  int
		wantSkipped int
		wantUnused  uint64
		wantErr     bool
	}{
		{
			// 28 GiB budget, 16 workers, source+dest: allowance 1 GiB,
			// data 27 GiB, per role 905969664 B (page-exact), no tail.
			name: "even split", budget: 28 << 30, workers: 16,
			roles: []Role{RoleSource, RoleDest}, roundTo: 4096,
			wantRegion: 905969664, wantServed: 16, wantSkipped: 0, wantUnused: 0,
		},
		{
			// 100 KiB for 100 workers x 2 roles (no allowance): equal
			// division lands below a page, so 12 workers get page-sized
			// regions and 88 are skipped explicitly; the 4096 B tail is
			// reported.
			name: "workers more than blocks", budget: 102400, workers: 100,
			roles: []Role{RoleSource, RoleDest}, roundTo: 4096,
			customAllow: true, allowance: 0,
			wantRegion: 4096, wantServed: 12, wantSkipped: 88, wantUnused: 4096,
		},
		{
			// Rounding tail is reported, never silently allocated or dropped.
			name: "page tail reported", budget: 100000, workers: 1,
			roles: []Role{RoleSource}, roundTo: 4096,
			customAllow: true, allowance: 0,
			wantRegion: 98304, wantServed: 1, wantSkipped: 0, wantUnused: 1696,
		},
		{
			// Not even one page per role for a single worker: honest
			// refusal (setup error), not a zero-size grant.
			name: "below one page", budget: 4096, workers: 1,
			roles: []Role{RoleSource, RoleDest}, roundTo: 4096,
			customAllow: true, allowance: 0,
			wantErr: true,
		},
		{
			// Allowance eats the budget: honest refusal, not a silent
			// data share of zero.
			name: "allowance dominates", budget: 1 << 20, workers: 32,
			roles: []Role{RoleSource}, roundTo: 4096,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := def
			if c.customAllow {
				p.PerWorkerAllowance = c.allowance
			}
			res, err := Partition(Budget{Bytes: c.budget}, Layout{Workers: c.workers, Roles: c.roles}, p, c.roundTo)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			served := 0
			for _, g := range res.Grants {
				if g.Requested != c.wantRegion {
					t.Fatalf("grant worker=%d region=%d size %d, want %d", g.Worker, g.Region, g.Requested, c.wantRegion)
				}
				served++
			}
			if got := len(res.Grants) / len(c.roles); got != c.wantServed {
				t.Errorf("served workers = %d, want %d", got, c.wantServed)
			}
			if len(res.Skipped) != c.wantSkipped {
				t.Errorf("skipped = %d, want %d", len(res.Skipped), c.wantSkipped)
			}
			if res.Unused != c.wantUnused {
				t.Errorf("unused = %d, want %d (rule: %s)", res.Unused, c.wantUnused, res.Rule)
			}
			// Every byte accounted for exactly once.
			if res.Allowance+res.DataTotal+res.Unused != c.budget {
				t.Errorf("allowance %d + data %d + unused %d != budget %d",
					res.Allowance, res.DataTotal, res.Unused, c.budget)
			}
			if err := ValidateGrants(Budget{Bytes: c.budget}, res); err != nil {
				t.Errorf("own partition failed its own validation: %v (rule: %s)", err, res.Rule)
			}
		})
	}
}

// The budget is one pool for the phase: no matter how many workers
// share it, the data total never exceeds the budget's data share.
// Multiplying the budget per worker is the exact failure mode the
// plan calls out, and it must be impossible by construction.
func TestPartitionNeverMultipliesBudget(t *testing.T) {
	budget := Budget{Bytes: 4 << 30}
	p := DefaultTrialParams()
	for _, workers := range []int{1, 2, 8, 33} {
		res, err := Partition(budget, Layout{Workers: workers, Roles: []Role{RoleSource, RoleDest}}, p, 4096)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if res.Allowance+res.DataTotal > budget.Bytes {
			t.Errorf("workers=%d: allowance %d + data %d exceeds budget %d",
				workers, res.Allowance, res.DataTotal, budget.Bytes)
		}
	}
}

// Grant ranges must be disjoint by construction: unique offsets,
// no overlap, checked arithmetic on the range ends.
func TestPartitionOffsetsDisjoint(t *testing.T) {
	res, err := Partition(Budget{Bytes: 1 << 30}, Layout{Workers: 7, Roles: []Role{RoleSource, RoleDest, RoleControl}},
		DefaultTrialParams(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]int{}
	for _, g := range res.Grants {
		if _, dup := seen[g.Offset]; dup {
			t.Errorf("duplicate offset %d", g.Offset)
		}
		seen[g.Offset]++
	}
	if err := ValidateGrants(Budget{Bytes: 1 << 30}, res); err != nil {
		t.Errorf("valid partition rejected: %v", err)
	}
}

// A child re-validates grants received over argv before trusting
// them: tampered or malformed assignments are refused, not allocated.
func TestValidateGrantsRejectsTampering(t *testing.T) {
	budget := Budget{Bytes: 1 << 30}
	res, err := Partition(budget, Layout{Workers: 4, Roles: []Role{RoleSource, RoleDest}}, DefaultTrialParams(), 4096)
	if err != nil {
		t.Fatal(err)
	}

	overlap := append([]Grant(nil), res.Grants...)
	overlap[1].Offset = overlap[0].Offset // overlap
	bad := res
	bad.Grants = overlap
	if err := ValidateGrants(budget, bad); err == nil {
		t.Error("overlapping grant ranges accepted")
	}

	huge := append([]Grant(nil), res.Grants...)
	huge[0].Requested = budget.Bytes // one grant alone exceeds the data share
	bad = res
	bad.Grants = huge
	if err := ValidateGrants(budget, bad); err == nil {
		t.Error("grant larger than the whole budget accepted")
	}

	zero := append([]Grant(nil), res.Grants...)
	zero[0].Requested = 0
	bad = res
	bad.Grants = zero
	if err := ValidateGrants(budget, bad); err == nil {
		t.Error("zero-size grant accepted")
	}
}

// The plan's fake-allocator test: when the Nth commit fails, Open
// must release everything it already committed - a partial
// allocation never leaks - and surface the resource error.
func TestOpenReleasesEverythingOnNthBlockFailure(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4} {
		fake := NewFakeAllocator()
		fake.FailOnCommit(failAt)
		grants := []Grant{
			{Worker: 0, Region: 0, Role: RoleSource, Offset: 0, Requested: 1 << 20},
			{Worker: 0, Region: 1, Role: RoleDest, Offset: 1 << 20, Requested: 1 << 20},
			{Worker: 1, Region: 0, Role: RoleSource, Offset: 2 << 20, Requested: 1 << 20},
			{Worker: 1, Region: 1, Role: RoleDest, Offset: 3 << 20, Requested: 1 << 20},
		}
		e, err := Open(grants, fake)
		if e != nil {
			t.Errorf("failAt=%d: Open returned an engine despite the failure", failAt)
		}
		if err == nil {
			t.Fatalf("failAt=%d: Open succeeded despite a forced commit failure", failAt)
		}
		if !errors.Is(err, ErrNoCommit) {
			t.Errorf("failAt=%d: error %v does not wrap ErrNoCommit", failAt, err)
		}
		commits, releases, live, allocated := fake.Stats()
		if commits != failAt {
			t.Errorf("failAt=%d: commits = %d", failAt, commits)
		}
		if live != 0 || allocated != 0 {
			t.Errorf("failAt=%d: leaked %d live blocks (%d bytes) after failed Open", failAt, live, allocated)
		}
		if releases != commits-1 {
			t.Errorf("failAt=%d: releases = %d, want %d (everything committed before the failure)",
				failAt, releases, commits-1)
		}
	}
}

// Low commit is simulated with a cap: the failure is ErrNoCommit,
// and again nothing is left committed.
func TestOpenLowCommitCap(t *testing.T) {
	fake := NewFakeAllocator()
	fake.CapTotal(3 << 20) // only three 1 MiB regions fit
	grants := make([]Grant, 0, 4)
	for i := 0; i < 4; i++ {
		grants = append(grants, Grant{Worker: i, Region: 0, Role: RoleSource, Offset: uint64(i) << 20, Requested: 1 << 20})
	}
	_, err := Open(grants, fake)
	if !errors.Is(err, ErrNoCommit) {
		t.Fatalf("err = %v, want ErrNoCommit", err)
	}
	if _, _, live, allocated := fake.Stats(); live != 0 || allocated != 0 {
		t.Errorf("low-commit failure leaked %d blocks (%d bytes)", live, allocated)
	}
}

// Happy path: Open commits every grant, Regions reports them with
// their grants, Close releases everything and is safe twice.
func TestOpenCloseHappyPath(t *testing.T) {
	fake := NewFakeAllocator()
	res, err := Partition(Budget{Bytes: 64 << 20}, Layout{Workers: 2, Roles: []Role{RoleSource, RoleDest}},
		TrialParams{MaxPct: 70, MinOSReserve: 4 << 30, CommitReserve: 2 << 30, PerWorkerAllowance: 0}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(res.Grants, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Regions()) != len(res.Grants) {
		t.Errorf("regions = %d, want %d", len(e.Regions()), len(res.Grants))
	}
	for _, r := range e.Regions() {
		if r.Block.Addr == 0 || r.Block.Allocated < r.Requested {
			t.Errorf("region worker=%d region=%d has a bad block: %+v", r.Worker, r.Region, r.Block)
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, live, allocated := fake.Stats(); live != 0 || allocated != 0 {
		t.Errorf("Close left %d blocks (%d bytes) committed", live, allocated)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPartitionRejectsBadInputs(t *testing.T) {
	p := DefaultTrialParams()
	if _, err := Partition(Budget{Bytes: 1 << 30}, Layout{Workers: 0, Roles: []Role{RoleSource}}, p, 4096); err == nil {
		t.Error("zero workers accepted")
	}
	if _, err := Partition(Budget{Bytes: 1 << 30}, Layout{Workers: 4}, p, 4096); err == nil {
		t.Error("zero roles accepted")
	}
	if _, err := Partition(Budget{Bytes: 1 << 30}, Layout{Workers: 4, Roles: []Role{RoleSource}}, p, 0); err == nil {
		t.Error("roundTo=0 accepted (would divide by zero)")
	}
	// Allowance multiplication overflow: refused, not wrapped.
	if _, err := Partition(Budget{Bytes: 1 << 30}, Layout{Workers: 1 << 40, Roles: []Role{RoleSource}}, p, 4096); err == nil {
		t.Error("allowance overflow accepted")
	}
}

// Partition must be deterministic: the same budget, layout and page
// size produce byte-identical grants every time. The P06 ram-* family
// derives expected data from seed -> region -> pass -> offset, so
// region identities (worker, region index, global offset, size) must
// not wobble between runs - otherwise the same seed would silently
// mean different data on different runs of the same config.
func TestPartitionDeterministic(t *testing.T) {
	p := DefaultTrialParams()
	layout := Layout{Workers: 3, Roles: []Role{RoleSource, RoleDest}}
	first, err := Partition(Budget{Bytes: 1 << 30}, layout, p, 4096)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Partition(Budget{Bytes: 1 << 30}, layout, p, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Grants) != len(second.Grants) {
		t.Fatalf("grant count changed: %d vs %d", len(first.Grants), len(second.Grants))
	}
	for i := range first.Grants {
		if first.Grants[i] != second.Grants[i] {
			t.Errorf("grant %d drifted: %+v vs %+v", i, first.Grants[i], second.Grants[i])
		}
	}
}
