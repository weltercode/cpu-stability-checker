package dashboard

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/temperature"
)

type panelTemperature struct {
	offline atomic.Bool
	at      time.Time
}

func (r *panelTemperature) Read() (temperature.Reading, error) {
	if r.offline.Load() {
		return temperature.Reading{}, errors.New("HWiNFO shared memory unavailable")
	}
	return temperature.Reading{Celsius: 72, Source: "HWiNFO", Sensor: "CPU (Tctl/Tdie)", At: r.at, CPU: true}, nil
}
func (r *panelTemperature) Close() {}

func TestPanelUsesTheCutoffObservationAndClearsStaleTemperature(t *testing.T) {
	r := &panelTemperature{at: time.Now()}
	m := temperature.NewWithReaders(200*time.Millisecond, time.Millisecond, r)
	defer m.Close()
	if s := m.Prepare(context.Background(), 85); !s.Known {
		t.Fatalf("prepare: %+v", s)
	}
	sampler := newSamplerWithTemperature(m)
	snap := Snapshot{TempC: 999, TempKnown: true, HWinfo: true}
	sampler.applyTemperature(&snap)
	if snap.TempC != 72 || !snap.TempKnown || snap.Temperature.Reading != m.Snapshot().Reading {
		t.Fatalf("panel and cutoff disagree: %+v", snap)
	}
	for _, size := range [][2]int{{90, 40}, {60, 10}} {
		snap.Busy = make([]float64, 4)
		text := strings.Join(RenderFor(snap, size[0], size[1]), "\n")
		for _, want := range []string{"temp 72.0°C", "control active", "85.0°C", "HWiNFO: CPU (Tctl/Tdie)"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q in %s", want, text)
			}
		}
	}
	r.offline.Store(true)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sampler.applyTemperature(&snap)
		if snap.Temperature.State == temperature.Lost {
			if snap.TempKnown || snap.TempC != 0 || snap.Temperature.Abort == "" {
				t.Fatalf("stale temperature still displayed as current: %+v", snap)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("panel did not show loss of readings")
}

// R26 (owner UX contract 2026-11-10): the live panel carries states,
// not error reports - "sensor unavailable" is the state phrase and the
// reason must NEVER be recited mid-run (the owner saw the HWiNFO pipe
// error wall inside a running test). The full reason stays honest
// where it is read deliberately: Diagnostics -> Telemetry capabilities
// and the run report.
func TestUnavailableTemperaturePanelStaysCondensed(t *testing.T) {
	s := Snapshot{Temperature: temperature.Status{State: temperature.Off, Reason: "HWiNFO: enable Shared Memory Support"}}
	text := strings.Join(Render(s, 4), "\n")
	if !strings.Contains(text, "control off") || !strings.Contains(text, "sensor unavailable") {
		t.Fatalf("missing condensed unavailable state: %s", text)
	}
	if strings.Contains(text, "Shared Memory Support") {
		t.Fatalf("panel recites the reason wall mid-run: %s", text)
	}
}
