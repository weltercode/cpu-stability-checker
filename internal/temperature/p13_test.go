package temperature

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// P13: an absent sensor source (no HWiNFO pipe, WMI refusing, no ACPI
// thermal zone) is a monitoring fact, stated calmly: the one-line
// readout says it is not a stop, the provider's raw error strings
// stay in Status.Reason for the detail views, and nothing in this
// state claims anything about the CPU.
func TestAbsentProviderReadoutIsNotAFaultStory(t *testing.T) {
	raw := `createfile \\.\HWiNFO-Sensor-Share: The system cannot find the file specified.`
	m := NewWithReaders(20*time.Millisecond, time.Millisecond, absentReader{errors.New(raw)})
	defer m.Close()
	// The reader is polled asynchronously: wait for its first failure
	// to land, exactly like the panel would a moment into the run.
	var s Status
	deadline := time.Now().Add(2 * time.Second)
	for {
		s = m.Snapshot()
		if !s.Known && strings.Contains(s.Reason, "cannot find the file") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider error never reached the snapshot: %+v", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !s.ProviderAbsent {
		t.Errorf("a monitor that never received a reading must be ProviderAbsent: %+v", s)
	}
	line := s.Line()
	if !strings.Contains(line, "not a stop") {
		t.Errorf("readout must state the absence is not a stop: %q", line)
	}
	if strings.Contains(line, "cannot find the file") {
		t.Errorf("raw Win32 noise must not sit in the one-line readout: %q", line)
	}
	// With thermal control off, an absent provider must never abort
	// or refuse anything: it is not a stop.
	if s2 := m.Prepare(context.Background(), 0); s2.Abort != "" {
		t.Errorf("disabled control must not abort over a missing sensor: %+v", s2)
	}
}

// A monitor that HAD readings and lost them is the other case: the
// readout says "readings lost" (and with control armed that one IS a
// stop), it is not papered over with the absent-provider wording.
func TestLostReadingsStayStatedAsLost(t *testing.T) {
	r := &flippableReader{offline: make(chan struct{}, 1)}
	m := NewWithReaders(30*time.Millisecond, time.Millisecond, r)
	defer m.Close()
	if s := m.Prepare(context.Background(), 85); !s.Known {
		t.Fatalf("expected a fresh reading first: %+v", s)
	}
	r.offline <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	var s Status
	for {
		s = m.Snapshot()
		if !s.Known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loss was never detected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.ProviderAbsent {
		t.Errorf("a monitor that once had readings is not absent: %+v", s)
	}
	line := s.Line()
	if strings.Contains(line, "not a stop") {
		t.Errorf("the absent-provider wording must not paper over a mid-run loss: %q", line)
	}
	if !strings.Contains(line, "no fresh sample") {
		t.Errorf("a real loss must state its reason: %q", line)
	}
}

type absentReader struct{ err error }

func (a absentReader) Read() (Reading, error) { return Reading{}, a.err }
func (a absentReader) Close()                 {}

type flippableReader struct {
	offline chan struct{}
	off     bool
}

func (r *flippableReader) Read() (Reading, error) {
	select {
	case <-r.offline:
		r.off = true
	default:
	}
	if r.off {
		return Reading{}, errors.New("no fresh sample")
	}
	return sample(60, "WMI", time.Now()), nil
}
func (r *flippableReader) Close() {}
