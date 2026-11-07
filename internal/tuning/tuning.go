// Package tuning is the P11 CO auto-tuning layer: a versioned,
// durable tuning plan (initial profile, candidate, target core,
// stage, seed, attempt budget, required checks and the full decision
// history) plus the trial lifecycle that runs each candidate through
// the proven cotx transaction engine.
//
// Architecture (Sol-validated): cotx stays UNTOUCHED - it owns the
// apply/verify/restore durability of one CO transaction. This package
// owns the plan and the trial around it: trial_prepared is durable
// BEFORE the transaction starts, test_running / test_finished are
// durable inside the transaction's phase, and the decision is durable
// only after the evidence. The full P11 sequence
// trial_prepared -> apply_pending -> applied_verified -> test_running
// -> test_finished -> restore_pending -> restored therefore spans two
// strict stores (this plan document + the cotx TxDoc), each with
// atomic transitions. A crash in the cross-store gap leaves the trial
// undecided: Reconcile re-derives from a fresh hardware read and the
// cotx document and classifies it interrupted/unknown - never
// accepted, never auto-resumed (a crash could be the OS, power, or
// another core; it is not proof the candidate is bad, and not proof
// the CPU is broken).
//
// Automation gate (plan P11 precondition): until P10+P12 prove the
// real backend's diagnostics, every tuning operation refuses any
// backend that does not carry the IsP11Fake marker. A real adapter
// cannot be wired into auto-tuning by accident.
//
// Honesty contracts inherited from co/cotx: unknown stays unknown
// (tri-state checks, never a silent false), the attempt budget is
// reserved when the trial is prepared (not refunded later), a
// candidate is accepted ONLY when the evidence is durable AND the
// cotx restore is confirmed, reading the plan never rewrites it, and
// a corrupt plan document is never auto-repaired - a backup is only
// promoted by an explicit call.
package tuning

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/co"
)

// PlanVersion is the tuning-plan document schema version. ParsePlan
// refuses anything else; a damaged document is never partially
// trusted and never auto-repaired.
const PlanVersion = 1

// Plan statuses. open accepts new trials; recovery_required means an
// undecided trial exists and Reconcile must run before tuning
// continues (never bypassed by starting a new trial).
const (
	StatusOpen             = "open"
	StatusRecoveryRequired = "recovery_required"
)

// Durable trial states. Decided is the only terminal state; anything
// else found on restart is an undecided trial awaiting Reconcile.
const (
	TrialPrepared = "trial_prepared"
	TestRunning   = "test_running"
	TestFinished  = "test_finished"
	TrialDecided  = "decided"
)

// Trial outcomes. accepted/rejected are decisions; incomplete means
// the observation could not confirm what it needed to (unknown
// check, unavailable sensor); interrupted_unknown means the trial
// died before a durable decision and the cause is genuinely unknown.
const (
	OutcomeAccepted           = "accepted"
	OutcomeRejected           = "rejected"
	OutcomeIncomplete         = "incomplete"
	OutcomeInterruptedUnknown = "interrupted_unknown"
)

// The three required per-candidate observations (plan P11): steady
// compute, short idle/load transitions, extended confirmation. A
// missing required observation gives incomplete, never a pass.
const (
	CheckSteady      = "steady_compute"
	CheckTransitions = "idle_load_transitions"
	CheckExtended    = "extended_confirmation"
)

// Check is one tri-state observation with its reason. Known=false is
// "could not look", never a silent no.
type Check struct {
	Name  string `json:"name"`
	Known bool   `json:"known"`
	Yes   bool   `json:"yes"`
	Why   string `json:"why,omitempty"`
}

func checkYes(name, why string) Check     { return Check{Name: name, Known: true, Yes: true, Why: why} }
func checkNo(name, why string) Check      { return Check{Name: name, Known: true, Yes: false, Why: why} }
func checkUnknown(name, why string) Check { return Check{Name: name, Why: why} }

// Evidence is one tester run's durable record: the three required
// checks, corrected-WHEA growth (known-yes growth FORBIDS
// acceptance), and the fresh CPU temperature observation against the
// plan's user-configured limit (the board thermal zone never
// substitutes for the CPU sensor in this profile).
type Evidence struct {
	Checks      []Check  `json:"checks"`
	WheaGrowth  Check    `json:"whea_growth"` // Yes=true means corrected WHEA grew
	Temperature Check    `json:"temperature"` // Yes=true means stayed within limit
	Notes       []string `json:"notes,omitempty"`
}

// Trial is one candidate attempt inside the plan's append-preserving
// history. CotxTxID/CotxStatus associate the cotx transaction that
// performed the write BEFORE its single-slot document is reused by
// the next trial; RestoreConfirmed records that the hardware was
// confirmed back at the baseline by that transaction's terminal read.
type Trial struct {
	TrialID          string    `json:"trial_id"`
	Attempt          int       `json:"attempt"`
	Core             string    `json:"core"`
	FromValue        int       `json:"from_value"`
	Candidate        int       `json:"candidate"`
	State            string    `json:"state"`
	Outcome          string    `json:"outcome,omitempty"`
	Why              string    `json:"why,omitempty"`
	CotxTxID         string    `json:"cotx_tx_id,omitempty"`
	CotxStatus       string    `json:"cotx_status,omitempty"`
	RestoreConfirmed bool      `json:"restore_confirmed"`
	Evidence         *Evidence `json:"evidence,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Plan is the durable tuning plan. It binds the verified initial
// profile and machine identity (host, hardware fingerprint, topology
// mapping fingerprint, tool+version) so a plan recorded for a
// different machine, build or topology cannot silently drive trials
// here. Baseline is the EXPLICIT policy for the non-target cores:
// the user's real profile, never silent zeros.
type Plan struct {
	PlanVersion  int         `json:"plan_version"`
	PlanID       string      `json:"plan_id"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	Status       string      `json:"status"`
	Fake         bool        `json:"fake"` // automation gate: only the marked fake backend
	Host         string      `json:"host"`
	HardwareID   string      `json:"hardware_id"`
	MappingFP    string      `json:"mapping_fp"`
	Tool         string      `json:"tool"`
	ToolVersion  string      `json:"tool_version"`
	Initial      co.Snapshot `json:"initial"`
	Baseline     co.Profile  `json:"baseline"`
	TargetCore   string      `json:"target_core"`
	RangeMin     int         `json:"range_min"`
	RangeMax     int         `json:"range_max"`
	TempLimitC   int         `json:"temp_limit_c"` // user-configured; 0 = not configured
	AttemptLimit int         `json:"attempt_limit"`
	AttemptsUsed int         `json:"attempts_used"` // reserved at trial_prepared, never refunded
	Seed         int64       `json:"seed"`
	Trials       []Trial     `json:"trials,omitempty"`
}

// Options configures a new plan. The range is the USER-chosen search
// bound (the backend's Validate still gates the write), the
// temperature limit is the user's explicit setting for this CPU
// (never a universal constant), and AttemptLimit bounds the total
// attempts.
type Options struct {
	TargetCore   string
	RangeMin     int
	RangeMax     int
	TempLimitC   int
	AttemptLimit int
	Seed         int64
}

// FakeBackend is the marker a backend must carry for P11 automation.
// *co.Fake implements it; the real adapter deliberately does not.
type FakeBackend interface {
	co.Backend
	IsP11Fake() bool
}

func notFakeErr(op string) *co.Error {
	return &co.Error{Kind: co.KindNotValidated, Op: op,
		Err: fmt.Errorf("P11 automation is allowed only against the marked fake backend until P12 proves the real diagnostics: refusing")}
}

// CreatePlan establishes the verified initial profile and machine
// identity against a marked fake backend and returns the new plan
// (not yet stored). It refuses a non-fake backend, an unconfirmed
// read/readback path, a non-exact or unfingerprinted read, a target
// core the machine does not address, and an inconsistent range or
// attempt limit.
func CreatePlan(ctx context.Context, b co.Backend, opts Options) (*Plan, error) {
	if _, ok := b.(FakeBackend); !ok {
		return nil, notFakeErr("plan")
	}
	if opts.TargetCore == "" {
		return nil, &co.Error{Kind: co.KindBadProfile, Op: "plan", Err: fmt.Errorf("target core is required")}
	}
	if opts.AttemptLimit <= 0 {
		return nil, &co.Error{Kind: co.KindBadProfile, Op: "plan", Err: fmt.Errorf("attempt limit must be positive")}
	}
	if opts.RangeMin > opts.RangeMax {
		return nil, &co.Error{Kind: co.KindBadProfile, Op: "plan", Err: fmt.Errorf("range %d..%d is inverted", opts.RangeMin, opts.RangeMax)}
	}
	caps, err := b.Probe(ctx)
	if err != nil {
		return nil, err
	}
	if !caps.Supported.Known || !caps.Supported.Yes {
		return nil, &co.Error{Kind: co.KindUnsupportedCPU, Op: "plan", Err: fmt.Errorf("tool/CPU support not confirmed")}
	}
	if !caps.Read.Known || !caps.Read.Yes || !caps.Readback.Known || !caps.Readback.Yes {
		return nil, &co.Error{Kind: co.KindNotValidated, Op: "plan",
			Err: fmt.Errorf("read/readback not confirmed: tuning trials need a trustworthy readback")}
	}
	mapping, err := b.Topology(ctx)
	if err != nil {
		return nil, err
	}
	addressable := false
	for _, s := range mapping.Slots {
		if s.ToolID == opts.TargetCore {
			addressable = true
			break
		}
	}
	if !addressable {
		return nil, &co.Error{Kind: co.KindBadProfile, Op: "plan",
			Err: fmt.Errorf("%s is not addressable in the confirmed topology", opts.TargetCore)}
	}
	snap, err := b.Read(ctx)
	if err != nil {
		return nil, err
	}
	if snap.Quality != "exact" || snap.HardwareID == "" {
		return nil, &co.Error{Kind: co.KindNotValidated, Op: "plan",
			Err: fmt.Errorf("initial read not exact/fingerprinted (quality %q): cannot bind a plan to an unverified profile", snap.Quality)}
	}
	inProfile := false
	for _, o := range snap.Profile.Offsets {
		if o.Core == opts.TargetCore {
			inProfile = true
			break
		}
	}
	if !inProfile {
		return nil, &co.Error{Kind: co.KindBadProfile, Op: "plan",
			Err: fmt.Errorf("%s is not in the read profile: baseline policy would silently substitute a value", opts.TargetCore)}
	}
	host, herr := os.Hostname()
	if herr != nil || host == "" {
		return nil, &co.Error{Kind: co.KindFingerprint, Op: "plan", Err: fmt.Errorf("host name unavailable: cannot bind the plan to a machine")}
	}
	now := time.Now()
	return &Plan{
		PlanVersion:  PlanVersion,
		PlanID:       newID("plan"),
		CreatedAt:    now,
		UpdatedAt:    now,
		Status:       StatusOpen,
		Fake:         true,
		Host:         host,
		HardwareID:   snap.HardwareID,
		MappingFP:    mappingFP(mapping),
		Tool:         caps.Tool,
		ToolVersion:  caps.Version,
		Initial:      snap,
		Baseline:     co.Profile{Offsets: append([]co.Offset(nil), snap.Profile.Offsets...)},
		TargetCore:   opts.TargetCore,
		RangeMin:     opts.RangeMin,
		RangeMax:     opts.RangeMax,
		TempLimitC:   opts.TempLimitC,
		AttemptLimit: opts.AttemptLimit,
		Seed:         opts.Seed,
	}, nil
}

// mappingFP fingerprints the confirmed mapping (quality + tool ids +
// SMU indices): a changed topology or tool enumeration changes it,
// and the plan refuses to run trials across such drift.
func mappingFP(m co.Mapping) string {
	parts := make([]string, 0, len(m.Slots)+1)
	parts = append(parts, "q:"+m.Quality)
	for _, s := range m.Slots {
		smu := "?"
		if s.SMUIndex != nil {
			smu = fmt.Sprintf("%d", *s.SMUIndex)
		}
		parts = append(parts, s.ToolID+"="+smu)
	}
	sort.Strings(parts[1:])
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// newID is a random correlation id (plan id / trial id).
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func now() time.Time { return time.Now() }

// ParsePlan reads one plan document strictly: missing or unknown
// PlanVersion, unknown fields, unknown status or trial state, an
// empty plan id, or a decided trial without an outcome are all
// errors. A damaged plan is never partially trusted and never
// auto-repaired.
func ParsePlan(data []byte) (Plan, error) {
	var probe struct {
		PlanVersion *int `json:"plan_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Plan{}, fmt.Errorf("co tune: not a JSON object: %w", err)
	}
	if probe.PlanVersion == nil {
		return Plan{}, fmt.Errorf("co tune: missing plan_version field")
	}
	if *probe.PlanVersion != PlanVersion {
		return Plan{}, fmt.Errorf("co tune: unsupported plan_version %d (this build writes %d)", *probe.PlanVersion, PlanVersion)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("co tune: version %d plan unreadable: %w", PlanVersion, err)
	}
	if p.PlanVersion != PlanVersion {
		return Plan{}, fmt.Errorf("co tune: plan_version drifted mid-decode")
	}
	if p.PlanID == "" {
		return Plan{}, fmt.Errorf("co tune: empty plan_id")
	}
	switch p.Status {
	case StatusOpen, StatusRecoveryRequired:
	default:
		return Plan{}, fmt.Errorf("co tune: unknown plan status %q", p.Status)
	}
	for _, t := range p.Trials {
		switch t.State {
		case TrialPrepared, TestRunning, TestFinished, TrialDecided:
		default:
			return Plan{}, fmt.Errorf("co tune: trial %s has unknown state %q", t.TrialID, t.State)
		}
		if t.State == TrialDecided && t.Outcome == "" {
			return Plan{}, fmt.Errorf("co tune: decided trial %s has no outcome", t.TrialID)
		}
	}
	return p, nil
}
