package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

// planHash is the identity of an experiment (audit A11): a sha256
// over the normalized request (kind, preset, cycles, shuffle seed,
// continue policy, phase list, registered external tool specs) and
// this concrete resolved plan. The config FILE hash missed CLI flags,
// menu choices and saved recipes - two different experiments could
// share it while a report compared them silently. This hash cannot:
// every parameter that shapes what runs is in it.
//
// The exclusions are deliberate: the log path (WHERE the story is
// written does not change WHAT was tested) and ShowMetrics (a display
// choice). Core order is normalized away - the core SET is the
// experiment, the listing order is not.
func planHash(req lastRun, opt plan.Options) string {
	p := opt
	p.LogFile = ""
	p.ShowMetrics = false
	p.PlanHash = "" // the hash of itself is not an input to itself
	if p.Bursts > 0 {
		p.B = "" // transition mode runs slot A only (see run.go): a
		// fresh run and its scenario replay must share one hash
	}
	if p.HandoffBlocks > 0 {
		p.B = "" // handoff phases run slot A only (the coordinator
		// drives ram-handoff leases): inherited slot B must not split
		// one experiment's identity in two
	}
	cores := append([]int(nil), p.Cores...)
	sort.Ints(cores)
	p.Cores = cores

	var ext []string // external tool identity: name + registered command path
	if req.Config != nil {
		for name, spec := range req.Config.External {
			ext = append(ext, name+"="+spec.Command)
		}
		sort.Strings(ext)
	}

	input := struct {
		Schema   string          `json:"schema"`
		Kind     string          `json:"kind"`
		Preset   string          `json:"preset,omitempty"`
		Cycles   int             `json:"cycles,omitempty"`
		Seed     *int64          `json:"seed,omitempty"`
		Continue bool            `json:"continue_on_fault,omitempty"`
		Ext      []string        `json:"ext,omitempty"`
		Session  *SessionOptions `json:"session,omitempty"`
		Phases   []config.Phase  `json:"phases,omitempty"`
		Plan     plan.Options    `json:"plan"`
	}{
		Schema: "planhash/1", Kind: req.Kind, Preset: req.Preset,
		Cycles: req.Cycles, Seed: req.Seed, Continue: req.ContinueOnFault,
		Ext: ext, Session: req.Session, Phases: req.Phases, Plan: p,
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "" // unmarshalable input: unknown, never a fabricated identity
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildRunRecipe expresses a resolved single run as the repeat
// request (audit A04: every entry point saves the same kind of
// request): a one-phase scenario plan plus the resolved-config
// snapshot, so menu repeat replays it through the very same compile
// pipeline as any other scenario - no second replay path to drift.
// The control-core POLICY (auto / off / explicit) is saved, not
// today's resolved answer: auto must stay auto on a machine whose
// layout may differ next time.
func buildRunRecipe(cfg config.Config, opt plan.Options, control *int,
	bursts int, burstGap, burstRun, burstJitter, burstStagger string, burstSeed uint64) lastRun {
	snap := recipeConfig(cfg)
	snap.ControlCore = control
	snap.MaxTemp, snap.Telemetry = opt.MaxTempC, opt.Telemetry
	snap.BeatEvery = ""
	if opt.BeatEvery > 0 {
		snap.BeatEvery = opt.BeatEvery.String()
	}
	snap.RamSeed, snap.LogFile = opt.RamSeed, opt.LogFile
	snap.Memory, snap.MemoryLock = opt.Memory, opt.MemLock
	snap.StopOnFault, snap.ShowMetrics = opt.StopOnFault, opt.ShowMetrics
	ph := config.Phase{
		Test: opt.TestName, A: opt.A, B: opt.B, Cores: opt.Cores, Workers: opt.Workers,
		Duration: opt.Duration.String(), StopOnFault: &opt.StopOnFault,
		PerProcess: opt.PerProcess, Rotate: &opt.Rotate,
		Bursts: bursts, BurstGap: burstGap, BurstRun: burstRun,
		BurstJitter: burstJitter, BurstSeed: burstSeed, BurstStagger: burstStagger,
	}
	return lastRun{Kind: lastRunScenario, Config: &snap, Phases: []config.Phase{ph},
		RecipeID: newRecipeID()}
}
