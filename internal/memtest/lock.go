package memtest

import (
	"errors"
	"fmt"
)

// ErrLockFailed means the OS refused to lock pages (working-set
// quota, resource limits, or an unsupported platform). It is a
// resource/setup condition, never a CPU fault.
var ErrLockFailed = errors.New("memtest: page lock failed")

// LockMode is the page-lock policy: off (default - never attempt),
// try (attempt and honestly report the outcome, run continues
// unlocked on failure), required (attempt; any failure is an error
// that must stop setup before measurement).
type LockMode string

const (
	LockOff      LockMode = "off"
	LockTry      LockMode = "try"
	LockRequired LockMode = "required"
)

// ParseLockMode maps the config string to a policy. Empty means off;
// anything unrecognized is a visible setup error, never a silent
// default.
func ParseLockMode(s string) (LockMode, error) {
	switch s {
	case "", string(LockOff):
		return LockOff, nil
	case string(LockTry):
		return LockTry, nil
	case string(LockRequired):
		return LockRequired, nil
	default:
		return "", fmt.Errorf("memtest: unknown memory_lock %q (want off, try or required)", s)
	}
}

// LockReport is what the lock policy actually did, with the process
// working-set limits for context. VirtualLock success is NOT a
// residency guarantee beyond those limits - the plan says so, and
// the report never claims more than the platform confirmed.
type LockReport struct {
	Mode            LockMode
	Attempted       bool
	LockedBytes     uint64
	FailedRegions   int
	Failure         string // first honest failure message, empty when all locked
	WorkingSetMin   uint64
	WorkingSetMax   uint64
	WorkingSetKnown bool
	LargePages      string // capability honesty, never a silent maybe
}

// LargePagesP05 is the explicit capability statement for this stage:
// large pages are not attempted at all in P05 (no MEM_LARGE_PAGES,
// no privilege elevation, no silent fallback).
const LargePagesP05 = "not attempted in P05"

// Lock applies the policy to every region. required returns an error
// wrapping ErrLockFailed on any failure; try reports the failure and
// continues; off reports nothing attempted.
func (e *Engine) Lock(mode LockMode) (LockReport, error) {
	rep := LockReport{Mode: mode, LargePages: LargePagesP05}
	if mode == LockOff {
		return rep, nil
	}
	rep.Attempted = true
	rep.WorkingSetMin, rep.WorkingSetMax, rep.WorkingSetKnown = workingSetLimits()
	var firstFailure string
	for _, r := range e.regions {
		if err := lockRegion(r.Block.Addr, r.Block.Allocated); err != nil {
			rep.FailedRegions++
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("worker=%d region=%d (%d bytes): %v",
					r.Grant.Worker, r.Grant.Region, r.Block.Allocated, err)
			}
			continue
		}
		rep.LockedBytes += r.Block.Allocated
	}
	rep.Failure = firstFailure
	if mode == LockRequired && rep.FailedRegions > 0 {
		return rep, fmt.Errorf("memtest: memory_lock=required but %d region(s) failed to lock: %s: %w",
			rep.FailedRegions, firstFailure, ErrLockFailed)
	}
	return rep, nil
}
