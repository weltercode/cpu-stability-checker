package cli

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// The handoff command refuses corrupted geometry and corrupted core
// lists with exit 2 BEFORE any process spawns (the real two-process
// proofs live in the subprocess package's e2e tests; the cli package
// stays light on purpose). Since S6 the GEOMETRY refusals are proven
// through the public form (the -helper gate is gone); the drifted-
// machine refusal even records the durable manifest before refusing,
// so the pin lives in TestHandoffPublicRunDriftRefusalRecordsManifest
// instead. The chdir keeps the S2 manifests those runs record out of
// the package directory.
func TestHandoffCmdRefusals(t *testing.T) {
	chdirTemp(t)
	jp := filepath.Join(t.TempDir(), "h.log")
	cases := []struct {
		name string
		args []string
	}{
		{"bogus block size", []string{"handoff", "-block-size", "bogus", "-journal", jp}},
		{"bogus cores", []string{"handoff", "-cores", "x", "-journal", jp}},
		{"single core proves nothing", []string{"handoff", "-cores", "0", "-journal", jp}},
		{"duplicate core proves nothing across cores", []string{"handoff", "-cores", "0,0", "-journal", jp}},
		{"ragged block division", []string{"handoff", "-cores", "0,1", "-blocks", "5", "-journal", jp}},
		{"zero rounds", []string{"handoff", "-cores", "0,1", "-rounds", "0", "-journal", jp}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := Run(c.args); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
		})
	}
}

// S6 OPENED the public mode: a bare 'handoff' run is now a tracked
// attempt (session manifest, receipt, lastresult, run_meta). The
// cheapest honest proof WITHOUT spawning workers is a manifest
// naming cpus this machine cannot have: the drift gate refuses
// before any identity exists (no session manifest, no receipt - the
// refuse-before-load discipline every entry point keeps), while the
// durable manifest is already on disk honestly describing the
// ATTEMPTED experiment.
func TestHandoffPublicRunDriftRefusalRecordsManifest(t *testing.T) {
	chdirTemp(t)
	jp := filepath.Join(t.TempDir(), "h.log")
	oldErr := captureStderr(t, func() {
		if code := Run([]string{"handoff", "-cores", "999,998", "-blocks", "4", "-journal", jp}); code != 2 {
			t.Errorf("exit = %d, want 2 (public run open, cpus refused by the drift gate)", code)
		}
	})
	if !strings.Contains(oldErr, "does not exist on this machine") {
		t.Errorf("the public refusal did not name the drift gate:\n%s", oldErr)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatalf("no readable manifest after the public attempt: %v", err)
	}
	if lr.Kind != lastRunHandoff || lr.Handoff == nil || len(lr.Handoff.Cores) != 2 || lr.Handoff.Blocks != 4 {
		t.Errorf("manifest does not describe the attempted experiment: %+v", lr.Handoff)
	}
	// The refusal came BEFORE the session identity: nothing else in
	// the history vocabulary may claim an attempt that never started.
	if _, err := os.Stat(historyDir); !os.IsNotExist(err) {
		t.Errorf("a refused attempt opened a session manifest it never ran: %v", err)
	}
	if _, err := os.Stat(lastResultPath); !os.IsNotExist(err) {
		t.Errorf("a refused attempt wrote an outcome receipt it never produced: %v", err)
	}
}

// A run refused at FLAG level before anything resolved must not
// touch the repeat slot either: the manifest promise belongs to a
// real attempt, not to a command line that never parsed.
func TestHandoffPublicBadFlagsSaveNoManifest(t *testing.T) {
	chdirTemp(t)
	jp := filepath.Join(t.TempDir(), "x.log")
	if code := Run([]string{"handoff", "-cores", "0,1", "-block-size", "bogus", "-journal", jp}); code != 2 {
		t.Fatalf("exit = %d, want 2 (unparseable size)", code)
	}
	if _, err := os.Stat(lastrunPath); !os.IsNotExist(err) {
		t.Errorf("bad flags left a repeat manifest for a run that was never attempted: %v", err)
	}
}

// S2: every executed helper run records its durable manifest BEFORE
// the first load, the same A04 discipline every other entry point
// follows. These cores cannot exist on this machine, so the
// coordinator refuses the run as a setup error (exit 2) before any
// allocation or spawn - cheap and deterministic - and the manifest
// must still be on disk: it honestly describes the ATTEMPTED
// experiment, and its absence would silently erase the promise that
// the journal evidence has a named owner.
func TestHandoffHelperRecordsManifest(t *testing.T) {
	chdirTemp(t)
	jp := filepath.Join(t.TempDir(), "h.log")
	if code := Run([]string{"handoff", "-helper", "-cores", "999,998", "-blocks", "4", "-journal", jp}); code != 2 {
		t.Fatalf("exit = %d, want 2 (nonexistent cpus refused before allocation)", code)
	}
	lr, err := loadLastRun()
	if err != nil {
		t.Fatalf("no readable manifest after the helper run: %v", err)
	}
	if lr.Kind != lastRunHandoff || lr.Handoff == nil {
		t.Fatalf("kind = %q, handoff block = %v: want the durable handoff manifest", lr.Kind, lr.Handoff)
	}
	h := lr.Handoff
	if h.CoresInput != "999,998" || len(h.Cores) != 2 || h.Blocks != 4 ||
		h.BlockBytes != 65536 || h.Rounds != 1 || h.Lock != "off" || h.Journal == "" {
		t.Errorf("manifest drifted from the requested run: %+v", h)
	}
	if lr.RecipeID == "" {
		t.Errorf("manifest carries no recipe id: its journal evidence could never bind to it")
	}
}

// S2: every executed helper run records its durable manifest BEFORE
// S6 (menu repeat, 'handoff -repeat', run -request, validate; the
// offline -replay-journal touches no hardware). The pins that keep
// replays honest: validate resolves the run and its hash, a foreign
// machine's manifest is refused by the drift gate on EVERY public
// entry (never approximated with today's defaults), and a managed
// stream for a run that never started says exactly that.
func TestHandoffPublicReplayOpens(t *testing.T) {
	requireMemoryAdmission(t, 8*65536) // the manifest requests 8 x 64KiB
	chdirTemp(t)
	h := &handoffRecipe{CoresInput: "0,1", Cores: []int{0, 1}, Blocks: 8, BlockBytes: 65536,
		BlockSize: "64KiB", Rounds: 2, Seed: 7, Lock: "off", Journal: "x.log.journal.jsonl"}
	reqPath := writeRequestFile(t, t.TempDir(), lastRun{Version: lastRunVersion, Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: h})

	// validate ACCEPTS the kind now: one run, one plan hash (the same
	// hash the real run journals - pinned against a real run in the
	// public e2e). The full run lifecycle is e2e-proven there; in the
	// light suite validate must not spawn anything, and it does not.
	out := captureStdout(t, func() {
		if code := Run([]string{"validate", "-request", reqPath, "-json"}); code != 0 {
			t.Errorf("validate of a handoff manifest: exit = %d, want 0 (the run mode is open)", code)
		}
	})
	var ans struct {
		OK         bool     `json:"ok"`
		Kind       string   `json:"kind"`
		Runs       int      `json:"runs"`
		PlanHashes []string `json:"plan_hashes"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("validate -json output unreadable: %v\n%s", err, out)
	}
	if !ans.OK || ans.Kind != lastRunHandoff || ans.Runs != 1 || len(ans.PlanHashes) != 1 || len(ans.PlanHashes[0]) != 64 {
		t.Errorf("validate answer wrong: %+v", ans)
	}

	// A manifest recorded on ANOTHER machine: every public replay
	// entry refuses through the drift gate by name - menu repeat and
	// run -request alike - without opening a session or a journal.
	foreign := &handoffRecipe{CoresInput: "999,998", Cores: []int{999, 998}, Blocks: 2, BlockBytes: 65536,
		Rounds: 1, Seed: 3, Lock: "off", Journal: "x.log.journal.jsonl"}
	if err := saveLastRun(lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: foreign}); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	errOut := captureStderr(t, func() {
		if code := repeatLastRun(); code != 2 {
			t.Errorf("menu repeat of a foreign handoff manifest: exit = %d, want 2 (drift refusal, not an approximation)", code)
		}
	})
	if !strings.Contains(errOut, "does not exist on this machine") {
		t.Errorf("menu repeat did not name the drift refusal:\n%s", errOut)
	}
	foreignReq := writeRequestFile(t, t.TempDir(), lastRun{Version: lastRunVersion, Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: foreign})
	errOut = captureStderr(t, func() {
		stream := captureStdout(t, func() {
			if code := Run([]string{"run", "-request", foreignReq, "-events", "jsonl"}); code != 2 {
				t.Errorf("run -request of a foreign manifest: exit = %d, want 2", code)
			}
		})
		if !strings.Contains(stream, `"error"`) || !strings.Contains(stream, "session never started") {
			t.Errorf("the managed stream must report a run that never started, honestly:\n%s", stream)
		}
		if strings.Contains(stream, `"completed"`) {
			t.Errorf("a run that never started cannot end with a completed record:\n%s", stream)
		}
	})
	if !strings.Contains(errOut, "does not exist on this machine") {
		t.Errorf("run -request did not name the drift refusal:\n%s", errOut)
	}
	if _, err := os.Stat(historyDir); !os.IsNotExist(err) {
		t.Errorf("drift-refused replays opened session manifests:\n%v", err)
	}
}

// writeCleanHandoffJournal writes a real two-core handoff journal
// with the journal writer itself (the bytes an executed run leaves),
// in the seed's schedule order, every lease by its grant owner and
// read back by the other core.
func writeCleanHandoffJournal(t *testing.T, path, runID string, seed uint64, endExit *int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	jw := journal.NewWriter(f, runID)
	jw.RunStart("handoff", "", []int{0, 1}, 2, "", true)
	seq := uint64(1)
	for _, b := range rand.New(rand.NewSource(int64(seed))).Perm(2) {
		owner, reader := 0, 1
		if b == 1 {
			owner, reader = 1, 0
		}
		for _, st := range []struct {
			mode string
			core int
			peer int
		}{{"lease-write", owner, reader}, {"lease-read", reader, owner}} {
			peer := st.peer
			jw.SessionLease(st.core, journal.LeaseObservation{Session: runID, Seq: seq, Block: uint64(b), Gen: 1, Mode: st.mode, PeerCore: &peer, Acked: true})
			seq++
		}
	}
	if endExit != nil {
		jw.RunEndValidation(*endExit, "")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// S3 in the internal helper form: -replay-journal verifies journaled
// lease evidence offline. Bound to the durable manifest it checks
// grant ownership and the seed's schedule order too. Replay touches
// no hardware, so these cases stay light in-process.
func TestHandoffReplayHelperForms(t *testing.T) {
	chdirTemp(t)
	const seed = 5
	zero := 0
	good := filepath.Join(t.TempDir(), "good.journal.jsonl")
	writeCleanHandoffJournal(t, good, "run-good", seed, &zero)
	// Bind the manifest slot to the SAME geometry and journal the
	// replayed file tells, so the bound checks must pass.
	if err := saveLastRun(lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: &handoffRecipe{
		CoresInput: "0,1", Cores: []int{0, 1}, Blocks: 2, BlockBytes: 65536, Rounds: 1, Seed: seed, Lock: "off", Journal: good,
	}}); err != nil {
		t.Fatalf("saveLastRun: %v", err)
	}
	if code := Run([]string{"handoff", "-helper", "-replay-journal", good}); code != 0 {
		t.Errorf("clean bounded replay exit = %d, want 0", code)
	}
	trunc := filepath.Join(t.TempDir(), "trunc.journal.jsonl")
	writeCleanHandoffJournal(t, trunc, "run-trunc", seed, nil)
	if code := Run([]string{"handoff", "-helper", "-replay-journal", trunc}); code != 2 {
		t.Errorf("truncated evidence exit = %d, want 2 (refused, not guessed)", code)
	}
	// A second generation in one journal: the two-generation pin.
	two := filepath.Join(t.TempDir(), "two.journal.jsonl")
	writeCleanHandoffJournal(t, two, "run-a", seed, &zero)
	f, err := os.OpenFile(two, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	jw := journal.NewWriter(f, "run-b")
	jw.RunStart("handoff", "", []int{0, 1}, 2, "", true)
	jw.RunEndValidation(0, "")
	f.Close()
	if code := Run([]string{"handoff", "-helper", "-replay-journal", two}); code != 2 {
		t.Errorf("mixed generations must refuse: exit = %d, want 2", code)
	}
	if code := Run([]string{"handoff", "-helper", "-replay-journal", two, "-replay-run", "run-a"}); code != 0 {
		t.Errorf("pinned generation exit = %d, want 0", code)
	}
	// S6: the replay job is PUBLIC (offline, touches no hardware).
	if code := Run([]string{"handoff", "-replay-journal", good}); code != 0 {
		t.Errorf("public replay form exit = %d, want 0", code)
	}
	// Conflicting jobs refuse.
	if code := Run([]string{"handoff", "-helper", "-repeat", "-replay-journal", good}); code != 2 {
		t.Errorf("conflicting replay jobs exit = %d, want 2", code)
	}
	if code := Run([]string{"handoff", "-helper", "-replay-run", "x"}); code != 2 {
		t.Errorf("orphan -replay-run exit = %d, want 2", code)
	}
}

// The settings-repeat gate refuses drifted machines BEFORE spawning
// anything: every case here must end without workers (the success
// path is the coordinator's own e2e, already heavy-proven; -repeat
// calls exactly that entry with the manifest's spec).
func TestHandoffRepeatHelperRefusals(t *testing.T) {
	t.Run("no durable request", func(t *testing.T) {
		chdirTemp(t)
		if code := Run([]string{"handoff", "-helper", "-repeat"}); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
	})
	t.Run("slot holds another kind", func(t *testing.T) {
		chdirTemp(t)
		if err := saveLastRun(lastRun{Kind: lastRunScenario, Config: recipeSnapshot(), Phases: []config.Phase{{Name: "x"}}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if code := Run([]string{"handoff", "-helper", "-repeat"}); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
	})
	t.Run("manifest from a different machine", func(t *testing.T) {
		chdirTemp(t)
		if err := saveLastRun(lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: &handoffRecipe{
			CoresInput: "999,998", Cores: []int{999, 998}, Blocks: 2, BlockBytes: 65536, Rounds: 1, Lock: "off", Journal: "x.journal.jsonl",
		}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if code := Run([]string{"handoff", "-helper", "-repeat"}); code != 2 {
			t.Errorf("exit = %d, want 2 (cores cannot quietly be re-picked)", code)
		}
	})
	t.Run("recorded llc provenance cannot reconfirm", func(t *testing.T) {
		chdirTemp(t)
		// Cores that DO exist here, with recorded llc domains no
		// machine reports: the cross-domain pairing claim cannot be
		// reproduced as described, so repeat refuses rather than
		// silently downgrading to core-only pairing.
		if err := saveLastRun(lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: &handoffRecipe{
			CoresInput: "auto", Cores: []int{0, 1}, Blocks: 2, BlockBytes: 65536, Rounds: 1, Lock: "off",
			Journal: "x.journal.jsonl", LLCDomainsSeen: 2, LLCByCore: map[int]int{0: 4242, 1: 4243},
		}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if code := Run([]string{"handoff", "-helper", "-repeat"}); code != 2 {
			t.Errorf("exit = %d, want 2 (topology drift refuses, never re-picks)", code)
		}
	})
	t.Run("public repeat refuses a foreign machine by name", func(t *testing.T) {
		// S6 opened the public repeat; what stays closed is running
		// a foreign machine's experiment: the drift gate refuses
		// before any session identity exists, naming the cpu.
		chdirTemp(t)
		if err := saveLastRun(lastRun{Kind: lastRunHandoff, Config: recipeSnapshot(), Handoff: &handoffRecipe{
			CoresInput: "999,998", Cores: []int{999, 998}, Blocks: 2, BlockBytes: 65536, Rounds: 1, Lock: "off", Journal: "x.journal.jsonl",
		}}); err != nil {
			t.Fatalf("save: %v", err)
		}
		oldErr := captureStderr(t, func() {
			if code := Run([]string{"handoff", "-repeat"}); code != 2 {
				t.Errorf("exit = %d, want 2 (cores cannot quietly be re-picked on the public path either)", code)
			}
		})
		if !strings.Contains(oldErr, "does not exist on this machine") {
			t.Errorf("the public repeat did not name the drift refusal:\n%s", oldErr)
		}
	})
}
