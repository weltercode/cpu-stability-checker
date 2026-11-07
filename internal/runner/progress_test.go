package runner

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestCompactProgressLineIsOneLine(t *testing.T) {
	rs := newRunStatus(2, 30*time.Second)
	rs.start = time.Now().Add(-5 * time.Second)
	rs.cores[0].workload = "float"
	rs.cores[1].workload = "float"
	rs.cores[0].Add(1_000)
	rs.cores[1].Add(2_000)

	line := compactProgressLine(rs, "general", false)
	if strings.ContainsAny(line, "\r\n") {
		t.Fatalf("progress contains a line break: %q", line)
	}
	for _, want := range []string{"general", "it=3.0k", "rate="} {
		if !strings.Contains(line, want) {
			t.Errorf("progress does not contain %q: %s", want, line)
		}
	}
}

func TestWriteSummaryGroupsHealthySlots(t *testing.T) {
	rs := newRunStatus(3, time.Second)
	for i, iterations := range []uint64{100, 120, 80} {
		rs.cores[i].workload = "float"
		rs.cores[i].Add(iterations)
	}

	var out bytes.Buffer
	writeSummary(&out, rs, "general", false, "", plan.ValVerified)
	text := out.String()
	if strings.Count(text, "float") != 1 {
		t.Fatalf("summary should contain one grouped float row:\n%s", text)
	}
	for _, want := range []string{"Status: OK", "300 iterations across 3 CPU slots", "slots=3", "range=80..120", "[self-checking by repeating the same algorithm]"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary does not contain %q:\n%s", want, text)
		}
	}
}

// A02: a run that ended its window without confirming its own checking
// must never print a bare "Status: OK": the structured verification
// claim rides on the status line, and an empty claim reads unknown.
func TestWriteSummaryNeverClaimsPassWithoutVerification(t *testing.T) {
	cases := []struct {
		validation plan.Validation
		want       string
	}{
		{plan.ValNotPerformed, "verification: not_performed"},
		{plan.ValPartial, "verification: partial"},
		{plan.ValUnknown, "verification: unknown"},
		{plan.Validation(""), "verification: unknown"},
	}
	for _, c := range cases {
		rs := newRunStatus(1, time.Second)
		rs.cores[0].workload = "int"
		var out bytes.Buffer
		writeSummary(&out, rs, "general", false, "", c.validation)
		text := out.String()
		if strings.Contains(text, "Status: OK") {
			t.Errorf("validation %q: summary claims OK without a confirmed verification claim:\n%s", c.validation, text)
		}
		if !strings.Contains(text, c.want) || !strings.Contains(text, "not a pass claim") {
			t.Errorf("validation %q: summary does not contain %q / not-a-pass-claim:\n%s", c.validation, c.want, text)
		}
	}
}

// A load-only loader must never read like a self-checking one in the
// summary: the tag states what each kernel's verification kind
// actually proves, and an unregistered name says "unknown", not a
// comfortable default.
func TestVerificationLabels(t *testing.T) {
	cases := map[string]string{
		"int":         "self-checking vs independent reference",
		"float":       "self-checking by repeating the same algorithm",
		"float-exact": "self-checking vs independent reference",
		"sha256":      "self-checking vs independent reference",
		"mem":         "self-checking vs independent reference",
		"mem-bw":      "load only, no data verification",
		"fault":       "load only, no data verification",
		"ghost":       "verification unknown",
	}
	for name, want := range cases {
		if got := verificationLabel(name); got != want {
			t.Errorf("verificationLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// The panel footer must say what is running: the phase tag alone
// ("session-c3-r1") left users guessing whether the run walks cores.
func TestRunSummary(t *testing.T) {
	cases := []struct {
		name  string
		opt   plan.Options
		slots int
		want  string
	}{
		{"same workload both slots says it once", plan.Options{A: "spin", B: "spin"}, 16, "spin ×16"},
		{"abab names both", plan.Options{A: "spin", B: "idle"}, 2, "spin vs idle ×2"},
		{"empty B stays single", plan.Options{A: "prime95-small"}, 1, "prime95-small ×1"},
		{"per-process is visible", plan.Options{A: "spin", PerProcess: true}, 4, "spin ×4 per-core"},
		{"rotate wins over per-core", plan.Options{A: "spin", PerProcess: true, Rotate: true}, 1, "spin ×1 rotate"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := plan.RunSummary(tt.opt, tt.slots); got != tt.want {
				t.Errorf("RunSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}
