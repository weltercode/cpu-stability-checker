package sysinfo

// CpuSetState is the OS's own view of one logical processor: the CPU Set
// whose "home" processor it is, and whether Windows power management
// currently parks that processor. This is the observation source for the
// park/wake idea — not a promise about C-states, but what the kernel's
// core-parking engine reports right now (ROADMAP level 2: observable).
type CpuSetState struct {
	ID              int // CPU Set id (usable with the CpuSet affinity APIs)
	LogicalCPU      int // home processor index (matches our logical core numbers on group 0)
	Core            int // physical core index (same for SMT siblings)
	LastLevelCache  int // shares its last-level cache with other cores having this index
	EfficiencyClass int // 0 = uniform on classic desktop parts
	Parked          bool
}

// ParkedCount returns how many of the sampled states are parked.
func ParkedCount(states []CpuSetState) int {
	n := 0
	for _, s := range states {
		if s.Parked {
			n++
		}
	}
	return n
}
