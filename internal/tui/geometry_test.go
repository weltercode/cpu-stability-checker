package tui

import (
	"testing"
	"time"
)

// The gate is the answer to the measured full <-> ~12x8 flicker: the
// panel re-queried the terminal every second and cleared+reflowed on
// every flip, so the user watched the frame alternate between the
// whole dashboard and unreadable fragments. These cases pin the
// acceptance policy with a fake clock: one collapsed observation
// never wins, alternating candidates never confirm each other, a
// failed query neither confirms nor resets, and a genuinely persistent
// geometry (a real resize, or a host that really reports 12x8) is
// adopted after the wait rather than masked forever.
func TestGeometryGate(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	type obs struct {
		dt   time.Duration
		cols int
		rows int
		ok   bool
		want [2]int
	}
	cases := []struct {
		name string
		obs  []obs
	}{
		{"first valid observation is adopted at once", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
		}},
		{"failed first query waits, next valid one adopts", []obs{
			{0, 0, 0, false, [2]int{0, 0}},
			{time.Second, 100, 40, true, [2]int{100, 40}},
		}},
		{"single blip never displaces the adopted geometry", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
			{time.Second, 12, 8, true, [2]int{120, 30}},
			{2 * time.Second, 120, 30, true, [2]int{120, 30}},
			{3 * time.Second, 12, 8, true, [2]int{120, 30}},
			{4 * time.Second, 120, 30, true, [2]int{120, 30}},
		}},
		{"alternating candidates never confirm each other", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
			{time.Second, 12, 8, true, [2]int{120, 30}},
			{2 * time.Second, 120, 30, true, [2]int{120, 30}},
			{3 * time.Second, 12, 8, true, [2]int{120, 30}},
			{4 * time.Second, 120, 30, true, [2]int{120, 30}},
			{5 * time.Second, 12, 8, true, [2]int{120, 30}},
			{6 * time.Second, 120, 30, true, [2]int{120, 30}}, // strict alternation: pending restarts forever
		}},
		{"persistent new geometry is adopted after the wait, not before", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
			{400 * time.Millisecond, 100, 25, true, [2]int{120, 30}},  // first sighting
			{800 * time.Millisecond, 100, 25, true, [2]int{120, 30}},  // count 2, elapsed 0.4s
			{1200 * time.Millisecond, 100, 25, true, [2]int{120, 30}}, // count 3, elapsed 0.8s
			{1400 * time.Millisecond, 100, 25, true, [2]int{100, 25}}, // count 4, elapsed 1.0s
		}},
		{"failed query neither confirms nor resets the pending candidate", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
			{400 * time.Millisecond, 12, 8, true, [2]int{120, 30}},
			{800 * time.Millisecond, 0, 0, false, [2]int{120, 30}},
			{1200 * time.Millisecond, 12, 8, true, [2]int{120, 30}}, // count 2, elapsed 0.8s
			{1500 * time.Millisecond, 12, 8, true, [2]int{12, 8}},   // count 3, elapsed 1.1s: persistent, adopt
		}},
		{"a third candidate restarts the clock", []obs{
			{0, 120, 30, true, [2]int{120, 30}},
			{400 * time.Millisecond, 12, 8, true, [2]int{120, 30}},
			{800 * time.Millisecond, 40, 12, true, [2]int{120, 30}},
			{1200 * time.Millisecond, 12, 8, true, [2]int{120, 30}},
			{1500 * time.Millisecond, 12, 8, true, [2]int{120, 30}}, // 12x8 pending only since 1.2s
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := t0
			g := &GeometryGate{Now: func() time.Time { return now }}
			for i, o := range c.obs {
				now = t0.Add(o.dt)
				cols, rows := g.Observe(o.cols, o.rows, o.ok)
				if cols != o.want[0] || rows != o.want[1] {
					t.Fatalf("obs %d (%dx%d ok=%v @%v) = (%d,%d), want (%d,%d)",
						i, o.cols, o.rows, o.ok, o.dt, cols, rows, o.want[0], o.want[1])
				}
			}
		})
	}
}

// State exposes the decision record for tracing; the trace line is
// what answers "which number lied" after the fact.
func TestGeometryGateState(t *testing.T) {
	now := t0State()
	g := &GeometryGate{Now: func() time.Time { return now }}
	g.Observe(120, 30, true)
	g.Observe(12, 8, true)
	st := g.State()
	if st.AccCols != 120 || st.AccRows != 30 || st.PendCols != 12 || st.PendRows != 8 || st.PendCount != 1 {
		t.Fatalf("State = %+v, want acc 120x30 pending 12x8 count 1", st)
	}
}

func t0State() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }

// An unset CPU_STABILITY_SIZEDIAG means no trace and no file: the
// panel never writes anything nobody asked for.
func TestSizeTraceDisabledByDefault(t *testing.T) {
	if tr := NewSizeTrace(); tr != nil {
		t.Fatalf("NewSizeTrace with env unset = %v, want nil", tr)
	}
}
