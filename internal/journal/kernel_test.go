package journal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// core_done's kernel_version names the implementation that produced the
// result, so results of different kernels (or adapters) are never merged
// into one comparable story. A slot recorded without a version - older
// journals, legacy external entries - reads as unknown (""), never as
// "the current implementation".
func TestKernelVersionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	jw := OpenRun(path)
	jw.RunStart("general", "", []int{0, 1}, 1, "1s", false)
	jw.CoreStart(0, "int")
	jw.CoreStart(1, "int")
	jw.CoreDoneChecks(0, OK, "", time.Second, nil, "", "int-affine-v1")
	// the pre-versioning writer path (CoreDoneElapsed) records no kernel:
	jw.CoreDoneElapsed(1, OK, "", time.Second)
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
	if c0.KernelVersion != "int-affine-v1" {
		t.Errorf("core 0 kernel = %q, want int-affine-v1", c0.KernelVersion)
	}
	if c1.KernelVersion != "" {
		t.Errorf("core 1 kernel = %q, want unknown (older record)", c1.KernelVersion)
	}

	var out bytes.Buffer
	PrintReport(&out, reports)
	got := out.String()
	if !strings.Contains(got, "kernel=int-affine-v1") {
		t.Errorf("report must name the kernel that did the work, got:\n%s", got)
	}
	if strings.Count(got, "kernel=") != 1 {
		t.Errorf("only the versioned core may claim a kernel, got:\n%s", got)
	}
}
