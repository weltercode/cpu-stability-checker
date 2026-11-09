// Package journal implements the append-only crash journal: a
// machine-readable record of what ran on which core.
package journal

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/human"
)

// Unlike the human log (internal/logging) the journal's purpose is crash
// recovery: a core that has a core_start but no core_done crashed (or the
// machine lost power), and the last core_beat shows when that core was last
// known to be healthy.
//
// One JSON object per line. Malformed or truncated lines are counted
// and reported (ParseAll), so a crash in the middle of a write never
// makes the whole journal unreadable and never silently cleans it up
// either. In per-process mode each child owns its own fragment file
// under <journal>.workers/<run-id>/ — no two writers ever share a
// file, because append atomicity across OSes is not something we may
// assume.
const (
	RunStart     = "run_start"
	RunMeta      = "run_meta" // run provenance: build identity, config hash, seed, machine; absence = an older journal (unknown, never guessed)
	Sources      = "sources"  // subprocess runs: the worker fragments this run expects
	CoreStart    = "core_start"
	CoreBeat     = "core_beat"
	CoreDone     = "core_done"
	MemDone      = "mem_done"      // memory-engine accounting for one slot; absence = no engine (unknown, never zero)
	BurstDone    = "burst_done"    // one verified burst of a P08 session; the session's allocation is counted once (mem_done), bursts are the work breakdown
	SessionBurst = "session_burst" // the PARENT's view of one commanded burst: when it was sent and when the acks came back
	SessionLease = "session_lease" // the PARENT's view of one P07e block-ownership lease (arbiter-serialized, never raced)
	ParkingObs   = "parking_obs"   // the PARENT's folded OS-view parking observation for one session slot (Q04): absence = no observation was made (policy off or a pre-Q04 journal) - unknown, never "not parked"
	Schedule     = "schedule"      // the PREPARED P08 burst timeline, journaled before the run so replay never depends on a future PRNG version
	// SwitchSchedule is the PREPARED Q05 pair-switch turn timeline
	// (two prepared groups alternating), journaled before anything
	// spawns: the same reproduction rule as "schedule" - the concrete
	// turns are the artifact, never the seed.
	SwitchSchedule = "switch_schedule"
	// SessionSwitch is the PARENT's view of one group turn of a
	// pair-switch: when the turn was actually commanded vs when the
	// group's STARTED acks and idle barrier closed (upper bounds,
	// incl IPC). A journal predating Q05 simply has none: the switch
	// story is unknown, never "no switches happened".
	SessionSwitch = "session_switch"
	Wake          = "wake"       // measured delay from thaw to first confirmed work on a core
	Telemetry     = "telemetry"  // effective clock + idle share over the load window
	Whea          = "whea"       // one hardware error record observed during the run
	WheaState     = "whea_state" // WHEA observation state: how much the counts below mean
	RunEnd        = "run_end"

	OK          = "ok"
	Fault       = "fault"
	Interrupted = "interrupted"
	Skipped     = "skipped"
	SetupError  = "setup_error"

	// Crashed is not written to the file; it is what recovery reports
	// for a core that started but never finished.
	Crashed = "crashed"
)

// SchemaVersion is the journal line format this build writes. Records
// without a schema field (0) come from older builds: their newer fields
// (checks, load time, journal_errs) read as unknown, never as success.
const SchemaVersion = 1

// MemAccounting is the memory engine's honest bookkeeping for one
// slot, written as a mem_done record. Every number is what the OS
// and the workload actually did - not what was asked for and not an
// estimate. A slot without an engine has no mem_done record at all:
// its numbers stay unknown, never zero-filled.
type MemAccounting struct {
	// Requested is the sum of grant sizes the parent assigned.
	Requested uint64 `json:"requested"`
	// Allocated is what the OS actually committed per region
	// (VirtualQuery's region size): exactly the "committed" bytes in
	// this layer, since every region is committed as it is reserved.
	// It differs from Requested exactly when the OS rounded a region,
	// and the report must show the difference, not hide it.
	Allocated uint64 `json:"allocated"`
	// Touched counts the bytes the first-touch pass wrote BEFORE the
	// measured window; a cancelled pass reports the honest partial.
	Touched uint64 `json:"touched"`
	// FirstTouchMS is how long that pass took, accounted separately
	// from the measured run. This machine's monotonic clock advances
	// in ~0.5 ms steps, so a small pass can honestly read 0.
	FirstTouchMS float64 `json:"first_touch_ms"`
	// Verified is the number of engine bytes the workload's last
	// completed pass actually checked; 0 means the engine memory was
	// allocated and touched but never verified by the workload.
	Verified uint64 `json:"verified"`
	// LockMode/Locked/LockFailure report what the page-lock policy
	// really achieved: "off" attempted nothing, "try" may have
	// failed and said so, "required" could not have failed (the run
	// would have stopped). Locked success is bounded by the process
	// working-set limits and is not a residency guarantee.
	LockMode    string `json:"lock_mode"`
	Locked      uint64 `json:"locked"`
	LockFailure string `json:"lock_failure,omitempty"`
	// Seed is the configured seed of a seed-consuming workload
	// (the P06 ram-* family): nil = no such workload ran (the
	// seed, if any was configured, was not consumed - unknown,
	// not zero). 0 here is a real seed, not "unset".
	Seed *uint64 `json:"seed,omitempty"`
	// VerifiedUniqueBytes is the engine coverage of the workload's
	// last completed pass (distinct bytes checked); VerifiedTotalBytes
	// is the cumulative verification traffic across all completed
	// passes (unique x passes for a kernel that rechecks everything
	// every pass). Both nil = the workload did not report pass
	// coverage (older records, non-ram workloads): unknown, not
	// zero. They separate "how much of the region did we actually
	// look at" from "how much checking work did we do" - the plan
	// demands both counts, never one pretending to be the other.
	VerifiedUnique *uint64 `json:"verified_unique_bytes,omitempty"`
	VerifiedTotal  *uint64 `json:"verified_total_bytes,omitempty"`
}

// BurstObservation is the PARENT's view of one commanded burst: the
// schedule deviation (actual TRIGGER minus the planned moment,
// signed - cutting a gap short is as honest as missing it), the
// intervals from the commanded trigger to each ack's ARRIVAL, and
// the counts the worker reported. These intervals include the IPC
// round trip: they are upper bounds for trigger_to_first_verified_block
// on this API (no verified shared monotonic source yet - the plan
// allows parent round-trip intervals when counted separately, and
// that is exactly what they are labeled as). The worker's own
// burst_done record carries the worker-local execution time; the
// two views stay separate records and are never merged.
type BurstObservation struct {
	Burst               string  `json:"burst"`
	Seq                 uint64  `json:"seq"`
	SchedDevMS          float64 `json:"sched_dev_ms"`           // signed: + late, - early
	TriggerToStartMS    float64 `json:"trigger_to_start_ms"`    // commanded -> STARTED ack (upper bound, incl IPC)
	TriggerToVerifiedMS float64 `json:"trigger_to_verified_ms"` // commanded -> VERIFIED ack (upper bound, incl IPC)
	IdleBeforeMS        float64 `json:"idle_before_ms"`         // parent-observed idle before this TRIGGER
	Checks              *uint64 `json:"checks,omitempty"`       // from the VERIFIED ack; nil = worker did not report
	Bytes               *uint64 `json:"bytes,omitempty"`
	Complete            bool    `json:"complete"` // the VERIFIED ack actually arrived
}

// LeaseObservation is the parent's view of one P07e block-ownership
// lease: which block at which generation, which worker ran it, when
// the command was sent and when the ack came back (upper bounds,
// including the IPC round trip, labeled as such like the burst
// intervals). The arbiter's ordering is what guarantees the tester
// never raced; this record is the per-step evidence that the
// guarantee held - or exactly where it stopped holding.
type LeaseObservation struct {
	Session  string  `json:"session"`
	Seq      uint64  `json:"seq"`
	Block    uint64  `json:"block"`
	Gen      uint64  `json:"gen"`
	Mode     string  `json:"mode"`                // "lease-write" | "lease-read"
	PeerCore *int    `json:"peer_core,omitempty"` // the other side of this handoff (nil = not applicable/unknown)
	ToAckMS  float64 `json:"to_ack_ms"`           // commanded -> ack (upper bound, incl IPC)
	Checks   *uint64 `json:"checks,omitempty"`    // from READ_OK; nil = not reported (unknown, never zero)
	Bytes    *uint64 `json:"bytes,omitempty"`
	Acked    bool    `json:"acked"` // the expected ack actually arrived
	Error    string  `json:"error,omitempty"`
}

// ScheduleWindow is one prepared burst slot in the journaled
// timeline: an idle gap (from the previous burst's close, or from
// session-ready for the first) and a run time. Nanoseconds as
// integers: float milliseconds would invent rounding drift.
type ScheduleWindow struct {
	GapNS    int64 `json:"gap_ns"`
	RunForNS int64 `json:"run_for_ns"`
}

// SchedulePlan is the PREPARED P08 burst timeline, journaled BEFORE
// the session runs. The concrete windows are the reproduction
// artifact: replay reads these numbers, never the seed, so a future
// PRNG change can never silently re-derive an old run's schedule.
// Seed nil means a fixed schedule (no jitter) - distinct from
// "jitter with seed 0", which is a real seeded schedule.
type SchedulePlan struct {
	BaseNS   int64            `json:"base_ns"`
	JitterNS int64            `json:"jitter_ns"`
	Seed     *uint64          `json:"seed,omitempty"`
	Windows  []ScheduleWindow `json:"windows"`
}

// SwitchPlanTurn is one prepared group turn in the journaled
// pair-switch timeline: which group loads, in which round, after what
// idle gap (from the PREVIOUS turn's actual barrier close) and for
// how long. Integer nanoseconds, same rule as ScheduleWindow.
type SwitchPlanTurn struct {
	Group    int   `json:"group"`
	Round    int   `json:"round"`
	GapNS    int64 `json:"gap_ns"`
	RunForNS int64 `json:"run_for_ns"`
}

// SwitchPlan is the PREPARED Q05 pair-switch timeline, journaled
// BEFORE the run - the concrete turns are the reproduction artifact,
// the seed is provenance only (nil = the order was fixed AND no
// jitter consumed a seed). Order is the order word as commanded
// ("" = fixed alternation).
type SwitchPlan struct {
	BaseNS   int64            `json:"base_ns"`
	JitterNS int64            `json:"jitter_ns"`
	Seed     *uint64          `json:"seed,omitempty"`
	Order    string           `json:"order,omitempty"`
	Turns    []SwitchPlanTurn `json:"turns"`
}

// SwitchObservation is the PARENT's view of one pair-switch turn:
// the deviation of the actual command moment from the planned one
// (signed), the switch latency to the last STARTED ack of the group
// and the turn's total span to its idle barrier close - both upper
// bounds INCLUDING the IPC round trip, labeled as such like the burst
// and lease intervals - and whether the idle barrier (every
// participant VERIFIED and back idle) actually closed. The per-
// participant detail rides the members' own session_burst records;
// this record is the turn level - the switch itself - and the two
// views are never merged.
type SwitchObservation struct {
	Turn            int     `json:"turn"` // index in the prepared turn sequence
	Group           int     `json:"group"`
	Round           int     `json:"round"`
	Cores           []int   `json:"cores,omitempty"`    // the group's participants (logical CPUs)
	PrimarySeq      uint64  `json:"primary_seq"`        // the turn key: the first member's TRIGGER seq
	SchedDevMS      float64 `json:"sched_dev_ms"`       // signed: + late, - early
	SwitchToStartMS float64 `json:"switch_to_start_ms"` // commanded -> last STARTED ack of the group (upper bound, incl IPC)
	TurnCloseMS     float64 `json:"turn_close_ms"`      // commanded -> idle barrier closed (upper bound, incl IPC)
	Complete        bool    `json:"complete"`           // every participant VERIFIED and back idle
	Error           string  `json:"error,omitempty"`    // why it never completed ("" = completed or not attempted)
}

// RunMetaFields is run-level provenance: what produced this journal
// and on what machine. Every field is optional and independently
// unknown — a build without VCS stamping (a vendored or prebuilt
// binary) simply leaves the build fields empty, and an OS that
// reports no brand string leaves CPUName empty. Empty means "not
// observed", never "verified to be nothing".
type RunMetaFields struct {
	ModuleVer     string  `json:"module_version,omitempty"` // module version at build ("(devel)" for a local build)
	BuildRev      string  `json:"build_rev,omitempty"`      // VCS revision the binary was built from
	BuildTime     string  `json:"build_time,omitempty"`     // VCS commit time of that build
	BuildModified *bool   `json:"build_modified,omitempty"` // working tree was dirty at build (nil = unknown)
	ConfigHash    string  `json:"config_sha256,omitempty"`  // sha256 of the config file as present at run start
	Seed          *uint64 `json:"seed,omitempty"`           // configured ram seed (0 is a valid seed; nil = not carried)
	CPUName       string  `json:"cpu_name,omitempty"`       // processor brand string as the OS reports it
	OSVersion     string  `json:"os_version,omitempty"`     // OS version string (Windows NT build / kernel release)
	// BIOSVersion is reserved: this tool does not collect BIOS identity
	// yet, so it stays empty and reads as unknown. It exists so a
	// future collector adds a field, not a schema argument.
	BIOSVersion string `json:"bios_version,omitempty"`
	// PlanHash is the sha256 of the normalized request plus this
	// concrete resolved plan: the experiment's identity. The config
	// FILE hash cannot see CLI flags, menu choices or a saved recipe,
	// so two different experiments could share it (audit A11); this
	// hash covers what actually ran. "" = the journal predates the
	// field (unknown, never assumed equal).
	PlanHash string `json:"plan_hash,omitempty"`
	// TopologyFP fingerprints the CPU topology the run resolved
	// against (physical core layout, SMT widths, groups): the same
	// plan on a different layout is a different experiment. "" =
	// topology could not be observed.
	TopologyFP string `json:"topology_fp,omitempty"`
	// SessionID is the unique id of the user session that started
	// this run (U02 history): one parent session collects its child
	// runs, and the id rides into every journal so runs stay
	// attributable to their session even when several sessions
	// share one log file. "" = the journal predates session
	// attribution, or the entry point started no tracked session
	// (unknown, never inferred).
	SessionID string `json:"session_id,omitempty"`
	// Memory is the SMBIOS memory identity exactly as the firmware
	// wrote it (Type 16 array + Type 17 device records): named
	// observations, never a stability diagnosis, and never personal
	// identity - serial numbers and asset tags are not collected
	// upstream (see internal/sysinfo MemoryDevice), so they cannot
	// reach the journal. nil = not observed: journal predating the
	// field, platform without an SMBIOS fetcher, or firmware refused
	// (unknown, never zero-filled).
	Memory *RunMemoryFields `json:"memory,omitempty"`
}

// RunMemoryFields / RunMemoryDevice mirror the sysinfo SMBIOS memory
// observations into the journal (R16). Field names state what SMBIOS
// wrote, under the names the spec attaches to those offsets; all
// three speed-shaped fields are recorded verbatim and the tool draws
// no conclusion from their disagreement, because real firmware is
// inconsistent: this dev board's Type 17 records report SPD speed
// 4800, max capability 5600 and a voltage-like 1100 in the slot the
// spec calls "configured memory clock speed" (verified live
// 2026-11-09). Comparing those numbers is the reader's work;
// repairing or interpreting them inside the evidence recorder is
// not.
type RunMemoryFields struct {
	SMBIOSVersion string `json:"smbios_version,omitempty"` // "3.4" style from the firmware header; "" = unknown
	// UnknownReason is set INSTEAD of observations when the collector
	// refused (no firmware table on this platform, truncated table,
	// and so on): the skip stays named in the record instead of
	// reading as "this machine has no memory records". Never a guess,
	// and never paired with fabricated numbers.
	UnknownReason      string            `json:"unknown_reason,omitempty"`
	ArrayMaxCapacityMB *int              `json:"array_max_capacity_mb,omitempty"` // referenced Type 16 array's declared ceiling in MB; nil = unknown
	Devices            []RunMemoryDevice `json:"devices,omitempty"`               // every slot record (populated or empty), table order
	// RejectedCandidates counts malformed memory records the parser
	// refused. 0 with devices means clean parsing; >0 says the
	// firmware published worse than what is shown, and what is shown
	// still passed every validation.
	RejectedCandidates int `json:"rejected_candidates,omitempty"`
}

// RunMemoryDevice is one slot as firmware declared it. nil numeric
// pointers mean "not reported", never zero; SizeMB=0 means the slot
// was reported empty.
type RunMemoryDevice struct {
	Locator       string `json:"locator,omitempty"`       // Device Locator as declared ("DIMM 1")
	BankLocator   string `json:"bank_locator,omitempty"`  // Bank Locator as declared ("P0 CHANNEL A")
	Manufacturer  string `json:"manufacturer,omitempty"`  // as declared; "Unknown" is a real firmware string on many kits
	PartNumber    string `json:"part_number,omitempty"`   // as declared, blanks trimmed
	SizeMB        *int   `json:"size_mb,omitempty"`       // nil = unknown, 0 = empty slot
	SPDMaxMTs     *int   `json:"spd_speed_mts,omitempty"` // Type 17 "Speed" as written
	MaxCapMTs     *int   `json:"max_speed_capability_mts,omitempty"`
	ConfiguredMTs *int   `json:"configured_clock_mts_as_written,omitempty"` // verbatim; may disagree with the two above - recorded, never repaired
}

// BuildProvenance reads the VCS stamp Go embeds automatically when the
// binary was built from a VCS tree (go build stamps vcs.revision,
// vcs.time and vcs.modified for git). A binary built without that
// stamping honestly yields empty fields.
func BuildProvenance() RunMetaFields {
	var m RunMetaFields
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return m
	}
	m.ModuleVer = bi.Main.Version
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			m.BuildRev = s.Value
		case "vcs.time":
			m.BuildTime = s.Value
		case "vcs.modified":
			mod := s.Value == "true"
			m.BuildModified = &mod
		}
	}
	return m
}

// BurstAccounting is one P08 session burst as the WORKER saw it:
// when it read TRIGGER, when the burst actually started inside the
// worker, when it closed, and what that burst verified. The times
// are worker-local observations (RFC3339Nano); the parent's
// commanded timestamps live in the parent's own records - the two
// views are never merged into one "transition time", because the
// plan demands the commanded transition and the observation stay
// distinguishable. Complete=false means the burst was aborted
// (IDLE/STOP) or self-completed without a VERIFY block close: its
// partial counts are real, but no verified block was closed.
type BurstAccounting struct {
	Burst       string  `json:"burst"`
	Seq         uint64  `json:"seq"`
	TriggeredAt string  `json:"triggered_at"`
	StartedAt   string  `json:"started_at"`
	ClosedAt    string  `json:"closed_at"`
	ExecMS      float64 `json:"exec_ms"` // worker-local start->close
	Checks      uint64  `json:"checks"`  // verified checks this burst added
	Bytes       uint64  `json:"bytes"`   // engine bytes verified this burst (0 = not a ram kernel)
	Complete    bool    `json:"complete"`
}

// RamFinding is one observed mismatch reported by a ram-* kernel, recorded
// with everything needed to reproduce it. The offset is LOGICAL
// (within the region): nothing here claims a physical or DIMM
// address - the tool cannot see memory wiring, and presenting a
// logical offset as a hardware location would be fabrication.
type RamFinding struct {
	Algorithm  string `json:"algorithm"`   // kernel identity, e.g. "ram-constant-v1"
	Seed       uint64 `json:"seed"`        // seed the expected data was derived from
	Pass       uint64 `json:"pass"`        // 1-based pass number in which the mismatch was seen
	Region     string `json:"region"`      // grant identity: worker/region/role/global offset
	Offset     uint64 `json:"offset"`      // logical offset within the region, never a physical address
	Expected   uint64 `json:"expected"`    // the word the kernel expected
	Actual     uint64 `json:"actual"`      // the word it read back
	Xor        uint64 `json:"xor"`         // Expected^Actual: which bits differ
	LogicalCPU int    `json:"logical_cpu"` // the pinned slot's logical CPU (pin verified by readback)
	// PhysicalCPU is the physical core only if the OS told us the
	// mapping with confidence; this platform's API does not, so it
	// stays nil = unknown rather than guessing from the logical number.
	PhysicalCPU *int   `json:"physical_cpu,omitempty"`
	Phase       string `json:"phase"` // which pass stage: fill/verify-fwd/verify-rev/final...
	When        string `json:"when"`  // RFC3339Nano stamp taken by the reporter at report time
}

// RamEvidence is the ram-* verdict attached to a core_done record:
// the detailed findings are capped (first MaxRamFindings kept), but
// the total observed count is never capped, and dropping details is
// flagged instead of hidden. A ram run that observed nothing still
// carries evidence with Total 0: "no mismatches seen" is a result,
// while absence of the field means "no ram kernel ran here"
// (unknown), never "clean".
type RamEvidence struct {
	Findings  []RamFinding `json:"findings,omitempty"`
	Total     uint64       `json:"total"`
	Truncated bool         `json:"truncated,omitempty"` // Total > len(Findings): details were dropped, the count was not
}

// MaxRamFindings bounds how many detailed mismatches one slot
// records: a wrong-seed run can mismatch every word of a gigabyte,
// and a journal full of duplicates buries the story without adding
// evidence. The total counter and the truncation flag keep the
// bound honest.
const MaxRamFindings = 16

// ParkingObservation is the journaled form of one Q04 parking
// evidence fold for one session slot: what source answered, at which
// cadence, how many samples landed inside the session's MEASURED
// idle windows, how many of those showed the kernel's Parked flag,
// and when. The freshness travels with the counts (§8.3: source,
// frequency and gaps are preserved, never smoothed):
// UnsampledWindows keeps a coverage hole visible even when the
// status reads observed. Durations are integer nanoseconds (float ms
// would invent rounding drift - the ScheduleWindow rule);
// FirstParked/LastParked are RFC3339Nano UTC stamps, empty ONLY when
// no parked sample landed in a measured idle window. Status is the
// transition package's vocabulary (observed|unconfirmed|unavailable)
// journaled as text, so a reader never needs the code to know what
// was and was not proved.
type ParkingObservation struct {
	Source           string `json:"source"`
	IntervalNS       int64  `json:"interval_ns"`
	Status           string `json:"status"`
	SamplesInIdle    int    `json:"samples_in_idle"`
	ParkedInIdle     int    `json:"parked_in_idle"`
	IdleWindows      int    `json:"idle_windows"`
	UnsampledWindows int    `json:"unsampled_windows"`
	FirstParked      string `json:"first_parked,omitempty"`
	LastParked       string `json:"last_parked,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// Record is a single journal line. All record types share one struct;
// fields not used by a type are omitted from the JSON.
type Record struct {
	Schema     int      `json:"schema,omitempty"` // writer format version; 0 = legacy record
	Type       string   `json:"type"`
	RunID      string   `json:"run_id"`
	Test       string   `json:"test,omitempty"`
	Phase      string   `json:"phase,omitempty"` // scenario phase label
	Cores      []int    `json:"cores,omitempty"`
	Workers    int      `json:"workers,omitempty"`
	Duration   string   `json:"duration,omitempty"`
	PerProcess bool     `json:"per_process,omitempty"` // core numbers are logical, not slots
	Core       *int     `json:"core,omitempty"`
	Workload   string   `json:"workload,omitempty"`
	PID        int      `json:"pid,omitempty"`
	Iters      uint64   `json:"iters,omitempty"`
	WakeMS     float64  `json:"wake_ms,omitempty"`   // wake records: measured wake latency
	ClockMHz   float64  `json:"clock_mhz,omitempty"` // telemetry: observed cycle rate (0 = unknown)
	IdlePct    *float64 `json:"idle_pct,omitempty"`  // telemetry: idle share of the window (nil = unknown, 0 is a real value)
	WindowMS   int64    `json:"window_ms,omitempty"` // telemetry: length of the measured window
	// CoreClocks: source-reported conventional core clocks over the
	// window, grouped by contiguous producer source. Absent = the run
	// had no real-clock source (older journals read as unknown too);
	// it never substitutes for or merges with clock_mhz.
	CoreClocks []CoreClockGroup `json:"core_clocks,omitempty"`
	LoadNS     *int64           `json:"load_ns,omitempty"` // core_done: measured load duration, including an explicit zero
	Checks     *uint64          `json:"checks_verified,omitempty"`
	// core_done: confirmed verified checks (self-checking iterations);
	// absent = unknown from our side (external tools, killed workers).
	// Evidence is what an external adapter's protocol proved for this
	// core ("unknown"/"partial"/"confirmed-complete"/"failed"/
	// "not_performed"); absent = no evidence reported (older journals
	// and built-in workloads) — read as unknown, never as success.
	Evidence string `json:"evidence,omitempty"`
	// KernelVersion identifies which kernel implementation produced
	// this slot's result ("int-affine-v1", an adapter name like
	// "ycruncher-v0.8", ...). Results of different implementations
	// must never be compared or merged as if they were the same work;
	// absent = the implementation is unknown (older journals, legacy
	// external entries) - read as unknown, never as the current one.
	KernelVersion string `json:"kernel_version,omitempty"`
	// Mem carries the memory engine's real numbers on a mem_done
	// record: what was requested, what the OS actually committed,
	// what the first touch touched, what the workload verified, and
	// what the lock policy achieved. Absent (nil) = no memory engine
	// for this slot - the numbers are unknown, never zero.
	Mem *MemAccounting `json:"mem,omitempty"`
	// Ram carries the ram-* family's mismatch evidence on a
	// core_done record: capped details, uncapped total, explicit
	// truncation. Absent (nil) = no ram kernel ran on this slot.
	Ram *RamEvidence `json:"ram,omitempty"`
	// Burst carries one P08 session burst's worker-side accounting
	// on a burst_done record. Absent = not a burst record.
	Burst *BurstAccounting `json:"burst,omitempty"`
	// Schedule carries the prepared P08 burst timeline on a
	// schedule record (written before the session ran). Absent =
	// no session schedule (unknown, never "no bursts planned").
	Schedule *SchedulePlan `json:"schedule,omitempty"`
	// SessionBurst carries the parent's view of one commanded burst
	// on a session_burst record. Absent = not a session-burst record.
	SessionBurst *BurstObservation `json:"session_burst,omitempty"`
	// SessionLease carries the parent's view of one P07e block
	// ownership lease on a session_lease record. Absent = not a
	// lease record. A journal predating P07e simply has none: the
	// handoff story is unknown, never "no handoffs happened".
	SessionLease *LeaseObservation `json:"session_lease,omitempty"`
	// Parking carries the parent's folded parking observation for one
	// session slot on a parking_obs record (Q04): the OS-view answer
	// about whether the parking engine was SEEN holding this CPU
	// during the session's measured idle windows. Absent = the run
	// made no such observation (policy off, or a journal predating
	// Q04): unknown, never "not parked".
	Parking *ParkingObservation `json:"parking_obs,omitempty"`
	// SwitchSchedule carries the prepared pair-switch turn timeline
	// (Q05) on a switch_schedule record, written before the run.
	// Absent = not a switch schedule; journals predating Q05 have
	// none - the planned switch story is unknown, never "no switches
	// were planned".
	SwitchSchedule *SwitchPlan `json:"switch_schedule,omitempty"`
	// SessionSwitch carries the parent's view of one pair-switch
	// group turn on a session_switch record. Absent = not a switch
	// record. Unknown kinds are simply ignored by older readers:
	// additive, like every earlier field.
	SessionSwitch *SwitchObservation `json:"session_switch,omitempty"`
	// Meta carries run-level provenance on a run_meta record: which
	// build wrote this journal, which config and seed were in force,
	// and what machine the run happened on. Absent = the journal is
	// older than provenance - every field reads as unknown, never as
	// "verified empty".
	Meta   *RunMetaFields `json:"meta,omitempty"`
	Result string         `json:"result,omitempty"`
	Msg    string         `json:"msg,omitempty"`
	Exit   int            `json:"exit,omitempty"`
	// JournalErrs counts this writer's failed writes known when run_end
	// was emitted: a journal that lost records must not read as a clean
	// story. Absent (0) in old journals simply means "not measured".
	JournalErrs uint64 `json:"journal_errs,omitempty"`
	// Validation carries the run's structured verification claim on a
	// run_end record (the plan package's Validation vocabulary). Absent
	// = the journal predates this field: the claim reads as unknown,
	// never as "verified" and never as "not performed".
	Validation string `json:"validation,omitempty"`
	// whea records: the event log facts, machine-readable and locale-free.
	// LogRecID is the event log's own EventRecordID: the identity that
	// dedups a retry after a failed persist.
	Provider string `json:"provider,omitempty"`
	EventID  int    `json:"event_id,omitempty"`
	LogRecID int    `json:"log_rec_id,omitempty"`
	Severity string `json:"severity,omitempty"` // corrected, uncorrected, unknown
	Nature   string `json:"nature,omitempty"`   // cpu, cache, interconnect, memory, pcie, unknown
	// EvTime is the event's own SystemTime (when the hardware error
	// happened); TS is when we recorded it. Late-published events keep
	// both so nobody re-dates them into (or out of) a phase.
	EvTime string `json:"ev_time,omitempty"`
	TS     string `json:"ts"`
}

// Writer appends records for one run. It is safe for concurrent use
// (the watchdog and worker goroutines write through the same writer).
// A journal write must never break a run — the run's verdict comes
// first — but a lost record must never stay invisible either: the
// first failure and its count are kept and reported through Errors,
// and run_end carries the count so even the journal itself admits
// being incomplete.
type Writer struct {
	mu        sync.Mutex
	w         io.Writer
	runID     string
	writeErr  error  // first failure (marshal, append, short write or sync)
	writeErrs uint64 // how many failures happened
}

func NewWriter(w io.Writer, runID string) *Writer {
	return &Writer{w: w, runID: runID}
}

var runSequence atomic.Uint64

// NewRunID does not depend on wall-clock resolution or PID uniqueness.
func NewRunID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	return fmt.Sprintf("%d-%d-%d", time.Now().UnixNano(), os.Getpid(), runSequence.Add(1))
}

// OpenFile opens the journal for appending.
// OpenFile opens the journal for appending. A torn tail (a writer that
// died mid-append) must not glue onto our first record, so if the file
// exists and does not end with a newline we delimit it first: the
// damaged line stays damaged and countable, our records stay parseable.
func OpenFile(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 && !tailEndsWithNewline(path) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte("\n")); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// tailEndsWithNewline peeks at the last byte of an existing file. When
// the peek itself fails we proceed without delimiting (the journal is
// already in an unusual state; failing the whole run over a stat race
// would be worse, and the decoder will flag whatever comes of it).
func tailEndsWithNewline(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	if _, err := f.Seek(-1, io.SeekEnd); err != nil {
		return true
	}
	b := make([]byte, 1)
	if n, _ := f.Read(b); n != 1 {
		return true
	}
	return b[0] == '\n'
}

// Path derives the journal file for a run log, so a run's machine
// record always sits next to its human-readable log.
func Path(logFile string) string {
	return logFile + ".journal.jsonl"
}

// OpenRun opens the crash journal for a run. A journal problem must
// never break the run itself, so on error it warns and returns nil; all
// Writer methods are nil-safe no-ops.
func OpenRun(logFile string) *Writer {
	f, err := OpenFile(Path(logFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: journal: %v\n", err)
		return nil
	}
	return NewWriter(f, NewRunID())
}

// FragmentsDir is where one run's per-worker journal fragments live:
// <journal>.workers/<run-id>/. The parent registers the run there and
// each child writes only its own file — no two writers ever share a
// file, so no OS-level append-atomicity assumption is being made.
func FragmentsDir(mainPath, runID string) string {
	return filepath.Join(mainPath+".workers", runID)
}

// FragmentPath is the one journal file a single worker owns.
func FragmentPath(mainPath, runID string, core int) string {
	return filepath.Join(FragmentsDir(mainPath, runID), fmt.Sprintf("core%d.jsonl", core))
}

// FragmentFiles lists the fragment files present for a run, in stable
// (sorted) order so merges are reproducible.
func FragmentFiles(mainPath, runID string) []string {
	files, _ := filepath.Glob(filepath.Join(FragmentsDir(mainPath, runID), "*.jsonl"))
	sort.Strings(files)
	return files
}

// ReadAll decodes the main journal plus every worker fragment found
// under its .workers tree, so a per-process run's story is complete
// even though no file ever had two writers. Records come back in file
// order (main first, fragments in stable sorted order); Issues
// aggregate across all files with per-file provenance. A fragment that
// cannot be opened is counted in Unreadable, never treated as if the
// core said nothing at all.
func ReadAll(mainPath string) ([]Record, *Issues) {
	var recs []Record
	iss := &Issues{}
	mainErr := ""
	f, err := os.Open(mainPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, &Issues{ReadErr: err.Error(), Unreadable: 1}
		}
		// The main file is absent: the fragment tree may still hold a
		// complete story (a run whose parent died before opening the
		// main journal). Keep reading; only an empty result makes the
		// absence a hard read error.
		mainErr = err.Error()
	} else {
		recs, iss = ParseAll(f, mainPath)
		f.Close()
	}
	files, _ := filepath.Glob(mainPath + ".workers" + string(filepath.Separator) + "*" + string(filepath.Separator) + "*.jsonl")
	sort.Strings(files)
	for _, file := range files {
		af, err := os.Open(file)
		if err != nil {
			iss.Unreadable++
			iss.add(file, 0, "fragment cannot be opened: "+err.Error())
			continue
		}
		frag, fiss := ParseAll(af, file)
		af.Close()
		recs = append(recs, frag...)
		iss.Malformed += fiss.Malformed
		iss.MissingField += fiss.MissingField
		iss.Unterminated = iss.Unterminated || fiss.Unterminated
		if fiss.ReadErr != "" && iss.ReadErr == "" {
			iss.ReadErr = fiss.ReadErr
		}
		for _, pi := range fiss.Found {
			iss.add(pi.Source, pi.Line, pi.Reason)
		}
	}
	if mainErr != "" && len(recs) == 0 {
		iss.ReadErr = mainErr // neither the main file nor any fragment exists
	}
	return recs, iss
}

func (j *Writer) write(rec Record, durable bool) error {
	if j == nil {
		return fmt.Errorf("journal unavailable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	rec.RunID = j.runID
	rec.Schema = SchemaVersion
	rec.TS = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(rec)
	if err != nil {
		j.noteErrorLocked("marshal", err)
		return fmt.Errorf("journal marshal: %w", err)
	}
	line := append(data, '\n')
	n, werr := j.w.Write(line)
	if werr != nil {
		j.noteErrorLocked("write", werr)
		return fmt.Errorf("journal write: %w", werr)
	}
	if n != len(line) {
		// A torn append is a lost record even when no error is reported.
		serr := fmt.Errorf("short write: %d of %d bytes", n, len(line))
		j.noteErrorLocked("write", serr)
		return serr
	}
	// Beats are frequent and disposable; everything that changes the story
	// of a run is flushed to disk immediately.
	if durable {
		if syncer, ok := j.w.(interface{ Sync() error }); ok {
			if serr := syncer.Sync(); serr != nil {
				j.noteErrorLocked("sync", serr)
				return fmt.Errorf("journal sync: %w", serr)
			}
		}
	}
	return nil
}

// noteErrorLocked keeps the first failure and counts the rest. Callers
// must hold j.mu.
func (j *Writer) noteErrorLocked(op string, err error) {
	j.writeErrs++
	if j.writeErr == nil {
		j.writeErr = fmt.Errorf("journal %s: %w", op, err)
	}
}

// Errors reports the first journal write failure and the total count
// (nil, 0 when the journal is intact). Engines must surface it as a
// finding and a visible note: an incomplete journal is never silently
// a complete story. CO transactions (P10) must refuse to touch the
// hardware while this is non-nil.
func (j *Writer) Errors() (error, uint64) {
	if j == nil {
		return nil, 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.writeErr, j.writeErrs
}

func (j *Writer) RunStart(test, phase string, cores []int, workers int, duration string, perProcess bool) {
	j.write(Record{
		Type:       RunStart,
		Test:       test,
		Phase:      phase,
		Cores:      cores,
		Workers:    workers,
		Duration:   duration,
		PerProcess: perProcess,
	}, true)
}

// RunMeta journals run-level provenance right after run_start: the
// build that wrote the journal, the config hash, the configured seed
// and the machine identity. Written durably (sync) because a
// provenance record that survives a crash is the whole point.
func (j *Writer) RunMeta(meta RunMetaFields) {
	j.write(Record{Type: RunMeta, Meta: &meta}, true)
}

// Sources registers, durably and before any child starts, the worker
// fragments a per-process run expects to find. Recovery can then tell
// "this core never told its story" from "this core finished cleanly":
// a missing fragment is a hole, not a silence.
func (j *Writer) Sources(cores []int) {
	j.write(Record{Type: Sources, Cores: cores}, true)
}

func (j *Writer) CoreStart(core int, workload string) {
	c := core
	j.write(Record{Type: CoreStart, Core: &c, Workload: workload, PID: os.Getpid()}, true)
}

func (j *Writer) CoreBeat(core int, iters uint64) {
	c := core
	j.write(Record{Type: CoreBeat, Core: &c, Iters: iters}, false)
}

func (j *Writer) CoreDone(core int, result, msg string) {
	c := core
	j.write(Record{Type: CoreDone, Core: &c, Result: result, Msg: msg}, true)
}

// CoreDoneElapsed records measured time rather than crediting the planned
// duration after an interruption or an early tool exit.
func (j *Writer) CoreDoneElapsed(core int, result, msg string, elapsed time.Duration) {
	j.CoreDoneChecks(core, result, msg, elapsed, nil, "", "")
}

// CoreDoneChecks additionally carries the confirmed verified check count
// (nil = unknown from our side) and the external protocol evidence the
// adapter proved for this slot ("" = none reported: unknown, never
// success). It is what lets the per-process engine report the same
// validation as the threaded one: the child counted its own self-checking
// iterations and recorded its adapter's evidence, and the parent
// recovers both from the journal instead of guessing. kernelVersion
// names the implementation that did the work ("" = unknown): it keeps
// results of different kernels from being merged into one story.
func (j *Writer) CoreDoneChecks(core int, result, msg string, elapsed time.Duration, checks *uint64, evidence, kernelVersion string) {
	j.CoreDoneRam(core, result, msg, elapsed, checks, evidence, kernelVersion, nil)
}

// CoreDoneRam additionally attaches the ram-* mismatch evidence
// (nil = no ram kernel ran on this slot, unknown, never "clean"):
// capped details, uncapped total count, explicit truncation flag.
func (j *Writer) CoreDoneRam(core int, result, msg string, elapsed time.Duration, checks *uint64, evidence, kernelVersion string, ram *RamEvidence) {
	c, ns := core, max(int64(0), int64(elapsed))
	j.write(Record{Type: CoreDone, Core: &c, Result: result, Msg: msg, LoadNS: &ns, Checks: checks, Evidence: evidence, KernelVersion: kernelVersion, Ram: ram}, true)
}

// MemDone writes the memory engine's accounting for one slot. It is
// emitted only when an engine really ran for that slot: a slot
// without one, or one that died before writing it, leaves the
// numbers unknown rather than zero.
func (j *Writer) MemDone(core int, acct MemAccounting) {
	c := core
	j.write(Record{Type: MemDone, Core: &c, Mem: &acct}, true)
}

// BurstDone writes one P08 session burst as the worker saw it. The
// session's allocation is counted exactly once (mem_done); bursts are
// the work breakdown, never a second allocation story. An aborted
// burst still gets a record (Complete false): its partial counts are
// real observations, and silence would erase them.
func (j *Writer) BurstDone(core int, acct BurstAccounting) {
	c := core
	j.write(Record{Type: BurstDone, Core: &c, Burst: &acct}, true)
}

// SchedulePlan writes the prepared P08 burst timeline BEFORE the
// session runs (the parent owns it): the concrete windows are the
// reproduction artifact, so a reader never needs the seed or any
// PRNG version to know what was commanded.
func (j *Writer) SchedulePlan(plan SchedulePlan) {
	j.write(Record{Type: Schedule, Schedule: &plan}, true)
}

// SessionBurst writes the parent's view of one commanded burst:
// when it was actually sent vs when the acks arrived. It rides the
// PARENT journal (the child's burst_done is the worker view); the
// two views of the same burst are separate records, never merged -
// the plan demands commanded and observed stay distinguishable.
func (j *Writer) SessionBurst(core int, obs BurstObservation) {
	c := core
	j.write(Record{Type: SessionBurst, Core: &c, SessionBurst: &obs}, true)
}

// SessionLease writes the parent's view of one P07e block-ownership
// lease (the worker's own view rides its core_done/mem_done as
// always; the arbiter's ordering guarantee is evidenced step by step
// here). Additive record type: readers that do not know it ignore it,
// exactly like every earlier additive field.
func (j *Writer) SessionLease(core int, obs LeaseObservation) {
	c := core
	j.write(Record{Type: SessionLease, Core: &c, SessionLease: &obs}, true)
}

// ParkingObs writes the parent's folded parking observation for one
// session slot (Q04): the observation half of the story, written only
// when a parking policy was actually in force. It never rides on the
// commanded schedule or the acks - commanded intent and observed OS
// state stay separate records, and a run that observed nothing writes
// no such record at all (absence = unknown, never "not parked").
// Additive record type: readers that do not know it ignore it, exactly
// like every earlier additive field.
func (j *Writer) ParkingObs(core int, ev ParkingObservation) {
	c := core
	j.write(Record{Type: ParkingObs, Core: &c, Parking: &ev}, true)
}

// SwitchPlan writes the prepared pair-switch turn timeline BEFORE
// anything spawns (Q05): the concrete turns are the reproduction
// artifact, so a reader never needs the seed or any PRNG version to
// know what was commanded (same rule as SchedulePlan).
func (j *Writer) SwitchPlan(plan SwitchPlan) {
	j.write(Record{Type: SwitchSchedule, SwitchSchedule: &plan}, true)
}

// SessionSwitch writes the parent's view of one pair-switch turn
// (when the group was actually commanded vs when its acks and idle
// barrier came back; upper bounds incl IPC). The per-participant
// views ride their own session_burst records: commanded turn and
// observed members stay separate evidence, never merged. Additive
// record type: readers that do not know it ignore it, exactly like
// every earlier additive field.
func (j *Writer) SessionSwitch(obs SwitchObservation) {
	j.write(Record{Type: SessionSwitch, SessionSwitch: &obs}, true)
}

// Wake records how long a frozen core took to produce its first confirmed
// work after being thawed — the functional proxy for how deep the platform
// let it idle (deeper C-states cost a longer way back).
func (j *Writer) Wake(core int, ms float64) {
	c := core
	j.write(Record{Type: Wake, Core: &c, WakeMS: ms}, true)
}

// Telemetry records what the core's load window looked like: the observed
// cycle rate of a pinned worker thread (effective clock) and the logical
// CPU's idle share over the same window. Unknown values stay out of the
// record rather than posing as zeros. clocks carries the parent's
// source-sampled real core clocks (nil when no producer was available);
// the observed cycle rate and the source clocks measure different
// quantities and are never merged.
func (j *Writer) Telemetry(core int, clockMHz float64, idlePct *float64, windowMS int64, clocks []CoreClockGroup) {
	c := core
	j.write(Record{Type: Telemetry, Core: &c, ClockMHz: clockMHz, IdlePct: idlePct, WindowMS: windowMS, CoreClocks: clocks}, true)
}

// RunEnd closes the run's story and carries the writer's own failure
// count known at that moment (the count before this very line: a
// journal cannot record its own last failed write — the engine's
// stderr note and the run Result are the channels that stay honest
// even when the file itself is broken).
// Whea durably records one observed hardware error record. It returns
// the write error so the WHEA monitor can keep its cursor and retry:
// the cursor must never advance past a record the journal could not
// keep. A retried record is deduplicated during recovery by LogRecID.
func (j *Writer) Whea(provider string, eventID, logRecID int, severity, nature, msg string, when time.Time) error {
	evTime := ""
	if !when.IsZero() {
		evTime = when.UTC().Format(time.RFC3339Nano)
	}
	return j.write(Record{Type: Whea, Provider: provider, EventID: eventID, LogRecID: logRecID,
		Severity: severity, Nature: nature, Msg: msg, EvTime: evTime}, true)
}

// WheaState records an observation-state transition: how much the WHEA
// records around it can be trusted (available / degraded / unavailable)
// and why. Without this line, "0 WHEA events" is ambiguous between
// "hardware was clean" and "we never looked".
func (j *Writer) WheaState(state, reason string) error {
	return j.write(Record{Type: WheaState, Result: state, Msg: reason}, true)
}

func (j *Writer) RunEnd(exit int) {
	j.RunEndValidation(exit, "")
}

// RunEndValidation closes the run carrying the structured verification
// claim (the plan's Validation vocabulary; "" = unknown, e.g. a caller
// that has no claim to make). The exit code alone never says whether
// the declared checking actually ran - the validation field does, and
// readers of older journals (no field) must see unknown, not a guess.
func (j *Writer) RunEndValidation(exit int, validation string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	errs := j.writeErrs
	j.mu.Unlock()
	j.write(Record{Type: RunEnd, Exit: exit, JournalErrs: errs, Validation: validation}, true)
}

// Close closes the underlying file if it is closeable.
func (j *Writer) Close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if c, ok := j.w.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// ParseIssue is one line the journal could not tell us, with enough
// provenance to find it again.
type ParseIssue struct {
	Source string `json:"source"` // file the line came from ("" = the stream we were given)
	Line   int    `json:"line"`   // 1-based line number within that source
	Reason string `json:"reason"`
}

// Issues describes everything a decode had to drop or flag. A journal
// with issues is never a clean story: the count travels with the
// records so reports can say what is missing instead of pretending.
type Issues struct {
	Found        []ParseIssue `json:"found,omitempty"` // bounded detail (first few)
	Malformed    int          `json:"malformed"`       // lines that failed to parse
	MissingField int          `json:"missing_field"`   // parsed but no type/run id
	Unterminated bool         `json:"unterminated"`    // stream ended mid-line (power loss)
	Unreadable   int          `json:"unreadable"`      // files (e.g. fragments) that could not be opened
	ReadErr      string       `json:"read_err,omitempty"`
}

// Empty reports whether the decode was fully clean.
func (i *Issues) Empty() bool {
	return i == nil || (i.Malformed == 0 && i.MissingField == 0 && i.Unreadable == 0 && !i.Unterminated && i.ReadErr == "")
}

// issueDetail caps how many individual issues we quote (a corrupted
// middle can produce thousands; the count is what matters).
const issueDetail = 5

func (i *Issues) add(source string, line int, reason string) {
	if len(i.Found) < issueDetail {
		i.Found = append(i.Found, ParseIssue{Source: source, Line: line, Reason: reason})
	}
}

// lastByteReader remembers the final byte it handed out, so the decoder
// can tell "file ends with a newline" from "file ends mid-line" without
// re-reading or buffering the whole journal.
type lastByteReader struct {
	r    io.Reader
	last byte
	seen bool
}

func (l *lastByteReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last = p[n-1]
		l.seen = true
	}
	return n, err
}

// ParseAll decodes journal lines and reports everything it had to drop
// or flag: malformed JSON (a torn append, a corrupt middle), records
// without the required type/run id, an unterminated final line, and
// stream read errors. Valid records still come back; the Issues say
// what the story is missing. source names the file for provenance
// ("" for anonymous streams).
func ParseAll(r io.Reader, source string) ([]Record, *Issues) {
	lb := &lastByteReader{r: r}
	var out []Record
	iss := &Issues{}
	sc := bufio.NewScanner(lb)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			iss.Malformed++
			iss.add(source, line, "malformed JSON: "+err.Error())
			continue
		}
		if rec.Type == "" || rec.RunID == "" {
			iss.MissingField++
			iss.add(source, line, "record without type or run id")
			continue
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		iss.ReadErr = err.Error() // e.g. a line too long to be a real record
		iss.add(source, line+1, "read error: "+err.Error())
	}
	// A stream that ends mid-line means the writer died between the
	// bytes: the last record is untrustworthy even if it parsed. After
	// a read error the stream position is unknown, so the tail claim
	// would be a guess — the ReadErr itself is the loud fact there.
	if lb.seen && lb.last != '\n' && iss.ReadErr == "" {
		iss.Unterminated = true
		iss.add(source, line, "journal ends mid-line (writer died mid-append)")
	}
	return out, iss
}

// Parse decodes journal lines, ignoring the issue report. Prefer
// ParseAll anywhere the reader of the story must admit its holes;
// this wrapper stays for callers that genuinely only want the records.
func Parse(r io.Reader) []Record {
	recs, _ := ParseAll(r, "")
	return recs
}

// CoreClockGroup is one contiguous stretch of real-clock samples from a
// single producer source within a load window. Min/Max/Avg are over the
// group's own samples (Avg is the plain sample average, N counts them);
// FirstMS/LastMS place the first and last accepted sample relative to
// the window start so partial coverage stays visible. AgeBasis says
// what MaxAgeMS measures: "producer" is the producer's own poll
// timestamp (HWiNFO), "acquired" is only our query time (WMI has no
// producer timestamp, so its age never proves the value was fresh).
type CoreClockGroup struct {
	Source   string  `json:"source"` // "hwinfo" | "wmi"
	MinMHz   float64 `json:"min_mhz"`
	AvgMHz   float64 `json:"avg_mhz"`
	MaxMHz   float64 `json:"max_mhz"`
	N        int     `json:"n"`
	FirstMS  int64   `json:"first_ms"`
	LastMS   int64   `json:"last_ms"`
	AgeBasis string  `json:"age_basis"` // "producer" | "acquired"
	MaxAgeMS int64   `json:"max_age_ms,omitempty"`
}

// TelemetrySample is one load window's observation for a core.
type TelemetrySample struct {
	ClockMHz   float64
	IdlePct    *float64 // nil when the platform could not report idle
	WindowMS   int64
	CoreClocks []CoreClockGroup // nil = no real-clock source (older journals read the same)
}

// CoreOutcome is the recovered fate of one core within a run.
type CoreOutcome struct {
	Core      int
	Workload  string
	Status    string // OK, Fault, Crashed, Interrupted, Skipped or SetupError
	Msg       string
	LastBeat  time.Time // last core_beat, zero if none
	LastIters uint64
	WakeMS    []float64 // measured wake latencies after rotation thaws
	Telemetry []TelemetrySample
	LoadTime  *time.Duration // nil for older journals without measured load time
	Checks    *uint64        // core_done's confirmed check count; nil = unknown
	Evidence  string         // core_done's external protocol evidence ("" = none recorded: unknown)
	// KernelVersion names the implementation that produced this
	// outcome ("" = unknown: older journals, legacy entries). Results
	// of different kernels are never silently comparable.
	KernelVersion string
	// Mem is the memory engine's accounting for this slot, from its
	// mem_done record; nil = no engine ran (the numbers are unknown,
	// not zero).
	Mem *MemAccounting
	// Ram is the ram-* mismatch evidence attached to this slot's
	// core_done; nil = no ram kernel ran here (unknown - NOT "no
	// mismatches"). A present record with Total 0 means a ram
	// kernel ran and saw nothing wrong.
	Ram *RamEvidence
	// Bursts are the P08 session's burst records for this slot, in
	// journal order; nil/empty = no burst records (an older journal
	// or a one-shot run - unknown, never "zero bursts").
	Bursts []BurstAccounting
	// SessionObs is the PARENT's view of this slot's commanded
	// bursts (session_burst records), in journal order; nil = the
	// parent recorded none (unknown - a child may still have
	// burst_done records: the views are independent).
	SessionObs []BurstObservation
	// LeaseObs is the PARENT's view of this slot's P07e block
	// ownership leases (session_lease records), in journal order;
	// nil = none recorded (a journal predating P07e: unknown, never
	// "no handoffs happened").
	LeaseObs []LeaseObservation
	// Parking is this slot's folded parking evidence (parking_obs
	// records, Q04): at most one per run, the OS-view answer about
	// the measured idle windows. nil = no observation was made
	// (policy off or a pre-Q04 journal: unknown, never "not parked").
	Parking []ParkingObservation
}

// RunReport is the recovered story of one run.
type RunReport struct {
	RunID      string
	Test       string
	Phase      string        // scenario phase label, empty for single runs
	Selected   []int         // logical CPUs chosen for the run, in slot order
	PerProcess bool          // core numbers below are logical CPUs, not slots
	Duration   time.Duration // configured load time of this run (0 = unknown)
	Ended      bool          // a run_end record was seen
	Exit       int
	// Validation is the run's structured verification claim from its
	// run_end record ("" = never recorded: unknown, never "verified"
	// and never "not performed"). The exit code alone never tells
	// whether the declared checking actually ran.
	Validation      string
	Schema          int           // journal format at run_start (0 = legacy, new fields read as unknown)
	JournalErrs     uint64        // write failures the writer admitted at run_end (0 = intact or unmeasured)
	Expected        []int         // per-process runs: cores whose fragments were registered (nil = legacy/threaded)
	Missing         []int         // registered cores whose fragments produced no records at all
	Whea            []WheaOutcome // observed hardware error records, deduped by event-log id
	WheaState       string        // last whea_state record: available/degraded/unavailable ("" = never watched)
	WheaStateReason string        // why the observation was degraded/unavailable
	Cores           []CoreOutcome // sorted by core number
	// Schedule is the prepared P08 session timeline for this run
	// (from its schedule record, written before the session ran);
	// nil = no session schedule (unknown, never "no bursts").
	Schedule *SchedulePlan
	// SwitchSchedule is the prepared Q05 pair-switch timeline for
	// this run (from its switch_schedule record); nil = no switch was
	// ever planned for this run (unknown, never "no switches").
	SwitchSchedule *SwitchPlan
	// Switches are the parent's switch-turn records (session_switch,
	// Q05) for this run, in journal order. Empty = no switch turns
	// were observed by this reader - for a pre-Q05 journal that is
	// unknown, never "no switches happened".
	Switches []SwitchObservation
	// Meta is the run's provenance (build/config/seed/machine) from
	// its run_meta record; nil = the journal predates provenance -
	// every provenance field reads as unknown.
	Meta *RunMetaFields
}

// WheaOutcome is one observed WHEA record. Severity/Nature use the
// whea package's vocabulary ("corrected"/"uncorrected"/"unknown",
// "cpu"/"cache"/"interconnect"/"memory"/"pcie"/"unknown"); journal
// stays dependency-light and compares them as strings.
type WheaOutcome struct {
	Time     time.Time // the event's own SystemTime (zero = unknown)
	Provider string
	EventID  int
	LogRecID int // event log RecordID: the dedup identity across persist retries
	Severity string
	Nature   string
	Msg      string
}

// runState accumulates records for one run during recovery.
type runState struct {
	rep      RunReport
	cores    map[int]*CoreOutcome
	wheaSeen map[string]bool // event-log RecordIDs already counted (persist retries)
}

// core returns (creating if needed) the outcome entry for the record's core.
func (st *runState) core(rec Record) *CoreOutcome {
	if rec.Core == nil {
		return nil
	}
	c, ok := st.cores[*rec.Core]
	if !ok {
		c = &CoreOutcome{Core: *rec.Core}
		st.cores[*rec.Core] = c
	}
	return c
}

func outcomeStatus(rep RunReport, core CoreOutcome) string {
	if core.Workload == "idle" {
		return Skipped
	}
	// Older runners marked fault-free peers OK even on aborted runs. With
	// no measured completion record we cannot claim that they finished.
	if core.Status == OK && core.LoadTime == nil && rep.Ended && rep.Exit != 0 {
		return Interrupted
	}
	return core.Status
}

// PrintReport renders recovered runs for the 'journal' command.
// orUnknown renders an empty provenance string as "unknown": absence
// of an observation is never printed as a blank or a zero.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// FormatProvenance renders one run's provenance line (build, config
// hash, seed, machine) for any report that shows runs. A nil meta -
// every journal older than run_meta - reads as unknown, never as a
// fabricated identity. The BIOS part stays "unknown (not collected)":
// this tool has no BIOS collector yet, and saying so is the honest
// answer.
func FormatProvenance(m *RunMetaFields) string {
	if m == nil {
		return "provenance: unknown (this journal predates run_meta records)"
	}
	mod := "unknown"
	if m.BuildModified != nil {
		mod = map[bool]string{true: "yes", false: "no"}[*m.BuildModified]
	}
	seed := "unknown"
	if m.Seed != nil {
		seed = fmt.Sprint(*m.Seed)
	}
	return fmt.Sprintf("provenance: build %s rev %s (modified: %s) · config sha256 %s · plan sha256 %s · topology %s · seed %s · cpu %q · os %q · bios unknown (not collected) · %s",
		orUnknown(m.ModuleVer), orUnknown(m.BuildRev), mod, orUnknown(m.ConfigHash),
		orUnknown(m.PlanHash), orUnknown(m.TopologyFP), seed,
		orUnknown(m.CPUName), orUnknown(m.OSVersion),
		FormatMemoryObservations(m.Memory))
}

// FormatMemoryDevice renders one memory slot's observations. Unknown
// numeric fields print as "unknown", never as 0: an unobserved value
// must not read like a measured zero. The three speed fields are named
// the way SMBIOS names them at those offsets and are never collapsed
// into one "running speed": the collector refused to interpret them
// and this renderer must not interpret them either.
func FormatMemoryDevice(d RunMemoryDevice) string {
	size := "size unknown"
	if d.SizeMB != nil {
		if *d.SizeMB == 0 {
			size = "reported empty"
		} else {
			size = fmt.Sprintf("%d MB", *d.SizeMB)
		}
	}
	mts := func(p *int, name string) string {
		if p == nil {
			return name + " unknown"
		}
		return name + " " + fmt.Sprint(*p)
	}
	part := d.PartNumber
	if part == "" {
		part = "part unknown"
	}
	return fmt.Sprintf("%s part %q (%s / %s / %s MT/s)", size, part,
		mts(d.SPDMaxMTs, "spd"), mts(d.MaxCapMTs, "max-cap"),
		mts(d.ConfiguredMTs, "configured-as-written"))
}

// FormatMemoryObservations renders the recorded memory observations as
// one provenance segment ("memory · SMBIOS 3.4 · ..."). It reports
// what firmware wrote and draws no conclusion: real firmware
// contradicts itself (this dev board writes SPD speed 4800, max
// capability 5600 and a voltage-like 1100 in the slot the spec calls
// "configured memory clock speed"), and a report that quietly picked
// one of those numbers would be inventing a hardware fact.
//
// Slots whose observations match fold behind a count, because the
// numbers - not the slot names - are what a reader compares across
// journals; the per-slot detail (locator, bank, manufacturer) stays in
// the journal record itself and in 'cpu-stability version'.
//
// nil renders as a named unknown, never as silence: an absent memory
// record must not read as "this machine published nothing about its
// memory" when the real reason is that this build or run did not look.
// A collector that looked and was refused carries UnknownReason, and
// that reason is printed verbatim.
func FormatMemoryObservations(m *RunMemoryFields) string {
	if m == nil {
		return "memory unknown (not observed by this run)"
	}
	if m.UnknownReason != "" {
		return "memory unknown (" + m.UnknownReason + ")"
	}
	parts := []string{"memory"}
	if m.SMBIOSVersion != "" {
		parts = append(parts, "SMBIOS "+m.SMBIOSVersion)
	} else {
		parts = append(parts, "SMBIOS version unknown")
	}
	if m.ArrayMaxCapacityMB != nil {
		parts = append(parts, fmt.Sprintf("array max %d MB", *m.ArrayMaxCapacityMB))
	}
	if len(m.Devices) == 0 {
		parts = append(parts, "no memory device records published")
	} else {
		seen := map[string]int{}
		order := []string{}
		for _, d := range m.Devices {
			o := FormatMemoryDevice(d)
			if _, ok := seen[o]; !ok {
				order = append(order, o)
			}
			seen[o]++
		}
		groups := make([]string, 0, len(order))
		for _, o := range order {
			if n := seen[o]; n > 1 {
				groups = append(groups, fmt.Sprintf("%d× %s", n, o))
			} else {
				groups = append(groups, o)
			}
		}
		parts = append(parts, fmt.Sprintf("%d slot record(s): %s",
			len(m.Devices), strings.Join(groups, "; ")))
	}
	if m.RejectedCandidates > 0 {
		parts = append(parts, fmt.Sprintf("%d malformed record(s) refused", m.RejectedCandidates))
	}
	return strings.Join(parts, " · ")
}

func PrintReport(w io.Writer, reports []RunReport) {
	for _, rep := range reports {
		fmt.Fprintf(w, "run %s test=%s", rep.RunID, rep.Test)
		if rep.Phase != "" {
			fmt.Fprintf(w, " phase=%s", rep.Phase)
		}
		if rep.Ended {
			// The verification claim rides beside the exit code: exit
			// 0 with validation=unknown or partial is not a pass, and
			// a journal older than validation records says unknown.
			fmt.Fprintf(w, " exit=%d validation=%s\n", rep.Exit, orUnknown(rep.Validation))
		} else {
			fmt.Fprintln(w, " NOT ENDED (process or machine died)")
		}
		if rep.JournalErrs > 0 {
			fmt.Fprintf(w, " JOURNAL INCOMPLETE: %d record(s) failed to write — the story below is missing pieces\n", rep.JournalErrs)
		}
		if rep.Schema == 0 {
			fmt.Fprintln(w, " PRE-VERSIONING RECORDS: this run predates journal schema versioning; newer fields read as unknown, not as zeros")
		}
		if len(rep.Missing) > 0 {
			fmt.Fprintf(w, " MISSING WORKER FRAGMENTS: cores %v were registered but never reported — their story is absent, not silent\n", rep.Missing)
		}
		// Run provenance: which build, which config, which machine.
		// A journal without this record says "provenance unknown" -
		// never a fabricated identity.
		fmt.Fprintln(w, " "+FormatProvenance(rep.Meta))
		// The prepared P08 schedule: what was COMMANDED, journaled
		// before the run. The burst records below are what was
		// OBSERVED - the two must stay distinguishable in the
		// report, and the plan demands the timeline be recoverable
		// without any PRNG version.
		if rep.Schedule != nil {
			s := rep.Schedule
			fmt.Fprintf(w, " prepared schedule: %d windows, base %s", len(s.Windows), time.Duration(s.BaseNS).Round(time.Millisecond))
			if s.JitterNS > 0 {
				if s.Seed != nil {
					fmt.Fprintf(w, " ± %s (seeded jitter, seed %d; the windows above are the reproduction artifact, not the seed)", time.Duration(s.JitterNS).Round(time.Millisecond), *s.Seed)
				} else {
					fmt.Fprintf(w, " ± %s (seed not journaled: unknown)", time.Duration(s.JitterNS).Round(time.Millisecond))
				}
			}
			fmt.Fprintln(w)
		}
		// The prepared Q05 switch timeline: what was COMMANDED,
		// journaled before anything spawned. The session_switch
		// records below are what was OBSERVED.
		if rep.SwitchSchedule != nil {
			s := rep.SwitchSchedule
			order := s.Order
			if order == "" {
				order = "fixed"
			}
			groups := 0
			for _, t := range s.Turns {
				if t.Group+1 > groups {
					groups = t.Group + 1
				}
			}
			fmt.Fprintf(w, " prepared switch: %d turns, %d group(s), order %s, base %s", len(s.Turns), groups, order, time.Duration(s.BaseNS).Round(time.Millisecond))
			if s.JitterNS > 0 {
				if s.Seed != nil {
					fmt.Fprintf(w, " ± %s (seeded jitter, seed %d; the turns above are the reproduction artifact, not the seed)", time.Duration(s.JitterNS).Round(time.Millisecond), *s.Seed)
				} else {
					fmt.Fprintf(w, " ± %s (seed not journaled: unknown)", time.Duration(s.JitterNS).Round(time.Millisecond))
				}
			}
			if len(s.Turns) == 0 {
				fmt.Fprint(w, " (zero turns: the plan promised a switch schedule and delivered none)")
			}
			fmt.Fprintln(w)
		}
		if len(rep.Switches) > 0 {
			done, maxStart := 0, -1.0
			for _, o := range rep.Switches {
				if o.Complete {
					done++
				}
				if o.SwitchToStartMS > maxStart {
					maxStart = o.SwitchToStartMS
				}
			}
			fmt.Fprintf(w, " switch turns observed: %d (complete %d) · max command->last-started %.1fms (upper bound, incl IPC round trip)\n", len(rep.Switches), done, maxStart)
			for _, o := range rep.Switches {
				if !o.Complete {
					fmt.Fprintf(w, "  turn %d (group %d round %d) never closed: %s\n", o.Turn, o.Group, o.Round, orUnknown(o.Error))
				}
			}
		}
		for _, c := range rep.Cores {
			// External protocol evidence rides beside the ending: an
			// "OK" from an external tool means different things when
			// the adapter confirmed the tool's verification versus
			// when it could only watch the process live.
			ev := ""
			if c.Evidence != "" {
				ev = " evidence=" + c.Evidence
			}
			if c.KernelVersion != "" {
				ev += " kernel=" + c.KernelVersion
			}
			// Memory engine accounting: the report shows the ACTUAL
			// sizes the OS committed and the workload verified, not
			// the sizes someone asked for. No mem_done means no
			// engine ran - the numbers stay unknown, and nothing is
			// printed for them rather than inventing zeros.
			if c.Mem != nil {
				ev += fmt.Sprintf(" mem requested=%s allocated=%s touched=%s verified=%s first_touch=%.1fms lock=%s",
					human.Bytes(c.Mem.Requested), human.Bytes(c.Mem.Allocated),
					human.Bytes(c.Mem.Touched), human.Bytes(c.Mem.Verified),
					c.Mem.FirstTouchMS, c.Mem.LockMode)
				if c.Mem.Locked > 0 {
					ev += fmt.Sprintf(" locked=%s", human.Bytes(c.Mem.Locked))
				}
				if c.Mem.LockFailure != "" {
					ev += " LOCK-FAILED: " + c.Mem.LockFailure
				}
				// Unique vs total coverage only when the workload
				// reported pass coverage: absent = unknown, and
				// printing zeros would invent a claim.
				if c.Mem.VerifiedUnique != nil && c.Mem.VerifiedTotal != nil {
					ev += fmt.Sprintf(" verified_unique=%s verified_total=%s",
						human.Bytes(*c.Mem.VerifiedUnique), human.Bytes(*c.Mem.VerifiedTotal))
				}
			}
			// ram-* evidence: the observed mismatch count is never
			// capped, the details are, and the truncation is said
			// out loud instead of hidden.
			if c.Ram != nil {
				ev += fmt.Sprintf(" ram mismatches: %d observed", c.Ram.Total)
				if len(c.Ram.Findings) > 0 {
					ev += fmt.Sprintf(", %d detailed", len(c.Ram.Findings))
				}
				if c.Ram.Truncated {
					ev += " (TRUNCATED: detail cap hit, the count was not)"
				}
			}
			// P08 session bursts: the session's allocation is counted
			// once (mem_done above); bursts are the work breakdown.
			// Bursts closed by VERIFY as verified blocks are counted
			// apart from aborted/incomplete ones - an aborted burst
			// is said out loud, never hidden. The exec total is the
			// worker-local load time; idle time is the parent's
			// observation, not this record's claim.
			if len(c.Bursts) > 0 {
				var complete, aborted int
				var execMS float64
				for _, b := range c.Bursts {
					if b.Complete {
						complete++
					} else {
						aborted++
					}
					execMS += b.ExecMS
				}
				ev += fmt.Sprintf(" bursts: %d (%d verified blocks closed", len(c.Bursts), complete)
				if aborted > 0 {
					ev += fmt.Sprintf(", %d aborted/incomplete", aborted)
				}
				ev += fmt.Sprintf(", worker exec total %.1fms)", execMS)
			}
			// The PARENT's view of the commanded transitions: the
			// intervals include the IPC round trip and are labeled
			// upper bounds (no verified shared monotonic source on
			// this API yet - the plan allows parent round-trip
			// intervals when counted separately, and that is what
			// they are). The schedule deviation is signed and the
			// worst magnitude is shown, never smoothed.
			if len(c.SessionObs) > 0 {
				var done int
				var maxDev, maxTV float64
				for _, o := range c.SessionObs {
					if o.Complete {
						done++
					}
					dev := o.SchedDevMS
					if dev < 0 {
						dev = -dev
					}
					if dev > maxDev {
						maxDev = dev
					}
					if o.TriggerToVerifiedMS > maxTV {
						maxTV = o.TriggerToVerifiedMS
					}
				}
				ev += fmt.Sprintf(" transitions: %d commanded (%d verified acks) · max sched dev %.1fms · trigger->first verified <= %.1fms (upper bound, incl IPC round trip)",
					len(c.SessionObs), done, maxDev, maxTV)
			}
			// P07e handoff leases this slot executed as writer or
			// reader. The arbiter serialized them (one outstanding
			// lease per block, readers only after the writer's ack
			// reached the parent), so a failed lease is either a
			// kernel mismatch or a channel death - the record says
			// which. Intervals are upper bounds incl the IPC round
			// trip, same labeling as the burst intervals.
			if len(c.LeaseObs) > 0 {
				var acked, failed int
				var maxTo float64
				for _, o := range c.LeaseObs {
					if o.Acked {
						acked++
					} else {
						failed++
					}
					if o.ToAckMS > maxTo {
						maxTo = o.ToAckMS
					}
				}
				ev += fmt.Sprintf(" · handoff leases: %d (%d acked, %d failed) · command->ack <= %.1fms (upper bound, incl IPC round trip)",
					len(c.LeaseObs), acked, failed, maxTo)
			}
			switch status := outcomeStatus(rep, c); status {
			case OK:
				fmt.Fprintf(w, "  core %d (%s): OK%s\n", c.Core, c.Workload, ev)
			case Fault:
				fmt.Fprintf(w, "  core %d (%s): FAULT — %s%s\n", c.Core, c.Workload, c.Msg, ev)
			case Interrupted, Skipped, SetupError:
				fmt.Fprintf(w, "  core %d (%s): %s — %s%s\n", c.Core, c.Workload, strings.ToUpper(status), c.Msg, ev)
			default:
				if c.LastBeat.IsZero() {
					fmt.Fprintf(w, "  core %d (%s): CRASHED — never reported progress\n", c.Core, c.Workload)
				} else {
					fmt.Fprintf(w, "  core %d (%s): CRASHED — last seen %s at %s iterations\n",
						c.Core, c.Workload, c.LastBeat.Format(time.RFC3339), human.Count(c.LastIters))
				}
			}
		}
		// WHEA observation: the count only means something together
		// with the state that produced it. No records and no state
		// means we cannot say anything about hardware errors for this
		// run — that is stated, never implied as "no errors".
		if len(rep.Whea) > 0 || rep.WheaState != "" {
			var corr, unc, unk int
			for _, ev := range rep.Whea {
				switch ev.Severity {
				case "corrected":
					corr++
				case "uncorrected":
					unc++
				default:
					unk++
				}
			}
			state := rep.WheaState
			if state == "" {
				state = "unknown (no state record: the run did not watch WHEA or the journal predates it)"
			}
			fmt.Fprintf(w, " WHEA: %d corrected, %d uncorrected, %d unknown; observation: %s\n", corr, unc, unk, state)
			if rep.WheaStateReason != "" {
				fmt.Fprintf(w, "  observation reason: %s\n", rep.WheaStateReason)
			}
			for _, ev := range rep.Whea {
				fmt.Fprintf(w, "  WHEA %s %s (provider %s, event %d, record %d): %s\n",
					ev.Severity, ev.Nature, ev.Provider, ev.EventID, ev.LogRecID, ev.Msg)
			}
			if unc > 0 {
				fmt.Fprintln(w, "  UNCORRECTED hardware error evidence during this run — adjudicate before trusting the result")
			}
		} else {
			fmt.Fprintln(w, " WHEA: no observation records — hardware-error state unknown, NOT \"no errors\"")
		}
	}
}

// RecoverRuns rebuilds what happened from journal records. Runs are returned
// in the order they first appear; cores within a run are sorted by number.
// A core with a core_start and no core_done is reported as crashed, with the
// last beat as the last known healthy moment.
func RecoverRuns(records []Record) []RunReport {
	var order []string
	states := make(map[string]*runState)

	for _, rec := range records {
		st, ok := states[rec.RunID]
		if !ok {
			st = &runState{rep: RunReport{RunID: rec.RunID}, cores: make(map[int]*CoreOutcome)}
			states[rec.RunID] = st
			order = append(order, rec.RunID)
		}
		switch rec.Type {
		case RunStart:
			st.rep.Schema = rec.Schema // 0 = this run predates journal versioning
			st.rep.Test = rec.Test
			st.rep.Phase = rec.Phase
			st.rep.Selected = rec.Cores
			st.rep.PerProcess = rec.PerProcess
			if d, derr := time.ParseDuration(rec.Duration); derr == nil && d > 0 {
				st.rep.Duration = d
			}
		case CoreStart:
			if c := st.core(rec); c != nil {
				c.Workload = rec.Workload
				// In per-process mode core_start lives in the worker's
				// fragment and may be read after a core_done the parent
				// wrote: never downgrade a known ending to "crashed".
				if c.Status == "" {
					c.Status = Crashed
				}
			}
		case Sources:
			st.rep.Expected = append(st.rep.Expected, rec.Cores...)
		case Whea:
			// A persist retry after a failed write must not double-count:
			// the event log's own RecordID is the identity.
			if rec.LogRecID != 0 {
				if st.wheaSeen == nil {
					st.wheaSeen = make(map[string]bool)
				}
				k := fmt.Sprintf("%s:%d", rec.Provider, rec.LogRecID)
				if st.wheaSeen[k] {
					continue
				}
				st.wheaSeen[k] = true
			}
			var when time.Time
			for _, ts := range []string{rec.EvTime, rec.TS} {
				if ts == "" {
					continue
				}
				if t, terr := time.Parse(time.RFC3339Nano, ts); terr == nil {
					when = t
					break
				}
			}
			st.rep.Whea = append(st.rep.Whea, WheaOutcome{
				Time: when, Provider: rec.Provider, EventID: rec.EventID, LogRecID: rec.LogRecID,
				Severity: rec.Severity, Nature: rec.Nature, Msg: rec.Msg,
			})
		case WheaState:
			// Last state record wins: it is the engine's final verdict
			// on how much the WHEA evidence above can be trusted.
			st.rep.WheaState = rec.Result
			st.rep.WheaStateReason = rec.Msg
		case CoreBeat:
			if c := st.core(rec); c != nil {
				if ts, err := time.Parse(time.RFC3339Nano, rec.TS); err == nil {
					c.LastBeat = ts
				}
				c.LastIters = rec.Iters
			}
		case CoreDone:
			if c := st.core(rec); c != nil {
				c.Status = rec.Result
				c.Msg = rec.Msg
				if rec.LoadNS != nil {
					d := time.Duration(max(int64(0), *rec.LoadNS))
					c.LoadTime = &d
				}
				c.Checks = rec.Checks
				c.Evidence = rec.Evidence
				c.KernelVersion = rec.KernelVersion
				c.Ram = rec.Ram
			}
		case Wake:
			if c := st.core(rec); c != nil {
				c.WakeMS = append(c.WakeMS, rec.WakeMS)
			}
		case MemDone:
			if c := st.core(rec); c != nil && rec.Mem != nil {
				c.Mem = rec.Mem
			}
		case BurstDone:
			if c := st.core(rec); c != nil && rec.Burst != nil {
				c.Bursts = append(c.Bursts, *rec.Burst)
			}
		case Schedule:
			if rec.Schedule != nil {
				st.rep.Schedule = rec.Schedule
			}
		case RunMeta:
			if rec.Meta != nil {
				st.rep.Meta = rec.Meta
			}
		case SessionBurst:
			if c := st.core(rec); c != nil && rec.SessionBurst != nil {
				c.SessionObs = append(c.SessionObs, *rec.SessionBurst)
			}
		case SessionLease:
			if c := st.core(rec); c != nil && rec.SessionLease != nil {
				c.LeaseObs = append(c.LeaseObs, *rec.SessionLease)
			}
		case ParkingObs:
			if c := st.core(rec); c != nil && rec.Parking != nil {
				c.Parking = append(c.Parking, *rec.Parking)
			}
		case SwitchSchedule:
			if rec.SwitchSchedule != nil {
				st.rep.SwitchSchedule = rec.SwitchSchedule
			}
		case SessionSwitch:
			if rec.SessionSwitch != nil {
				st.rep.Switches = append(st.rep.Switches, *rec.SessionSwitch)
			}
		case Telemetry:
			if c := st.core(rec); c != nil {
				c.Telemetry = append(c.Telemetry, TelemetrySample{
					ClockMHz: rec.ClockMHz, IdlePct: rec.IdlePct, WindowMS: rec.WindowMS,
					CoreClocks: rec.CoreClocks,
				})
			}
		case RunEnd:
			st.rep.Ended = true
			st.rep.Exit = rec.Exit
			st.rep.JournalErrs = rec.JournalErrs
			if rec.Validation != "" {
				st.rep.Validation = rec.Validation
			}
		}
	}

	out := make([]RunReport, 0, len(order))
	for _, runID := range order {
		st := states[runID]
		// A registered fragment that produced nothing is a hole in the
		// story (the child died before opening its file, or the file
		// was lost) — name it, never let it read as a clean absence.
		if len(st.rep.Expected) > 0 {
			seen := make(map[int]bool, len(st.cores))
			for core := range st.cores {
				seen[core] = true
			}
			for _, core := range st.rep.Expected {
				if !seen[core] {
					st.rep.Missing = append(st.rep.Missing, core)
				}
			}
		}
		for _, c := range st.cores {
			st.rep.Cores = append(st.rep.Cores, *c)
		}
		sort.Slice(st.rep.Cores, func(i, j int) bool { return st.rep.Cores[i].Core < st.rep.Cores[j].Core })
		out = append(out, st.rep)
	}
	return out
}

// CoreSummary is the aggregated fate of one logical CPU across every run
// in the journal: how often it was loaded and how often it failed.
type CoreSummary struct {
	Core        int
	Runs        int
	Faults      int
	Crashed     int // started a workload but never reported a result
	Interrupted int
	Skipped     int
	SetupErrors int
	LastFault   string        // "phase test/workload: msg" of the most recent fault
	Exposure    time.Duration // measured load time; completed legacy runs use configured duration
	WakeN       int
	WakeMinMS   float64 // wake latency stats: how long thawed cores took to
	WakeMaxMS   float64 // confirm their first work — the wake transient proxy
	WakeAvgMS   float64
	ClockN      int // telemetry: observed cycle rate (effective clock proxy)
	ClockMinMHz float64
	ClockMaxMHz float64
	ClockAvgMHz float64
	IdleN       int
	IdleAvgPct  float64 // weighted by each window's length
	IdleMaxPct  float64
	IdleWeight  int64 // window-ms accumulated for the weighted average
	// Source-reported core clocks (never the observed cycle rate): stats
	// over every group sample of every window, weighted by group N.
	CoreClockN       int
	CoreClockMinMHz  float64
	CoreClockMaxMHz  float64
	CoreClockAvgMHz  float64  // sample-weighted; SummarizeCores divides it once
	CoreClockSources []string // distinct sources in first-seen order
}

// Summary is the whole-journal picture the 'report' command renders.
type Summary struct {
	Runs    int
	Unended int // runs without run_end (the process or the machine died)
	Cores   []CoreSummary
}

// SummarizeCores aggregates recovered runs per logical CPU. The journal
// numbers cores by plan slot in the in-process runner and by logical CPU
// in per-process mode; run_start carries the selected cores and the mode,
// so both map onto the same logical numbers here. Journals written before
// the mode flag existed fall back to the slot interpretation when the
// number fits the selection and are taken at face value otherwise.
func SummarizeCores(reports []RunReport) Summary {
	sum := Summary{Runs: len(reports)}
	byCore := map[int]*CoreSummary{}

	for _, rep := range reports {
		if !rep.Ended {
			sum.Unended++
		}
		for _, c := range rep.Cores {
			status := outcomeStatus(rep, c)
			logical := c.Core
			if !rep.PerProcess && logical >= 0 && logical < len(rep.Selected) {
				logical = rep.Selected[logical]
			}
			cs := byCore[logical]
			if cs == nil {
				cs = &CoreSummary{Core: logical}
				byCore[logical] = cs
			}
			cs.Runs++
			if status != Skipped {
				if c.LoadTime != nil {
					cs.Exposure += max(time.Duration(0), *c.LoadTime)
				} else if status == OK && rep.Ended && rep.Exit == 0 {
					// Only completed legacy runs can safely use their planned time.
					cs.Exposure += rep.Duration
				}
			}
			for _, ms := range c.WakeMS {
				if cs.WakeN == 0 || ms < cs.WakeMinMS {
					cs.WakeMinMS = ms
				}
				if ms > cs.WakeMaxMS {
					cs.WakeMaxMS = ms
				}
				cs.WakeAvgMS += ms
				cs.WakeN++
			}
			for _, t := range c.Telemetry {
				if t.ClockMHz > 0 {
					if cs.ClockN == 0 || t.ClockMHz < cs.ClockMinMHz {
						cs.ClockMinMHz = t.ClockMHz
					}
					if t.ClockMHz > cs.ClockMaxMHz {
						cs.ClockMaxMHz = t.ClockMHz
					}
					cs.ClockAvgMHz += t.ClockMHz
					cs.ClockN++
				}
				if t.IdlePct != nil {
					w := t.WindowMS
					if w < 1 {
						w = 1
					}
					cs.IdleAvgPct += *t.IdlePct * float64(w)
					cs.IdleWeight += w
					if *t.IdlePct > cs.IdleMaxPct {
						cs.IdleMaxPct = *t.IdlePct
					}
					cs.IdleN++
				}
				for _, g := range t.CoreClocks {
					if g.N <= 0 {
						continue // an empty group proves nothing
					}
					if cs.CoreClockN == 0 || g.MinMHz < cs.CoreClockMinMHz {
						cs.CoreClockMinMHz = g.MinMHz
					}
					if g.MaxMHz > cs.CoreClockMaxMHz {
						cs.CoreClockMaxMHz = g.MaxMHz
					}
					cs.CoreClockAvgMHz += g.AvgMHz * float64(g.N)
					cs.CoreClockN += g.N
					seen := false
					for _, s := range cs.CoreClockSources {
						if s == g.Source {
							seen = true
							break
						}
					}
					if !seen {
						cs.CoreClockSources = append(cs.CoreClockSources, g.Source)
					}
				}
			}
			switch status {
			case Fault:
				cs.Faults++
				label := rep.Test + "/" + c.Workload
				if rep.Phase != "" {
					label = rep.Phase + " " + label
				}
				cs.LastFault = fmt.Sprintf("%s: %s", label, c.Msg)
			case Crashed:
				cs.Crashed++
			case Interrupted:
				cs.Interrupted++
			case Skipped:
				cs.Skipped++
			case SetupError:
				cs.SetupErrors++
			}
		}
	}

	// Averages divide the accumulated sums exactly once: the accumulators
	// live across every run of the journal, so dividing per run would
	// re-average partial sums and skew the result.
	sum.Cores = make([]CoreSummary, 0, len(byCore))
	for _, cs := range byCore {
		if cs.WakeN > 0 {
			cs.WakeAvgMS /= float64(cs.WakeN)
		}
		if cs.ClockN > 0 {
			cs.ClockAvgMHz /= float64(cs.ClockN)
		}
		if cs.CoreClockN > 0 {
			cs.CoreClockAvgMHz /= float64(cs.CoreClockN)
		}
		if cs.IdleWeight > 0 {
			cs.IdleAvgPct /= float64(cs.IdleWeight)
		}
		sum.Cores = append(sum.Cores, *cs)
	}
	sort.Slice(sum.Cores, func(i, j int) bool { return sum.Cores[i].Core < sum.Cores[j].Core })
	return sum
}

// PrintSummary renders the per-core report for the 'report' command.
func PrintSummary(w io.Writer, sum Summary) {
	for _, cs := range sum.Cores {
		fmt.Fprintf(w, "  core %d: %d run(s)", cs.Core, cs.Runs)
		if cs.Faults > 0 {
			fmt.Fprintf(w, ", %d FAULT", cs.Faults)
		}
		if cs.Crashed > 0 {
			fmt.Fprintf(w, ", %d crashed", cs.Crashed)
		}
		if cs.Interrupted > 0 {
			fmt.Fprintf(w, ", %d interrupted", cs.Interrupted)
		}
		if cs.Skipped > 0 {
			fmt.Fprintf(w, ", %d skipped", cs.Skipped)
		}
		if cs.SetupErrors > 0 {
			fmt.Fprintf(w, ", %d setup error(s)", cs.SetupErrors)
		}
		if cs.Faults == 0 && cs.Crashed == 0 && cs.Interrupted == 0 && cs.Skipped == 0 && cs.SetupErrors == 0 {
			fmt.Fprint(w, ", OK")
		}
		if cs.Exposure > 0 {
			fmt.Fprintf(w, ", exposure %s", cs.Exposure.Truncate(time.Millisecond))
		}
		fmt.Fprintln(w)
		if cs.LastFault != "" {
			fmt.Fprintf(w, "    last fault: %s\n", cs.LastFault)
		}
		if cs.WakeN > 0 {
			fmt.Fprintf(w, "    wake: %d, min %.0fms, avg %.0fms, max %.0fms\n",
				cs.WakeN, cs.WakeMinMS, cs.WakeAvgMS, cs.WakeMaxMS)
		}
		if cs.ClockN > 0 {
			fmt.Fprintf(w, "    observed cycle rate: %d, min %.0f avg %.0f max %.0f MHz\n",
				cs.ClockN, cs.ClockMinMHz, cs.ClockAvgMHz, cs.ClockMaxMHz)
		}
		if cs.CoreClockN > 0 {
			fmt.Fprintf(w, "    core clock (%s): %d, min %.0f avg %.0f max %.0f MHz\n",
				strings.Join(cs.CoreClockSources, "+"), cs.CoreClockN, cs.CoreClockMinMHz, cs.CoreClockAvgMHz, cs.CoreClockMaxMHz)
		}
		if cs.IdleN > 0 {
			fmt.Fprintf(w, "    idle: avg %.1f%%, max %.1f%%\n", cs.IdleAvgPct, cs.IdleMaxPct)
		}
	}
	fmt.Fprintf(w, "%d run(s) total", sum.Runs)
	if sum.Unended > 0 {
		fmt.Fprintf(w, ", %d did not end cleanly", sum.Unended)
	}
	fmt.Fprintln(w)
}
