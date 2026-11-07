package sysinfo

import (
	"fmt"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/human"
)

const metricsUnknown = -1.0 // internal sentinel used by sampling functions

// MemoryStats returns total and used physical memory in bytes when the
// platform supports it.
func MemoryStats() (total, used uint64, ok bool) {
	return memoryStatsImpl()
}

// MemorySnapshot is what this machine reports about its memory right
// now, physical and commit together. The zero value means "nothing
// known" - a field is never a trustworthy zero unless Known is set.
// TotalPageFile/AvailPageFile are the Windows commit limit and free
// commit (RAM plus page file), not disk space.
type MemorySnapshot struct {
	Known         bool
	CommitKnown   bool // TotalPageFile/AvailPageFile actually observed
	TotalPhys     uint64
	AvailPhys     uint64
	TotalPageFile uint64
	AvailPageFile uint64
}

// MemorySnapshotNow reads one consistent snapshot from the platform.
func MemorySnapshotNow() MemorySnapshot {
	return memorySnapshotImpl()
}

// Metrics is a best-effort snapshot of CPU state for the live UI.
// Known flags deliberately separate a real zero from an unavailable metric.
// This also makes the useful zero value of Metrics mean "nothing known".
type Metrics struct {
	LoadPct    float64 // 0..100 system-wide utilization
	FreqMinMHz float64 // slowest core's current frequency
	FreqMaxMHz float64 // fastest core's current frequency
	TempC      float64 // hottest exposed sensor, °C
	LoadKnown  bool
	FreqKnown  bool
	TempKnown  bool
}

// CPUStats returns a platform snapshot. Never panics; degrades to unknowns.
func CPUStats() Metrics { return cpuStatsImpl() }

// CoreFrequencies returns the OS-reported current frequency (MHz) for each
// logical CPU, indexed by logical CPU number; ok=false means the platform
// cannot report it at all. This is the frequency the kernel schedules
// against — an honest label, not a measured clock: the per-core cycle
// counters elsewhere measure what a worker actually achieved.
func CoreFrequencies() ([]float64, bool) { return coreFrequencies() }

// minMaxMHz reduces a frequency array to its bounds; empty means unknown.
// Pure, so the aggregation is testable without a platform query.
func minMaxMHz(freqs []float64) (minMHz, maxMHz float64, ok bool) {
	if len(freqs) == 0 {
		return 0, 0, false
	}
	minMHz, maxMHz = freqs[0], freqs[0]
	for _, f := range freqs[1:] {
		if f < minMHz {
			minMHz = f
		}
		if f > maxMHz {
			maxMHz = f
		}
	}
	return minMHz, maxMHz, true
}

func (m Metrics) loadLabel() string {
	if !m.LoadKnown {
		return "load n/a"
	}
	return fmt.Sprintf("load %d%%", int(m.LoadPct+0.5))
}

func (m Metrics) freqLabel() string {
	if !m.FreqKnown {
		return "freq n/a"
	}
	if m.FreqMinMHz == m.FreqMaxMHz {
		return fmt.Sprintf("freq %.0f MHz", m.FreqMaxMHz)
	}
	return fmt.Sprintf("freq %.0f–%.0f MHz", m.FreqMinMHz, m.FreqMaxMHz)
}

func (m Metrics) tempLabel() string {
	if !m.TempKnown {
		return "temp n/a"
	}
	return fmt.Sprintf("temp %.0f°C", m.TempC)
}

// MetricsLine builds one compact display line, e.g.
//
//	load 42%  freq 3100–3500 MHz  temp 63°C  mem 12.3GiB/32.0GiB used
func MetricsLine(m Metrics) string {
	parts := []string{m.loadLabel(), m.freqLabel(), m.tempLabel()}
	if total, used, ok := MemoryStats(); ok {
		parts = append(parts, fmt.Sprintf("mem %s/%s used", human.Bytes(used), human.Bytes(total)))
	}
	return strings.Join(parts, "  ")
}
