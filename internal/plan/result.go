package plan

import (
	"fmt"

	"github.com/weltercode/cpu-stability-checker/internal/human"
)

// Result is the structured outcome of one run, returned by both executors:
// runner.ExecuteWithResult (threads in one process) and
// subprocess.ExecuteSubprocessWithResult (one pinned child per CPU).
//
// The structured fields are the truth; Exit/Aborted/AbortMsg/Faults are a
// derived compatibility view (see Finalize) that the scenario engine, soak
// and the menu still act on. A bare bool "OK" never covered this: a run
// can finish its duration while verifying nothing (external tools), be
// cancelled with real findings already stored, or fail its infrastructure
// without any CPU at fault.
//
// Note: the journal's core numbers are slot indices within a run; the
// Faults map here is keyed by logical CPU number, which is what stays
// stable across phases.
type Result struct {
	// Execution says how the run itself ended, independent of what it
	// found: a cancelled run that had already caught a mismatch keeps
	// both facts.
	Execution Execution
	// Validation says how much of the declared checking actually ran.
	// It is independent from Findings: a verified pass can carry
	// findings, and a clean-looking run can carry validation=unknown.
	Validation Validation
	// Coverage contrasts the plan with what actually ran.
	Coverage Coverage
	// Findings are the observed facts with their source and how firmly
	// they point at a component. A finding never claims more than the
	// observation supports: a mismatch seen on CPU 5 names the
	// observer, not the guilty DIMM or core.
	Findings []Finding

	// Compatibility view, derived by Finalize from the fields above.
	Exit     int
	Aborted  bool
	AbortMsg string
	// Faults maps logical CPU -> first fault message, and only ever
	// holds findings with confirmed per-CPU attribution. A fault in
	// one shared external process is a run-level finding and adds
	// nothing here: blacklisting every CPU it served would invent
	// sixteen culprits for one process.
	Faults map[int]string
}

// Execution describes how the run ended.
type Execution string

const (
	// ExecCompleted: ran to its declared end (deadline or criterion).
	ExecCompleted Execution = "completed"
	// ExecCancelled: stopped early by the user (Ctrl+C, esc).
	ExecCancelled Execution = "cancelled"
	// ExecSetupError: could not start or run reliably (config, tool,
	// sensor preflight). Never evidence of a CPU fault.
	ExecSetupError Execution = "setup_error"
	// ExecRuntimeError: stopped early by an observed runtime condition:
	// a detected data mismatch, WHEA critical, thermal stop, sensor
	// loss. The condition itself lives in Findings.
	ExecRuntimeError Execution = "runtime_error"
	// ExecInterrupted: killed as collateral (parent cancelled a
	// worker, run aborted because a peer faulted).
	ExecInterrupted Execution = "interrupted"
	// ExecSkipped: deliberately did not run (idle slot, phase skipped
	// by policy).
	ExecSkipped Execution = "skipped"
)

// Validation says how much of the declared verification actually happened.
// It never claims more than the evidence supports.
type Validation string

const (
	// ValVerified: the planned checks ran to the declared contract
	// and confirmed their results. Not a promise of absolute
	// stability, and not the absence of findings.
	ValVerified Validation = "verified"
	// ValPartial: some checks were confirmed, the contract was not
	// fully met (cancellation, interruption, missing evidence).
	ValPartial Validation = "partial"
	// ValNotPerformed: the mode declared no data verification (pure
	// load like mem-bw). A legitimate outcome, never a pass.
	ValNotPerformed Validation = "not_performed"
	// ValUnknown: the checking cannot be known from our side (an
	// external tool we cannot confirm). Not the same as performed.
	ValUnknown Validation = "unknown"
	// ValExternalConfirmed: an external tool's own protocol evidence
	// confirmed its verification ran to completion (adapter-confirmed
	// completion format, not a timer guess). We still did not verify
	// the data ourselves - we confirmed the tool said it did, per a
	// protocol our adapter parses. Never produced for a legacy
	// adapter that has no such proof.
	ValExternalConfirmed Validation = "external_confirmed"
)

// AttributionKind names the kind of component a finding points at.
type AttributionKind string

const (
	AttrWorker       AttributionKind = "worker"
	AttrLogicalCPU   AttributionKind = "logical_cpu"
	AttrPhysicalCore AttributionKind = "physical_core"
	AttrMemoryRegion AttributionKind = "memory_region"
	// AttrRun: the fact belongs to the run or one shared process in
	// it; no component is singled out.
	AttrRun AttributionKind = "run"
	// AttrUnknown: observed but unplaceable; do not promote it.
	AttrUnknown AttributionKind = "unknown"
)

// Confidence says how firmly the attribution is supported.
type Confidence string

const (
	ConfUnknown   Confidence = "unknown"
	ConfInferred  Confidence = "inferred"
	ConfConfirmed Confidence = "confirmed"
)

// FindingKind classifies what was observed.
type FindingKind string

const (
	FindDataMismatch FindingKind = "data_mismatch" // computed result != reference
	// FindSyntheticProbe: the deliberate fault prober's observation.
	// It proves the fault-reporting pipeline (journal record, exit
	// code, blacklist) on demand - it is NOT hardware evidence, and
	// reports must never render it as a real mismatch.
	FindSyntheticProbe FindingKind = "synthetic_probe"
	FindWHEA           FindingKind = "whea"         // OS hardware error record
	FindWHEAUnknown    FindingKind = "whea_unknown" // WHEA evidence we could not classify: adjudication required
	FindPCIeObs        FindingKind = "pcie_obs"     // corrected PCIe WHEA record: separate, no core blame
	FindThermalStop    FindingKind = "thermal_stop"
	FindSensorLoss     FindingKind = "sensor_loss"
	FindToolFailure    FindingKind = "tool_failure" // external process misbehaved
	FindSetupError     FindingKind = "setup_error"
	FindInterrupted    FindingKind = "interrupted" // collateral of a stop
	FindJournalError   FindingKind = "journal_error"
	// FindProcessState: the process-wide scheduler state (affinity mask,
	// GOMAXPROCS) a run only borrowed could not be fully handed back.
	// Run-level infrastructure evidence: it blames no CPU, and Finalize
	// deliberately never lets it change a run's own verdict.
	FindProcessState FindingKind = "process_state_error"
	// FindParking: the Q04 parking-evidence observation for a
	// prepared transition run - what the OS parking source said (or
	// refused to say) during the measured idle windows. Run-level
	// evidence: it blames no CPU, enters no blacklist and never
	// changes the exit code. Its ONLY verdict power is the required
	// gate: without observed evidence a run cannot claim a full
	// verification (validation drops to partial), because "required"
	// means exactly that.
	FindParking FindingKind = "parking_evidence"
)

// Attribution records what a finding points at and how firmly. Index is
// the identifier matching Kind (logical CPU number, worker slot, ...);
// Region names a memory region when applicable. Both stay zero when the
// finding is run-level.
//
// ObservedOn is the logical CPU the fact was SEEN on, kept separate
// from the blame-bearing attribution (A06): a mismatch observed while
// a worker ran on CPU 5 does not by itself indict CPU 5 - a memory
// finding points at a memory region and merely records where it was
// observed. Only an attribution of Kind AttrLogicalCPU with Confirmed
// confidence feeds the faulted-core blacklist.
type Attribution struct {
	Kind       AttributionKind
	Index      *int
	Region     string
	Confidence Confidence
	// ObservedOn: logical CPU of observation (nil = not recorded).
	// Never a claim of guilt, and never a blacklist entry on its own.
	ObservedOn *int
}

// Finding is one observed fact with its source. Sources: "workload",
// "whea", "temperature", "journal", "signal", "tool", "runner".
type Finding struct {
	Kind        FindingKind
	Source      string
	Message     string
	Attribution Attribution
}

// WHEAFinding classifies one observed WHEA record into a run-level
// finding. The severity/nature vocabulary is the whea package's, kept
// here as plain strings so the model layer stays dependency-light:
//
//   - uncorrected (any nature), or corrected on cpu/cache/interconnect/
//     memory: FindWHEA — direct hardware evidence during the run. The
//     result is not clean (exit 1); only uncorrected additionally
//     aborts, because corrected errors can be logged while the machine
//     keeps working.
//   - corrected PCIe: FindPCIeObs — recorded separately per plan P02
//     (PCIe errors have no core to blame and do not by themselves make
//     a CPU stability run fail), visible but exit-neutral.
//   - anything unrecognized: FindWHEAUnknown — requires adjudication,
//     never an automatic pass and never an automatic abort.
//
// Attribution is always run-level with unknown confidence: the OS
// names the error source, and a CPER APIC ID is not our logical CPU
// numbering, so no fault (blacklist entry) can ever come from WHEA.
func WHEAFinding(severity, nature, msg string) Finding {
	kind := FindWHEA
	switch {
	case severity == "uncorrected":
	case severity == "corrected" && (nature == "cpu" || nature == "cache" ||
		nature == "interconnect" || nature == "memory"):
	case severity == "corrected" && nature == "pcie":
		kind = FindPCIeObs
	default:
		kind = FindWHEAUnknown
	}
	return Finding{Kind: kind, Source: "whea", Message: msg,
		Attribution: Attribution{Kind: AttrRun, Confidence: ConfUnknown}}
}

// Coverage contrasts the plan with the fact. Verified counts confirmed
// verified checks only (iterations of self-checking workloads); timer
// beats of a live external process are not checks. ChecksKnown=false
// means the number cannot be known from our side.
type Coverage struct {
	Planned     int
	Started     int
	Completed   int
	Verified    uint64
	ChecksKnown bool
	// Mem is the memory engine's run-level accounting, present only
	// when at least one slot reported a mem_done record in this run.
	// Absence means no engine accounting was observed - unknown,
	// never zero. The numbers are CUMULATIVE engine byte accounting
	// over the slots that reported: a slot without a mem_done record
	// counts as unknown, not as zero, and none of this is a claim
	// about unique physical-RAM coverage.
	Mem *RAMTotals `json:"mem,omitempty"`
}

// RAMTotals folds the per-slot mem_done accounting of one run into
// run-level facts. It never claims more than the records support:
// EngineSlots counts slots whose accounting was observed,
// UnknownSlots counts slots whose accounting was never written
// (absence = unknown, never zero).
type RAMTotals struct {
	// EngineSlots counts slots that reported a mem_done record.
	EngineSlots int `json:"engine_slots"`
	// UnknownSlots counts planned slots whose memory accounting was
	// never reported (no engine, a killed child, or an older journal).
	UnknownSlots int `json:"unknown_slots"`
	// Requested is the sum of grant bytes the parent assigned to the
	// accounting slots.
	Requested uint64 `json:"requested_bytes"`
	// Allocated is what the OS actually committed for those slots
	// (differs from Requested exactly when a region was rounded).
	Allocated uint64 `json:"allocated_bytes"`
	// Touched is the first-touch coverage before the measured window;
	// an interrupted pass reports the honest partial.
	Touched uint64 `json:"touched_bytes"`
	// Verified is CUMULATIVE verified byte-work (a byte verified in
	// each of five passes counts five times). It is work performed,
	// not a physical region count.
	Verified uint64 `json:"verified_bytes_cumulative"`
	// LastPass sums the per-slot unique-in-last-pass volumes where
	// the slot proved them. Slots without proof are unknown and are
	// not counted here, so this is a floor, not a coverage promise.
	LastPass uint64 `json:"last_pass_verified_bytes"`
}

// MemAccounting is one slot's memory-engine facts as the executor
// reports them at run end (translated from the engine's own mem_done
// record by the executor; the plan layer never invents them). Every
// field is what the OS and the workload actually did. VerifiedUnique
// and VerifiedTotal are nil when the slot never proved a last-pass
// volume: unknown, never zero.
type MemAccounting struct {
	Requested      uint64  `json:"requested"`
	Allocated      uint64  `json:"allocated"`
	Touched        uint64  `json:"touched"`
	Verified       uint64  `json:"verified"`
	VerifiedUnique *uint64 `json:"verified_unique,omitempty"` // nil = the slot never proved a unique last-pass volume
	VerifiedTotal  *uint64 `json:"verified_total,omitempty"`  // nil = same unknown, in pair with VerifiedUnique
}

// RAMString renders the run-level accounting as one honest line for
// the run summary. The wording carries the meaning: CUMULATIVE
// verified byte-work, last-pass bytes only where slots proved them,
// and unknown slots stated - never a physical-RAM coverage claim.
func (m *RAMTotals) RAMString() string {
	if m == nil {
		return ""
	}
	s := fmt.Sprintf("memory coverage (cumulative): %d slot(s) accounted, requested=%s allocated=%s touched=%s verified=%s",
		m.EngineSlots, human.Bytes(m.Requested), human.Bytes(m.Allocated),
		human.Bytes(m.Touched), human.Bytes(m.Verified))
	if m.LastPass > 0 {
		s += fmt.Sprintf(" last-pass=%s", human.Bytes(m.LastPass))
	}
	if m.UnknownSlots > 0 {
		s += fmt.Sprintf(" - %d slot(s) without accounting (unknown, not zero)", m.UnknownSlots)
	}
	return s
}

// IntPtr is a small helper for Attribution.Index.
func IntPtr(v int) *int { return &v }
