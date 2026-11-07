package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func metaWith(rev, cfgHash string) *journal.RunMetaFields {
	seed := uint64(1)
	return &journal.RunMetaFields{BuildRev: rev, ConfigHash: cfgHash,
		PlanHash: "plan-" + cfgHash, TopologyFP: "topo1",
		Seed: &seed, CPUName: "c", OSVersion: "o"}
}

// The comparability checks must fire on the axes the plan names —
// config profile, tool build, algorithm version, verified volume, core
// coverage — and stay silent when the two sides really match.
func TestComparabilityWarnings(t *testing.T) {
	rep := func(meta *journal.RunMetaFields, kern string, verified *uint64, cores ...int) journal.RunReport {
		r := journal.RunReport{Meta: meta, Ended: true}
		for _, c := range cores {
			co := journal.CoreOutcome{Core: c, KernelVersion: kern}
			if verified != nil {
				co.Mem = &journal.MemAccounting{VerifiedUnique: verified}
			}
			r.Cores = append(r.Cores, co)
		}
		return r
	}
	u := func(n uint64) *uint64 { return &n }

	t.Run("identical sides produce no comparability warnings", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0, 1)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0, 1)}
		if w := comparabilityWarnings(a, b); len(w) != 0 {
			t.Fatalf("warnings = %v, want none for identical profiles", w)
		}
	})

	t.Run("different config profile warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hashA"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{rep(metaWith("rev1", "hashB"), "ram-constant-v1", u(1<<30), 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "config profile differs") {
			t.Errorf("warnings = %v, want the config-profile warning", w)
		}
	})

	t.Run("different tool build warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{rep(metaWith("rev2", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "tool build differs") {
			t.Errorf("warnings = %v, want the tool-build warning", w)
		}
	})

	t.Run("different algorithm version warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-random-v1", u(1<<30), 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "algorithm (kernel) versions differ") {
			t.Errorf("warnings = %v, want the algorithm-version warning", w)
		}
	})

	t.Run("verified volume differing beyond tolerance warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<28), 0)} // 4x less
		if w := comparabilityWarnings(a, b); !containsWarning(w, "verified memory volume per run differs") {
			t.Errorf("warnings = %v, want the verified-volume warning", w)
		}
	})

	// A11: equal AGGREGATES must not hide different per-run budgets:
	// one 1GiB run vs two 512MiB runs aggregate the same but are not
	// the same coverage.
	t.Run("equal aggregate but different per-run volume warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{
			rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<29), 0),
			rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<29), 0),
		}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "verified memory volume per run differs") {
			t.Errorf("warnings = %v, want the per-run volume warning (aggregate is equal)", w)
		}
	})

	t.Run("coverage on one side only warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "ram-constant-v1", u(1<<30), 0)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "int-v1", nil, 0)} // no mem coverage
		if w := comparabilityWarnings(a, b); !containsWarning(w, "coverage present on one side only") {
			t.Errorf("warnings = %v, want the one-sided-coverage warning", w)
		}
	})

	// A11: the plan hash sees what the config-file hash cannot -
	// CLI/menu overrides and recipe drift change the experiment even
	// when config.json never moves.
	t.Run("same config file but different resolved plans warns", func(t *testing.T) {
		ma, mb := metaWith("rev1", "hash1"), metaWith("rev1", "hash1")
		ma.PlanHash, mb.PlanHash = "planA", "planB"
		a := []journal.RunReport{rep(ma, "int-v1", nil, 0)}
		b := []journal.RunReport{rep(mb, "int-v1", nil, 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "resolved plans differ") {
			t.Errorf("warnings = %v, want the resolved-plan warning", w)
		}
	})

	t.Run("missing plan hash is unknown, never assumed equal", func(t *testing.T) {
		ma := metaWith("rev1", "hash1")
		ma.PlanHash = "" // journal written before the plan-hash field
		a := []journal.RunReport{rep(ma, "int-v1", nil, 0)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "int-v1", nil, 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "plan identity unknown") {
			t.Errorf("warnings = %v, want the unknown-plan warning", w)
		}
	})

	// A11: equal COUNTS of different core sets are not the same
	// coverage.
	t.Run("same core count on different cores warns", func(t *testing.T) {
		a := []journal.RunReport{rep(metaWith("rev1", "hash1"), "int-v1", nil, 0, 1)}
		b := []journal.RunReport{rep(metaWith("rev1", "hash1"), "int-v1", nil, 2, 3)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "core coverage differs") {
			t.Errorf("warnings = %v, want the core-set warning (counts are equal)", w)
		}
	})

	t.Run("different topology fingerprint warns", func(t *testing.T) {
		ma, mb := metaWith("rev1", "hash1"), metaWith("rev1", "hash1")
		ma.TopologyFP, mb.TopologyFP = "topoAMD", "topoIntel"
		a := []journal.RunReport{rep(ma, "int-v1", nil, 0)}
		b := []journal.RunReport{rep(mb, "int-v1", nil, 0)}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "CPU topology differs") {
			t.Errorf("warnings = %v, want the topology warning", w)
		}
	})

	t.Run("unknown provenance is reported unknown, never assumed equal", func(t *testing.T) {
		a := []journal.RunReport{{Ended: true, Cores: []journal.CoreOutcome{{Core: 0}}}}
		b := []journal.RunReport{{Ended: true, Cores: []journal.CoreOutcome{{Core: 0}}}}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "config profile unknown") {
			t.Errorf("warnings = %v, want the unknown-profile warning", w)
		}
		if w := comparabilityWarnings(a, b); !containsWarning(w, "plan identity unknown") {
			t.Errorf("warnings = %v, want the unknown-plan warning too", w)
		}
	})
}

// The JSON run_details + provenance blocks must be unknown-safe: mem /
// ram / bursts absent when the journal does not say, meta null for a
// run that predates provenance, and the two burst views kept separate.
func TestRunDetailsAndProvenance(t *testing.T) {
	verified := uint64(1 << 20)
	reports := []journal.RunReport{
		{
			RunID: "r1", Test: "general", Ended: true, Exit: 0, Validation: "verified", Meta: metaWith("rev1", "hash1"),
			Cores: []journal.CoreOutcome{{
				Core: 0, Status: "OK", KernelVersion: "ram-constant-v1",
				Mem: &journal.MemAccounting{Requested: 1 << 20, VerifiedUnique: &verified},
				Ram: &journal.RamEvidence{Total: 0},
			}},
		},
		{
			RunID: "r2", Test: "general", Ended: true, Exit: 0, // no Meta: predates provenance
			Cores: []journal.CoreOutcome{{Core: 0, Status: "OK"}}, // no mem/ram/bursts: unknown
		},
	}

	details := runDetails(reports)
	if len(details) != 2 {
		t.Fatalf("run_details = %d, want 2", len(details))
	}
	if details[0].Cores[0].Mem == nil {
		t.Fatal("run r1 core mem must be present")
	}
	if details[1].Meta != nil {
		t.Error("run r2 provenance must be nil (unknown), not fabricated")
	}
	if details[1].Cores[0].Mem != nil || details[1].Cores[0].Ram != nil || details[1].Cores[0].Bursts != nil {
		t.Errorf("run r2 core must carry no mem/ram/bursts blocks (unknown), got %+v", details[1].Cores[0])
	}
	// ram present with Total 0 is a RESULT (clean), distinct from absent (unknown).
	if details[0].Cores[0].Ram == nil || details[0].Cores[0].Ram.Total != 0 {
		t.Errorf("run r1 ram must be present with Total 0, got %+v", details[0].Cores[0].Ram)
	}
	// A02: the verification claim is per-run; a journal without the
	// run_end field renders unknown, never a guess.
	if details[0].Validation != "verified" {
		t.Errorf("run r1 validation = %q, want verified", details[0].Validation)
	}
	if details[1].Validation != "unknown" {
		t.Errorf("run r2 validation = %q, want unknown (no run_end claim recorded)", details[1].Validation)
	}

	prov := provenanceList(reports)
	if len(prov) != 2 {
		t.Fatalf("provenance entries = %d, want 2 (known + unknown fold separately)", len(prov))
	}

	// Full envelope round-trips through encoding/json.
	blob, err := json.Marshal(reportJSON{Schema: reportSchemaVersion, Provenance: prov, RunDetails: details})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back reportJSON
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.RunDetails[0].Cores[0].KernelVersion != "ram-constant-v1" {
		t.Errorf("kernel version lost in round-trip: %s", string(blob))
	}
	if !strings.Contains(string(blob), `"verified_unique_bytes"`) {
		t.Errorf("verified volume missing from JSON: %s", string(blob))
	}
	if !strings.Contains(string(blob), `"validation":"verified"`) {
		t.Errorf("validation claim missing from JSON: %s", string(blob))
	}
}

// The two burst views (parent commanded vs worker verified) must stay
// separate records, never merged into one number.
func TestRunDetailsBurstViews(t *testing.T) {
	reports := []journal.RunReport{{
		RunID: "r", Test: "general", Ended: true,
		Cores: []journal.CoreOutcome{{
			Core: 0, Status: "OK",
			SessionObs: []journal.BurstObservation{{Burst: "b0"}, {Burst: "b1"}},
			Bursts:     []journal.BurstAccounting{{Complete: true}, {Complete: false}},
		}},
	}}
	d := runDetails(reports)[0].Cores[0].Bursts
	if d == nil {
		t.Fatal("bursts block must be present")
	}
	if d.Commanded != 2 || d.Recorded != 2 || d.Verified != 1 {
		t.Errorf("burst views = %+v, want commanded 2 / recorded 2 / verified 1", d)
	}
}
