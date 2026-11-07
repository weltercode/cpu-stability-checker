package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// WHEA records must survive recovery exactly once per event-log id (a
// persist retry after a failed write must not double-count), and the
// last whea_state record is the window verdict: without it the counts
// below it mean nothing.
func TestWheaRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	jw := OpenRun(path)
	jw.RunStart("general", "", []int{0}, 1, "1s", false)
	when := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if err := jw.Whea("Microsoft-Windows-WHEA-Logger", 19, 501, "corrected", "memory",
		"WHEA corrected memory (provider Microsoft-Windows-WHEA-Logger, event 19, record 501)", when); err != nil {
		t.Fatalf("Whea() = %v, want nil", err)
	}
	// The same event-log record written twice (a persist retry after a
	// failed write): recovery must count it once.
	if err := jw.Whea("Microsoft-Windows-WHEA-Logger", 19, 501, "corrected", "memory", "retry", when); err != nil {
		t.Fatalf("Whea() retry = %v, want nil", err)
	}
	if err := jw.Whea("Microsoft-Windows-WHEA-Logger", 17, 502, "uncorrected", "cache", "fatal", when); err != nil {
		t.Fatalf("Whea() = %v, want nil", err)
	}
	if err := jw.WheaState("available", ""); err != nil {
		t.Fatalf("WheaState() = %v, want nil", err)
	}
	if err := jw.WheaState("degraded", "query failed: wevtutil exploded"); err != nil {
		t.Fatalf("WheaState() = %v, want nil", err)
	}
	jw.RunEnd(0)
	jw.Close()

	recs, iss := ReadAll(Path(path)) // OpenRun derives the journal file from the log path
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("%d runs, want 1", len(reports))
	}
	rep := reports[0]
	if len(rep.Whea) != 2 {
		t.Fatalf("Whea records = %d (%+v), want 2 (persist retry deduped by event-log id)", len(rep.Whea), rep.Whea)
	}
	if rep.Whea[0].LogRecID != 501 || rep.Whea[0].Severity != "corrected" || rep.Whea[0].Nature != "memory" {
		t.Errorf("first WHEA = %+v, want deduped corrected memory 501", rep.Whea[0])
	}
	if !rep.Whea[0].Time.Equal(when) {
		t.Errorf("Whea time = %v, want the event's own SystemTime %v", rep.Whea[0].Time, when)
	}
	if rep.WheaState != "degraded" || rep.WheaStateReason != "query failed: wevtutil exploded" {
		t.Errorf("WheaState = %q %q, want degraded with the last recorded reason", rep.WheaState, rep.WheaStateReason)
	}

	var out bytes.Buffer
	PrintReport(&out, reports)
	s := out.String()
	for _, want := range []string{
		"WHEA: 1 corrected, 1 uncorrected, 0 unknown; observation: degraded",
		"observation reason: query failed: wevtutil exploded",
		"UNCORRECTED hardware error evidence",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report missing %q:\n%s", want, s)
		}
	}
}

// A journal without WHEA records (old builds, or a run that never
// watched) must not read as "hardware was clean": the state is
// unknown and the report says so.
func TestWheaRecoveryAbsentIsUnknown(t *testing.T) {
	var out bytes.Buffer
	PrintReport(&out, RecoverRuns(Parse(strings.NewReader(
		`{"schema":1,"type":"run_start","run_id":"r1","test":"general","cores":[0],"duration":"1s","ts":"2026-10-06T12:00:00Z"}`+"\n"+
			`{"schema":1,"type":"core_done","run_id":"r1","core":0,"workload":"int","result":"ok","ts":"2026-10-06T12:00:01Z"}`+"\n"+
			`{"schema":1,"type":"run_end","run_id":"r1","exit":0,"ts":"2026-10-06T12:00:02Z"}`+"\n"))))
	if s := out.String(); !strings.Contains(s, "no observation records") || strings.Contains(s, "WHEA: 0 corrected") {
		t.Errorf("report must admit the hardware-error state is unknown, not imply zero:\n%s", s)
	}
}

// A nil Writer cannot keep a WHEA record: the persist gate must see
// that as an error, never as silent success.
func TestWheaNilWriterErrors(t *testing.T) {
	var jw *Writer
	if err := jw.Whea("p", 19, 1, "corrected", "unknown", "m", time.Time{}); err == nil {
		t.Error("nil writer Whea() = nil error, want unavailable")
	}
	if err := jw.WheaState("available", ""); err == nil {
		t.Error("nil writer WheaState() = nil error, want unavailable")
	}
}
