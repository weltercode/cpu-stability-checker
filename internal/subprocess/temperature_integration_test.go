package subprocess

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
)

type childTemperature struct {
	value          float64
	missing        bool
	loseAfterStart string
}

func (r *childTemperature) Read() (temperature.Reading, error) {
	if r.missing {
		return temperature.Reading{}, errors.New("no temperature sensor")
	}
	if r.loseAfterStart != "" {
		data, _ := os.ReadFile(r.loseAfterStart)
		if !bytes.Contains(data, []byte(`"type":"core_start"`)) {
			// per-process mode: each child writes core_start to its own
			// fragment under the run's .workers tree, never to main.
			files, _ := filepath.Glob(r.loseAfterStart + ".workers" + string(filepath.Separator) + "*" + string(filepath.Separator) + "*.jsonl")
			for _, f := range files {
				if d, _ := os.ReadFile(f); bytes.Contains(d, []byte(`"type":"core_start"`)) {
					data = d
					break
				}
			}
		}
		if bytes.Contains(data, []byte(`"type":"core_start"`)) {
			return temperature.Reading{}, errors.New("sensor disconnected")
		}
	}
	return temperature.Reading{Celsius: r.value, Source: "WMI/LibreHardwareMonitor", Sensor: "CPU Package", At: time.Now(), CPU: true}, nil
}
func (r *childTemperature) Close() {}

func TestSubprocessTemperaturePreflightDoesNotStartChildren(t *testing.T) {
	useTestBinaryAsWorker(t)
	for _, tc := range []struct {
		name    string
		value   float64
		missing bool
		exit    int
	}{{"hot", 95, false, 1}, {"missing", 60, true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &childTemperature{value: tc.value, missing: tc.missing}
			m := temperature.NewWithReaders(20*time.Millisecond, time.Millisecond, r)
			panel := dashboard.NewWithTemperature(os.Stdout, m)
			defer panel.Stop()
			opt := plan.Options{TestName: "general", Cores: []int{0}, A: "int", Workers: 1, Duration: time.Second,
				ControlCore: -1, MaxTempC: 85, LogFile: filepath.Join(t.TempDir(), "run.log")}
			res := ExecuteSubprocessWithPanel(opt, panel)
			if res.Exit != tc.exit || len(res.Faults) != 0 {
				t.Fatalf("unsafe preflight: %+v", res)
			}
			if rep := readRecoveredRun(t, journal.Path(opt.LogFile)); len(rep.Cores) != 0 {
				t.Fatalf("children started before temperature ready: %+v", rep.Cores)
			}
		})
	}
}

func TestSubprocessTemperatureLossStopsChildrenWithoutBlamingCPU(t *testing.T) {
	useTestBinaryAsWorker(t)
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "idle", Workers: 1, Duration: 3 * time.Second,
		StopOnFault: false, ControlCore: -1, MaxTempC: 85, LogFile: filepath.Join(t.TempDir(), "run.log")}
	r := &childTemperature{value: 60, loseAfterStart: journal.Path(opt.LogFile)}
	m := temperature.NewWithReaders(30*time.Millisecond, time.Millisecond, r)
	panel := dashboard.NewWithTemperature(os.Stdout, m)
	defer panel.Stop()
	start := time.Now()
	res := ExecuteSubprocessWithPanel(opt, panel)
	if res.Exit != 1 || !res.Aborted || len(res.Faults) != 0 || !strings.Contains(res.AbortMsg, "readings lost") {
		t.Fatalf("sensor loss did not cancel children: %+v", res)
	}
	if time.Since(start) >= opt.Duration {
		t.Fatal("sensor loss waited for full child duration")
	}
	rep := readRecoveredRun(t, journal.Path(opt.LogFile))
	if len(rep.Cores) != 1 || rep.Cores[0].Status != journal.Interrupted {
		t.Fatalf("cancelled child not recorded as interrupted: %+v", rep.Cores)
	}
}
