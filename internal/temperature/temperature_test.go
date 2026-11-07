package temperature

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

func sample(value float64, source string, now time.Time) Reading {
	return Reading{Celsius: value, Source: source, Sensor: "CPU Package", At: now, CPU: true}
}

func TestProviderPriorityAndFreshness(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		values []observation
		source string
		known  bool
	}{
		{"HWiNFO preferred", []observation{{reading: sample(70, "HWiNFO", now)}, {reading: sample(60, "WMI", now)}}, "HWiNFO", true},
		{"missing CPU sensor falls back", []observation{{err: errors.New("only clocks published")}, {reading: sample(60, "WMI", now)}}, "WMI", true},
		{"stale producer falls back", []observation{{reading: sample(70, "HWiNFO", now.Add(-time.Minute))}, {reading: sample(60, "WMI", now)}}, "WMI", true},
		{"unavailable sources", []observation{{err: errors.New("offline")}}, "", false},
		{"NaN rejected", []observation{{reading: sample(math.NaN(), "HWiNFO", now)}, {reading: sample(60, "WMI", now)}}, "WMI", true},
		{"infinite rejected", []observation{{reading: sample(math.Inf(1), "HWiNFO", now)}}, "", false},
		{"future timestamp rejected", []observation{{reading: sample(70, "HWiNFO", now.Add(time.Minute))}}, "", false},
		{"zero timestamp rejected", []observation{{reading: sample(70, "HWiNFO", time.Time{})}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithReaders(MaxAge, time.Second)
			m.values = tc.values
			s := m.status(now)
			if s.Known != tc.known || s.Reading.Source != tc.source {
				t.Fatalf("selected %+v", s)
			}
		})
	}
}

func TestCutoffLossAndLatching(t *testing.T) {
	now := time.Now()
	m := NewWithReaders(MaxAge, time.Second)
	m.limit, m.armedAt = 85, now
	m.values = []observation{{reading: sample(70, "HWiNFO", now)}}
	if s := m.status(now); s.State != Active || s.Abort != "" {
		t.Fatalf("healthy source: %+v", s)
	}
	m.values[0].err = errors.New("monitor closed")
	if s := m.status(now.Add(time.Second)); s.State != Waiting || s.Known || s.Abort != "" {
		t.Fatalf("grace state: %+v", s)
	}
	if s := m.status(now.Add(MaxAge)); s.State != Lost || !strings.Contains(s.Abort, "temperature readings lost") {
		t.Fatalf("loss did not stop: %+v", s)
	}
	m.values[0] = observation{reading: sample(50, "HWiNFO", now.Add(MaxAge))}
	if s := m.status(now.Add(MaxAge)); s.State != Lost || s.Abort == "" {
		t.Fatalf("stopped run resumed automatically: %+v", s)
	}
	m.Disarm()
	if s := m.status(now.Add(MaxAge)); s.State != Off || s.Abort != "" {
		t.Fatalf("disarm: %+v", s)
	}
}

func TestCPUControlDoesNotSilentlyDegradeToBoard(t *testing.T) {
	now := time.Now()
	m := NewWithReaders(MaxAge, time.Second)
	m.limit = 85
	m.armedAt = now
	m.values = []observation{{reading: sample(70, "HWiNFO", now)}}
	m.status(now)
	board := Reading{Celsius: 40, Source: "system", Sensor: "ACPI thermal zone", At: now.Add(MaxAge)}
	m.values = []observation{{reading: board}}
	if s := m.status(now.Add(MaxAge)); s.Known || s.State != Lost {
		t.Fatalf("CPU loss hidden by board sensor: %+v", s)
	}
}

func TestCutoffBoundaryAndDisabledControl(t *testing.T) {
	for _, tc := range []struct {
		limit, value float64
		state        string
	}{{85, 84.9, Active}, {85, 85, Limit}, {85, 100, Limit}, {0, 100, Off}} {
		m := NewWithReaders(MaxAge, time.Second)
		now := time.Now()
		m.limit = tc.limit
		m.armedAt = now
		m.values = []observation{{reading: sample(tc.value, "WMI", now)}}
		s := m.status(now)
		if s.State != tc.state || (s.Abort != "") != (tc.state == Limit) {
			t.Fatalf("cutoff limit %v value %v: %+v", tc.limit, tc.value, s)
		}
	}
}

type testReader struct {
	mu     sync.Mutex
	value  float64
	fail   bool
	closed int
}

func (r *testReader) Read() (Reading, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return Reading{}, errors.New("offline")
	}
	return sample(r.value, "WMI", time.Now()), nil
}
func (r *testReader) Close() { r.mu.Lock(); r.closed++; r.mu.Unlock() }

func TestPrepareRequiresSensorAndSupportsRepeatedRuns(t *testing.T) {
	empty := NewWithReaders(20*time.Millisecond, time.Millisecond)
	defer empty.Close()
	if s := empty.Prepare(context.Background(), 85); s.Abort == "" || s.Known {
		t.Fatalf("load permitted without a sensor: %+v", s)
	}
	if s := empty.Prepare(context.Background(), 0); s.Abort != "" || s.State != Off {
		t.Fatalf("disabled cutoff required a sensor: %+v", s)
	}
	r := &testReader{value: 70}
	m := NewWithReaders(time.Second, time.Millisecond, r)
	defer m.Close()
	for range 2 {
		if s := m.Prepare(context.Background(), 85); !s.Known || s.State != Active {
			t.Fatalf("prepare: %+v", s)
		}
		m.Disarm()
	}
}

type blockedReader struct{ release chan struct{} }

func (r *blockedReader) Read() (Reading, error) {
	<-r.release
	return Reading{}, errors.New("blocked provider")
}
func (r *blockedReader) Close() {}

func TestBlockedProviderDoesNotBlockHealthySensorOrLossDetection(t *testing.T) {
	r := &blockedReader{release: make(chan struct{})}
	healthy := &testReader{value: 60}
	m := NewWithReaders(30*time.Millisecond, time.Millisecond, r, healthy)
	defer func() { close(r.release); m.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s := m.Prepare(ctx, 85); !s.Known || s.State != Active {
		t.Fatalf("blocked preferred provider prevented fallback: %+v", s)
	}
	healthy.mu.Lock()
	healthy.fail = true
	healthy.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s := m.Snapshot(); s.State == Lost {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("blocked provider prevented readings-loss cutoff")
}

func TestCloseIsIdempotentAndReleasesReaders(t *testing.T) {
	r := &testReader{value: 60}
	m := NewWithReaders(MaxAge, time.Millisecond, r)
	m.Close()
	m.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed != 1 {
		t.Fatalf("reader closed %d times", r.closed)
	}
}

func TestCPUSelectionsRejectWrongMetricsAndInvalidValues(t *testing.T) {
	now := time.Now()
	hw := []hwinfo.Reading{
		{Type: hwinfo.TypeTemp, Name: "GPU Temperature", Unit: "°C", Value: 99},
		{Type: hwinfo.TypeTemp, Name: "CPU (Tctl/Tdie)", Unit: "°C", Value: math.NaN()},
		{Type: hwinfo.TypeTemp, Name: "CPU Package", Unit: "F", Value: 190},
		{Type: hwinfo.TypeTemp, Name: "CPU Package", Unit: "°C", Value: 75},
	}
	if r, ok := hwTemperature(hw, now, now); !ok || r.Celsius != 75 || r.Sensor != "CPU Package" {
		t.Fatalf("HWiNFO selection: %+v %v", r, ok)
	}
	if _, ok := hwTemperature(hw, now.Add(-time.Minute), now); ok {
		t.Fatal("stale HWiNFO mapping counted as fresh")
	}
	wmi := []wmisensor.Reading{{Type: "Temperature", Name: "GPU Core", Value: 99}, {Type: "Temperature", Name: "CPU Package", Value: math.Inf(1)}, {Type: "Temperature", Name: "CPU Core", Value: 65}}
	if r, ok := wmiTemperature(wmi, now, now, "LibreHardwareMonitor"); !ok || r.Celsius != 65 || r.Source != "WMI/LibreHardwareMonitor" {
		t.Fatalf("WMI selection: %+v %v", r, ok)
	}
	if _, ok := wmiTemperature(wmi, now.Add(-time.Minute), now, "test"); ok {
		t.Fatal("stale WMI cache counted as fresh")
	}
}
