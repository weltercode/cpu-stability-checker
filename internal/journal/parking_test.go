package journal

import (
	"path/filepath"
	"testing"
	"time"
)

// The Q04 evidence record must survive write -> read -> recover with
// every honesty-bearing field intact: the counts, the unsampled-window
// hole, the status, and the parked stamps. And absence stays honest:
// a slot with no parking_obs record (policy off, older journal) reads
// back nil - unknown, never "not parked".
func TestParkingObsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	jw := NewWriter(f, "rid-p")
	jw.RunStart("general", "", []int{3, 5}, 1, "5s", true)
	jw.CoreStart(3, "int")
	jw.CoreStart(5, "int")
	jw.ParkingObs(3, ParkingObservation{
		Source: "cpuset", IntervalNS: int64(200 * time.Millisecond),
		Status: "observed", SamplesInIdle: 7, ParkedInIdle: 3,
		IdleWindows: 4, UnsampledWindows: 1,
		FirstParked: "2026-10-24T12:00:01.400000000Z",
		LastParked:  "2026-10-24T12:00:04.200000000Z",
		Reason:      "observed, though 1 of 4 idle window(s) received no successful sample",
	})
	jw.CoreDone(3, OK, "")
	jw.CoreDone(5, OK, "")
	jw.RunEnd(0)
	jw.Close()

	records, iss := ReadAll(path)
	if iss.ReadErr != "" {
		t.Fatalf("read: %s", iss.ReadErr)
	}
	runs := RecoverRuns(records)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	byCore := map[int]*CoreOutcome{}
	for i := range runs[0].Cores {
		c := runs[0].Cores[i]
		byCore[c.Core] = &c
	}

	c3 := byCore[3]
	if len(c3.Parking) != 1 {
		t.Fatalf("parking records on core 3 = %d, want 1", len(c3.Parking))
	}
	got := c3.Parking[0]
	want := ParkingObservation{
		Source: "cpuset", IntervalNS: int64(200 * time.Millisecond),
		Status: "observed", SamplesInIdle: 7, ParkedInIdle: 3,
		IdleWindows: 4, UnsampledWindows: 1,
		FirstParked: "2026-10-24T12:00:01.400000000Z",
		LastParked:  "2026-10-24T12:00:04.200000000Z",
		Reason:      "observed, though 1 of 4 idle window(s) received no successful sample",
	}
	if got != want {
		t.Errorf("parking record lost fidelity:\n got %+v\nwant %+v", got, want)
	}
	// One slot's evidence must never leak into another slot's story.
	if len(byCore[5].Parking) != 0 {
		t.Errorf("core 5 carries parking evidence it was never given: %+v", byCore[5].Parking)
	}
}
