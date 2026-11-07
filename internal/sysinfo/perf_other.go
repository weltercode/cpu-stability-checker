//go:build !windows

package sysinfo

import "errors"

// PerfClock exists as an inert type so callers compile everywhere;
// the "% Processor Performance" counter and SMBIOS are Windows things.
type PerfClock struct{}

func OpenPerfClock() (*PerfClock, error) {
	return nil, errors.New("PDH performance clocks are Windows-only")
}

func (p *PerfClock) Base() float64 { return 0 }

func (p *PerfClock) Read() map[int]float64 { return nil }

func (p *PerfClock) Close() {}
