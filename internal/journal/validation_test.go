package journal

import (
	"bytes"
	"strings"
	"testing"
)

// A02: the structured verification claim rides on run_end so the
// detailed report can state it after the fact. A journal written
// without the claim (RunEnd, or any older file) must read as unknown -
// never as "verified" and never as "not performed".
func TestRunEndValidationRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	jw := NewWriter(&buf, "run-1")
	jw.RunStart("general", "", []int{0}, 1, "30s", false)
	jw.RunEndValidation(0, "verified")
	jw.Close()

	recs, iss := ParseAll(bytes.NewReader(buf.Bytes()), "mem")
	if iss.ReadErr != "" {
		t.Fatalf("parse: %v", iss.ReadErr)
	}
	reports := RecoverRuns(recs)
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	if reports[0].Validation != "verified" {
		t.Errorf("Validation = %q, want verified", reports[0].Validation)
	}

	var out bytes.Buffer
	PrintReport(&out, reports)
	if !strings.Contains(out.String(), "exit=0 validation=verified") {
		t.Errorf("report does not carry the validation claim:\n%s", out.String())
	}
}

// The last run_end wins (a run can be re-closed by recovery paths):
// an empty later claim never erases an earlier known one, and a known
// later claim replaces the earlier one.
func TestRunEndValidationLastKnownWins(t *testing.T) {
	var buf bytes.Buffer
	jw := NewWriter(&buf, "run-1")
	jw.RunStart("general", "", []int{0}, 1, "30s", false)
	jw.RunEndValidation(0, "partial")
	jw.RunEnd(1) // older call site: no claim
	jw.Close()
	reports := RecoverRuns(mustParse(t, buf.String()))
	if reports[0].Validation != "partial" {
		t.Errorf("Validation = %q, want the earlier known claim kept", reports[0].Validation)
	}

	buf.Reset()
	jw = NewWriter(&buf, "run-2")
	jw.RunStart("general", "", []int{0}, 1, "30s", false)
	jw.RunEnd(1) // no claim
	jw.RunEndValidation(1, "not_performed")
	jw.Close()
	reports = RecoverRuns(mustParse(t, buf.String()))
	if reports[0].Validation != "not_performed" {
		t.Errorf("Validation = %q, want the later known claim", reports[0].Validation)
	}
}

// A journal that never recorded validation (every file older than this
// field, and plain RunEnd callers) prints unknown - the absence of an
// observation is never rendered as a claim.
func TestPrintReportValidationUnknownWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	jw := NewWriter(&buf, "run-1")
	jw.RunStart("general", "", []int{0}, 1, "30s", false)
	jw.RunEnd(0)
	jw.Close()
	reports := RecoverRuns(mustParse(t, buf.String()))
	if reports[0].Validation != "" {
		t.Errorf("Validation = %q, want empty (unknown)", reports[0].Validation)
	}
	var out bytes.Buffer
	PrintReport(&out, reports)
	if !strings.Contains(out.String(), "validation=unknown") {
		t.Errorf("report does not say validation unknown:\n%s", out.String())
	}
	if strings.Contains(out.String(), "validation=\n") {
		t.Errorf("report renders an empty validation:\n%s", out.String())
	}
}

func mustParse(t *testing.T, s string) []Record {
	t.Helper()
	recs, iss := ParseAll(strings.NewReader(s), "mem")
	if iss.ReadErr != "" {
		t.Fatalf("parse: %v", iss.ReadErr)
	}
	return recs
}
