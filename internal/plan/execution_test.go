package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// registerExecFake adds a registry entry with the given requirements
// so the resolver matrix can test ram-* and single-instance rules
// without touching real engines.
func registerExecFake(t *testing.T, name string, requiresMemory, singleInstance bool) {
	t.Helper()
	workload.Register(&workload.Workload{
		Name:           name,
		Description:    "resolver test stand-in",
		RequiresMemory: requiresMemory,
		SingleInstance: singleInstance,
		Run:            func(context.Context, workload.Reporter) (uint64, error) { return 0, nil },
	})
	t.Cleanup(func() { workload.Delete(name) })
}

// U01 (menu plan section 4): the worker mode is policy, resolved from
// the workload's hard requirements - and an override that
// contradicts a requirement is refused with the fix named, never
// quietly switched.
func TestResolveExecutionMatrix(t *testing.T) {
	registerExecFake(t, "exec-fake-ram", true, false)
	registerExecFake(t, "exec-fake-single", false, true)

	cases := []struct {
		name       string
		policy     string
		a, b       string
		bursts     int
		wantProc   bool
		wantRotate bool
		wantReason string // substring of the decision reason
		wantErr    string // substring of the refusal
	}{
		{name: "auto plain cpu", policy: "auto", a: "int", wantReason: "default"},
		{name: "empty policy is auto", policy: "", a: "int", wantReason: "default"},
		{name: "auto ram picks subprocess", policy: "auto", a: "exec-fake-ram", wantProc: true, wantReason: "memory-engine"},
		{name: "auto ram in slot B too", policy: "auto", a: "int", b: "exec-fake-ram", wantProc: true, wantReason: "memory-engine"},
		{name: "auto single instance picks threaded", policy: "auto", a: "exec-fake-single", wantReason: "single-instance"},
		{name: "auto bursts pick subprocess", policy: "auto", a: "int", bursts: 3, wantProc: true, wantReason: "bursts"},
		{name: "override threaded plain", policy: "threaded", a: "int", wantReason: "explicit override"},
		{name: "override threaded ram refused", policy: "threaded", a: "exec-fake-ram", wantErr: "contradicts exec-fake-ram"},
		{name: "override threaded bursts refused", policy: "threaded", a: "int", bursts: 2, wantErr: "contradicts bursts"},
		{name: "override subprocess", policy: "subprocess", a: "int", wantProc: true, wantReason: "explicit override"},
		{name: "override rotate", policy: "rotate", a: "int", wantProc: true, wantRotate: true, wantReason: "rotating"},
		{name: "override rotate bursts refused", policy: "rotate", a: "int", bursts: 2, wantErr: "contradicts bursts"},
		{name: "unknown policy refused", policy: "teleport", a: "int", wantErr: "not one of auto"},
		// ram + bursts: the burst rule already gives subprocess, so
		// the ram requirement is satisfied either way.
		{name: "auto ram bursts", policy: "auto", a: "exec-fake-ram", bursts: 2, wantProc: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := ResolveExecution(tc.policy, tc.a, tc.b, tc.bursts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dec.PerProcess != tc.wantProc || dec.Rotate != tc.wantRotate {
				t.Errorf("decision = %+v, want perProcess=%t rotate=%t", dec, tc.wantProc, tc.wantRotate)
			}
			if tc.wantReason != "" && !strings.Contains(dec.Reason, tc.wantReason) {
				t.Errorf("reason %q lacks %q: 'why this mode' must be data", dec.Reason, tc.wantReason)
			}
		})
	}
}

// The config vocabulary and the resolver vocabulary are one list: a
// value the config accepts must be a value the resolver understands.
func TestExecutionVocabularyMatchesConfig(t *testing.T) {
	for _, v := range []string{config.ExecutionAuto, config.ExecutionThreaded,
		config.ExecutionSubprocess, config.ExecutionRotate} {
		if _, err := ResolveExecution(v, "int", "", 0); err != nil {
			t.Errorf("config value %q refused by the resolver: %v", v, err)
		}
	}
}
