package cli

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// wizardTestTopology is two single-thread physical cores: enough for
// the session wizard to build runs without depending on the real
// machine (and without an SMT prompt in the scripted input).
func wizardTestTopology() sysinfo.Topology {
	return sysinfo.Topology{Source: "test", PhysicalCores: []sysinfo.PhysicalCore{
		{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}},
		{Index: 1, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 1}}},
	}}
}

// registerRAMWizardFake adds a workload that promises memory regions
// without touching any: the wizard only reads RequiresMemory.
func registerRAMWizardFake(t *testing.T) {
	t.Helper()
	workload.Register(&workload.Workload{
		Name:           "ram-wizard-fake",
		Description:    "test stand-in that promises memory-engine regions",
		RequiresMemory: true,
		Run:            func(context.Context, workload.Reporter) (uint64, error) { return 0, nil },
	})
	t.Cleanup(func() { workload.Delete("ram-wizard-fake") })
}

func wizardTestConfig() config.Config {
	cfg := config.Default()
	cfg.WorkloadA, cfg.Duration, cfg.LogFile = "ram-wizard-fake", "10ms", "wiz.log"
	return cfg
}

// Audit A03 + U01: a RAM-family workload chosen in the wizard gets
// its per-process mode from the auto resolver WITHOUT a question (the
// mode is a mechanism, not a user goal), while its memory budget is
// still collected IN the form - an invalid answer re-prompts with the
// reason instead of reaching the executor's preflight refusal.
func TestConfigureSessionAsksRAMRequirements(t *testing.T) {
	registerRAMWizardFake(t)

	t.Run("auto resolver picks subprocess without asking; budget is collected", func(t *testing.T) {
		// mode default (per-core) | workload default (ram fake) |
		// cores all | duration | passes default |
		// budget "banana" (invalid, re-prompted) | budget auto | confirm y
		in := bufio.NewReader(strings.NewReader("\n\nall\n10ms\n\nbanana\nauto\ny\n"))
		session, ok := configureSession(in, newScreenUI(os.Stdout), wizardTestConfig(), wizardTestTopology())
		if !ok {
			t.Fatal("wizard cancelled a completable RAM session")
		}
		if session.Workload != "ram-wizard-fake" || !session.PerProcess || session.Rotate {
			t.Fatalf("RAM workload did not get the mode that can carry it: %+v", session)
		}
		if session.Memory != "auto" {
			t.Fatalf("budget prompt lost: Memory = %q", session.Memory)
		}
		// U01 provenance: the plan must say WHY this mode, not just which.
		if session.Execution != config.ExecutionAuto || !strings.Contains(session.ExecutionReason, "memory-engine") {
			t.Errorf("execution provenance = %q / %q, want auto + a memory-engine reason",
				session.Execution, session.ExecutionReason)
		}
	})

	t.Run("config budget is the prompt default and survives Enter", func(t *testing.T) {
		cfg := wizardTestConfig()
		cfg.Memory, cfg.MemoryLock, cfg.RamSeed = "64MiB", "try", 123
		// budget answered with plain Enter
		in := bufio.NewReader(strings.NewReader("\n\nall\n10ms\n\n\ny\n"))
		session, ok := configureSession(in, newScreenUI(os.Stdout), cfg, wizardTestTopology())
		if !ok {
			t.Fatal("wizard cancelled")
		}
		if session.Memory != "64MiB" || session.MemoryLock != "try" || session.RamSeed != 123 {
			t.Fatalf("configured RAM settings not carried into the session: %+v", session)
		}
	})

	t.Run("q at the budget prompt cancels without saving or running", func(t *testing.T) {
		in := bufio.NewReader(strings.NewReader("\n\nall\n10ms\n\nq\n"))
		if _, ok := configureSession(in, newScreenUI(os.Stdout), wizardTestConfig(), wizardTestTopology()); ok {
			t.Fatal("q did not cancel the wizard")
		}
	})

	t.Run("q at the workload prompt cancels", func(t *testing.T) {
		in := bufio.NewReader(strings.NewReader("\nq\n"))
		if _, ok := configureSession(in, newScreenUI(os.Stdout), wizardTestConfig(), wizardTestTopology()); ok {
			t.Fatal("q did not cancel the wizard")
		}
	})

	// U01 (menu plan section 4): a settings-level execution override
	// that contradicts the workload's hard requirement is refused
	// BEFORE the confirm step, naming the fix - the wizard does not
	// quietly ignore the override, and the user is not sent into the
	// executor's preflight to discover it.
	t.Run("a contradicting execution override is refused with the fix named", func(t *testing.T) {
		cfg := wizardTestConfig()
		cfg.Execution = config.ExecutionThreaded

		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		os.Stdout = w
		// Drain the pipe CONCURRENTLY with the wizard: the workload
		// menu plus the refusal text exceed the ~8 KiB anonymous pipe
		// buffer, and reading only after the call returns deadlocks
		// the writer (measured: the menu output crossed that line the
		// moment the registry grew another entry).
		var buf bytes.Buffer
		readDone := make(chan struct{})
		go func() {
			buf.ReadFrom(r)
			close(readDone)
		}()
		// the resolver refusal prints, then the wizard waits for Enter
		// (the scripted "x" is that Enter) and goes back
		in := bufio.NewReader(strings.NewReader("\n\nx\n"))
		ok := false
		_, ok = configureSession(in, newScreenUI(os.Stdout), cfg, wizardTestTopology())
		w.Close()
		os.Stdout = old
		<-readDone
		if ok {
			t.Fatal("a threaded override survived a RAM workload")
		}
		if !strings.Contains(buf.String(), "contradicts") || !strings.Contains(buf.String(), "ram-wizard-fake") {
			t.Errorf("refusal does not name the contradiction and the workload:\n%s", buf.String())
		}
	})
}

// A04 on a fresh run: an external workload whose registered tool is
// not on disk returns to the workload list with the reason stated -
// it never reaches the child's setup error after a confirmation.
func TestConfigureSessionExternalMissingReAsks(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone", "tool.exe")
	workload.RegisterExternal(map[string]workload.ExternalSpec{"ext-wiz-fake": {Command: gone}})
	t.Cleanup(func() { workload.Delete("ext-wiz-fake") })

	cfg := config.Default()
	cfg.WorkloadA = "int"
	cfg.External = map[string]workload.ExternalSpec{"ext-wiz-fake": {Command: gone}}

	// mode default | workload "1" (ext-wiz-fake sorts first) ->
	// missing tool, re-ask | q cancels the re-ask
	in := bufio.NewReader(strings.NewReader("\n1\nq\n"))
	if _, ok := configureSession(in, newScreenUI(os.Stdout), cfg, wizardTestTopology()); ok {
		t.Fatal("a missing external tool did not return to the workload list")
	}
}
