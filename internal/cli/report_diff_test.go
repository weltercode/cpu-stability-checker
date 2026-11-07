package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

// The diff must put the fault delta first, but it must also refuse to
// mislead: unequal exposure, one-sided cores, unended runs and different
// phase sets all get said out loud.
func TestDiffSummaries(t *testing.T) {
	cs := func(core, runs, faults int, exp time.Duration) journal.CoreSummary {
		return journal.CoreSummary{Core: core, Runs: runs, Faults: faults, Exposure: exp}
	}

	t.Run("fault delta is the headline", func(t *testing.T) {
		a := journal.Summary{Cores: []journal.CoreSummary{cs(5, 10, 0, 10*time.Minute)}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(5, 10, 1, 10*time.Minute)}}
		rows, warnings := diffSummaries(a, b, []string{"p"}, []string{"p"})
		if len(warnings) != 0 {
			t.Fatalf("warnings = %v, want none for comparable journals", warnings)
		}
		if len(rows) != 1 || rows[0].Note != "+1 fault(s) in B" {
			t.Fatalf("rows = %+v, want one row noting +1 fault in B", rows)
		}
	})

	t.Run("a core only in one journal is noted, not passed", func(t *testing.T) {
		a := journal.Summary{Cores: []journal.CoreSummary{cs(0, 5, 0, time.Minute)}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(1, 5, 0, time.Minute)}}
		rows, _ := diffSummaries(a, b, nil, nil)
		if len(rows) != 2 || rows[0].Note != "only in A" || rows[1].Note != "only in B" {
			t.Fatalf("rows = %+v, want only-in-A and only-in-B notes", rows)
		}
	})

	t.Run("exposure differing more than 2x warns", func(t *testing.T) {
		a := journal.Summary{Cores: []journal.CoreSummary{cs(0, 2, 0, time.Minute)}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(0, 200, 1, 3*time.Hour)}}
		_, warnings := diffSummaries(a, b, nil, nil)
		if !containsWarning(warnings, "exposure differs more than 2x") {
			t.Errorf("warnings = %v, want the exposure ratio warning", warnings)
		}
	})

	t.Run("zero exposure on either side warns", func(t *testing.T) {
		a := journal.Summary{Cores: []journal.CoreSummary{{Core: 0, Runs: 3}}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(0, 3, 0, time.Minute)}}
		_, warnings := diffSummaries(a, b, nil, nil)
		if !containsWarning(warnings, "no recorded exposure") {
			t.Errorf("warnings = %v, want the no-exposure warning", warnings)
		}
	})

	t.Run("unended runs warn", func(t *testing.T) {
		a := journal.Summary{Unended: 1, Cores: []journal.CoreSummary{cs(0, 3, 0, time.Minute)}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(0, 3, 0, time.Minute)}}
		_, warnings := diffSummaries(a, b, nil, nil)
		if !containsWarning(warnings, "unended runs present") {
			t.Errorf("warnings = %v, want the unended warning", warnings)
		}
	})

	t.Run("different phase sets warn", func(t *testing.T) {
		a := journal.Summary{Cores: []journal.CoreSummary{cs(0, 3, 0, time.Minute)}}
		b := journal.Summary{Cores: []journal.CoreSummary{cs(0, 3, 0, time.Minute)}}
		_, warnings := diffSummaries(a, b, []string{"night-a"}, []string{"night-b"})
		if !containsWarning(warnings, "phase/test sets differ") {
			t.Errorf("warnings = %v, want the phase-set warning", warnings)
		}
	})
}

func containsWarning(ws []string, want string) bool {
	for _, w := range ws {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}

func TestPhaseSet(t *testing.T) {
	reports := []journal.RunReport{
		{RunID: "r1", Test: "general", Phase: "heavy"},
		{RunID: "r2", Test: "general", Phase: "heavy"},
		{RunID: "r3", Test: "isolate"},
	}
	got := phaseSet(reports)
	want := []string{"heavy", "test:isolate"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("phaseSet = %v, want %v (deduped, sorted, single runs labelled by test)", got, want)
	}
}
