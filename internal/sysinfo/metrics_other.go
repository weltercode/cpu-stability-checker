//go:build !windows && !linux

package sysinfo

func memoryStatsImpl() (uint64, uint64, bool) { return 0, 0, false }

func memorySnapshotImpl() MemorySnapshot { return MemorySnapshot{} }

func coreFrequencies() ([]float64, bool) { return nil, false }

func cpuStatsImpl() Metrics { return Metrics{} }
