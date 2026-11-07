package sysinfo

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// LogicalCPU identifies one hardware thread as Windows sees it. Number is
// local to a processor group; the pair (Group, Number) is the stable address.
type LogicalCPU struct {
	Group         uint16
	Number        uint8
	CPUSetID      uint32
	CPUSetIDKnown bool
	Parked        bool
}

// PhysicalCore groups the logical CPUs (SMT siblings) that belong to one core.
// Index is our stable, zero-based display index after normalization.
// NativeIndex is the platform's core index inside a processor group.
// CCXIndex groups cores that share one L3 cache (the CCD/CCX unit on AMD);
// CCXKnown is false when the platform did not report cache topology.
type PhysicalCore struct {
	Index           int
	Group           uint16
	NativeIndex     uint8
	NUMANode        uint8
	EfficiencyClass uint8
	CCXIndex        int
	CCXKnown        bool
	LogicalCPUs     []LogicalCPU
}

// Topology is a normalized snapshot returned by one platform provider.
type Topology struct {
	Source        string
	Estimated     bool
	PhysicalCores []PhysicalCore
}

// CoreOf maps every logical CPU number 0..n-1 (the OS processor-number
// order CoreTimes indexes) to its physical core's Index, or returns nil
// unless the topology covers exactly those numbers: single-group machines
// with no gaps and no duplicates. Anything else stays unmapped rather
// than guessing sibling pairs the OS never promised — callers then keep
// per-core clock attribution unknown instead of mislabeling a core.
func (t Topology) CoreOf(n int) []int {
	coreOf := make([]int, n)
	for i := range coreOf {
		coreOf[i] = -1
	}
	for _, core := range t.PhysicalCores {
		for _, cpu := range core.LogicalCPUs {
			if cpu.Group != 0 || int(cpu.Number) >= n {
				return nil
			}
			if coreOf[cpu.Number] != -1 {
				return nil // duplicate logical CPU
			}
			coreOf[cpu.Number] = core.Index
		}
	}
	for _, core := range coreOf {
		if core < 0 {
			return nil // the topology missed a CPU the caller counts
		}
	}
	return coreOf
}

// rawLogicalCPU is the small platform-neutral record produced by an OS
// backend and consumed by normalizeTopology. Keeping normalization pure makes
// the important grouping rules easy to test without calling Windows APIs.
// l3ID names one L3 cache instance (any backend-stable value); cores sharing
// it form a CCX.
type rawLogicalCPU struct {
	group           uint16
	number          uint8
	coreIndex       uint8
	numaNode        uint8
	efficiencyClass uint8
	cpuSetID        uint32
	cpuSetIDKnown   bool
	l3ID            uint32
	l3Known         bool
	parked          bool
}

// DiscoverTopology reads the platform topology and normalizes it.
func DiscoverTopology() (Topology, error) {
	raw, source, estimated, err := discoverRawLogicalCPUs()
	if err != nil {
		return Topology{}, err
	}
	cores, err := normalizeTopology(raw)
	if err != nil {
		return Topology{}, fmt.Errorf("normalize topology: %w", err)
	}
	return Topology{Source: source, Estimated: estimated, PhysicalCores: cores}, nil
}

func normalizeTopology(raw []rawLogicalCPU) ([]PhysicalCore, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("platform returned no logical CPUs")
	}

	records := append([]rawLogicalCPU(nil), raw...)
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.coreIndex != b.coreIndex {
			return a.coreIndex < b.coreIndex
		}
		return a.number < b.number
	})

	type coreKey struct {
		group uint16
		core  uint8
	}
	type logicalKey struct {
		group  uint16
		number uint8
	}

	coreByKey := make(map[coreKey]int)
	seenLogical := make(map[logicalKey]struct{})
	seenCPUSet := make(map[uint32]struct{})
	cores := make([]PhysicalCore, 0, len(records))
	coreL3 := make([]rawLogicalCPU, 0, len(records)) // l3 fields per core, indexed like cores

	for _, r := range records {
		lk := logicalKey{group: r.group, number: r.number}
		if _, exists := seenLogical[lk]; exists {
			return nil, fmt.Errorf("duplicate logical CPU group=%d number=%d", r.group, r.number)
		}
		seenLogical[lk] = struct{}{}

		if r.cpuSetIDKnown {
			if _, exists := seenCPUSet[r.cpuSetID]; exists {
				return nil, fmt.Errorf("duplicate CPU Set ID %d", r.cpuSetID)
			}
			seenCPUSet[r.cpuSetID] = struct{}{}
		}

		ck := coreKey{group: r.group, core: r.coreIndex}
		idx, exists := coreByKey[ck]
		if !exists {
			idx = len(cores)
			coreByKey[ck] = idx
			cores = append(cores, PhysicalCore{
				Index:           idx,
				Group:           r.group,
				NativeIndex:     r.coreIndex,
				NUMANode:        r.numaNode,
				EfficiencyClass: r.efficiencyClass,
			})
			coreL3 = append(coreL3, r)
		} else {
			core := &cores[idx]
			if core.NUMANode != r.numaNode || core.EfficiencyClass != r.efficiencyClass {
				return nil, fmt.Errorf("conflicting metadata for group=%d core=%d", r.group, r.coreIndex)
			}
			if ref := coreL3[idx]; ref.l3Known && r.l3Known && ref.l3ID != r.l3ID {
				return nil, fmt.Errorf("conflicting L3 cache for group=%d core=%d", r.group, r.coreIndex)
			}
		}

		cores[idx].LogicalCPUs = append(cores[idx].LogicalCPUs, LogicalCPU{
			Group:         r.group,
			Number:        r.number,
			CPUSetID:      r.cpuSetID,
			CPUSetIDKnown: r.cpuSetIDKnown,
			Parked:        r.parked,
		})
	}

	// Cores sharing one L3 instance form a CCX (the CCD unit on AMD). Number
	// the groups in first-seen order so the display is stable per discovery.
	ccxByL3 := make(map[uint32]int)
	for i, ref := range coreL3 {
		if !ref.l3Known {
			continue
		}
		ccx, ok := ccxByL3[ref.l3ID]
		if !ok {
			ccx = len(ccxByL3)
			ccxByL3[ref.l3ID] = ccx
		}
		cores[i].CCXIndex = ccx
		cores[i].CCXKnown = true
	}

	return cores, nil
}

// WriteTopology renders the core table for the 'topology' command.
func WriteTopology(w io.Writer, topology Topology) {
	logicalCount := 0
	for _, core := range topology.PhysicalCores {
		logicalCount += len(core.LogicalCPUs)
	}

	estimated := ""
	if topology.Estimated {
		estimated = " (estimated)"
	}
	fmt.Fprintf(w, "Topology source: %s%s\n", topology.Source, estimated)
	fmt.Fprintf(w, "Physical cores: %d, logical CPUs: %d\n\n", len(topology.PhysicalCores), logicalCount)
	fmt.Fprintln(w, "CORE  NATIVE  GROUP  NUMA  CCX  EFF  LOGICAL CPUS              CPU SET IDS  PARKED")

	for _, core := range topology.PhysicalCores {
		logical := make([]string, 0, len(core.LogicalCPUs))
		cpuSets := make([]string, 0, len(core.LogicalCPUs))
		parked := make([]string, 0, len(core.LogicalCPUs))
		for _, cpu := range core.LogicalCPUs {
			logical = append(logical, fmt.Sprintf("g%d:%d", cpu.Group, cpu.Number))
			if cpu.CPUSetIDKnown {
				cpuSets = append(cpuSets, fmt.Sprint(cpu.CPUSetID))
			} else {
				cpuSets = append(cpuSets, "-")
			}
			if cpu.Parked {
				parked = append(parked, fmt.Sprintf("g%d:%d", cpu.Group, cpu.Number))
			}
		}
		parkedLabel := "-"
		if len(parked) > 0 {
			parkedLabel = strings.Join(parked, ",")
		}
		ccxLabel := "-"
		if core.CCXKnown {
			ccxLabel = fmt.Sprint(core.CCXIndex)
		}
		fmt.Fprintf(w, "%-5d %-7d %-6d %-5d %-4s %-4d %-25s %-12s %s\n",
			core.Index,
			core.NativeIndex,
			core.Group,
			core.NUMANode,
			ccxLabel,
			core.EfficiencyClass,
			strings.Join(logical, ","),
			strings.Join(cpuSets, ","),
			parkedLabel,
		)
	}
}
