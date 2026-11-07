package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/journal"
)

func TestSummaryToJSON(t *testing.T) {
	sum := journal.Summary{
		Runs:    2,
		Unended: 1,
		Cores: []journal.CoreSummary{
			{Core: 3, Runs: 2, Faults: 1, LastFault: "p1 general/int: bad result",
				WakeN: 2, WakeMinMS: 1, WakeAvgMS: 3, WakeMaxMS: 5,
				ClockN: 2, ClockMinMHz: 3600, ClockAvgMHz: 3800, ClockMaxMHz: 4000,
				IdleN: 2, IdleAvgPct: 1.5, IdleMaxPct: 2.5},
			{Core: 4, Runs: 1}, // no samples at all
		},
	}
	got := summaryToJSON("x.jsonl", sum, nil, &journal.Issues{}, nil)
	if got.Journal != "x.jsonl" || got.Runs != 2 || got.Unended != 1 || len(got.Cores) != 2 {
		t.Fatalf("top level = %+v, want journal/x runs 2 unended 1 two cores", got)
	}
	c3 := got.Cores[0]
	if c3.Wake == nil || c3.Wake.N != 2 || c3.Wake.Min != 1 || c3.Wake.Avg != 3 || c3.Wake.Max != 5 {
		t.Errorf("core 3 wake = %+v, want n 2 min 1 avg 3 max 5", c3.Wake)
	}
	if c3.Clock == nil || c3.Clock.Min != 3600 || c3.Clock.Max != 4000 {
		t.Errorf("core 3 clock = %+v, want min 3600 max 4000", c3.Clock)
	}
	if c3.Idle == nil || c3.Idle.Avg != 1.5 || c3.Idle.Max != 2.5 {
		t.Errorf("core 3 idle = %+v, want avg 1.5 max 2.5", c3.Idle)
	}
	c4 := got.Cores[1]
	if c4.Wake != nil || c4.Clock != nil || c4.Idle != nil {
		t.Errorf("core 4 blocks = %+v, want all absent (never observed)", c4)
	}
}

// End to end: a written journal comes back as parseable JSON with the
// same numbers the text report would show.
func TestReportCmdJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.log.journal.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	jw := journal.NewWriter(f, "run-1")
	jw.RunStart("general", "", []int{6}, 1, "30s", false)
	jw.CoreStart(0, "int")
	jw.Telemetry(0, 3900, ptrFloat(0.5), 3000, []journal.CoreClockGroup{{Source: "hwinfo", MinMHz: 5400, AvgMHz: 5500, MaxMHz: 5600, N: 2, FirstMS: 1000, LastMS: 2000, AgeBasis: "producer"}})
	jw.Wake(0, 2.5)
	jw.CoreDone(0, journal.OK, "")
	if err := jw.Whea("Microsoft-Windows-WHEA-Logger", 19, 77, "corrected", "memory",
		"WHEA corrected memory (provider Microsoft-Windows-WHEA-Logger, event 19, record 77)", time.Now()); err != nil {
		t.Fatalf("Whea() = %v", err)
	}
	if err := jw.WheaState("available", ""); err != nil {
		t.Fatalf("WheaState() = %v", err)
	}
	jw.RunEnd(0)
	jw.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := reportCmd([]string{"-json", path})
	w.Close()
	os.Stdout = old
	if code != 0 {
		t.Fatalf("reportCmd(-json) exit code = %d, want 0", code)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	var got reportJSON
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}
	if got.Runs != 1 || len(got.Cores) != 1 {
		t.Fatalf("decoded = %+v, want one run one core", got)
	}
	c := got.Cores[0]
	if c.Core != 6 || c.Faults != 0 || c.Wake == nil || c.Wake.Max != 2.5 {
		t.Errorf("decoded core = %+v, want core 6 clean with wake max 2.5", c)
	}
	if c.Clock == nil || c.Clock.Min != 3900 {
		t.Errorf("decoded clock block = %+v, want min 3900", c.Clock)
	}
	if c.Idle == nil || c.Idle.Avg != 0.5 {
		t.Errorf("decoded idle block = %+v, want avg 0.5", c.Idle)
	}
	if got.Whea == nil || got.Whea.Corrected != 1 || got.Whea.Observation != "available" {
		t.Errorf("whea block = %+v, want 1 corrected with observation available", got.Whea)
	}
}

// The WHEA block folds all runs together and takes the worst
// observation state: one degraded window makes the aggregate counts
// suspect. A journal with no state records at all reports observation
// "unknown", never "available" — absence of watching is not cleanliness.
func TestWheaSummaryAggregation(t *testing.T) {
	reports := []journal.RunReport{
		{Whea: []journal.WheaOutcome{
			{Severity: "corrected", Nature: "memory"},
			{Severity: "corrected", Nature: "pcie"},
			{Severity: "uncorrected", Nature: "cache"},
			{Severity: "unknown", Nature: "unknown"},
		}, WheaState: "available"},
		{WheaState: "degraded", WheaStateReason: "wevtutil exploded"},
	}
	s := wheaSummary(reports)
	if s == nil {
		t.Fatal("nil summary, want an aggregate")
	}
	if s.Corrected != 2 || s.Uncorrected != 1 || s.Unknown != 1 || s.PCIe != 1 {
		t.Errorf("counts = %+v, want corrected 2 (pcie 1) uncorrected 1 unknown 1", s)
	}
	if s.Observation != "degraded" || s.Reason != "wevtutil exploded" {
		t.Errorf("observation = %q %q, want the worst state degraded with its reason", s.Observation, s.Reason)
	}

	s2 := wheaSummary([]journal.RunReport{{Test: "general"}})
	if s2 == nil || s2.Observation != "unknown" {
		t.Errorf("observation = %+v, want unknown for a journal without state records", s2)
	}
	if wheaSummary(nil) != nil {
		t.Error("no runs must give no block, not a zero-filled one")
	}
}

func ptrFloat(v float64) *float64 { return &v }

// The JSON envelope versions itself and admits the journal's holes:
// external tooling must be able to tell "intact" from "damaged"
// without re-parsing the raw file.
func TestReportJSONSchemaAndIssues(t *testing.T) {
	clean := summaryToJSON("x.jsonl", journal.Summary{}, nil, &journal.Issues{}, nil)
	if clean.Schema != reportSchemaVersion {
		t.Errorf("Schema = %d, want %d", clean.Schema, reportSchemaVersion)
	}
	if clean.JournalIssues != nil {
		t.Errorf("clean journal claims damage: %+v", clean.JournalIssues)
	}
	damaged := summaryToJSON("x.jsonl", journal.Summary{}, nil,
		&journal.Issues{Malformed: 2, Unterminated: true}, nil)
	if damaged.JournalIssues == nil || damaged.JournalIssues.Malformed != 2 || !damaged.JournalIssues.Unterminated {
		t.Errorf("damage hidden in JSON: %+v", damaged.JournalIssues)
	}
}
