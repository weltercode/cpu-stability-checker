package subprocess

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/memtest"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// testedBytes is the panel's "tested" segment source: the exact grant
// envelope the parent computed once (the same numbers the children's
// mem_done evidence reports), summed. A nil plan reads as 0 so the
// panel can omit the segment instead of claiming a zero-byte test.
func TestTestedBytesSum(t *testing.T) {
	var nilPlan *memPlanSpec
	if nilPlan.testedBytes() != 0 {
		t.Error("nil plan must read as 0 (no tested region)")
	}
	mp := &memPlanSpec{grants: [][]memtest.Grant{
		{{Worker: 0, Region: 0, Role: memtest.RoleSource, Requested: 4096},
			{Worker: 0, Region: 1, Role: memtest.RoleDest, Requested: 8192}},
		{{Worker: 1, Region: 0, Role: memtest.RoleSource, Requested: 12288}},
		{},
	}}
	if got := mp.testedBytes(); got != 4096+8192+12288 {
		t.Errorf("testedBytes = %d, want %d", got, 4096+8192+12288)
	}
}

func TestParseMemGrantRoundTrip(t *testing.T) {
	good := []struct {
		s    string
		want memtest.Grant
	}{
		{"0:0:source:0:4096", memtest.Grant{Worker: 0, Region: 0, Role: memtest.RoleSource, Offset: 0, Requested: 4096}},
		{"3:1:destination:1048576:1073741824", memtest.Grant{Worker: 3, Region: 1, Role: memtest.RoleDest, Offset: 1 << 20, Requested: 1 << 30}},
		{"7:2:control:4096:4096", memtest.Grant{Worker: 7, Region: 2, Role: memtest.RoleControl, Offset: 4096, Requested: 4096}},
	}
	for _, c := range good {
		got, err := parseMemGrant(c.s)
		if err != nil {
			t.Errorf("parseMemGrant(%q): %v", c.s, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMemGrant(%q) = %+v, want %+v", c.s, got, c.want)
		}
		// The wire format must round-trip: what the parent formats,
		// the child parses back identical, or the argv contract drifts.
		again, err := parseMemGrant(formatMemGrant(got))
		if err != nil || again != got {
			t.Errorf("round trip of %+v = %+v, %v", got, again, err)
		}
	}

	bad := []string{
		"",                                     // empty
		"0:0:source:0",                         // too few fields
		"0:0:source:0:4096:9",                  // too many fields
		"0:0:bogus:0:4096",                     // unknown role is refused, not defaulted
		"0:0:Source:0:4096",                    // wrong case: a typo, not a quiet match
		"-1:0:source:0:4096",                   // negative worker
		"0:0:source:0:0",                       // zero size
		"0:0:source:-4096:4096",                // negative offset
		"0:0:source:0:notanumber",              // bad length
		"0:0:source:0:99999999999999999999999", // length overflow past uint64
	}
	for _, s := range bad {
		if g, err := parseMemGrant(s); err == nil {
			t.Errorf("parseMemGrant(%q) = %+v, want an error", s, g)
		}
	}
}

func TestWorkerArgsMemFlags(t *testing.T) {
	mem := &memPlanSpec{
		budgetBytes: 1 << 20,
		allowance:   65536,
		lock:        "try",
		ramSeed:     42,
		grants: [][]memtest.Grant{{
			{Worker: 0, Region: 0, Role: memtest.RoleSource, Offset: 0, Requested: 524288},
			{Worker: 0, Region: 1, Role: memtest.RoleDest, Offset: 524288, Requested: 524288},
		}},
	}
	got := workerArgs(0, "float", time.Second, 0, 0, "j.jsonl", "rid", 1, mem)
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"--mem-budget 1048576", "--mem-allowance 65536", "--mem-lock try", "--ram-seed 42",
		"--mem-grant 0:0:source:0:524288", "--mem-grant 0:1:destination:524288:524288",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("workerArgs with mem plan missing %q in %q", want, joined)
		}
	}

	// forSlot must carry the seed too: narrowing the plan to one
	// child may drop other children's grants, never the seed.
	if fs := mem.forSlot(0); fs == nil || fs.ramSeed != 42 {
		t.Errorf("forSlot lost the seed: %+v", fs)
	}

	// The grants in the argv must parse back to exactly the ones the
	// parent partitioned: the child trusts nothing else.
	var rep grantFlag
	for i, a := range got {
		if a == "--mem-grant" {
			if err := rep.Set(got[i+1]); err != nil {
				t.Fatalf("re-parsing emitted grant %q: %v", got[i+1], err)
			}
		}
	}
	if len(rep) != 2 || rep[0] != mem.grants[0][0] || rep[1] != mem.grants[0][1] {
		t.Errorf("grants did not survive the command line: %+v", rep)
	}

	// Engine off: no mem flags at all, old command lines unchanged.
	off := strings.Join(workerArgs(3, "float", time.Second, 0, 0, "j.jsonl", "rid", 2, nil), " ")
	if strings.Contains(off, "--mem-") {
		t.Errorf("engine off must emit no mem flags: %q", off)
	}
}

// A tampered or corrupted command line must be refused BEFORE any
// allocation: the child re-validates the grants it received, and a
// refusal is a setup error (exit 2), never a CPU fault (exit 1).
func TestWorkerCmdMemRejectsTamperedGrants(t *testing.T) {
	restoreProcessState(t)
	tests := []struct {
		name string
		args []string
	}{
		{"malformed grant", []string{"--mem-grant", "0:0:source:0"}},
		{"unknown role", []string{"--mem-grant", "0:0:bogus:0:4096"}},
		{"zero size", []string{"--mem-grant", "0:0:source:0:0"}},
		{"bad lock mode", []string{"--mem-lock", "yes", "--mem-grant", "0:0:source:0:4096"}},
		{"overlapping ranges", []string{
			"--mem-budget", "65536",
			"--mem-grant", "0:0:source:0:4096",
			"--mem-grant", "0:1:destination:0:4096",
		}},
		{"grant beyond budget", []string{
			"--mem-budget", "4096",
			"--mem-grant", "0:0:source:0:8192",
		}},
		{"grants without a budget", []string{
			"--mem-grant", "0:0:source:0:4096",
			"--mem-grant", "0:1:destination:4096:4096",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journalPath := filepath.Join(t.TempDir(), "j.jsonl")
			args := append([]string{
				"--core", "0", "--workload", "float", "--duration", "300ms",
				"--journal", journalPath, "--run-id", "rid-tamper",
			}, tt.args...)
			if code := WorkerCmd(args); code != 2 {
				t.Fatalf("WorkerCmd() code = %d, want 2 for %s", code, tt.name)
			}
		})
	}
}

// Grants that pass validation reach the real allocator: the child
// commits, first-touches, runs, and releases everything. Exit 0 and
// a clean core_done prove the full lifecycle on the real platform.
func TestWorkerCmdMemOpensAndReleases(t *testing.T) {
	restoreProcessState(t)
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "float", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-mem",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--mem-grant", "0:0:source:0:65536",
		"--mem-grant", "0:1:destination:65536:65536",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	// The accounting must ride into the journal exactly as the OS
	// confirmed it: the float workload verified none of the engine
	// memory, and Verified: 0 says that plainly instead of hiding.
	m := rep.Cores[0].Mem
	if m == nil {
		t.Fatal("mem_done record missing: the engine ran and must be accounted")
	}
	if m.Requested != 131072 || m.Touched != 131072 {
		t.Errorf("requested/touched = %d/%d, want 131072/131072", m.Requested, m.Touched)
	}
	if m.Allocated < m.Requested {
		t.Errorf("allocated %d below requested %d", m.Allocated, m.Requested)
	}
	if m.Verified != 0 {
		t.Errorf("verified = %d, want 0: the float workload checked no engine bytes", m.Verified)
	}
	if m.LockMode != "off" || m.Locked != 0 {
		t.Errorf("lock fields wrong: %+v", m)
	}
	// float consumes no seed: the journal must say unknown (nil),
	// not zero, or a reader would think a seed-driven kernel ran.
	if m.Seed != nil {
		t.Errorf("seed recorded for a workload that never consumes one: %d", *m.Seed)
	}
}

// A seed-consuming workload (RequiresMemory, the ram-* contract) must
// find the configured seed in the child and carry it into mem_done:
// the run is only reproducible if the journal says which seed produced
// the data. 4242 is arbitrary but must survive argv untouched.
func TestWorkerCmdMemSeedJournaled(t *testing.T) {
	restoreProcessState(t)
	workload.Register(&workload.Workload{
		Name:           "ram-seed-fake",
		RequiresMemory: true,
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			for ctx.Err() == nil {
				rep.Add(1)
				rep.Beat()
				time.Sleep(2 * time.Millisecond)
			}
			return 0, nil
		},
	})
	t.Cleanup(func() { workload.Delete("ram-seed-fake") })
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "ram-seed-fake", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-seed",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--ram-seed", "4242",
		"--mem-grant", "0:0:source:0:65536",
		"--mem-grant", "0:1:destination:65536:65536",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v, want core 0 ok", rep.Cores)
	}
	m := rep.Cores[0].Mem
	if m == nil {
		t.Fatal("mem_done missing")
	}
	if m.Seed == nil || *m.Seed != 4242 {
		t.Errorf("seed in mem_done = %v, want 4242", m.Seed)
	}
}

// A budget too small to give this slot any region still runs the
// core - but says out loud that it is memory-less, never silently.
func TestWorkerCmdMemSkippedSlotSpeaksUp(t *testing.T) {
	restoreProcessState(t)
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "float", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-skip",
		"--mem-budget", "1048576", "--mem-allowance", "0", "--mem-lock", "off",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0 (the core still runs its workload)", code)
	}
}

// End to end through a real child process: the parent computes the
// budget once, partitions it, and the child re-validates, commits,
// first-touches and releases. The fixed 128 MiB request must be
// honored exactly - never quietly reduced - and the mem workload
// must verify every engine byte it was given, with the whole story
// landing in the journal.
func TestExecuteSubprocessMemoryEndToEnd(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	cfg := config.Default()
	if err := cfg.Save(config.DefaultPath); err != nil {
		t.Fatal(err)
	}
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "mem", B: "idle",
		Workers: 1, Duration: 500 * time.Millisecond, PerProcess: true,
		ControlCore: -1, LogFile: filepath.Join(dir, "run.log"),
		Memory: "128MiB"}
	res := ExecuteSubprocessWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("memory run exit = %d, want 0: %+v", res.Exit, res)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("memory run journal lost the story: %+v", rep)
	}
	m := rep.Cores[0].Mem
	if m == nil {
		t.Fatal("no mem_done record: the engine ran and its numbers must be in the journal")
	}
	// 128 MiB budget, one child: 64 MiB service allowance carved
	// out, 64 MiB of data split source+destination. The mem kernel
	// verifies every data byte every pass.
	const data = 64 << 20
	if m.Requested != data || m.Touched != data || m.Verified != data {
		t.Errorf("accounting = requested %d / touched %d / verified %d, want all %d",
			m.Requested, m.Touched, m.Verified, data)
	}
	if m.Allocated < m.Requested {
		t.Errorf("allocated %d below requested %d", m.Allocated, m.Requested)
	}
	if rep.Cores[0].Checks == nil || *rep.Cores[0].Checks == 0 {
		t.Errorf("mem iterations missing from verified checks: %+v", rep.Cores[0])
	}
}

// A ram kernel that ran clean must still leave evidence (Total 0 is
// a result), and its declared pass coverage must land in mem_done as
// unique-vs-total byte counts - the plan's two separate numbers.
func TestWorkerCmdRamEvidenceCleanRun(t *testing.T) {
	restoreProcessState(t)
	workload.Register(&workload.Workload{
		Name: "ram-clean-fake", RequiresMemory: true, KernelVersion: "ram-clean-fake-v1",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rr, ok := rep.(workload.RamReporter)
			if !ok {
				return 0, fmt.Errorf("reporter lost RamReporter")
			}
			rr.RamPass(65536)
			rr.RamPass(65536)
			return 2, nil
		},
	})
	t.Cleanup(func() { workload.Delete("ram-clean-fake") })
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "ram-clean-fake", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-ram-clean",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--ram-seed", "9",
		"--mem-grant", "0:0:source:0:65536",
		"--mem-grant", "0:1:destination:65536:65536",
	})
	if code != 0 {
		t.Fatalf("WorkerCmd() code = %d, want 0", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("recovered cores = %+v", rep.Cores)
	}
	ev := rep.Cores[0].Ram
	if ev == nil {
		t.Fatal("clean ram run lost its evidence: nil reads as unknown, not clean")
	}
	if ev.Total != 0 || ev.Truncated || len(ev.Findings) != 0 {
		t.Errorf("clean evidence wrong: %+v", ev)
	}
	m := rep.Cores[0].Mem
	if m == nil {
		t.Fatal("mem_done missing")
	}
	if m.VerifiedUnique == nil || *m.VerifiedUnique != 65536 {
		t.Errorf("verified_unique = %v, want 65536 (last pass coverage)", m.VerifiedUnique)
	}
	if m.VerifiedTotal == nil || *m.VerifiedTotal != 2*65536 {
		t.Errorf("verified_total = %v, want %d (cumulative across passes)", m.VerifiedTotal, 2*65536)
	}
}

// Twenty observed mismatches must come back as: all twenty counted,
// only the first journal.MaxRamFindings detailed, truncation flagged
// out loud - and every finding carrying the XOR the reproduction
// needs, computed by the reporter so a misreported pair cannot hide
// which bits differ.
func TestWorkerCmdRamEvidenceCappedAndCounted(t *testing.T) {
	restoreProcessState(t)
	workload.Register(&workload.Workload{
		Name: "ram-dirty-fake", RequiresMemory: true, KernelVersion: "ram-dirty-fake-v1",
		Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
			rr, ok := rep.(workload.RamReporter)
			if !ok {
				return 0, fmt.Errorf("reporter lost RamReporter")
			}
			for i := range 20 {
				rr.ReportRamMismatch(workload.RamMismatch{
					Algorithm: "ram-dirty-fake-v1", Seed: 9, Pass: 1,
					Region: "worker=0 region=0 role=source offset=0",
					Offset: uint64(i) * 4, Expected: 0xAA, Actual: 0xAB,
					Phase: "verify-fwd",
				})
			}
			return 0, fmt.Errorf("injected ram mismatch")
		},
	})
	t.Cleanup(func() { workload.Delete("ram-dirty-fake") })
	journalPath := filepath.Join(t.TempDir(), "j.jsonl")

	code := WorkerCmd([]string{
		"--core", "0", "--workload", "ram-dirty-fake", "--duration", "300ms",
		"--journal", journalPath, "--run-id", "rid-ram-dirty",
		"--mem-budget", "131072", "--mem-allowance", "0", "--mem-lock", "off",
		"--ram-seed", "9",
		"--mem-grant", "0:0:source:0:65536",
		"--mem-grant", "0:1:destination:65536:65536",
	})
	if code != 1 {
		t.Fatalf("WorkerCmd() code = %d, want 1 (a real mismatch is a fault)", code)
	}
	rep := readRecoveredRun(t, journalPath)
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.Fault {
		t.Fatalf("recovered cores = %+v, want fault", rep.Cores)
	}
	ev := rep.Cores[0].Ram
	if ev == nil {
		t.Fatal("ram evidence missing from the fault record")
	}
	if ev.Total != 20 {
		t.Errorf("total = %d, want all 20 observations counted", ev.Total)
	}
	if len(ev.Findings) != journal.MaxRamFindings {
		t.Errorf("detailed findings = %d, want the cap %d", len(ev.Findings), journal.MaxRamFindings)
	}
	if !ev.Truncated {
		t.Error("truncation not flagged: dropping details must be visible")
	}
	f := ev.Findings[0]
	if f.Xor != 0xAA^0xAB || f.Expected != 0xAA || f.Actual != 0xAB {
		t.Errorf("finding values/XOR wrong: %+v", f)
	}
	if f.Seed != 9 || f.Algorithm != "ram-dirty-fake-v1" || f.Phase != "verify-fwd" {
		t.Errorf("finding identity wrong: %+v", f)
	}
	if f.LogicalCPU != 0 || f.PhysicalCPU != nil {
		t.Errorf("cpu fields wrong: logical=%d physical=%v (physical must stay unknown here)", f.LogicalCPU, f.PhysicalCPU)
	}
	if f.When == "" {
		t.Error("finding lost its observation timestamp")
	}
}

// The real ram-constant kernel through a real child process: the
// parent partitions a fixed 128 MiB budget, the child opens and
// first-touches the regions, the kernel verifies every byte every
// pass, and clean evidence + coverage land in the journal.
func TestExecuteSubprocessRamConstantE2E(t *testing.T) {
	dir := chdirTemp(t)
	useTestBinaryAsWorker(t)
	cfg := config.Default()
	if err := cfg.Save(config.DefaultPath); err != nil {
		t.Fatal(err)
	}
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "ram-constant", B: "idle",
		Workers: 1, Duration: 500 * time.Millisecond, PerProcess: true,
		ControlCore: -1, LogFile: filepath.Join(dir, "run.log"),
		Memory: "128MiB", RamSeed: 0xABCD}
	res := ExecuteSubprocessWithResult(opt)
	if res.Exit != 0 {
		t.Fatalf("ram-constant run exit = %d, want 0: %+v", res.Exit, res)
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
		t.Fatalf("ram-constant journal lost the story: %+v", rep)
	}
	ev := rep.Cores[0].Ram
	if ev == nil {
		t.Fatal("clean ram run must carry Total-0 evidence, not nil")
	}
	if ev.Total != 0 || ev.Truncated {
		t.Errorf("clean evidence wrong: %+v", ev)
	}
	m := rep.Cores[0].Mem
	const data = 64 << 20
	if m == nil || m.VerifiedUnique == nil || *m.VerifiedUnique != data {
		t.Errorf("verified_unique = %v, want %d (every engine data byte per pass)",
			func() any {
				if m != nil {
					return m.VerifiedUnique
				}
				return nil
			}(), data)
	}
	if m != nil && m.VerifiedTotal != nil && *m.VerifiedTotal < data {
		t.Errorf("verified_total = %d, want at least one pass of %d", *m.VerifiedTotal, data)
	}
	if m != nil && (m.Seed == nil || *m.Seed != 0xABCD) {
		t.Errorf("seed = %v, want the configured 0xABCD", func() any {
			if m != nil {
				return m.Seed
			}
			return nil
		}())
	}
	if rep.Cores[0].Checks == nil || *rep.Cores[0].Checks == 0 {
		t.Errorf("pattern passes missing from verified checks: %+v", rep.Cores[0])
	}
}

// The other two ram kernels through the full child-process chain:
// same budget, same evidence contract, kernel-specific story.
func TestExecuteSubprocessRamKernelsE2E(t *testing.T) {
	for _, wl := range []string{"ram-address", "ram-moving", "ram-random"} {
		t.Run(wl, func(t *testing.T) {
			dir := chdirTemp(t)
			useTestBinaryAsWorker(t)
			cfg := config.Default()
			if err := cfg.Save(config.DefaultPath); err != nil {
				t.Fatal(err)
			}
			opt := plan.Options{TestName: "general", Cores: []int{0}, A: wl, B: "idle",
				Workers: 1, Duration: 500 * time.Millisecond, PerProcess: true,
				ControlCore: -1, LogFile: filepath.Join(dir, "run.log"),
				Memory: "128MiB", RamSeed: 0x1234}
			res := ExecuteSubprocessWithResult(opt)
			if res.Exit != 0 {
				t.Fatalf("%s run exit = %d, want 0: %+v", wl, res.Exit, res)
			}
			rep := readRecoveredRun(t, journal.Path(opt.LogFile))
			if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.OK {
				t.Fatalf("%s journal lost the story: %+v", wl, rep)
			}
			ev := rep.Cores[0].Ram
			if ev == nil || ev.Total != 0 {
				t.Errorf("%s: clean run must carry Total-0 evidence: %+v", wl, ev)
			}
			m := rep.Cores[0].Mem
			const data = 64 << 20
			if m == nil || m.VerifiedUnique == nil || *m.VerifiedUnique != data {
				t.Errorf("%s: verified_unique wrong: %+v", wl, m)
			}
			if rep.Cores[0].Checks == nil || *rep.Cores[0].Checks == 0 {
				t.Errorf("%s: passes missing from verified checks", wl)
			}
		})
	}
}
