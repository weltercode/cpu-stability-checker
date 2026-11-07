//go:build windows

package wmisensor

import (
	"testing"
	"time"
)

func TestSnapshotDoesNotRefreshCachedSensorTime(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	s := &Source{cache: []Reading{{Type: "Temperature", Name: "CPU Package", Value: 70}}, updatedAt: at}
	reads, stamp := s.Snapshot()
	if len(reads) != 1 || !stamp.Equal(at) {
		t.Fatalf("old cache got a new timestamp: %+v %v", reads, stamp)
	}
	reads[0].Value = 999
	values, again := s.Snapshot()
	if values[0].Value != 70 || !again.Equal(at) {
		t.Fatal("caller changed WMI cache or advanced its time")
	}
}
