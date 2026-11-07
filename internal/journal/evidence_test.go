package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// core_done carries what an external adapter's protocol proved for that
// core ("evidence"), and recovery must carry it into the report: an OK
// from an external tool means different things when the adapter
// confirmed the tool's own verification versus when it could only watch
// the process live. A journal written by an older build has no evidence
// field: that reads as unknown, never as success.
func TestEvidenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	jw := OpenRun(path)
	jw.RunStart("general", "", []int{0, 1}, 1, "1s", false)
	jw.CoreStart(0, "ycruncher")
	jw.CoreStart(1, "mprime")
	jw.CoreDoneChecks(0, OK, "", time.Second, nil, "confirmed-complete", "")
	jw.CoreDoneChecks(1, OK, "", time.Second, nil, "unknown", "")
	jw.RunEnd(0)
	jw.Close()

	recs, iss := ReadAll(Path(path))
	if !iss.Empty() {
		t.Fatalf("clean journal reported issues: %+v", iss)
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 || len(reports[0].Cores) != 2 {
		t.Fatalf("recovered %+v, want one run with two cores", reports)
	}
	c0, c1 := reports[0].Cores[0], reports[0].Cores[1]
	if c0.Core != 0 || c0.Evidence != "confirmed-complete" {
		t.Errorf("core 0 = %+v, want evidence confirmed-complete", c0)
	}
	if c1.Core != 1 || c1.Evidence != "unknown" {
		t.Errorf("core 1 = %+v, want evidence unknown", c1)
	}

	var out bytes.Buffer
	PrintReport(&out, reports)
	s := out.String()
	if !strings.Contains(s, "OK evidence=confirmed-complete") || !strings.Contains(s, "OK evidence=unknown") {
		t.Errorf("report must show the evidence beside each ending:\n%s", s)
	}
}

// A legacy core_done line (no evidence field at all) recovers with an
// empty evidence string: the engine and reports treat that as unknown.
// Absence of the field is never absence of doubt.
func TestEvidenceLegacyAbsent(t *testing.T) {
	reports := RecoverRuns(Parse(strings.NewReader(
		`{"schema":1,"type":"run_start","run_id":"r1","test":"general","cores":[0],"duration":"1s","ts":"2026-10-06T12:00:00Z"}` + "\n" +
			`{"schema":1,"type":"core_start","run_id":"r1","core":0,"workload":"ycruncher","ts":"2026-10-06T12:00:01Z"}` + "\n" +
			`{"schema":1,"type":"core_done","run_id":"r1","core":0,"result":"ok","load_ns":1000000000,"ts":"2026-10-06T12:00:02Z"}` + "\n" +
			`{"schema":1,"type":"run_end","run_id":"r1","exit":0,"ts":"2026-10-06T12:00:03Z"}` + "\n")))
	if len(reports) != 1 || len(reports[0].Cores) != 1 {
		t.Fatalf("recovered %+v, want one run with one core", reports)
	}
	if reports[0].Cores[0].Evidence != "" {
		t.Errorf("legacy record evidence = %q, want empty (unknown)", reports[0].Cores[0].Evidence)
	}
	var out bytes.Buffer
	PrintReport(&out, reports)
	if strings.Contains(out.String(), "evidence=") {
		t.Errorf("legacy ending must not claim any evidence:\n%s", out.String())
	}
}
