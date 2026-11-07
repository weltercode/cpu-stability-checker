package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/dashboard"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
	"github.com/weltercode/cpu-stability-checker/internal/temperature"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

type liveMenuTemperature struct{ reads atomic.Int32 }

func (r *liveMenuTemperature) Read() (temperature.Reading, error) {
	r.reads.Add(1)
	return temperature.Reading{Celsius: 60, Source: "test", Sensor: "CPU Package", At: time.Now(), CPU: true}, nil
}
func (r *liveMenuTemperature) Close() {}

type heldMenuInput struct {
	reading, release chan struct{}
	once             sync.Once
	eof              bool
}

func (r *heldMenuInput) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.reading) })
	<-r.release
	if r.eof {
		return 0, io.EOF
	}
	p[0] = '\n'
	return 1, nil
}

func TestMenuKeepsMonitoringAfterSessionUntilEnterOrEOF(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	mask, known := sysinfo.ProcessAffinity()
	t.Cleanup(func() {
		runtime.GOMAXPROCS(procs)
		if known {
			var cores []int
			for i := range 64 {
				if mask&(uint64(1)<<uint(i)) != 0 {
					cores = append(cores, i)
				}
			}
			if err := sysinfo.SetAffinity(cores); err != nil {
				t.Error(err)
			}
		}
	})
	for _, tc := range []struct {
		name       string
		fault, eof bool
	}{{"completed then Enter", false, false}, {"faulted then Enter", true, false}, {"completed then EOF", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			sensor := &liveMenuTemperature{}
			monitor := temperature.NewWithReaders(time.Second, 5*time.Millisecond, sensor)
			file, err := os.CreateTemp(t.TempDir(), "panel")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			panel := dashboard.NewWithTemperature(file, monitor)
			ui := &screenUI{panel: panel}
			defer ui.finish()
			workload.Register(&workload.Workload{Name: "menu-wait-test", Run: func(ctx context.Context, rep workload.Reporter) (uint64, error) {
				if tc.fault {
					return 0, errors.New("injected fault")
				}
				rep.Add(1)
				rep.Beat()
				<-ctx.Done()
				return 1, nil
			}})
			defer workload.Delete("menu-wait-test")
			control := -1
			// 500ms, not 20ms: under a full-suite load a worker thread can
			// miss a 20ms window entirely, and a run whose mandatory slot
			// never started honestly exits 1 (incomplete mandatory check).
			session := SessionOptions{Mode: sessionAllCore, PhysicalCores: []int{0}, Workload: "menu-wait-test", ThreadsPerCore: 1,
				Duration: 500 * time.Millisecond, Passes: 1, StopOnFault: true, ControlCore: &control, MaxTemp: 85, LogFile: filepath.Join(t.TempDir(), "run.log")}
			topology := sysinfo.Topology{PhysicalCores: []sysinfo.PhysicalCore{{Index: 0, LogicalCPUs: []sysinfo.LogicalCPU{{Number: 0}}}}}
			code := runSessionWithPanel(session, topology, ui.monitor(), nil, "")
			want := 0
			if tc.fault {
				want = 1
			}
			if code != want || panel.Stopped() {
				t.Fatalf("session closed menu monitoring: exit=%d stopped=%v", code, panel.Stopped())
			}
			// A completed run must not leave a stale progress footer while waiting.
			panel.Push(dashboard.Snapshot{HasRun: true, Label: "finished"})
			input := &heldMenuInput{reading: make(chan struct{}), release: make(chan struct{}), eof: tc.eof}
			done := make(chan struct{})
			var release sync.Once
			go func() { defer close(done); ui.waitForEnter(bufio.NewReader(input)) }()
			defer func() { release.Do(func() { close(input.release) }); <-done }()
			select {
			case <-input.reading:
			case <-time.After(time.Second):
				t.Fatal("menu did not reach Enter prompt")
			}
			if panel.Current().HasRun || panel.Stopped() {
				t.Fatal("monitor stopped or kept finished progress at Enter prompt")
			}
			before := sensor.reads.Load()
			deadline := time.Now().Add(time.Second)
			for sensor.reads.Load() <= before && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if sensor.reads.Load() <= before {
				t.Fatal("hardware sampling froze while waiting for input")
			}
			release.Do(func() { close(input.release) })
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("menu did not release monitoring after input")
			}
			if !panel.Stopped() || ui.panel != nil {
				t.Fatal("panel survived return to the menu")
			}
			data, err := os.ReadFile(file.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "Press Enter to return to the menu") != 1 {
				t.Fatalf("missing or duplicate prompt: %s", data)
			}
		})
	}
}

func TestMenuReleasesPanelOnScreenChangeAndExit(t *testing.T) {
	for _, finish := range []bool{false, true} {
		ui := &screenUI{}
		panel := ui.monitor()
		if finish {
			ui.finish()
		} else {
			ui.screenPrompt(nil, "")
		}
		if !panel.Stopped() || ui.panel != nil {
			t.Fatal("previous panel retained after menu took ownership")
		}
	}
}
