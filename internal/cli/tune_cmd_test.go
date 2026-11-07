package cli

import (
	"path/filepath"
	"testing"
)

// tunePaths points every durable path at ONE temp dir for the whole
// test, so successive Run calls see the same plan, lock and
// transaction documents (and never touch the machine-wide ones).
func tunePaths(t *testing.T) []string {
	dir := chdirTemp(t)
	return []string{
		"-store", filepath.Join(dir, "tune.json"),
		"-lock", filepath.Join(dir, "tune.lock"),
		"-tx-lock", filepath.Join(dir, "co.lock"),
		"-tx-store", filepath.Join(dir, "co-tx.json"),
	}
}

func TestTuneRefusesWithoutFake(t *testing.T) {
	// P11 automation is fake-only until P12: without -fake every
	// mutating subcommand must refuse before anything runs.
	if code := Run([]string{"tune", "trial", "-candidate", "-21"}); code != 2 {
		t.Errorf("trial without -fake: exit %d, want 2", code)
	}
	if code := Run([]string{"tune", "plan-create", "-core", "core1"}); code != 2 {
		t.Errorf("plan-create without -fake: exit %d, want 2", code)
	}
	if code := Run([]string{"tune", "sweep", "-step", "2"}); code != 2 {
		t.Errorf("sweep without -fake: exit %d, want 2", code)
	}
	if code := Run([]string{"tune", "reconcile"}); code != 2 {
		t.Errorf("reconcile without -fake: exit %d, want 2", code)
	}
}

func TestTuneUnknownSubcommand(t *testing.T) {
	if code := Run([]string{"tune", "magic"}); code != 2 {
		t.Errorf("unknown subcommand: exit %d, want 2", code)
	}
	if code := Run([]string{"tune"}); code != 2 {
		t.Errorf("no subcommand: exit %d, want 2", code)
	}
}

func TestTuneFakeEndToEnd(t *testing.T) {
	p := tunePaths(t)
	run := func(extra ...string) int { return Run(append(append([]string{}, extra...), p...)) }
	if code := run("tune", "plan-create", "-fake", "-core", "core1", "-min", "-40", "-max", "0", "-attempts", "5"); code != 0 {
		t.Fatalf("plan-create: exit %d", code)
	}
	if code := run("tune", "trial", "-fake", "-candidate", "-22"); code != 0 {
		t.Fatalf("trial: exit %d", code)
	}
	if code := run("tune", "sweep", "-fake", "-step", "2", "-direction", "down"); code != 0 {
		t.Fatalf("sweep: exit %d", code)
	}
	if code := run("tune", "reconcile", "-fake"); code != 0 {
		t.Fatalf("reconcile: exit %d", code)
	}
	if code := run("tune", "plan-show", "-json"); code != 0 {
		t.Fatalf("plan-show json: exit %d", code)
	}
	if code := run("tune", "plan-show"); code != 0 {
		t.Fatalf("plan-show: exit %d", code)
	}
}

func TestTunePlanShowNoPlan(t *testing.T) {
	p := tunePaths(t)
	if code := Run(append([]string{"tune", "plan-show"}, p...)); code != 0 {
		t.Errorf("plan-show with no plan: exit %d, want 0", code)
	}
}
