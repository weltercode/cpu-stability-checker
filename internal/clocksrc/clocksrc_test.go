package clocksrc

import (
	"errors"
	"testing"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/hwinfo"
	"github.com/weltercode/cpu-stability-checker/internal/wmisensor"
)

type fakeHW struct {
	snapFn func() ([]hwinfo.Reading, time.Time)
	closed bool
}

func (f *fakeHW) Snapshot() ([]hwinfo.Reading, time.Time) { return f.snapFn() }
func (f *fakeHW) Close()                                  { f.closed = true }

type fakeWMI struct {
	reads  []wmisensor.Reading
	closed bool
}

func (f *fakeWMI) Readings() []wmisensor.Reading { return f.reads }
func (f *fakeWMI) Close()                        { f.closed = true }

func hwClock(name string, mhz float64) hwinfo.Reading {
	return hwinfo.Reading{Type: hwinfo.TypeClock, Name: name, Unit: "MHz", Value: mhz}
}

func wmiClock(name string, mhz float64) wmisensor.Reading {
	return wmisensor.Reading{Type: "Clock", Name: name, Value: mhz}
}

func readerWith(hw func() (hwFeed, error), wmi func() (wmiFeed, error)) *Reader {
	return &Reader{openHW: hw, openWMI: wmi}
}

func TestSampleSourcePreference(t *testing.T) {
	now := time.Now()

	t.Run("fresh hwinfo wins and carries the producer stamp", func(t *testing.T) {
		poll := now.Add(-time.Second)
		r := readerWith(
			func() (hwFeed, error) {
				return &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
					return []hwinfo.Reading{hwClock("Core #1 Clock", 5500)}, poll
				}}, nil
			},
			func() (wmiFeed, error) { return nil, errors.New("no wmi feed open path") },
		)
		smp, ok := r.Sample(now)
		if !ok || smp.Src != SrcHWinfo || smp.Basis != BasisProducer || !smp.At.Equal(poll) {
			t.Fatalf("sample = %+v ok=%v, want hwinfo/producer stamped at poll", smp, ok)
		}
		if smp.Clocks[0] != 5500 {
			t.Errorf("clocks = %v, want core 0 at 5500", smp.Clocks)
		}
	})

	t.Run("stale hwinfo falls through to wmi (rereading launders nothing)", func(t *testing.T) {
		stale := now.Add(-maxProducerAge - time.Second)
		r := readerWith(
			func() (hwFeed, error) {
				return &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
					return []hwinfo.Reading{hwClock("Core #1 Clock", 5500)}, stale
				}}, nil
			},
			func() (wmiFeed, error) { return &fakeWMI{reads: []wmisensor.Reading{wmiClock("Core #1", 5400)}}, nil },
		)
		smp, ok := r.Sample(now)
		if !ok || smp.Src != SrcWMI || smp.Basis != BasisAcquired {
			t.Fatalf("sample = %+v ok=%v, want the WMI fallback labeled acquired", smp, ok)
		}
	})

	t.Run("a future producer stamp is rejected, not trusted", func(t *testing.T) {
		r := readerWith(
			func() (hwFeed, error) {
				return &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
					return []hwinfo.Reading{hwClock("Core #1 Clock", 5500)}, now.Add(time.Minute)
				}}, nil
			},
			func() (wmiFeed, error) { return nil, errors.New("no wmi") },
		)
		if _, ok := r.Sample(now); ok {
			t.Fatal("future-stamped sample was accepted")
		}
	})

	t.Run("an unchanged producer stamp is a duplicate, not a fresh tick", func(t *testing.T) {
		poll := now.Add(-time.Second)
		hw := &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
			return []hwinfo.Reading{hwClock("Core #1 Clock", 5500)}, poll
		}}
		r := readerWith(
			func() (hwFeed, error) { return hw, nil },
			func() (wmiFeed, error) { return nil, errors.New("no wmi") },
		)
		if _, ok := r.Sample(now); !ok {
			t.Fatal("first fresh tick rejected")
		}
		if _, ok := r.Sample(now.Add(time.Second)); ok {
			t.Fatal("the same producer stamp was laundered into a fresh sample")
		}
	})

	t.Run("torn snapshot yields nothing", func(t *testing.T) {
		r := readerWith(
			func() (hwFeed, error) {
				return &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
					return nil, time.Time{}
				}}, nil
			},
			func() (wmiFeed, error) { return nil, errors.New("no wmi") },
		)
		if _, ok := r.Sample(now); ok {
			t.Fatal("nil snapshot produced a sample")
		}
	})

	t.Run("no producer at all stays unknown", func(t *testing.T) {
		r := readerWith(
			func() (hwFeed, error) { return nil, errors.New("no HWiNFO") },
			func() (wmiFeed, error) { return nil, errors.New("no WMI provider") },
		)
		if smp, ok := r.Sample(now); ok || smp.Clocks != nil {
			t.Fatalf("sample = %+v ok=%v, want unknown", smp, ok)
		}
	})

	t.Run("reopen backs off and producers that arrive late are picked up", func(t *testing.T) {
		tries := 0
		hwOpened := false
		r := readerWith(
			func() (hwFeed, error) {
				tries++
				if !hwOpened {
					return nil, errors.New("not running yet")
				}
				return &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) {
					return []hwinfo.Reading{hwClock("Core #1 Clock", 5500)}, now.Add(-time.Second)
				}}, nil
			},
			func() (wmiFeed, error) { return nil, errors.New("no wmi") },
		)
		for i := 0; i < openRetryEvery; i++ {
			if _, ok := r.Sample(now); ok {
				t.Fatal("opened too eagerly")
			}
		}
		hwOpened = true
		if _, ok := r.Sample(now); !ok {
			t.Fatalf("late producer missed after %d tries", tries)
		}
	})
}

func TestAggGroups(t *testing.T) {
	t0 := time.Now()
	start := t0
	agg := NewAgg(start)

	hw := Sample{Clocks: map[int]float64{2: 5500}, Src: SrcHWinfo, Basis: BasisProducer, At: t0.Add(-500 * time.Millisecond)}
	agg.Add(hw, 2, t0)
	// Source switch starts a new group.
	wmi := Sample{Clocks: map[int]float64{2: 5400}, Src: SrcWMI, Basis: BasisAcquired, At: t0.Add(time.Second)}
	agg.Add(wmi, 2, t0.Add(time.Second))
	// A core the producer never names contributes nothing.
	hw2 := Sample{Clocks: map[int]float64{5: 5000}, Src: SrcWMI, Basis: BasisAcquired, At: t0.Add(2 * time.Second)}
	agg.Add(hw2, 2, t0.Add(2*time.Second))

	groups := agg.Groups()
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want two (hwinfo then wmi)", groups)
	}
	g0, g1 := groups[0], groups[1]
	if g0.Source != SrcHWinfo || g0.N != 1 || g0.MinMHz != 5500 || g0.AgeBasis != BasisProducer || g0.MaxAgeMS != 500 {
		t.Errorf("group 0 = %+v, want one 5500 sample labeled producer with 500ms age", g0)
	}
	if g1.Source != SrcWMI || g1.N != 1 || g1.MaxMHz != 5400 || g1.AgeBasis != BasisAcquired {
		t.Errorf("group 1 = %+v, want the wmi group labeled acquired", g1)
	}
	if g1.FirstMS != 1000 || g1.LastMS != 1000 {
		t.Errorf("group 1 window offsets = %d..%d, want 1000..1000 relative to the window start", g1.FirstMS, g1.LastMS)
	}

	if a := NewAgg(start); a.Groups() != nil {
		t.Errorf("empty accumulation = %v, want nil: unknown stays absent", a.Groups())
	}
}

func TestReaderCloseReleasesFeeds(t *testing.T) {
	hw := &fakeHW{snapFn: func() ([]hwinfo.Reading, time.Time) { return nil, time.Time{} }}
	wmi := &fakeWMI{}
	r := readerWith(
		func() (hwFeed, error) { return hw, nil },
		func() (wmiFeed, error) { return wmi, nil },
	)
	now := time.Now()
	r.Sample(now) // opens hw; snapshot nil falls to wmi and opens it too
	r.Close()
	if !hw.closed || !wmi.closed {
		t.Errorf("Close() left feeds open: hw=%v wmi=%v", hw.closed, wmi.closed)
	}
}
