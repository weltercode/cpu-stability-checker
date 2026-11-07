package subprocess

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// Observed-order replay (P07e S3) re-derives the lease story from the
// durable manifest and the arbiter contract. These tests build
// journals with the REAL writer (the same bytes a run leaves behind)
// and verify the validator catches exactly the shapes that would
// otherwise pose a broken story as a clean one. No processes spawn:
// replay touches no hardware by contract.
type leaseStep struct {
	block, gen uint64
	mode       string
	core       int // the core the record attributes the lease to
	peer       int
	acked      bool
	errMsg     string
	checks     *uint64
}

func writeHandoffJournal(t *testing.T, path, runID string, cores []int, steps []leaseStep, endExit *int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	jw := journal.NewWriter(f, runID)
	jw.RunStart("handoff", "", cores, len(cores), "", true)
	seq := uint64(1)
	for _, s := range steps {
		peer := s.peer
		obs := journal.LeaseObservation{Session: runID, Seq: seq, Block: s.block, Gen: s.gen, Mode: s.mode, PeerCore: &peer, Acked: s.acked, Checks: s.checks}
		if s.errMsg != "" {
			obs.Error = s.errMsg
		}
		jw.SessionLease(s.core, obs)
		seq++
	}
	if endExit != nil {
		jw.RunEndValidation(*endExit, "")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

// cleanSteps is the honest two-core, two-block, one-round stream in
// the manifest seed's order: every block written by its grant owner,
// read back by the OTHER core, all acked.
func cleanSteps(seed uint64, blocks int, cores []int) []leaseStep {
	per := blocks / len(cores)
	perm := rand.New(rand.NewSource(int64(seed))).Perm(blocks)
	var steps []leaseStep
	for _, b := range perm {
		owner := cores[b/per]
		reader := cores[0]
		if reader == owner {
			reader = cores[1]
		}
		// Real arbiter generations: the first ISSUED write lease of a
		// block is generation 1, and the read lease carries the
		// generation of the write it re-reads.
		steps = append(steps,
			leaseStep{block: uint64(b), gen: 1, mode: "lease-write", core: owner, peer: reader, acked: true},
			leaseStep{block: uint64(b), gen: 1, mode: "lease-read", core: reader, peer: owner, acked: true},
		)
	}
	return steps
}

func TestVerifyHandoffJournal(t *testing.T) {
	const seed = 3
	cores := []int{0, 1}
	exp := func() *HandoffExpectation {
		return &HandoffExpectation{Cores: cores, Blocks: 2, Rounds: 1, Seed: seed}
	}
	zero, one := 0, 1

	journalWith := func(t *testing.T, mutate func([]leaseStep) []leaseStep, endExit *int) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "h.journal.jsonl")
		writeHandoffJournal(t, p, "run-one", cores, mutate(cleanSteps(seed, 2, cores)), endExit)
		return p
	}

	t.Run("clean generation passes", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep { return s }, &zero)
		rep, err := VerifyHandoffJournal(p, "", exp())
		if err != nil {
			t.Fatalf("clean stream refused: %v", err)
		}
		if len(rep.Violations) > 0 {
			t.Fatalf("violations on a clean stream: %v", rep.Violations)
		}
		if rep.Tried != 2 || rep.Done != 2 || rep.Unfinished != 0 || rep.RecordedExit != 0 || !rep.RecordedExitKnown {
			t.Errorf("counters wrong: %+v", rep)
		}
	})

	t.Run("generation without expectation stays honest about what it checked", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep { return s }, &zero)
		rep, err := VerifyHandoffJournal(p, "", nil)
		if err != nil || len(rep.Violations) > 0 || rep.ManifestBound {
			t.Fatalf("unbound replay wrong: err=%v rep=%+v", err, rep)
		}
	})

	t.Run("read before its published write", func(t *testing.T) {
		s := cleanSteps(seed, 2, cores)
		// cleanSteps emits [W(b),R(b),W(c),R(c)]: putting the read of
		// the first pair BEFORE its write breaks the arbiter contract
		// (the reader was granted data nobody published).
		swapped := []leaseStep{s[1], s[0]}
		swapped = append(swapped, s[2:]...)
		p := journalWith(t, func([]leaseStep) []leaseStep { return swapped }, &zero)
		rep, err := VerifyHandoffJournal(p, "", exp())
		if err != nil {
			t.Fatalf("validator errored instead of reporting: %v", err)
		}
		if !anyViolation(rep, "no published write") {
			t.Errorf("missing the arbiter-contract violation: %v", rep.Violations)
		}
	})

	t.Run("owner reading back its own block", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep {
			s[1].core = s[0].core // the read is done by the WRITER: pickReader never does this
			return s
		}, &zero)
		rep, _ := VerifyHandoffJournal(p, "", exp())
		if !anyViolation(rep, "own grant owner") && !anyViolation(rep, "no published write") {
			t.Errorf("reader-identity violation not named: %v", rep.Violations)
		}
	})

	t.Run("generation going backwards", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep {
			// a second acked write of the same block WITHOUT the
			// generation the arbiter would have advanced to
			s = append(s, leaseStep{block: s[0].block, gen: 1, mode: "lease-write", core: s[0].core, peer: s[0].peer, acked: true})
			return s
		}, &zero)
		rep, _ := VerifyHandoffJournal(p, "", exp())
		if !anyViolation(rep, "advances a block's generation") {
			t.Errorf("gen-backwards violation not named: %v", rep.Violations)
		}
	})

	t.Run("lease step after a failed lease", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep {
			s[1].acked = false
			s[1].errMsg = "read lease timed out"
			return s
		}, &one)
		rep, err := VerifyHandoffJournal(p, "", exp())
		if err != nil {
			t.Fatalf("validator errored: %v", err)
		}
		if rep.Fault == "" || rep.FaultCore < 0 {
			t.Errorf("journaled failure not surfaced: %+v", rep)
		}
		if !anyViolation(rep, "after a failed lease") {
			t.Errorf("post-failure lease not named: %v", rep.Violations)
		}
		if rep.RecordedExit != 1 {
			t.Errorf("recorded fault exit %d must stay visible, got %d", 1, rep.RecordedExit)
		}
	})

	t.Run("wrong grant owner writing a block", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep {
			s[0].core, s[1].core = s[1].core, s[0].core // write by the non-owner, read by the owner
			return s
		}, &zero)
		rep, _ := VerifyHandoffJournal(p, "", exp())
		if !anyViolation(rep, "grant puts that block's ownership") {
			t.Errorf("per-lease ownership violation not named: %v", rep.Violations)
		}
	})

	t.Run("schedule order not matching the seed", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep {
			// Emit the two block-pairs in the REVERSE order: with two
			// blocks any seed's perm is the opposite of this order,
			// so the mismatch is guaranteed to be about the seed and
			// nothing else (ownership stays correct pair by pair).
			s2 := append([]leaseStep{}, s[2:]...)
			s2 = append(s2, s[0], s[1])
			return s2
		}, &zero)
		rep, _ := VerifyHandoffJournal(p, "", exp())
		if !anyViolation(rep, "orders block") {
			t.Errorf("seed-order mismatch not named: %v", rep.Violations)
		}
	})

	t.Run("truncated evidence is refused, not guessed", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep { return s }, nil)
		if _, err := VerifyHandoffJournal(p, "", exp()); err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Errorf("missing run_end must refuse as truncation, got %v", err)
		}
	})

	t.Run("malformed tail is refused", func(t *testing.T) {
		p := journalWith(t, func(s []leaseStep) []leaseStep { return s }, &zero)
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		f.WriteString(`{"type":"session_lease"`) // torn mid-line append: a writer died mid-record
		f.Close()
		if _, err := VerifyHandoffJournal(p, "", exp()); err == nil {
			t.Fatal("torn journal accepted")
		}
	})

	t.Run("generations never mix", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "two.journal.jsonl")
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		for _, id := range []string{"run-a", "run-b"} {
			jw := journal.NewWriter(f, id)
			jw.RunStart("handoff", "", cores, 2, "", true)
			steps := cleanSteps(seed, 2, cores)
			seq := uint64(1)
			for _, s := range steps {
				peer := s.peer
				jw.SessionLease(s.core, journal.LeaseObservation{Session: id, Seq: seq, Block: s.block, Gen: s.gen, Mode: s.mode, PeerCore: &peer, Acked: true})
				seq++
			}
			jw.RunEndValidation(0, "")
		}
		f.Close()
		if _, err := VerifyHandoffJournal(p, "", exp()); err == nil || !strings.Contains(err.Error(), "never mixes generations") {
			t.Fatalf("two generations without a run_id must refuse: %v", err)
		}
		rep, err := VerifyHandoffJournal(p, "run-b", exp())
		if err != nil || rep.RunID != "run-b" {
			t.Fatalf("pinned generation refused: %v", err)
		}
	})

	t.Run("cancelled tail is a stated reason not a violation", func(t *testing.T) {
		c130 := 130
		p := filepath.Join(t.TempDir(), "c.journal.jsonl")
		writeHandoffJournal(t, p, "run-c", cores, nil, &c130) // cancelled during prepare: zero lease steps
		rep, err := VerifyHandoffJournal(p, "", exp())
		if err != nil || len(rep.Violations) > 0 {
			t.Fatalf("a zero-step cancelled run is a coherent story: err=%v rep=%+v", err, rep)
		}
		if rep.Tried != 0 || rep.RecordedExit != 130 {
			t.Errorf("cancel counters wrong: %+v", rep)
		}
	})
}

func anyViolation(rep *HandoffReplay, want string) bool {
	for _, v := range rep.Violations {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
