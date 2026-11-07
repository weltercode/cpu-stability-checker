package subprocess

import (
	"fmt"
	"math/rand"
	"os"
	"sort"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// P07e S3: the replay slot has two distinct jobs, and the whole point
// of this file is that they never substitute for each other.
//
//   - settings-repeat re-RUNS the recorded geometry (same cores, same
//     seed: fresh workers, fresh lease evidence under a fresh run_id)
//     - it proves nothing about the OLD attempt, and it says so.
//   - observed-order replay verifies an existing journal against the
//     arbiter contract WITHOUT touching hardware: the journaled
//     session_lease stream must be exactly the order the seed and the
//     grant ownership prescribe.
//
// The checks below re-derive that story from the durable manifest.
// The seed-order re-derivation uses Go's math/rand on this repo's
// toolchain (the same source that produced the original schedule);
// journals and manifests share this repo's build identity, and a
// mismatch is reported as a violation - never silently tolerated.

// HandoffExpectation is the durable manifest's view of what a
// handoff run OUGHT to look like: the resolved core set, the block
// geometry that defines the per-block grant ownership (block b belongs
// to slot b/per), the round count and the seed that ordered every
// round. nil expectation = verify the arbiter invariants only; bound
// expectation = also verify grant ownership and seed order.
type HandoffExpectation struct {
	Cores  []int
	Blocks int
	Rounds int
	Seed   uint64
}

// HandoffReplay is the verdict over one journaled handoff generation.
type HandoffReplay struct {
	RunID             string
	ManifestBound     bool
	Tried             int    // lease-write records observed
	Done              int    // acked lease-read records (full handoffs completed)
	Unfinished        int    // blocks published but never verified-read (fault/cancel tails)
	Fault             string // the failed lease's error ("" = none observed)
	FaultCore         int    // core of the failed lease (-1 = none)
	RecordedExit      int    // the journaled run_end exit of THIS generation
	RecordedExitKnown bool   // false = no run_end was journaled (refused earlier as truncation)
	Checks            uint64 // summed READ_OK counts (only meaningful when ChecksKnown)
	ChecksKnown       bool   // false once any acked read omitted its count
	Violations        []string
}

// VerifyHandoffJournal replays the observed lease order of ONE
// handoff generation in the journal at path. Generations must never
// mix (the R04c lesson: journals append): with runID "" the journal
// must hold exactly one handoff run, otherwise replay names every
// generation it found and refuses to guess.
func VerifyHandoffJournal(path, runID string, exp *HandoffExpectation) (*HandoffReplay, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read journal: %w", err)
	}
	defer f.Close()
	recs, iss := journal.ParseAll(f, path)
	if iss.Malformed > 0 || iss.Unterminated || iss.ReadErr != "" {
		return nil, fmt.Errorf("journal evidence is not whole: %d malformed record(s), unterminated=%v, read error=%q - replay refuses rather than trusting a torn story", iss.Malformed, iss.Unterminated, iss.ReadErr)
	}

	// Generations: every run_start naming the handoff phase.
	var gens []string
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Type == journal.RunStart && r.Test == "handoff" && !seen[r.RunID] {
			seen[r.RunID] = true
			gens = append(gens, r.RunID)
		}
	}
	if len(gens) == 0 {
		return nil, fmt.Errorf("the journal holds no handoff run: there is no lease story here to replay (unknown is not 'no handoffs happened')")
	}
	if runID == "" {
		if len(gens) > 1 {
			return nil, fmt.Errorf("the journal holds %d handoff generations (%v): replay never mixes generations - name one", len(gens), gens)
		}
		runID = gens[0]
	} else {
		found := false
		for _, g := range gens {
			if g == runID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("run_id %q is not a handoff generation of this journal (found: %v)", runID, gens)
		}
	}

	rep := &HandoffReplay{RunID: runID, ManifestBound: exp != nil, FaultCore: -1, ChecksKnown: true}

	// The run_start of the pinned generation: its cores are the
	// machine shape the run claimed, and when a manifest is bound they
	// must match the manifest's resolved set exactly.
	var journaledCores []int
	for _, r := range recs {
		if r.Type == journal.RunStart && r.RunID == runID {
			journaledCores = r.Cores
			break
		}
	}
	if len(journaledCores) < 2 {
		return nil, fmt.Errorf("run_start of %s names %d cores: a handoff between fewer than two cores proves nothing, and its evidence cannot be replayed", runID, len(journaledCores))
	}

	per := 0
	if exp != nil {
		same := len(exp.Cores) == len(journaledCores)
		if same {
			for i, c := range exp.Cores {
				if journaledCores[i] != c {
					same = false
					break
				}
			}
		}
		if !same {
			rep.Violations = append(rep.Violations, fmt.Sprintf("manifest cores %v differ from the journaled run's cores %v: this journal is not the manifest's run", exp.Cores, journaledCores))
		}
		if exp.Blocks > 0 && exp.Blocks%len(journaledCores) == 0 {
			per = exp.Blocks / len(journaledCores)
		}
	}

	type pending struct {
		gen   uint64
		wcore int
	}
	lastGen := map[uint64]uint64{}
	pd := map[uint64]pending{}
	failed := false
	// The seed-ordered schedule expectation: the k-th lease-write of
	// the generation must be block perm[k] of round k/blocks (bound
	// manifest only).
	var wantOrder []uint64
	if exp != nil {
		rng := rand.New(rand.NewSource(int64(exp.Seed)))
		for r := 0; r < exp.Rounds; r++ {
			for _, b := range rng.Perm(exp.Blocks) {
				wantOrder = append(wantOrder, uint64(b))
			}
		}
	}
	written := 0

	for _, r := range recs {
		if r.RunID != runID {
			continue
		}
		switch r.Type {
		case journal.RunEnd:
			rep.RecordedExit, rep.RecordedExitKnown = r.Exit, true
		case journal.SessionLease:
			if r.SessionLease == nil {
				continue
			}
			obs := r.SessionLease
			core := -1
			if r.Core != nil {
				core = *r.Core
			}
			if failed {
				rep.Violations = append(rep.Violations, fmt.Sprintf("lease block %d gen %d (%s) after a failed lease: the coordinator stops at the first lease failure, so a further lease in the same generation is not this run's story", obs.Block, obs.Gen, obs.Mode))
			}
			switch obs.Mode {
			case "lease-write":
				rep.Tried++
				last, seenGen := lastGen[obs.Block]
				wantGen := uint64(1) // the arbiter's first issued write lease is generation 1
				if seenGen {
					wantGen = last + 1
				}
				if obs.Gen != wantGen {
					rep.Violations = append(rep.Violations, fmt.Sprintf("block %d written at generation %d but the arbiter contract advances a block's generation exactly one per ISSUED write lease (expected %d)", obs.Block, obs.Gen, wantGen))
				}
				lastGen[obs.Block] = obs.Gen
				if per > 0 {
					owner := journaledCores[int(obs.Block)/per]
					if core == owner {
						pd[obs.Block] = pending{obs.Gen, core}
					} else if exp != nil {
						rep.Violations = append(rep.Violations, fmt.Sprintf("block %d written by core %d: the manifest grant puts that block's ownership on core %d (per-lease ownership IS the grant map)", obs.Block, core, owner))
					} else {
						pd[obs.Block] = pending{obs.Gen, core}
					}
				} else {
					pd[obs.Block] = pending{obs.Gen, core}
				}
				if written < len(wantOrder) && wantOrder[written] != obs.Block {
					rep.Violations = append(rep.Violations, fmt.Sprintf("lease #%d is block %d but the manifest seed %d orders block %d at that position: the journaled schedule does not match the recorded seed", written+1, obs.Block, exp.Seed, wantOrder[written]))
				}
				written++
				if !obs.Acked {
					rep.Fault, rep.FaultCore = obs.Error, core
					failed = true
				}
			case "lease-read":
				p, open := pd[obs.Block]
				if !open || p.gen != obs.Gen {
					rep.Violations = append(rep.Violations, fmt.Sprintf("lease-read of block %d at generation %d has no published write at that generation (open=%v gen=%v): the reader was granted data nobody published - the arbiter contract's own evidence is broken", obs.Block, obs.Gen, open, p.gen))
				}
				if per > 0 {
					owner := journaledCores[int(obs.Block)/per]
					if core == owner {
						rep.Violations = append(rep.Violations, fmt.Sprintf("block %d read back by its own grant owner core %d: pickReader never hands a block to the writer", obs.Block, core))
					}
					if obs.PeerCore != nil && *obs.PeerCore != owner {
						rep.Violations = append(rep.Violations, fmt.Sprintf("lease-read of block %d names peer core %d but the grant owner is core %d", obs.Block, *obs.PeerCore, owner))
					}
				}
				if obs.Acked {
					if open {
						delete(pd, obs.Block)
					}
					rep.Done++
					if obs.Checks != nil {
						rep.Checks += *obs.Checks
					} else {
						rep.ChecksKnown = false
					}
				} else {
					rep.Fault, rep.FaultCore = obs.Error, core
					failed = true
				}
			default:
				rep.Violations = append(rep.Violations, fmt.Sprintf("session_lease record with unknown mode %q", obs.Mode))
			}
		}
	}

	if !rep.RecordedExitKnown {
		return nil, fmt.Errorf("generation %s has no run_end: its evidence is truncated mid-story and replay refuses rather than guessing the verdict", runID)
	}
	for range pd {
		rep.Unfinished++
	}
	if written > len(wantOrder) && exp != nil {
		rep.Violations = append(rep.Violations, fmt.Sprintf("%d lease-writes journaled but the manifest plans %d (%d block(s) x %d round(s))", written, len(wantOrder), exp.Blocks, exp.Rounds))
	}
	if exp != nil && !failed && written < len(wantOrder) && rep.RecordedExit != 130 {
		rep.Violations = append(rep.Violations, fmt.Sprintf("only %d of %d planned lease-writes journaled without a failure record: the schedule stopped early with no stated reason", written, len(wantOrder)))
	}
	sort.Strings(rep.Violations)
	return rep, nil
}
