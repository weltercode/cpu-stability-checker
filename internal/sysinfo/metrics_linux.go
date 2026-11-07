//go:build linux

package sysinfo

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func memoryStatsImpl() (uint64, uint64, bool) {
	total, avail, ok := meminfoPhys()
	if !ok {
		return 0, 0, false
	}
	return total, total - avail, true
}

// memorySnapshotImpl reads physical AND commit numbers from
// /proc/meminfo. Commit is only "known" when both CommitLimit and
// Committed_AS are present; a machine without swap accounting reports
// commit unknown rather than guessing zero, and the budget layer
// refuses auto when a required observation is missing.
func memorySnapshotImpl() MemorySnapshot {
	total, avail, ok := meminfoPhys()
	if !ok {
		return MemorySnapshot{}
	}
	snap := MemorySnapshot{Known: true, TotalPhys: total, AvailPhys: avail}
	f, err := os.Open("/proc/meminfo")
	if err == nil {
		defer f.Close()
		var limit, used uint64
		var haveLimit, haveUsed bool
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "CommitLimit:"):
				limit, haveLimit = meminfoBytes(line), true
			case strings.HasPrefix(line, "Committed_AS:"):
				used, haveUsed = meminfoBytes(line), true
			}
		}
		if haveLimit && haveUsed {
			snap.CommitKnown = true
			snap.TotalPageFile = limit
			if used < limit {
				snap.AvailPageFile = limit - used
			}
			// used >= limit means the machine is already over its
			// commit limit: free commit is honestly zero, and the
			// budget math will refuse rather than pretend otherwise.
		}
	}
	return snap
}

func meminfoPhys() (total, avail uint64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = meminfoBytes(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = meminfoBytes(line)
		}
	}
	if total == 0 {
		return 0, 0, false
	}
	return total, avail, true
}

func meminfoBytes(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v * 1024
}

// coreFrequencies reads each CPU's scaling_cur_freq (kHz) when cpufreq
// exposes it; a machine without cpufreq drivers simply has no files and
// reports unknown rather than guessing.
func coreFrequencies() ([]float64, bool) {
	var out []float64
	for i := 0; ; i++ {
		b, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", i))
		if err != nil {
			break
		}
		khz, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			return nil, false
		}
		out = append(out, khz/1000)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Linux telemetry is intentionally best-effort. The scenario engine can run
// without lying about metrics that this backend does not provide yet.
func cpuStatsImpl() Metrics { return Metrics{} }
