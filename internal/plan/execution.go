package plan

import (
	"fmt"
	"strings"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// ExecutionDecision is the resolver's answer for one run or phase:
// the concrete worker mode plus the reason it chose it. The reason is
// data, not decoration: the plan output and the wizard print it, so
// "why this mode" is never an after-the-fact guess (menu plan section
// 4: the user picks goals, the policy picks the mechanism).
type ExecutionDecision struct {
	PerProcess bool
	Rotate     bool
	Reason     string
}

// ResolveExecution turns the execution policy ("" or auto: the
// resolver decides; threaded / subprocess / rotate: an explicit
// settings-level override) and the workloads' hard requirements into
// one concrete mode BEFORE anything starts:
//
//   - prepared bursts drive per-process workers (the transition
//     lifecycle): auto picks subprocess; a threaded override is
//     refused here instead of dying later in ValidateTransition
//   - ram-* workloads verify data inside memory-engine regions, and
//     the engine allocates in per-process children only: auto picks
//     subprocess without asking (menu plan section 4), while an
//     explicit threaded override is refused HERE with the fix named -
//     not quietly switched, and not left to the executor's preflight
//     after other phases already heated the machine
//   - single-instance tools share one process for the whole machine:
//     auto picks threaded; an explicit subprocess override stays
//     allowed because the executor prints its visible downgrade note
//     (a pinned contract: visible, never silent)
//   - default: threaded - fewer launches, the established mode
//
// An override that contradicts a hard requirement is an error naming
// the workload and the fix. Refusing before load is honesty;
// switching the promise mid-run is the quiet substitution this project
// forbids.
func ResolveExecution(policy, a, b string, bursts int) (ExecutionDecision, error) {
	ramName := ""
	if requiresMemory(a) {
		ramName = a
	} else if requiresMemory(b) {
		ramName = b
	}
	single := workload.IsSingleInstance(a) || workload.IsSingleInstance(b)

	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "", config.ExecutionAuto:
		switch {
		case bursts > 0:
			return ExecutionDecision{PerProcess: true,
				Reason: "prepared bursts drive per-process workers (the transition lifecycle)"}, nil
		case ramName != "":
			return ExecutionDecision{PerProcess: true,
				Reason: fmt.Sprintf("%s verifies data inside memory-engine regions: the engine allocates in per-process children only", ramName)}, nil
		case single:
			return ExecutionDecision{
				Reason: "single-instance tool: one process for the whole machine, shared by every slot"}, nil
		default:
			return ExecutionDecision{Reason: "default: worker threads inside this process"}, nil
		}
	case config.ExecutionThreaded:
		if ramName != "" {
			return ExecutionDecision{}, fmt.Errorf(
				"execution override %q contradicts %s: it verifies data in memory-engine regions, which only per-process children carry; set execution to %q (or drop the override) to let the resolver pick",
				config.ExecutionThreaded, ramName, config.ExecutionAuto)
		}
		if bursts > 0 {
			return ExecutionDecision{}, fmt.Errorf(
				"execution override %q contradicts bursts: prepared bursts drive per-process workers; set execution to %q",
				config.ExecutionThreaded, config.ExecutionAuto)
		}
		return ExecutionDecision{Reason: "explicit override: worker threads in this process"}, nil
	case config.ExecutionSubprocess:
		return ExecutionDecision{PerProcess: true,
			Reason: "explicit override: one pinned subprocess per CPU"}, nil
	case config.ExecutionRotate:
		if bursts > 0 {
			return ExecutionDecision{}, fmt.Errorf(
				"execution override %q contradicts bursts: the schedule commands the bursts, rotate freezes whole windows",
				config.ExecutionRotate)
		}
		return ExecutionDecision{PerProcess: true, Rotate: true,
			Reason: "explicit override: rotating exclusive windows"}, nil
	default:
		return ExecutionDecision{}, fmt.Errorf("execution %q is not one of %s/%s/%s/%s",
			policy, config.ExecutionAuto, config.ExecutionThreaded, config.ExecutionSubprocess, config.ExecutionRotate)
	}
}

func requiresMemory(name string) bool {
	if name == "" {
		return false
	}
	wl, ok := workload.Get(name)
	return ok && wl.RequiresMemory
}
