package subprocess

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/clocksrc"
)

// fakeCounter hands out scripted readings, then reports the child gone.
func fakeCounter(vals ...uint64) func() (uint64, error) {
	i := 0
	return func() (uint64, error) {
		if i < len(vals) {
			v := vals[i]
			i++
			return v, nil
		}
		return 0, errors.New("child gone")
	}
}

// The sampler must mirror the in-process contract: first tick only
// baselines, deltas become exact MHz over the scripted window, and
// anything unknown shrinks the line instead of faking a zero.
func TestProcTelSample(t *testing.T) {
	t0 := time.Now()
	noLive := clocksrc.Sample{}

	t.Run("first tick only establishes baselines", func(t *testing.T) {
		tel := &procTel{}
		kids := map[int]kidSnap{0: {cycles: fakeCounter(1000)}}
		if line := tel.sample(kids, []int{0}, noLive, false, nil, t0); line != "" {
			t.Errorf("first tick = %q, want baseline only (empty)", line)
		}
	})

	t.Run("cycle delta over the window is the observed rate", func(t *testing.T) {
		tel := &procTel{}
		kids := map[int]kidSnap{0: {cycles: fakeCounter(1000, 1000+4_100_000_000)}}
		tel.sample(kids, nil, noLive, false, nil, t0)
		line := tel.sample(kids, nil, noLive, false, nil, t0.Add(time.Second))
		if !strings.Contains(line, "observed cycle rate min 4100 avg 4100 max 4100 MHz") {
			t.Errorf("line = %q, want observed cycle rate min 4100 avg 4100 max 4100 MHz", line)
		}
	})

	t.Run("a zero delta is a real reading, not a missing one", func(t *testing.T) {
		tel := &procTel{}
		kids := map[int]kidSnap{0: {cycles: fakeCounter(500, 500)}}
		tel.sample(kids, nil, noLive, false, nil, t0)
		line := tel.sample(kids, nil, noLive, false, nil, t0.Add(time.Second))
		if !strings.Contains(line, "observed cycle rate min 0 avg 0 max 0 MHz") {
			t.Errorf("line = %q, want observed cycle rate min 0 avg 0 max 0 MHz", line)
		}
	})

	t.Run("frozen children never depress the observed rate", func(t *testing.T) {
		tel := &procTel{}
		c := fakeCounter(0, 1_000_000_000, 2_000_000_000, 5_000_000_000)
		kids := map[int]kidSnap{0: {cycles: c}}
		tel.sample(kids, nil, noLive, false, nil, t0)
		kids[0] = kidSnap{cycles: c, frozen: true}
		if line := tel.sample(kids, nil, noLive, false, nil, t0.Add(time.Second)); strings.Contains(line, "cycle rate") {
			t.Errorf("frozen child leaked into the observed rate: %q", line)
		}
		// The thaw bumps the rotation epoch: the crossing window is still
		// meaningless, but the one after it measures cleanly.
		kids[0] = kidSnap{cycles: c, epoch: 1}
		if line := tel.sample(kids, nil, noLive, false, nil, t0.Add(2*time.Second)); strings.Contains(line, "cycle rate") {
			t.Errorf("freeze-crossing window leaked into the observed rate: %q", line)
		}
		kids[0] = kidSnap{cycles: c, epoch: 1}
		line := tel.sample(kids, nil, noLive, false, nil, t0.Add(3*time.Second))
		if !strings.Contains(line, "observed cycle rate min 3000 avg 3000 max 3000 MHz") { // 3e9 cycles / 1s
			t.Errorf("post-thaw line = %q, want observed cycle rate min 3000 avg 3000 max 3000 MHz", line)
		}
	})

	t.Run("a vanished counter rebaselines when it returns", func(t *testing.T) {
		tel := &procTel{}
		c := fakeCounter(1000, 1000+1_000_000_000) // then errors: child gone
		kids := map[int]kidSnap{0: {cycles: c}}
		tel.sample(kids, nil, noLive, false, nil, t0)
		if line := tel.sample(kids, nil, noLive, false, nil, t0.Add(time.Second)); !strings.Contains(line, "observed cycle rate min 1000 avg 1000 max 1000 MHz") {
			t.Errorf("line = %q, want observed cycle rate min 1000 avg 1000 max 1000 MHz before the counter vanishes", line)
		}
		if line := tel.sample(kids, nil, noLive, false, nil, t0.Add(2*time.Second)); strings.Contains(line, "cycle rate") {
			t.Errorf("vanished counter leaked into the observed rate: %q", line)
		}
		// A readable counter again starts a fresh baseline, and the next
		// window measures from it.
		kids[0] = kidSnap{cycles: fakeCounter(9000, 9000+2_500_000_000)}
		tel.sample(kids, nil, noLive, false, nil, t0.Add(3*time.Second))
		line := tel.sample(kids, nil, noLive, false, nil, t0.Add(4*time.Second))
		if !strings.Contains(line, "observed cycle rate min 2500 avg 2500 max 2500 MHz") {
			t.Errorf("rebaselined line = %q, want observed cycle rate min 2500 avg 2500 max 2500 MHz", line)
		}
	})

	t.Run("live core clock maps through topology and names its source", func(t *testing.T) {
		tel := &procTel{}
		kids := map[int]kidSnap{4: {cycles: fakeCounter(1000, 1000+4_100_000_000)}}
		live := clocksrc.Sample{Clocks: map[int]float64{2: 5500}, Src: clocksrc.SrcHWinfo, Basis: clocksrc.BasisProducer, At: t0}
		coreOf := []int{0, 1, 2, 3, 2, 5} // logical 4 sits on physical core 2
		tel.sample(kids, []int{4}, live, true, coreOf, t0)
		line := tel.sample(kids, []int{4}, live, true, coreOf, t0.Add(time.Second))
		if !strings.Contains(line, "core clock min 5500 avg 5500 max 5500 MHz (hwinfo)") {
			t.Errorf("line = %q, want the core clock segment naming hwinfo", line)
		}
	})

	t.Run("unmapped or frozen cores get no core clock attributed", func(t *testing.T) {
		tel := &procTel{}
		live := clocksrc.Sample{Clocks: map[int]float64{2: 5500}, Src: clocksrc.SrcWMI, Basis: clocksrc.BasisAcquired, At: t0}
		coreOf := []int{0, 1, 2, 3, 2, 5} // logical 4 maps to a core the producer does name: only "frozen" may suppress
		kids := map[int]kidSnap{4: {cycles: fakeCounter(1000, 1000), frozen: true}}
		tel.sample(kids, []int{4}, live, true, coreOf, t0)
		line := tel.sample(kids, []int{4}, live, true, coreOf, t0.Add(time.Second))
		if strings.Contains(line, "core clock") {
			t.Errorf("frozen child got a core clock anyway: %q", line)
		}
		// No mapping: attribution stays unknown even for an alive child.
		tel2 := &procTel{}
		kids2 := map[int]kidSnap{4: {cycles: fakeCounter(1000, 1000)}}
		tel2.sample(kids2, []int{4}, live, true, nil, t0)
		if line := tel2.sample(kids2, []int{4}, live, true, nil, t0.Add(time.Second)); strings.Contains(line, "core clock") {
			t.Errorf("unmapped topology produced a core clock anyway: %q", line)
		}
	})
}

// The stderr transport must survive a child's noise: the telemetry line
// is extracted, removed from the error surface, and unknowns (crash, no
// line, malformed line) come back as nil.
func TestWorkerTelemetryTransport(t *testing.T) {
	t.Run("line is parsed and stripped from stderr", func(t *testing.T) {
		buf := new(bytes.Buffer)
		buf.WriteString("worker: something noisy happened\n")
		buf.WriteString("worker-telemetry {\"clock_mhz\":4285,\"idle_pct\":0,\"window_ms\":30000}\n")
		buf.WriteString("trailing noise\n")
		tr := takeWorkerTelemetry(buf)
		if tr == nil || tr.ClockMHz != 4285 || tr.WindowMS != 30000 {
			t.Fatalf("takeWorkerTelemetry = %+v, want clock 4285 window 30000", tr)
		}
		if tr.IdlePct == nil || *tr.IdlePct != 0 {
			t.Errorf("idle = %v, want real 0%% present", tr.IdlePct)
		}
		if s := buf.String(); strings.Contains(s, "worker-telemetry") {
			t.Errorf("stderr still surfaces the telemetry line: %q", s)
		}
	})

	t.Run("a child that never published reads as nil", func(t *testing.T) {
		buf := new(bytes.Buffer)
		buf.WriteString("panic: runtime error\n")
		if tr := takeWorkerTelemetry(buf); tr != nil {
			t.Errorf("takeWorkerTelemetry = %+v, want nil (unknown, not zero)", tr)
		}
	})

	t.Run("a malformed line is dropped, not trusted", func(t *testing.T) {
		buf := new(bytes.Buffer)
		buf.WriteString("worker-telemetry {oops\n")
		if tr := takeWorkerTelemetry(buf); tr != nil {
			t.Errorf("takeWorkerTelemetry = %+v, want nil for malformed payload", tr)
		}
	})
}
