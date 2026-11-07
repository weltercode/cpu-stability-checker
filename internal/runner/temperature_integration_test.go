package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/journal"
	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

type runTemperature struct {
	value   float64
	offline atomic.Bool
}

func (r *runTemperature) Read() (temperature.Reading, error) {
	if r.offline.Load() {
		return temperature.Reading{}, errors.New("sensor disconnected")
	}
	return temperature.Reading{Celsius: r.value, Source: "HWiNFO", Sensor: "CPU Package", At: time.Now(), CPU: true}, nil
}
func (r *runTemperature) Close() {}

func TestTemperaturePreflightDoesNotStartLoad(t *testing.T) {
	preserveRunnerState(t)
	for _, tc := range []struct {
		name    string
		value   float64
		missing bool
		exit    int
	}{{"over limit", 95, false, 1}, {"no sensor", 60, true, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runTemperature{value: tc.value}
			r.offline.Store(tc.missing)
			m := temperature.NewWithReaders(20*time.Millisecond, time.Millisecond, r)
			panel := dashboard.NewWithTemperature(os.Stdout, m)
			defer panel.Stop()
			var calls atomic.Int32
			workload.Register(&workload.Workload{Name: "temp-preflight", Run: func(context.Context, workload.Reporter) (uint64, error) { calls.Add(1); return 0, nil }})
			defer workload.Delete("temp-preflight")
			opt := plan.Options{TestName: "general", Cores: []int{0}, A: "temp-preflight", Workers: 1, Duration: time.Second, ControlCore: -1, MaxTempC: 85, LogFile: filepath.Join(t.TempDir(), "run.log")}
			res := ExecuteWithPanel(opt, panel)
			if res.Exit != tc.exit || calls.Load() != 0 || len(res.Faults) != 0 {
				t.Fatalf("unsafe preflight: %+v calls=%d", res, calls.Load())
			}
			if rep := readRun(t, opt.LogFile); len(rep.Cores) != 0 {
				t.Fatalf("load started: %+v", rep.Cores)
			}
		})
	}
}

func TestTemperatureLossStopsRunEvenWithStopOnFaultDisabled(t *testing.T) {
	preserveRunnerState(t)
	r := &runTemperature{value: 60}
	m := temperature.NewWithReaders(30*time.Millisecond, time.Millisecond, r)
	panel := dashboard.NewWithTemperature(os.Stdout, m)
	defer panel.Stop()
	workload.Register(&workload.Workload{Name: "temp-wait", Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
		rep.Add(1)
		rep.Beat()
		r.offline.Store(true)
		<-ctx.Done()
		return 1, nil
	}})
	defer workload.Delete("temp-wait")
	opt := plan.Options{TestName: "general", Cores: []int{0}, A: "temp-wait", Workers: 1, Duration: 3 * time.Second,
		StopOnFault: false, ControlCore: -1, MaxTempC: 85, LogFile: filepath.Join(t.TempDir(), "run.log")}
	start := time.Now()
	res := ExecuteWithPanel(opt, panel)
	if res.Exit != 1 || !res.Aborted || len(res.Faults) != 0 || !strings.Contains(res.AbortMsg, "readings lost") {
		t.Fatalf("temperature loss did not abort: %+v", res)
	}
	if time.Since(start) >= opt.Duration {
		t.Fatal("temperature loss waited for full duration")
	}
	rep := readRun(t, opt.LogFile)
	if rep.Cores[0].Status != journal.Interrupted {
		t.Fatalf("lost sensor blamed CPU or passed: %+v", rep.Cores)
	}
}
