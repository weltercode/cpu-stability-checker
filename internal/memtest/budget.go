// Package memtest owns the memory budget and native regions for RAM
// testing: one budget per phase/session, not one per worker; explicit
// allocation with returned errors (never recover-from-a-fatal-Go-heap-
// OOM); disjoint regions with identity; and honest accounting of what
// was requested, allocated, committed, touched and verified.
//
// The budget is computed once by the parent before a phase and handed
// down to workers as exact grants; children never re-derive an auto
// rule (their view of the machine could differ and multiply the
// budget). The auto-rule numbers are trial settings for experiments,
// not a residency guarantee - the plan says so and this code does not
// pretend otherwise.
package memtest

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// --- size parsing ------------------------------------------------------------

// ParseSize parses a size like "512MiB", "2GiB" or a plain byte count.
// Units are binary (KiB=1024). The multiplication is overflow-checked:
// "9999999999999999999TiB" is an error, not a wrapped number.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	unit := uint64(1)
	num := s
	for _, u := range []struct {
		suffix string
		mult   uint64
	}{
		{"KiB", 1 << 10},
		{"MiB", 1 << 20},
		{"GiB", 1 << 30},
		{"TiB", 1 << 40},
	} {
		if strings.HasSuffix(s, u.suffix) {
			unit = u.mult
			num = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	num = strings.TrimSpace(num)
	if num == "" {
		return 0, fmt.Errorf("size %q: missing number", s)
	}
	v, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	if unit != 1 && v > ^uint64(0)/unit {
		return 0, fmt.Errorf("size %q: overflows 64-bit bytes", s)
	}
	return v * unit, nil
}

// ParseRequest parses a memory request: "auto" or an exact size.
// A fixed request is never silently reduced later.
func ParseRequest(s string) (Request, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "auto") {
		return Request{Auto: true}, nil
	}
	n, err := ParseSize(s)
	if err != nil {
		return Request{}, err
	}
	if n == 0 {
		return Request{}, fmt.Errorf("memory request must be > 0")
	}
	return Request{Bytes: n}, nil
}

// --- budget ------------------------------------------------------------------

// Request is what the user/plan asked for: auto, or an exact byte
// count that must be honored or refused, never quietly shrunk.
type Request struct {
	Auto  bool
	Bytes uint64
}

// TrialParams are the auto-rule knobs. They are trial settings for
// experiments, not a residency guarantee: the real limit is whatever
// the allocator and the machine agree to at allocation time.
type TrialParams struct {
	// MaxPct caps the fraction of AVAILABLE physical memory the auto
	// rule may take (percent, 1..100).
	MaxPct uint64
	// MinOSReserve is the floor of the physical memory left to the OS;
	// the effective reserve is max(MinOSReserve, 10% of installed).
	MinOSReserve uint64
	// CommitReserve is the commit (page file + RAM) that must stay
	// free after the budget is committed.
	CommitReserve uint64
	// PerWorkerAllowance is the service/process overhead carved out of
	// the budget for every worker that will run (the test buffers are
	// not the only thing a process commits).
	PerWorkerAllowance uint64
}

// DefaultTrialParams are the plan's starting rule: at most 70% of
// available physical, an OS reserve of at least max(4 GiB, 10% of
// installed), 2 GiB of commit headroom kept free, and 64 MiB of
// process allowance per worker. All four are trial settings chosen
// for experimentation on this machine class, not guarantees.
func DefaultTrialParams() TrialParams {
	return TrialParams{
		MaxPct:             70,
		MinOSReserve:       4 << 30,
		CommitReserve:      2 << 30,
		PerWorkerAllowance: 64 << 20,
	}
}

// Budget is the outcome of the budget rule: the total bytes available
// for test data in this phase, plus a human-readable account of which
// constraint applied.
type Budget struct {
	Bytes uint64
	Rule  string
}

// ComputeBudget applies the plan's rule with checked arithmetic:
//
//	R  = max(MinOSReserve, ceil(TotalPhys/10))
//	B  = min(floor(AvailPhys*MaxPct/100), AvailPhys-R, AvailPageFile-CommitReserve)
//
// A missing observation forbids auto (the rule cannot be applied to
// numbers nobody measured); a subtraction that would go negative is
// an error naming the constraint, never a silent zero. A fixed
// request is honored exactly or refused with the constraint that
// blocks it - when the machine cannot be observed, the fixed request
// passes the budget layer and the allocator's honest failure decides.
func ComputeBudget(req Request, snap sysinfo.MemorySnapshot, p TrialParams) (Budget, error) {
	if p.MaxPct == 0 || p.MaxPct > 100 {
		return Budget{}, fmt.Errorf("memtest: MaxPct must be 1..100, got %d", p.MaxPct)
	}
	if req.Auto {
		if !snap.Known {
			return Budget{}, fmt.Errorf("memtest: auto budget refused: physical memory is not observed on this platform")
		}
		if !snap.CommitKnown {
			return Budget{}, fmt.Errorf("memtest: auto budget refused: commit information is not observed on this platform")
		}
		reserve := ceilDiv(snap.TotalPhys, 10)
		if p.MinOSReserve > reserve {
			reserve = p.MinOSReserve
		}
		capFrac := mulDiv(snap.AvailPhys, p.MaxPct, 100)
		capPhys, okPhys := subChecked(snap.AvailPhys, reserve)
		capCommit, okCommit := subChecked(snap.AvailPageFile, p.CommitReserve)
		if !okPhys {
			return Budget{}, fmt.Errorf("memtest: auto budget refused: available physical %s is below the OS reserve %s",
				exactBytes(snap.AvailPhys), exactBytes(reserve))
		}
		if !okCommit {
			return Budget{}, fmt.Errorf("memtest: auto budget refused: free commit %s is below the commit reserve %s",
				exactBytes(snap.AvailPageFile), exactBytes(p.CommitReserve))
		}
		b, which := capFrac, "70%%-of-available-physical"
		if p.MaxPct != 70 {
			which = fmt.Sprintf("%d%%-of-available-physical", p.MaxPct)
		}
		if capPhys < b {
			b, which = capPhys, "available-physical-minus-OS-reserve"
		}
		if capCommit < b {
			b, which = capCommit, "free-commit-minus-commit-reserve"
		}
		if b == 0 {
			return Budget{}, fmt.Errorf("memtest: auto budget is zero (%s bound it at zero)", which)
		}
		return Budget{Bytes: b, Rule: fmt.Sprintf("auto: min(%s=%s, availPhys-reserve=%s, commitFree-reserve=%s)",
			which, exactBytes(capFrac), exactBytes(capPhys), exactBytes(capCommit))}, nil
	}

	// Fixed: exact or error, never reduced.
	if snap.Known {
		reserve := ceilDiv(snap.TotalPhys, 10)
		if p.MinOSReserve > reserve {
			reserve = p.MinOSReserve
		}
		if want, ok := addChecked(req.Bytes, reserve); !ok || want > snap.AvailPhys {
			return Budget{}, fmt.Errorf("memtest: fixed request %s refused: available physical %s cannot cover it plus the OS reserve %s",
				exactBytes(req.Bytes), exactBytes(snap.AvailPhys), exactBytes(reserve))
		}
	}
	if snap.CommitKnown {
		if want, ok := addChecked(req.Bytes, p.CommitReserve); !ok || want > snap.AvailPageFile {
			return Budget{}, fmt.Errorf("memtest: fixed request %s refused: free commit %s cannot cover it plus the commit reserve %s",
				exactBytes(req.Bytes), exactBytes(snap.AvailPageFile), exactBytes(p.CommitReserve))
		}
	}
	return Budget{Bytes: req.Bytes, Rule: fmt.Sprintf("fixed: %s honored exactly", exactBytes(req.Bytes))}, nil
}

// --- checked arithmetic helpers ----------------------------------------------

// mulDiv computes floor(a*b/d) without intermediate overflow.
func mulDiv(a, b, d uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi == 0 {
		return lo / d
	}
	// Callers keep b <= 100 and d = 100, which guarantees hi < d and
	// a fitting quotient; if that ever changes, saturate at max
	// rather than letting bits.Div64 panic.
	if hi >= d {
		return ^uint64(0)
	}
	q, _ := bits.Div64(hi, lo, d)
	return q
}

func subChecked(a, b uint64) (uint64, bool) {
	if b > a {
		return 0, false
	}
	return a - b, true
}

func addChecked(a, b uint64) (uint64, bool) {
	s := a + b
	if s < a {
		return 0, false
	}
	return s, true
}

func ceilDiv(a, b uint64) uint64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}

// exactBytes formats byte counts without rounding: budget accounts
// must show the real number, not a friendly approximation.
func exactBytes(n uint64) string {
	return fmt.Sprintf("%d B", n)
}
