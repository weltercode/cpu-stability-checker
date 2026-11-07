package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// The memory-engine knobs are validated at config resolution: a
// malformed budget or lock mode is a visible setup error, never a
// silent fallback to auto or off. Empty memory means the engine
// stays off - existing runs keep their exact behavior until someone
// asks for memory on purpose.
func TestOptionsFromConfigMemoryKnobs(t *testing.T) {
	tests := []struct {
		name     string
		memory   string
		lock     string
		wantMem  string
		wantLock string
		wantErr  string
	}{
		{name: "off by default", memory: "", lock: "", wantMem: "", wantLock: ""},
		{name: "auto", memory: "auto", lock: "", wantMem: "auto", wantLock: ""},
		{name: "fixed size", memory: "2GiB", lock: "off", wantMem: "2GiB", wantLock: "off"},
		{name: "lock try", memory: "1GiB", lock: "try", wantMem: "1GiB", wantLock: "try"},
		{name: "lock required", memory: "1GiB", lock: "required", wantMem: "1GiB", wantLock: "required"},
		{name: "malformed size", memory: "2xGiB", wantErr: "memory"},
		{name: "zero size", memory: "0", wantErr: "memory"},
		{name: "made-up size", memory: "lots", wantErr: "memory"},
		{name: "made-up lock", memory: "1GiB", lock: "yes", wantErr: "memory_lock"},
		{name: "wrong-case lock", memory: "1GiB", lock: "Required", wantErr: "memory_lock"},
		{name: "bad lock even with engine off", lock: "definitely", wantErr: "memory_lock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Memory = tt.memory
			cfg.MemoryLock = tt.lock
			opt, err := OptionsFromConfig(cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("OptionsFromConfig(memory=%q, lock=%q) err = %v, want one containing %q",
						tt.memory, tt.lock, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OptionsFromConfig: %v", err)
			}
			if opt.Memory != tt.wantMem || opt.MemLock != tt.wantLock {
				t.Errorf("opt.Memory/MemLock = %q/%q, want %q/%q",
					opt.Memory, opt.MemLock, tt.wantMem, tt.wantLock)
			}
		})
	}
}

// PreflightMemoryWorkloads refuses, before anything starts, a run that
// selected a memory-engine workload but cannot provide the engine:
// no budget, or in-process mode. The fake name is registered only
// here - the real ram-* family lands in P06-C3..C6 - but the
// mechanism must already reject exactly, including the B slot.
func TestPreflightMemoryWorkloads(t *testing.T) {
	workload.Register(&workload.Workload{
		Name:           "ram-preflight-fake",
		RequiresMemory: true,
		Run:            func(context.Context, workload.Reporter) (uint64, error) { return 0, nil },
	})
	t.Cleanup(func() { workload.Delete("ram-preflight-fake") })

	tests := []struct {
		name    string
		opt     Options
		wantErr string
	}{
		{"needs a budget", Options{A: "ram-preflight-fake"}, "need the memory engine"},
		{"needs per-process", Options{A: "ram-preflight-fake", Memory: "1GiB"}, "per-process"},
		{"satisfied", Options{A: "ram-preflight-fake", Memory: "1GiB", PerProcess: true}, ""},
		{"B slot checked too", Options{B: "ram-preflight-fake", Memory: "1GiB"}, "per-process"},
		{"ordinary workloads unaffected", Options{A: "float", B: "mem"}, ""},
		{"idle is not a workload", Options{A: "idle", B: "ram-preflight-fake", Memory: "1GiB", PerProcess: true}, ""},
		// Audit C01 closeout: one-shot memory with threads is legal BY
		// OWNERSHIP (per-thread grant sets); sessions are single-engine
		// per core BY CONTRACT (P08 serves bursts one at a time, P07e
		// ownership is the per-block grant plus the parent arbiter), so
		// workers>1 in a burst run is a corrupted request shape for ANY
		// workload - the extra threads would be parsed and never run.
		{"one-shot memory with threads is legal", Options{A: "ram-preflight-fake", Memory: "1GiB", PerProcess: true, Workers: 2}, ""},
		{"one-shot built-in mem with an engine and threads", Options{A: "mem", Memory: "1GiB", PerProcess: true, Workers: 2}, ""},
		{"bursts with memory threads stay refused", Options{A: "ram-preflight-fake", Memory: "1GiB", PerProcess: true, Workers: 2, Bursts: 3}, "audit C01"},
		{"bursts with built-in mem and engine refused too", Options{A: "mem", Memory: "1GiB", PerProcess: true, Workers: 2, Bursts: 3}, "audit C01"},
		{"bursts with a CPU workload and threads are corrupt contract too", Options{A: "float", Workers: 2, Bursts: 3}, "audit C01"},
		{"bursts with a single thread legal", Options{A: "ram-preflight-fake", Memory: "1GiB", PerProcess: true, Workers: 1, Bursts: 3}, ""},
		{"built-in mem without an engine keeps private buffers", Options{A: "mem", Workers: 2}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PreflightMemoryWorkloads(tt.opt)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("PreflightMemoryWorkloads: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("PreflightMemoryWorkloads err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// The seed knob rides config -> Options unchanged: whatever the
// budget partition ends up doing, the seed itself must not be
// re-derived from worker counts or region sizes.
func TestOptionsFromConfigRamSeed(t *testing.T) {
	cfg := config.Default()
	cfg.RamSeed = 0xDEADBEEF
	opt, err := OptionsFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if opt.RamSeed != 0xDEADBEEF {
		t.Errorf("opt.RamSeed = %d, want the configured 0xDEADBEEF", opt.RamSeed)
	}
}
