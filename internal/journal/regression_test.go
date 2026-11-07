package journal

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRapidRunsRemainDistinctIncludingFaults(t *testing.T) {
	const count = 1000
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range count / 8 {
				ids <- NewRunID()
			}
		}()
	}
	wg.Wait()
	close(ids)
	var buf bytes.Buffer
	for id := range ids {
		w := NewWriter(&buf, id)
		w.RunStart("general", "short", []int{2}, 1, "10ms", false)
		w.CoreStart(0, "int")
		w.CoreDone(0, Fault, "must not be overwritten by a following phase")
		w.RunEnd(1)
	}
	reports := RecoverRuns(Parse(&buf))
	if len(reports) != count {
		t.Fatalf("recovered %d runs, want %d", len(reports), count)
	}
	sum := SummarizeCores(reports)
	if len(sum.Cores) != 1 || sum.Cores[0].Faults != count || sum.Cores[0].Runs != count {
		t.Fatalf("rapid runs lost their faults: %+v", sum)
	}
}

func TestMeasuredExposureAndIncompleteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status, load string
		elapsed      time.Duration
		want         time.Duration
	}{
		{OK, "int", 200 * time.Millisecond, 200 * time.Millisecond},
		{Interrupted, "int", 25 * time.Millisecond, 25 * time.Millisecond},
		{Fault, "int", 5 * time.Millisecond, 5 * time.Millisecond},
		{SetupError, "external", 0, 0},
		{Skipped, "idle", time.Second, 0},
		{Interrupted, "int", 0, 0},
	} {
		t.Run(tc.status+"/"+tc.load+"/"+tc.elapsed.String(), func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf, "measured")
			w.RunStart("general", "", []int{2}, 1, "1h", false)
			w.CoreStart(0, tc.load)
			w.CoreDoneElapsed(0, tc.status, "reason", tc.elapsed)
			w.RunEnd(1)
			reports := RecoverRuns(Parse(&buf))
			c := reports[0].Cores[0]
			if c.LoadTime == nil || *c.LoadTime != tc.elapsed || c.Status != tc.status {
				t.Fatalf("recovered %+v", c)
			}
			sum := SummarizeCores(reports)
			if sum.Cores[0].Exposure != tc.want {
				t.Fatalf("exposure=%s want %s", sum.Cores[0].Exposure, tc.want)
			}
			var text bytes.Buffer
			PrintSummary(&text, sum)
			if tc.status != OK && strings.Contains(text.String(), ", OK") {
				t.Fatalf("incomplete run called OK: %s", text.String())
			}
			text.Reset()
			PrintReport(&text, reports)
			if tc.status != OK && strings.Contains(text.String(), ": OK") {
				t.Fatalf("incomplete core called OK: %s", text.String())
			}
		})
	}
}

func TestLegacyPartialRunsDoNotReceivePlannedExposure(t *testing.T) {
	for _, status := range []string{OK, Interrupted, Crashed, Fault, SetupError} {
		reports := []RunReport{{Ended: true, Exit: 1, Duration: time.Hour,
			Cores: []CoreOutcome{{Core: 0, Workload: "int", Status: status}}}}
		sum := SummarizeCores(reports)
		if sum.Cores[0].Exposure != 0 {
			t.Fatalf("%s credited planned duration: %+v", status, sum.Cores[0])
		}
		if status == OK && sum.Cores[0].Interrupted != 1 {
			t.Fatal("legacy aborted peer still called OK")
		}
		var text bytes.Buffer
		PrintReport(&text, reports)
		if strings.Contains(text.String(), ": OK") {
			t.Fatalf("legacy partial result called OK: %s", text.String())
		}
	}
}
