package dashboard

import (
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

// Sampler watches the OS's own counters between frames — the same
// parent-side trick per-process telemetry uses: we read what the kernel
// already accounts for, never instrument the children. Optional premium
// sources ride along: the ACPI thermal zone (PDH), HWiNFO's shared
// memory, and the PDH per-CPU performance clocks (real MHz against the
// firmware-rated base). A board without a thermal zone and a machine
// without HWiNFO stay honest n/a for the whole run; either one
// appearing later (HWiNFO started mid-session) is picked up
// automatically.
type Sampler struct {
	prev           []sysinfo.CoreTime
	temperature    *temperature.Monitor
	ownTemperature bool
	hw             *hwinfo.Source
	hwTries        int
	hwReads        []hwinfo.Reading
	hwRefresh      time.Time
	perf           *sysinfo.PerfClock
	perfTries      int
	hwClocks       map[int]float64
	wmi            *wmisensor.Source
	wmiTries       int
	wmiClocks      map[int]float64
	coreOf         []int // logical index -> physical core index, cached once
	topoTries      int
}

// NewSampler opens the (optional) temperature source. Failure is not an
// error: the panel just never shows a temperature.
func NewSampler() *Sampler {
	return &Sampler{temperature: temperature.New(), ownTemperature: true}
}

func newSamplerWithTemperature(m *temperature.Monitor) *Sampler { return &Sampler{temperature: m} }

// Apply fills the hardware half of the snapshot: per-core busy % from
// CoreTimes deltas (the first call only establishes the baseline),
// system load and memory, and the latest temperature sample.
func (s *Sampler) Apply(snap *Snapshot) {
	now, err := sysinfo.CoreTimes()
	if err == nil {
		if s.prev != nil && len(s.prev) == len(now) {
			busy := make([]float64, len(now))
			for i := range now {
				idle, ok := s.prev[i].IdlePct(now[i])
				if !ok {
					// The window carried no accounting at all (too
					// short): keep the previous reading rather than
					// inventing 0% or 100%.
					if i < len(snap.Busy) {
						busy[i] = snap.Busy[i]
					}
					continue
				}
				busy[i] = 100 - idle
			}
			snap.Busy = busy
		}
		s.prev = now
	}

	if m := sysinfo.CPUStats(); m.LoadKnown {
		snap.LoadPct, snap.LoadKnown = m.LoadPct, true
	}
	if total, used, ok := sysinfo.MemoryStats(); ok {
		snap.MemTotal, snap.MemUsed, snap.MemKnown = total, used, true
	}

	// Driver-backed sources first: HWiNFO reads the SMU directly, then
	// LibreHardwareMonitor/OpenHardwareMonitor via WMI, and either can
	// also carry temperature, power and voltage the OS cannot see.
	s.applyHWinfo(snap)
	s.applyWMI(snap)
	// Temperature never comes from the independent clock/power samplers.
	// The engine checks this exact monitor, including its freshness policy.
	s.applyTemperature(snap)
	// The clock column is built only from real sources: the PDH
	// performance counters cover every logical CPU, and the measured
	// clocks (WMI, then HWiNFO on top) override where published. TSC
	// values are dropped entirely once any real source speaks -
	// mixing them under an MHz label would lie.
	if n := len(snap.Busy); n > 0 {
		if s.coreOf == nil {
			// Retry with backoff: the API is static, but the first
			// frames may race early startup; a machine whose numbering
			// does not match the panel's indexing maps to nil forever.
			s.topoTries++
			if s.topoTries%10 == 1 {
				if topo, err := sysinfo.DiscoverTopology(); err == nil {
					s.coreOf = buildCoreOf(topo, n)
				}
			}
		}
		if len(s.coreOf) == n {
			snap.CoreOf = s.coreOf
		}
	}
	if clocks := s.mergeClocks(s.applyPerfClock(snap), snap); len(clocks) > 0 {
		snap.MHz = clocks
		snap.ClockLabel = "MHz"
	}
	if s.perf != nil {
		snap.BaseMHz = s.perf.Base()
	}
}

// coreClocks turns per-thread PDH percentages into display clocks.
// % Processor Performance is utilization-weighted: an idle thread reads
// ~0 no matter what the core's real clock is (the panel's own capture:
// idle -> 31 MHz). Dividing the utilization back out only works while
// the thread is essentially saturated: at partial load the perf window
// and the busy window disagree, and the division amplifies the
// disagreement into impossible numbers (the user's capture: 56% busy
// -> 8357 MHz on a 5.7 GHz part). So only a >=80% busy thread is
// trusted; its estimate is shown on ALL of the core's threads, because
// the two SMT siblings share one core clock. A core with no saturated
// thread has no honest reading and stays out of the map ("--"), rather
// than inventing a clock.
func coreClocks(perf map[int]float64, busy []float64, coreOf []int, base float64) map[int]float64 {
	if len(perf) == 0 || base <= 0 {
		return nil
	}
	groups := map[int][]int{}
	for cpu := range perf {
		key := cpu // unmapped topology: each thread stands for itself
		if len(coreOf) == len(busy) && cpu < len(coreOf) {
			key = coreOf[cpu]
		}
		groups[key] = append(groups[key], cpu)
	}
	out := make(map[int]float64, len(perf))
	for _, cpus := range groups {
		best := 0.0
		for _, cpu := range cpus {
			if cpu >= len(busy) || busy[cpu] < 80 {
				continue // partial load: the ratio is noise amplified
			}
			if est := perf[cpu] / (busy[cpu] / 100); est > best {
				best = est
			}
		}
		if best < 0.2*base || best > 2.2*base {
			continue // outside any plausible silicon envelope
		}
		for _, cpu := range cpus {
			out[cpu] = best
		}
	}
	return out
}

// buildCoreOf maps every logical CPU index the panel shows (the OS
// processor-number order CoreTimes returns) to its physical core index.
// The strict coverage rules (single group, no gaps, no duplicates)
// live in sysinfo Topology.CoreOf.
func buildCoreOf(topo sysinfo.Topology, n int) []int {
	return topo.CoreOf(n)
}

// mergeClocks layers the real sources: PDH percentages as the floor
// (it covers every logical CPU), WMI's measured clocks over it, and
// HWiNFO's conventional Core Clock on top. Physical sensor IDs are
// expanded to every SMT sibling using the OS topology.
func (s *Sampler) mergeClocks(perf map[int]float64, snap *Snapshot) map[int]float64 {
	if len(perf) == 0 && len(s.wmiClocks) == 0 && len(s.hwClocks) == 0 {
		return nil
	}
	floor := 100.0
	if s.perf != nil {
		if base := s.perf.Base(); base > 0 {
			floor = 0.1 * base
		}
	}
	out := make(map[int]float64, len(perf)+len(s.wmiClocks)+len(s.hwClocks))
	for k, v := range perf {
		out[k] = v
	}
	for k, v := range s.wmiClocks {
		if !parkedCore(snap.Busy, k, v, floor) {
			out[k] = v
		}
	}
	for cpu, v := range logicalClocks(s.hwClocks, snap.CoreOf, len(snap.Busy)) {
		out[cpu] = v // Core Clock remains valid for display at low load.
	}
	return out
}

func logicalClocks(physical map[int]float64, coreOf []int, n int) map[int]float64 {
	if n == 0 || len(coreOf) != n {
		return nil // do not guess sibling numbering without topology
	}
	out := make(map[int]float64, n)
	for cpu, core := range coreOf {
		if mhz, ok := physical[core]; ok {
			out[cpu] = mhz
		}
	}
	return out
}

// parkedCore filters near-zero effective WMI readings from idle threads.
// HWiNFO's conventional Core Clock bypasses this utilization filter.
func parkedCore(busy []float64, cpu int, mhz, floor float64) bool {
	return mhz < floor && cpu < len(busy) && busy[cpu] < 5
}

// applyPerfClock opens (with backoff) and reads the PDH per-CPU
// performance clocks, reduced to honest per-core frequencies; nil when
// unavailable.
func (s *Sampler) applyPerfClock(snap *Snapshot) map[int]float64 {
	if s.perf == nil {
		s.perfTries++
		if s.perfTries%10 != 1 {
			return nil
		}
		p, err := sysinfo.OpenPerfClock()
		if err != nil {
			return nil
		}
		s.perf = p
	}
	return coreClocks(s.perf.Read(), snap.Busy, snap.CoreOf, s.perf.Base())
}

// applyHWinfo refreshes the HWiNFO readings (at most once a second) and
// fills temperature, power, voltage and the real per-core clocks. It
// reports whether HWiNFO is the active source.
func (s *Sampler) applyHWinfo(snap *Snapshot) bool {
	if s.hw == nil {
		// Retry with backoff: HWiNFO may be started after the panel.
		s.hwTries++
		if s.hwTries%10 != 1 {
			return false
		}
		src, err := hwinfo.Open()
		if err != nil {
			return false
		}
		s.hw = src
	}
	if time.Since(s.hwRefresh) >= time.Second {
		s.hwReads = s.hw.Readings()
		s.hwRefresh = time.Now()
	}
	snap.HWinfo = true
	s.hwClocks = hwinfo.CoreClocks(s.hwReads)
	if r, ok := hwinfo.Find(s.hwReads, hwinfo.TypePower, "cpu package", "package power"); ok {
		snap.PowerW, snap.PowerKnown = r.Value, true
	}
	if r, ok := hwinfo.Find(s.hwReads, hwinfo.TypeVolt, "cpu core vid", "core vid", "vid", "vcore"); ok {
		snap.VoltV, snap.VoltKnown = r.Value, true
	}
	return true
}

// applyWMI opens (with backoff) the LibreHardwareMonitor /
// OpenHardwareMonitor WMI feed and fills temperature, power, voltage
// and measured per-core clocks when HWiNFO is not already speaking.
// It reports whether the WMI source is active.
func (s *Sampler) applyWMI(snap *Snapshot) bool {
	if s.wmi == nil {
		s.wmiTries++
		if s.wmiTries%10 != 1 {
			return false
		}
		src, err := wmisensor.Open()
		if err != nil {
			return false
		}
		s.wmi = src
	}
	reads := s.wmi.Readings()
	if len(reads) == 0 {
		return false
	}
	s.wmiClocks = wmisensor.CoreClocks(reads)
	if !snap.HWinfo {
		if r, ok := wmisensor.Find(reads, "Power", "cpu package", "package power"); ok {
			snap.PowerW, snap.PowerKnown = r.Value, true
		}
		if r, ok := wmisensor.Find(reads, "Voltage", "cpu core", "core vid", "vcore"); ok {
			snap.VoltV, snap.VoltKnown = r.Value, true
		}
	}
	snap.Wmi = true
	return true
}

// Close releases the temperature sensor, the HWiNFO view, the WMI
// connection and the PDH performance query.
func (s *Sampler) Close() {
	if s.ownTemperature && s.temperature != nil {
		s.temperature.Close()
	}
	if s.hw != nil {
		s.hw.Close()
		s.hw = nil
	}
	if s.wmi != nil {
		s.wmi.Close()
		s.wmi = nil
	}
	if s.perf != nil {
		s.perf.Close()
		s.perf = nil
	}
}

func (s *Sampler) applyTemperature(snap *Snapshot) {
	if s.temperature == nil {
		return
	}
	snap.Temperature = s.temperature.Snapshot()
	snap.TempKnown = snap.Temperature.Known
	snap.TempC = 0
	if snap.TempKnown {
		snap.TempC = snap.Temperature.Reading.Celsius
	}
}
