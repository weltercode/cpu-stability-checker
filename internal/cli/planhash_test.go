package cli

import (
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/config"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

func hashSnap(cfg config.Config) *config.Config {
	s := recipeConfig(cfg)
	return &s
}

func hashCase() (lastRun, plan.Options) {
	req := lastRun{Kind: lastRunScenario, Config: hashSnap(config.Default()),
		Phases: []config.Phase{{Name: "p", Test: "general", A: "int"}}}
	opt := plan.Options{TestName: "general", A: "int", Cores: []int{0, 1},
		Workers: 1, Duration: 10 * time.Second, LogFile: "run-a.log"}
	return req, opt
}

// A11: the plan hash is the experiment's identity. It must be stable
// for the same experiment, sensitive to every parameter that shapes
// what runs, and blind only to what provably does not shape it (the
// log path, the display choice, the core listing order).
func TestPlanHashIdentity(t *testing.T) {
	t.Run("same experiment hashes the same", func(t *testing.T) {
		ra, oa := hashCase()
		rb, ob := hashCase()
		if planHash(ra, oa) != planHash(rb, ob) {
			t.Fatal("identical requests and plans hashed differently")
		}
		if h := planHash(ra, oa); len(h) != 64 {
			t.Errorf("hash length = %d, want 64 hex chars", len(h))
		}
	})

	t.Run("the reserved-cpu naming is transport, not the experiment", func(t *testing.T) {
		// A reserve policy shrinks the EFFECTIVE Cores - that is a
		// different experiment and must hash differently. The
		// ReservedCPUs display field alone cannot move the identity:
		// it only names what the Cores already encode, and a field
		// that displays must not fork receipts from journals.
		_, oa := hashCase()
		_, ob := hashCase()
		ob.ReservedCPUs = []int{31}
		r, _ := hashCase()
		if planHash(r, oa) != planHash(r, ob) {
			t.Error("ReservedCPUs leaked into the plan identity")
		}
	})

	t.Run("a CLI override changes the identity", func(t *testing.T) {
		// The whole point of A11: the config FILE hash could not see
		// this; the plan hash can.
		ra, oa := hashCase()
		rb, ob := hashCase()
		ob.Cores = []int{0, 2}
		if planHash(ra, oa) == planHash(rb, ob) {
			t.Error("different core sets share a hash: overrides are invisible again")
		}
	})

	t.Run("core listing order is not the experiment", func(t *testing.T) {
		_, oa := hashCase()
		_, ob := hashCase()
		oa.Cores, ob.Cores = []int{0, 1}, []int{1, 0}
		r, _ := hashCase()
		if planHash(r, oa) != planHash(r, ob) {
			t.Error("core order leaked into the identity")
		}
	})

	t.Run("log path and display choice are excluded", func(t *testing.T) {
		r, oa := hashCase()
		_, ob := hashCase()
		ob.LogFile = "somewhere-else.log"
		ob.ShowMetrics = true
		if planHash(r, oa) != planHash(r, ob) {
			t.Error("WHERE the story is written changed WHAT was tested")
		}
	})

	t.Run("seed 0 is distinct from no seed", func(t *testing.T) {
		ra, oa := hashCase()
		rb, ob := hashCase()
		zero := int64(0)
		rb.Seed = &zero
		if planHash(ra, oa) == planHash(rb, ob) {
			t.Error("an explicit seed 0 hashes like an unseeded run")
		}
	})

	t.Run("cycles, continue policy and preset are part of the identity", func(t *testing.T) {
		ra, oa := hashCase()
		for _, mut := range []func(*lastRun){
			func(r *lastRun) { r.Cycles = 2 },
			func(r *lastRun) { r.ContinueOnFault = true },
			func(r *lastRun) { r.Preset = "cpu-baseline" },
		} {
			rb, ob := hashCase()
			mut(&rb)
			if planHash(ra, oa) == planHash(rb, ob) {
				t.Error("a request mutation (cycles/continue/preset) did not change the hash")
			}
		}
	})

	t.Run("transition mode normalizes the never-loaded slot B", func(t *testing.T) {
		// A fresh -bursts run (slot B cleared) and its scenario
		// replay (B present in the phase) must share one identity.
		r, oa := hashCase()
		_, ob := hashCase()
		oa.Bursts, oa.BurstRun = 2, time.Second
		ob.Bursts, ob.BurstRun, ob.B = 2, time.Second, "mem"
		if planHash(r, oa) != planHash(r, ob) {
			t.Error("a burst run and its replay hash apart: slot B was judged although it never loads")
		}
		// Without bursts, slot B IS loaded: it must change the hash.
		_, oc := hashCase()
		_, od := hashCase()
		od.B = "mem"
		if planHash(r, oc) == planHash(r, od) {
			t.Error("slot B invisible for a loaded run")
		}
	})

	t.Run("the registered external tool path is part of the identity", func(t *testing.T) {
		ra, oa := hashCase()
		rb, ob := hashCase()
		ra.Config.External = map[string]workload.ExternalSpec{"yc": {Command: `C:\tools\yc-v0.8.exe`}}
		rb.Config.External = map[string]workload.ExternalSpec{"yc": {Command: `C:\tools\yc-v0.9.exe`}}
		if planHash(ra, oa) == planHash(rb, ob) {
			t.Error("a swapped external binary kept the old experiment's identity")
		}
	})

	t.Run("session requests hash by their own choices", func(t *testing.T) {
		sa := lastRun{Kind: lastRunSession, Session: &SessionOptions{Workload: "int", Duration: time.Minute, Passes: 1}}
		sb := lastRun{Kind: lastRunSession, Session: &SessionOptions{Workload: "float", Duration: time.Minute, Passes: 1}}
		_, opt := hashCase()
		if planHash(sa, opt) == planHash(sb, opt) {
			t.Error("session workload invisible to the identity")
		}
	})
}

// Q04 parking identity rules, end to end through the real resolve +
// hash pipeline: (1) nothing a pre-Q04 recipe saved may move - an
// absent parking word keeps every old hash byte-identical, and even
// spelling "off" out loud must resolve to that same absence; (2) a
// policy that actually shapes a transition run (it adds observation
// and journal records) splits the identity - try, required and off
// are three different experiments, never one experiment with three
// spellings.
func TestPlanHashParkingIdentity(t *testing.T) {
	burstCase := func(t *testing.T, cfg config.Config) string {
		t.Helper()
		opt, err := plan.OptionsFromPhase(cfg, config.Phase{Name: "tr", PerProcess: true, Bursts: 2})
		if err != nil {
			t.Fatal(err)
		}
		req := lastRun{Kind: lastRunScenario, Config: hashSnap(cfg),
			Phases: []config.Phase{{Name: "tr", PerProcess: true, Bursts: 2}}}
		return planHash(req, opt)
	}
	plainCase := func(t *testing.T, cfg config.Config) string {
		t.Helper()
		opt, err := plan.OptionsFromPhase(cfg, config.Phase{Name: "p"})
		if err != nil {
			t.Fatal(err)
		}
		req := lastRun{Kind: lastRunScenario, Config: hashSnap(cfg),
			Phases: []config.Phase{{Name: "p"}}}
		return planHash(req, opt)
	}

	absent, off, try, req := config.Default(), config.Default(), config.Default(), config.Default()
	off.ParkingPolicy = "off"
	try.ParkingPolicy = "try"
	req.ParkingPolicy = "required"

	t.Run("bursts: absent and off share the pre-Q04 hash", func(t *testing.T) {
		if h1, h2 := burstCase(t, absent), burstCase(t, off); h1 != h2 {
			t.Errorf(`"off" forked the default's identity: %s vs %s`, h1, h2)
		}
	})
	t.Run("bursts: try and required each change the experiment", func(t *testing.T) {
		hOff, hTry, hReq := burstCase(t, off), burstCase(t, try), burstCase(t, req)
		if hOff == hTry || hTry == hReq || hOff == hReq {
			t.Errorf("parking policies share one identity (off=%s try=%s required=%s): observation is part of the experiment", hOff, hTry, hReq)
		}
	})
	t.Run("ordinary phases: the inherited word is inert in every direction", func(t *testing.T) {
		// On a phase that cannot observe parking, even "required"
		// must not move the hash: it shapes nothing there, and
		// hashing what does not run invents identity forks.
		if h1, h2 := plainCase(t, absent), plainCase(t, req); h1 != h2 {
			t.Errorf("an inert global parking word forked an ordinary run: %s vs %s", h1, h2)
		}
	})
}
