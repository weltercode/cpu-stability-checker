package dashboard

import (
	"fmt"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
)

// The PDH "% Processor Performance" counter is utilization-weighted,
// not a frequency: the panel captures that motivated this logic showed
// an idle thread reading 31 MHz and a 56%-busy thread "recovering" an
// impossible 8357 MHz once the utilization was divided back out. Only
// a saturated (>=80%) thread's ratio is trusted; its estimate is shown
// on both SMT siblings, and cores without one honestly show "--".
func TestCoreClocksFromPDH(t *testing.T) {
	cases := []struct {
		name   string
		perf   map[int]float64
		busy   []float64
		coreOf []int
		base   float64
		want   map[int]float64
	}{
		{"idle sibling mirrors the busy one", map[int]float64{0: 5539, 1: 31},
			[]float64{100, 0}, []int{0, 0}, 4300,
			map[int]float64{0: 5539, 1: 5539}},
		{"partial thread is not trusted", map[int]float64{0: 883},
			[]float64{16.0}, nil, 4300,
			map[int]float64{}}, // 16% is below the trust threshold
		{"half-busy thread is not trusted either", map[int]float64{0: 4680},
			[]float64{56}, nil, 4300,
			map[int]float64{}}, // the user's capture: this "recovered" 8357 MHz
		{"best estimate wins across siblings", map[int]float64{0: 2760, 1: 4431},
			[]float64{50, 80}, []int{0, 0}, 4300,
			map[int]float64{0: 5539, 1: 5539}}, // only the 80% thread is trusted
		{"fully idle core has no honest clock", map[int]float64{0: 31, 1: 20},
			[]float64{0, 0}, []int{0, 0}, 4300,
			map[int]float64{}},
		{"implausible estimate dropped", map[int]float64{0: 100},
			[]float64{100}, nil, 4300,
			map[int]float64{}},
		{"downclocked saturated core stays plausible", map[int]float64{0: 1500},
			[]float64{100}, nil, 4300,
			map[int]float64{0: 1500}},
		{"no base no clocks", map[int]float64{0: 5539},
			[]float64{100}, nil, 0,
			map[int]float64{}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := coreClocks(tt.perf, tt.busy, tt.coreOf, tt.base)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for cpu, want := range tt.want {
				if got[cpu] < want*0.995 || got[cpu] > want*1.005 {
					t.Errorf("cpu %d = %.1f, want ~%.1f", cpu, got[cpu], want)
				}
			}
		})
	}
}

// HWiNFO's effective-clock counter barely ticks while a thread is
// halted: 15-35 MHz on a parked core, and 120 MHz on the idle SMT
// sibling of a core whose other thread runs at 100%. Both readings are
// real but measure "off", so they drop to "--"; a thread doing
// something keeps its honest time-averaged clock (385 MHz at 9%).
func TestParkedCoreClocks(t *testing.T) {
	cases := []struct {
		name   string
		busy   []float64
		cpu    int
		mhz    float64
		floor  float64
		parked bool
	}{
		{"idle core at 17 MHz is off, not slow", []float64{0, 0}, 1, 17, 430, true},
		{"idle sibling of a busy core is still halted", []float64{100, 2}, 1, 120, 430, true},
		{"lightly loaded thread keeps its average", []float64{9, 12}, 0, 385, 430, false},
		{"busy core's sibling reading is the core clock", []float64{100, 11}, 1, 5291, 430, false},
		{"above the floor stays even when idle", []float64{0, 0}, 0, 500, 430, false},
		{"no busy data, no judgement", nil, 0, 17, 430, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := parkedCore(tt.busy, tt.cpu, tt.mhz, tt.floor); got != tt.parked {
				t.Errorf("parkedCore() = %v, want %v", got, tt.parked)
			}
		})
	}
}

func TestMergeClocksDropsParkedReadings(t *testing.T) {
	// WMI's low idle reading is filtered; HWiNFO's ordinary Core Clock
	// is mapped from the physical core to both logical siblings.
	s := &Sampler{
		hwClocks:  map[int]float64{0: 5693},
		wmiClocks: map[int]float64{10: 20},
	}
	snap := &Snapshot{
		Busy:   []float64{100, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		CoreOf: []int{0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5},
	}
	got := s.mergeClocks(nil, snap)
	want := map[int]float64{0: 5693, 1: 5693}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for cpu, w := range want {
		if got[cpu] != w {
			t.Errorf("cpu %d = %v, want %v", cpu, got[cpu], w)
		}
	}
}

func TestHWiNFOCoreClockRemainsVisibleForAllThreadsAfterLoad(t *testing.T) {
	var reads []hwinfo.Reading
	for core := 0; core < 16; core++ {
		reads = append(reads,
			hwinfo.Reading{Type: hwinfo.TypeClock, Name: fmt.Sprintf("Core %d Clock (perf #%d)", core, 16-core), Unit: "MHz", Value: 5000 + float64(core)},
			hwinfo.Reading{Type: hwinfo.TypeClock, Name: fmt.Sprintf("Core %d T0 Effective Clock", core), Unit: "MHz", Value: 15},
			hwinfo.Reading{Type: hwinfo.TypeClock, Name: fmt.Sprintf("Core %d T1 Effective Clock", core), Unit: "MHz", Value: 2})
	}
	s := &Sampler{hwClocks: hwinfo.CoreClocks(reads), wmiClocks: map[int]float64{0: 4000}}
	snap := &Snapshot{Busy: make([]float64, 32), CoreOf: make([]int, 32)}
	for cpu := range snap.CoreOf {
		snap.CoreOf[cpu] = cpu / 2
	}
	for _, busy := range []float64{100, 15, 0} {
		for cpu := range snap.Busy {
			snap.Busy[cpu] = busy
		}
		got := s.mergeClocks(map[int]float64{0: 6000}, snap)
		if len(got) != 32 {
			t.Fatalf("%.0f%% busy: clocks lost after workload: %v", busy, got)
		}
		for cpu := 0; cpu < 32; cpu++ {
			if want := 5000 + float64(cpu/2); got[cpu] != want {
				t.Fatalf("CPU %d at %.0f%% busy: got %.0f, want %.0f", cpu, busy, got[cpu], want)
			}
		}
	}
}

func TestPhysicalClockMappingUsesTopology(t *testing.T) {
	physical := map[int]float64{0: 5100, 1: 5300}
	got := logicalClocks(physical, []int{0, 1, 0, 1}, 4)
	for cpu, want := range []float64{5100, 5300, 5100, 5300} {
		if got[cpu] != want {
			t.Fatalf("nonadjacent SMT sibling %d: got %.0f, want %.0f", cpu, got[cpu], want)
		}
	}
	if got := logicalClocks(physical, nil, 4); len(got) != 0 {
		t.Fatalf("guessed topology: %v", got)
	}
	if got := logicalClocks(map[int]float64{0: 5100}, []int{0, 1, 0, 1}, 4); len(got) != 2 {
		t.Fatalf("missing physical sensor was invented: %v", got)
	}
}
