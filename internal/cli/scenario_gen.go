package cli

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// scenarioCmd runs the config's "phases" list as one session: every phase

// journalEstimate approximates how many journal records this plan writes,
// so an overnight run can be sized before it starts. core_beat dominates
// (one per core per beat interval), everything else is fixed per phase or
// per core. Rotation keeps every child alive across the whole rotation,
// so its beat span is the rotation length, not the phase duration.
//
// The byte figures use per-type averages MEASURED from real journals of
// this schema on this machine (documented below): an approximation is
// honest only while its basis is stated - the estimate says "approximate"
// and the constants say where they came from.
const (
	estBytesBeat      = 133 // core_beat
	estBytesRunStart  = 170
	estBytesRunEnd    = 110
	estBytesCoreStart = 155
	estBytesCoreDone  = 215
	estBytesTelemetry = 200
	estBytesWheaState = 153 // two per run: opening + verdict
	estBytesMemDone   = 350
	estBytesSchedule  = 390 // one per transition session
	estBytesSessBurst = 350 // parent view of a burst
	estBytesBurstDone = 390 // worker view of a burst
	// Q05 pair-switch records, measured from this schema's switch
	// journals: the prepared turn timeline is one record (~100 overhead
	// + ~45 per prepared turn), and the parent's turn view is ~271B.
	estBytesSwitchSched = 300 // prepared switch schedule (4-turn plan measured ~433B)
	estBytesSwitchTurn  = 275 // parent view of one switch turn (session_switch)
	estBytesLease       = 430 // parent view of one handoff lease (session_lease)
)

func journalEstimate(opts []plan.Options, cycles, numCPU int) (records, beats, bytes int) {
	for _, opt := range opts {
		cores := len(opt.Cores)
		if cores == 0 {
			cores = numCPU
		}
		beatEvery := opt.BeatEvery
		if beatEvery <= 0 {
			beatEvery = time.Second
		}
		// whea_state: an opening and a verdict record per run.
		phaseBytes := estBytesRunStart + estBytesRunEnd + 2*estBytesWheaState
		if opt.Bursts > 0 {
			// Transition mode: the session children beat at the same
			// cadence while alive (idle included), and the phase spans
			// the prepared schedule, not a duration.
			span := time.Duration(opt.Bursts) * (opt.BurstGap + opt.BurstRun)
			if opt.BurstStagger > 0 && cores > 1 {
				span += time.Duration(cores-1) * opt.BurstStagger
			}
			perCore := (span + beatEvery - 1) / beatEvery
			beats += cores * int(perCore)
			// schedule per session + core_start/core_done per core +
			// session_burst AND burst_done per burst per core (the two
			// views) + mem_done per session when the engine ran.
			records += 2 + cores + cores*2 + cores*opt.Bursts*2
			phaseBytes += cores*estBytesSchedule + cores*(estBytesCoreStart+estBytesCoreDone) +
				cores*opt.Bursts*(estBytesSessBurst+estBytesBurstDone)
			if opt.Memory != "" {
				records += cores
				phaseBytes += cores * estBytesMemDone
			}
			phaseBytes += cores * int(perCore) * estBytesBeat
			bytes += phaseBytes
			continue
		}
		if opt.HandoffBlocks > 0 {
			// Handoff phase (P07e): leases, not a clock, fill the
			// journal. The parent's session_lease records are EXACT in
			// count (two per block per round: write + read), the worker
			// fragments per core; beats ride the workers' wall time,
			// which hardware-dependent lease RTTs bound roughly - so
			// the estimate allows about one beat per four leases (the
			// estimate line is labeled approximate by its own wording).
			steps := opt.HandoffBlocks * opt.HandoffRounds
			beatsPerCore := steps/4 + 1
			records += 2 + cores + 2*steps
			beats += cores * beatsPerCore
			phaseBytes += cores*(estBytesCoreStart+estBytesCoreDone) + 2*steps*estBytesLease +
				cores*beatsPerCore*estBytesBeat
			bytes += phaseBytes
			continue
		}
		if plan.SwitchActive(opt) {
			// Q05 pair-switch: turns, not a clock, fill the journal.
			// Counts are EXACT where the parent controls them: one
			// switch_schedule, one session_switch per turn (group
			// level), one session_burst per participant per turn (the
			// parent view) and one burst_done per participant per turn
			// (the worker view), core_start/core_done per participant;
			// the participants live (and beat) across the whole
			// prepared span.
			part := 2 * len(opt.SwitchPairs) // ValidateSwitch pins exactly two slots
			turns := len(opt.SwitchPairs) * opt.SwitchRounds
			span := time.Duration(turns) * (opt.SwitchGap + opt.SwitchRun)
			perCore := (span + beatEvery - 1) / beatEvery
			beats += part * int(perCore)
			records += 1 + turns + part*turns*2 + part
			phaseBytes += estBytesSwitchSched + turns*estBytesSwitchTurn +
				part*turns*(estBytesSessBurst+estBytesBurstDone) +
				part*(estBytesCoreStart+estBytesCoreDone) +
				part*int(perCore)*estBytesBeat
			if opt.Memory != "" {
				records += part
				phaseBytes += part * estBytesMemDone
			}
			bytes += phaseBytes
			continue
		}
		span := opt.Duration
		if opt.Rotate && cores > 1 {
			span = opt.Duration*time.Duration(cores) + 2*time.Second
		}
		perCore := (span + beatEvery - 1) / beatEvery // ceil
		beats += cores * int(perCore)
		// run_start/run_end + core_start/core_done per core + telemetry
		// (one per run in-process, one per child in per-process mode).
		tele := 1
		if opt.PerProcess {
			tele = cores
		}
		records += 2 + cores*2 + tele + cores*int(perCore)
		phaseBytes += cores*(estBytesCoreStart+estBytesCoreDone) + tele*estBytesTelemetry +
			cores*int(perCore)*estBytesBeat
		if opt.Memory != "" {
			// per-process engine sessions journal mem_done per child.
			if opt.PerProcess {
				records += cores
				phaseBytes += cores * estBytesMemDone
			}
		}
		bytes += phaseBytes
	}
	return records * cycles, beats * cycles, bytes * cycles
}

// phaseLabel names a phase for humans: its configured name, or its
// position when unnamed.
func phaseLabel(name string, index int) string {
	if name != "" {
		return name
	}
	return fmt.Sprintf("phase %d", index+1)
}

// expandBursts compiles "burst" entries into ordinary phases in place:
// for each core, repeat times, an idle settle followed by a load run on
// that single core — the idle→load wake transient the park/wake idea
// rests on. Generators share the phase pipeline, so the runner never
// knows a phase came from a generator (roadmap: presets compile into
// the same Phase sequence, no separate execution logic).
func expandBursts(phases []config.Phase, cfg config.Config) ([]config.Phase, error) {
	var out []config.Phase
	for _, ph := range phases {
		b := ph.Burst
		if b == nil {
			out = append(out, ph)
			continue
		}
		if ph.Pairs != nil || ph.Curve != nil || ph.Cycle != nil {
			return nil, fmt.Errorf("phase %q: only one generator (burst, pairs, curve, cycle) per entry", phaseLabel(ph.Name, len(out)))
		}
		if len(b.Cores) == 0 {
			return nil, fmt.Errorf("phase %q: burst needs an explicit cores list", phaseLabel(ph.Name, len(out)))
		}
		n := b.Repeat
		if n < 1 {
			n = 1
		}
		load := b.Load
		if load == "" {
			load = cfg.WorkloadA
		}
		loadDur := b.LoadDuration
		if loadDur == "" {
			loadDur = cfg.Duration
		}
		idleDur := b.IdleDuration
		if idleDur == "" {
			idleDur = "2s"
		}
		base := ph.Name
		if base == "" {
			base = "burst"
		}
		for _, core := range b.Cores {
			for r := 1; r <= n; r++ {
				tag := fmt.Sprintf("%s-c%d-r%d", base, core, r)
				// One phase, one worker: the worker holds the pinned core
				// idle for idle_duration, then loads it — the wake happens
				// inside a single run and gets measured (journal wake record).
				out = append(out, config.Phase{
					Name: tag, Test: "general", A: load, B: "idle",
					Cores: []int{core}, Duration: loadDur,
					Settle: idleDur, PerProcess: true,
				})
			}
		}
	}
	return out, nil
}

// expandRepeats turns "repeat: N" into N concrete phases. Named phases get
// a "#k" suffix per repetition so the journal and the summary can tell the
// repetitions apart; unnamed ones stay positional.
func expandRepeats(phases []config.Phase) []config.Phase {
	var out []config.Phase
	for _, ph := range phases {
		n := ph.Repeat
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			e := ph
			e.Repeat = 0
			if n > 1 && ph.Name != "" {
				e.Name = fmt.Sprintf("%s#%d", ph.Name, i+1)
			}
			out = append(out, e)
		}
	}
	return out
}

// expandPairs compiles "pairs" entries into ordinary phases in place: one
// directed abab run per ordered (aggressor, observer) pair of the selected
// cores. Slot 0 (the aggressor) gets the aggressor workload, slot 1 (the
// observer) the observer workload; per-process mode pins each participant
// to its own core, and all_or_none keeps a pair intact when the fault
// blacklist grows. ccx maps each logical CPU to its L3 domain; scope
// "ccx" (the default) pairs only cores inside one domain — the interference
// that matters on AMD is intra-CCX.
func expandPairs(phases []config.Phase, cfg config.Config, ccx, smt map[int]int) ([]config.Phase, error) {
	var out []config.Phase
	for _, ph := range phases {
		p := ph.Pairs
		if p == nil {
			out = append(out, ph)
			continue
		}
		if ph.Curve != nil || ph.Cycle != nil {
			return nil, fmt.Errorf("phase %q: only one generator (burst, pairs, curve, cycle) per entry", phaseLabel(ph.Name, len(out)))
		}
		label := phaseLabel(ph.Name, len(out))
		if len(p.Cores) < 2 {
			return nil, fmt.Errorf("phase %q: pairs needs at least two cores", label)
		}
		seen := map[int]bool{}
		for _, c := range p.Cores {
			if c < 0 {
				return nil, fmt.Errorf("phase %q: pairs core %d is negative", label, c)
			}
			if seen[c] {
				return nil, fmt.Errorf("phase %q: pairs core %d is listed more than once", label, c)
			}
			seen[c] = true
		}
		scope := p.Scope
		if scope == "" {
			scope = "ccx"
		}
		if scope != "ccx" && scope != "all" && scope != "smt" {
			return nil, fmt.Errorf(`phase %q: pairs scope must be "ccx", "smt" or "all", got %q`, label, scope)
		}
		if scope == "ccx" && len(ccx) == 0 {
			return nil, fmt.Errorf("phase %q: pairs scope ccx needs CPU cache topology, which this system did not provide", label)
		}
		if scope == "smt" && len(smt) == 0 {
			return nil, fmt.Errorf("phase %q: pairs scope smt needs CPU topology, which this system did not provide", label)
		}
		aggressor := p.Aggressor
		if aggressor == "" {
			aggressor = cfg.WorkloadA
		}
		observer := p.Observer
		if observer == "" {
			observer = cfg.WorkloadB
		}
		dur := p.Duration
		if dur == "" {
			dur = cfg.Duration
		}
		base := ph.Name
		if base == "" {
			base = "pairs"
		}
		emitted := 0
		for _, a := range p.Cores {
			for _, o := range p.Cores {
				if a == o {
					continue
				}
				if scope == "ccx" {
					ca, okA := ccx[a]
					co, okO := ccx[o]
					if !okA || !okO {
						return nil, fmt.Errorf("phase %q: CCX unknown for core %d", label, pickUnknown(a, o, okA))
					}
					if ca != co {
						continue
					}
				}
				if scope == "smt" {
					// SMT siblings share one physical core: the aggressor
					// hammers the shared execution units and the observer
					// watches its own core falter — the classic CoreCycler
					// SMT pair. Both directions are emitted so each CPU
					// gets to be the aggressor.
					sa, okA := smt[a]
					so, okO := smt[o]
					if !okA || !okO {
						return nil, fmt.Errorf("phase %q: physical core unknown for core %d", label, pickUnknown(a, o, okA))
					}
					if sa != so {
						continue
					}
				}
				out = append(out, config.Phase{
					Name: fmt.Sprintf("%s-c%d-to-c%d", base, a, o),
					Test: "abab", A: aggressor, B: observer,
					Cores: []int{a, o}, Duration: dur,
					Settle: p.Settle, PerProcess: true, AllOrNone: true,
				})
				emitted++
			}
		}
		if emitted == 0 {
			return nil, fmt.Errorf("phase %q: pairs produced no pairs (scope %q?)", label, scope)
		}
	}
	return out, nil
}

// expandSwitchGroups compiles the Q05 scope matrix in place: a phase
// carrying switch_scope and/or switch_cores gets concrete disjoint
// [aggressor, observer] groups generated from the DETECTED topology -
// the same vocabulary the pairs generator uses ("ccx" default, "smt",
// "all"), but instead of one phase per pair the groups all live in ONE
// prepared pair-switch session: the turn ledger alternates them, so a
// generated 8-group phase is one experiment, not eight. Greedy
// pairing walks the pool in listed order: each unpaired core takes the
// first still-unpaired later partner the scope allows. The greedy is
// exactly complete when every scope domain has an even pool
// population (smt: sibling pairs; ccx: domain members) - and where it
// is not complete the leftover is refused by name, never silently
// dropped: dropping a core would quietly narrow the run's promise
// after the plan already named it. Pool order is data with meaning:
// under "all" the ORDER decides which domains the generated pairs
// cross (an interleaved pool pairs across domains), and the scope
// word never claims a domain relationship it did not enforce.
func expandSwitchGroups(phases []config.Phase, inv []int, ccx, smt map[int]int) ([]config.Phase, error) {
	var out []config.Phase
	for _, ph := range phases {
		if ph.SwitchScope == "" && len(ph.SwitchCores) == 0 {
			out = append(out, ph) // manual switch_pairs, or no switch shape at all
			continue
		}
		label := phaseLabel(ph.Name, len(out))
		if ph.SwitchPairs != nil {
			return nil, fmt.Errorf("phase %q: one source of switch groups: switch_pairs names them by hand while switch_scope/switch_cores pair them off the detected topology - pick one", label)
		}
		scope := ph.SwitchScope
		if scope == "" {
			scope = "ccx" // pool without scope mirrors the pairs default
		}
		if scope != "ccx" && scope != "smt" && scope != "all" {
			return nil, fmt.Errorf(`phase %q: switch scope must be "ccx", "smt" or "all", got %q`, label, scope)
		}
		pool := ph.SwitchCores
		if len(pool) == 0 {
			pool = inv
		}
		if len(pool) < 2 {
			return nil, fmt.Errorf("phase %q: switch scope %q needs a pool of at least two logical CPUs, got %d", label, scope, len(pool))
		}
		inInv := make(map[int]bool, len(inv))
		for _, c := range inv {
			inInv[c] = true
		}
		seen := map[int]bool{}
		for _, c := range pool {
			if c < 0 {
				return nil, fmt.Errorf("phase %q: switch core %d is negative", label, c)
			}
			if seen[c] {
				return nil, fmt.Errorf("phase %q: switch core %d is listed more than once", label, c)
			}
			seen[c] = true
			if !inInv[c] {
				return nil, fmt.Errorf("phase %q: switch core %d is not a logical CPU this system reported", label, c)
			}
		}
		if scope == "ccx" && len(ccx) == 0 {
			return nil, fmt.Errorf("phase %q: switch scope ccx needs CPU cache topology, which this system did not provide", label)
		}
		if scope == "smt" && len(smt) == 0 {
			return nil, fmt.Errorf("phase %q: switch scope smt needs CPU topology, which this system did not provide", label)
		}
		if scope == "ccx" {
			for _, c := range pool {
				if _, ok := ccx[c]; !ok {
					return nil, fmt.Errorf("phase %q: CCX unknown for core %d", label, c)
				}
			}
		}
		if scope == "smt" {
			for _, c := range pool {
				if _, ok := smt[c]; !ok {
					return nil, fmt.Errorf("phase %q: physical core unknown for core %d", label, c)
				}
			}
		}
		used := make([]bool, len(pool))
		var groups [][]int
		for i, a := range pool {
			if used[i] {
				continue
			}
			partner := -1
			for j := i + 1; j < len(pool); j++ {
				if !used[j] && switchScopeAllows(scope, a, pool[j], ccx, smt) {
					partner = j
					break
				}
			}
			if partner < 0 {
				return nil, fmt.Errorf("phase %q: switch scope %q left core %d unpaired in the pool: every generated participant takes turns, and a pool whose scope domains are odd cannot alternate honestly (refusing to drop the core silently)", label, scope, a)
			}
			used[i], used[partner] = true, true
			groups = append(groups, []int{a, pool[partner]})
		}
		if len(groups) < 2 {
			return nil, fmt.Errorf("phase %q: switch scope %q produced one group: pair-switch alternates turns between at least two prepared groups (the geometry rule names it again)", label, scope)
		}
		ph.SwitchPairs = groups
		out = append(out, ph)
	}
	return out, nil
}

// switchScopeAllows answers whether scope lets a and b share one
// generated group. "all" demands only distinct CPUs (pool order, not
// this predicate, decides which domains end up crossed).
func switchScopeAllows(scope string, a, b int, ccx, smt map[int]int) bool {
	switch scope {
	case "all":
		return a != b
	case "ccx":
		ca, okA := ccx[a]
		cb, okB := ccx[b]
		return okA && okB && ca == cb
	case "smt":
		sa, okA := smt[a]
		sb, okB := smt[b]
		return okA && okB && sa == sb
	}
	return false
}

func pickUnknown(a, o int, okA bool) int {
	if okA {
		return o
	}
	return a
}

// expandCurve compiles "curve" entries into ordinary phases in place: one
// general run per ramp step, loading the first k cores of the pool at once
// after a common idle settle. Per-process mode pins every participant, and
// all_or_none keeps a step whole when the fault blacklist grows — a step
// missing a core is no longer that step of the ramp.
func expandCurve(phases []config.Phase, cfg config.Config) ([]config.Phase, error) {
	var out []config.Phase
	for _, ph := range phases {
		c := ph.Curve
		if c == nil {
			out = append(out, ph)
			continue
		}
		if ph.Cycle != nil {
			return nil, fmt.Errorf("phase %q: only one generator (burst, pairs, curve, cycle) per entry", phaseLabel(ph.Name, len(out)))
		}
		label := phaseLabel(ph.Name, len(out))
		if len(c.Cores) < 2 {
			return nil, fmt.Errorf("phase %q: curve needs a pool of at least two cores", label)
		}
		seen := map[int]bool{}
		for _, core := range c.Cores {
			if core < 0 {
				return nil, fmt.Errorf("phase %q: curve core %d is negative", label, core)
			}
			if seen[core] {
				return nil, fmt.Errorf("phase %q: curve core %d is listed more than once", label, core)
			}
			seen[core] = true
		}
		steps := c.Steps
		if len(steps) == 0 {
			for k := 1; k < len(c.Cores); k *= 2 {
				steps = append(steps, k)
			}
			steps = append(steps, len(c.Cores))
		}
		seenStep := map[int]bool{}
		for _, k := range steps {
			if k < 1 || k > len(c.Cores) {
				return nil, fmt.Errorf("phase %q: curve step %d is outside 1..%d", label, k, len(c.Cores))
			}
			if seenStep[k] {
				return nil, fmt.Errorf("phase %q: curve step %d is listed more than once", label, k)
			}
			seenStep[k] = true
		}
		load := c.Load
		if load == "" {
			load = cfg.WorkloadA
		}
		dur := c.Duration
		if dur == "" {
			dur = cfg.Duration
		}
		settle := c.Settle
		if settle == "" {
			settle = "2s"
		}
		base := ph.Name
		if base == "" {
			base = "curve"
		}
		for _, k := range steps {
			out = append(out, config.Phase{
				Name: fmt.Sprintf("%s-k%d", base, k),
				Test: "general", A: load,
				Cores: c.Cores[:k], Duration: dur,
				Settle: settle, PerProcess: true, AllOrNone: true,
			})
		}
	}
	return out, nil
}

// CycleGroup is one physical core and the group-0 logical CPUs that
// belong to it, sorted. The core-cycle generator walks these groups.
type CycleGroup struct {
	Physical int
	Logicals []int
}

// cycleGroups discovers the physical-core groups a core-cycle phase needs,
// but only when some cycle phase actually asks for one: scenarios without
// a cycle never touch topology. Groups are sorted by physical index —
// the "sequential" walk order.
func cycleGroups(phases []config.Phase) ([]CycleGroup, error) {
	want := false
	for _, ph := range phases {
		if ph.Cycle != nil {
			want = true
		}
	}
	if !want {
		return nil, nil
	}
	topology, err := sysinfo.DiscoverTopology()
	if err != nil {
		return nil, fmt.Errorf("discover CPU topology for core cycle: %w", err)
	}
	var groups []CycleGroup
	for _, core := range topology.PhysicalCores {
		var logicals []int
		for _, cpu := range core.LogicalCPUs {
			if cpu.Group != 0 {
				continue // logical CPU numbers elsewhere are group-0 only
			}
			logicals = append(logicals, int(cpu.Number))
		}
		if len(logicals) == 0 {
			continue
		}
		sort.Ints(logicals)
		groups = append(groups, CycleGroup{Physical: core.Index, Logicals: logicals})
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("CPU topology reported no physical cores for the cycle")
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Physical < groups[j].Physical })
	return groups, nil
}

// shuffleGroups applies Fisher-Yates over a splitmix64 stream: the
// permutation depends only on (order, seed), never on the Go version's
// PRNG. The realized order is what the emitted phase names carry into
// the journal, so replay reads the journal, not the generator.
func shuffleGroups(groups []CycleGroup, seed uint64) []CycleGroup {
	out := make([]CycleGroup, len(groups))
	copy(out, groups)
	st := seed + 1 // splitmix64: a zero state would be a weak stream start
	for i := len(out) - 1; i > 0; i-- {
		st += 0x9e3779b97f4a7c15
		z := st
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		j := z % uint64(i+1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// drawSeed takes one 63-bit seed from the OS (crypto/rand): a "randomized"
// walk must not repeat between runs just because the clock is coarse.
func drawSeed() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("draw cycle seed: %w", err)
	}
	return binary.LittleEndian.Uint64(b[:]) & 0x7fffffffffffffff, nil
}

// expandCycle compiles "cycle" entries into ordinary phases in place: one
// per-core phase per physical core (or per selected subset), walked in the
// requested order. Each phase is per-process so every participant is
// pinned, and starts from an idle settle on its own core — the walk also
// exercises the idle→load transition per core. "both" SMT groups carry
// all_or_none: half a core's siblings loading is not that core anymore.
//
// A "randomized" entry draws ONE seed, shuffles with it, and writes the
// realized (order, seed) back into the generator: the saved plan then
// replays the same walk as "seeded" without any PRNG-version dependency,
// and the journal already carries the realized order in its phase names.
func expandCycle(phases []config.Phase, cfg config.Config, groups []CycleGroup, notices *[]string) ([]config.Phase, error) {
	var out []config.Phase
	for _, ph := range phases {
		c := ph.Cycle
		if c == nil {
			out = append(out, ph)
			continue
		}
		if ph.Burst != nil || ph.Pairs != nil || ph.Curve != nil {
			return nil, fmt.Errorf("phase %q: only one generator (burst, pairs, curve, cycle) per entry", phaseLabel(ph.Name, len(out)))
		}
		label := phaseLabel(ph.Name, len(out))
		smt := c.SMT
		if smt == "" {
			smt = "one"
		}
		if smt != "one" && smt != "both" {
			return nil, fmt.Errorf(`phase %q: cycle smt must be "one" or "both", got %q`, label, smt)
		}
		order := c.Order
		if order == "" {
			order = "sequential"
		}
		if order != "sequential" && order != "seeded" && order != "randomized" {
			return nil, fmt.Errorf(`phase %q: cycle order must be "sequential", "seeded" or "randomized", got %q`, label, order)
		}
		if len(groups) == 0 {
			return nil, fmt.Errorf("phase %q: cycle needs CPU topology (physical cores), which this system did not provide", label)
		}
		var seed uint64
		switch order {
		case "seeded":
			if c.Seed == nil {
				return nil, fmt.Errorf(`phase %q: cycle order "seeded" needs a seed`, label)
			}
			seed = uint64(*c.Seed)
		case "randomized":
			drawn, derr := drawSeed()
			if derr != nil {
				return nil, derr
			}
			seed = drawn
			// Write the realized walk back through the shared pointer: the
			// saved plan (and any later repeat) replays it as "seeded".
			c.Order, c.Seed = "seeded", protoInt64(int64(seed))
			*notices = append(*notices, fmt.Sprintf(
				"core-cycle %q: randomized walk realized seed %d — the saved plan replays this order as \"seeded\"", label, seed))
		}

		walk := groups
		if order != "sequential" {
			walk = shuffleGroups(groups, seed)
		}
		// Decide which logical CPUs each phase loads. "one" means one
		// logical CPU per phase (siblings tested separately, the lowest
		// sibling by default); "both" means the whole sibling group as one
		// indivisible phase.
		type cycleStep struct {
			physical int
			cpus     []int
		}
		var steps []cycleStep
		if len(c.Cores) == 0 {
			for _, g := range walk {
				if smt == "both" {
					steps = append(steps, cycleStep{g.Physical, g.Logicals})
					continue
				}
				steps = append(steps, cycleStep{g.Physical, g.Logicals[:1]})
			}
		} else {
			want := map[int]bool{}
			for _, cpu := range c.Cores {
				if cpu < 0 {
					return nil, fmt.Errorf("phase %q: cycle core %d is negative", label, cpu)
				}
				want[cpu] = true
			}
			for _, g := range walk {
				var chosen []int
				for _, cpu := range g.Logicals {
					if want[cpu] {
						chosen = append(chosen, cpu)
						delete(want, cpu)
					}
				}
				if len(chosen) == 0 {
					continue
				}
				if smt == "both" {
					steps = append(steps, cycleStep{g.Physical, g.Logicals}) // the group is indivisible
					continue
				}
				for _, cpu := range chosen {
					steps = append(steps, cycleStep{g.Physical, []int{cpu}})
				}
			}
			if len(want) > 0 {
				left := make([]int, 0, len(want))
				for cpu := range want {
					left = append(left, cpu)
				}
				sort.Ints(left)
				return nil, fmt.Errorf("phase %q: cycle cores %v match no physical core of this machine", label, left)
			}
		}
		if len(steps) == 0 {
			return nil, fmt.Errorf("phase %q: cycle selected no physical cores", label)
		}
		// A core that contributes several single-CPU phases (both siblings
		// selected under "one") names each phase by its CPU, so journal
		// labels never collide and each phase says what it tested.
		perCore := map[int]int{}
		for _, s := range steps {
			perCore[s.physical]++
		}

		load := c.Load
		if load == "" {
			load = cfg.WorkloadA
		}
		dur := c.Duration
		if dur == "" {
			dur = cfg.Duration
		}
		settle := c.Settle
		if settle == "" {
			settle = "2s"
		}
		base := ph.Name
		if base == "" {
			base = "cycle"
		}
		for _, s := range steps {
			name := fmt.Sprintf("%s-p%d", base, s.physical)
			if perCore[s.physical] > 1 {
				name = fmt.Sprintf("%s-p%d-c%d", base, s.physical, s.cpus[0])
			}
			out = append(out, config.Phase{
				Name: name,
				Test: "general", A: load,
				Cores: s.cpus, Duration: dur,
				Settle: settle, PerProcess: true, AllOrNone: smt == "both",
			})
		}
	}
	return out, nil
}

func protoInt64(v int64) *int64 { return &v }

// topologyMaps discovers what the scope-driven generators need: the
// L3-domain (ccx) and physical-core (smt) membership maps, plus the
// sorted group-0 logical-CPU INVENTORY the switch scope pool validates
// against (and defaults to). Maps are logical CPU -> group index,
// group 0 only. Discovery is demanded by the phase words, never by
// guesswork: pairs need their scope maps, switch scopes need theirs,
// and EVERY generated switch phase (scope "all" included) needs the
// inventory - a pool member that this system does not report is a
// compile error even when no scope filter consults it.
func topologyMaps(phases []config.Phase) (inv []int, ccx, smt map[int]int, err error) {
	wantCCX, wantSMT, wantINV := false, false, false
	for _, ph := range phases {
		if ph.Pairs != nil {
			switch ph.Pairs.Scope {
			case "", "ccx":
				wantCCX = true
			case "smt":
				wantSMT = true
			}
		}
		if ph.SwitchScope != "" || len(ph.SwitchCores) > 0 {
			wantINV = true
			switch ph.SwitchScope {
			case "", "ccx":
				wantCCX = true
			case "smt":
				wantSMT = true
			}
		}
	}
	if !wantCCX && !wantSMT && !wantINV {
		return nil, nil, nil, nil
	}
	topology, err := sysinfo.DiscoverTopology()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discover CPU topology for scope generators: %w", err)
	}
	if wantCCX {
		ccx = map[int]int{}
	}
	if wantSMT {
		smt = map[int]int{}
	}
	if wantINV {
		inv = []int{}
	}
	for _, core := range topology.PhysicalCores {
		for _, cpu := range core.LogicalCPUs {
			if cpu.Group != 0 {
				continue // logical CPU numbers elsewhere are group-0 only
			}
			if wantCCX && core.CCXKnown {
				ccx[int(cpu.Number)] = core.CCXIndex
			}
			if wantSMT {
				smt[int(cpu.Number)] = core.Index
			}
			if wantINV {
				inv = append(inv, int(cpu.Number))
			}
		}
	}
	sort.Ints(inv)
	return inv, ccx, smt, nil
}
