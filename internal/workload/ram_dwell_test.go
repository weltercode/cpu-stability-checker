package workload

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// dwellReporter wraps the shared harness with a shortened pause:
// the override changes the dwell LENGTH, never the data contract
// (documented on DwellParamsReporter).
type dwellReporter struct {
	*regionReporter
	base, jitter time.Duration
}

func (d *dwellReporter) RamDwell() (time.Duration, time.Duration) { return d.base, d.jitter }

func dwellTestReporter(t *testing.T, base, jitter time.Duration) *dwellReporter {
	t.Helper()
	return &dwellReporter{regionReporter: ramTestReporter(t), base: base, jitter: jitter}
}

// The production pause spec is part of the kernel's coverage story:
// pinned constants, and the jitter schedule is a golden-pinned pure
// function of (seed, pass) - the same seed replays the same pauses.
func TestRamDwellTargetGolden(t *testing.T) {
	if ramDwellBaseMs != 250 || ramDwellJitterMs != 125 {
		t.Errorf("production dwell spec drifted: base=%dms jitter=%dms, want 250ms/125ms", ramDwellBaseMs, ramDwellJitterMs)
	}
	const ms = time.Millisecond
	for _, c := range []struct {
		seed, pass uint64
		want       time.Duration
	}{
		{7, 1, 955804 * time.Nanosecond},
		{7, 2, 609346 * time.Nanosecond},
		{7, 3, 472203 * time.Nanosecond},
		{7, 7, 389182 * time.Nanosecond},
		{99, 1, 494564 * time.Nanosecond},
	} {
		if got := ramDwellTarget(c.seed, c.pass, 0, ms); got != c.want {
			t.Errorf("ramDwellTarget(%d,%d) jitter = %v, want the pinned %v", c.seed, c.pass, got, c.want)
		}
	}
	// Replay, sensitivity and range over a long pass sequence.
	if ramDwellTarget(7, 5, 10*ms, ms) != ramDwellTarget(7, 5, 10*ms, ms) {
		t.Error("same (seed, pass) produced different pauses: replay is broken")
	}
	if ramDwellTarget(7, 1, 10*ms, ms) == ramDwellTarget(8, 1, 10*ms, ms) {
		t.Error("seed does not change the pause")
	}
	for p := uint64(1); p <= 200; p++ {
		got := ramDwellTarget(7, p, 10*ms, ms)
		if got < 10*ms || got >= 11*ms {
			t.Fatalf("pass %d pause %v outside [base, base+jitter)", p, got)
		}
	}
	// No jitter means exactly base (and negative jitter must not
	// subtract from the promised pause).
	if got := ramDwellTarget(7, 1, 10*ms, 0); got != 10*ms {
		t.Errorf("zero jitter: got %v, want exactly base", got)
	}
	if got := ramDwellTarget(7, 1, 10*ms, -5*ms); got != 10*ms {
		t.Errorf("negative jitter: got %v, want exactly base", got)
	}
}

// Timed run with a shortened pause: exact accounting, zero false
// mismatches, and the two-state contents layout the family pins
// (completed pass everywhere, interrupted next pass as a
// chunk-aligned fill prefix).
func TestRamDwellPassesAndCoverage(t *testing.T) {
	rep := dwellTestReporter(t, 2*time.Millisecond, 2*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	it, err := runRamDwell(ctx, rep)
	if err != nil {
		t.Fatalf("runRamDwell: %v", err)
	}
	if it == 0 {
		t.Fatal("no dwell passes completed in 300ms")
	}
	const all = uint64(192 << 10)
	if rep.ramPasses != it || rep.adds != it || rep.ramFound != 0 {
		t.Errorf("accounting drift: iters=%d passes=%d adds=%d found=%d", it, rep.ramPasses, rep.adds, rep.ramFound)
	}
	if rep.ramUnique != all || rep.ramTotal != it*all || rep.verified != all {
		t.Errorf("coverage drift: unique=%d total=%d verified=%d", rep.ramUnique, rep.ramTotal, rep.verified)
	}
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		state := 0
		for i := 0; i+4 <= len(v); i += 4 {
			cur := ramAddressWord(7, it+1, base+uint64(i)/4)
			prev := ramAddressWord(7, it, base+uint64(i)/4)
			got := binary.LittleEndian.Uint32(v[i:])
			switch {
			case state == 0 && got == cur:
			case got == prev:
				state = 1
			default:
				t.Fatalf("region %d word %d = %08x: not a prefix of image(%d) over image(%d)",
					r.Grant.Region, i/4, got, it+1, it)
			}
		}
	}
}

// End-to-end corruption. With a 50ms base the dwell is exactly two
// sleep slices (25ms + remainder), so the beat map is stable:
// pass 1: fill 1,2,3 | dwell 4,5 | verify 6,7,8;
// pass 2: fill 9,10,11 | dwell 12,13 | verify 14,15,16.
// The flip lands on pass 2's LAST fill beat - after every refill -
// because a flip during fill would honestly be overwritten by the
// next chunk and go unseen (the data ends correct). The post-dwell
// verify must fault with the ram-dwell identity.
func TestRamDwellEndToEndFlip(t *testing.T) {
	rep := dwellTestReporter(t, 50*time.Millisecond, 0)
	v := rep.regions[0].Block.Bytes()
	rep.flipAt = 11 // pass 2's last fill beat: r0c1 (beat 9) now holds pass 2's image
	rep.flip = func() { v[4] ^= 0x01 }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	it, err := runRamDwell(ctx, rep)
	if err == nil {
		t.Fatal("flipped word survived the dwell unseen")
	}
	if !strings.Contains(err.Error(), "ram-dwell") {
		t.Errorf("fault message does not name the kernel: %v", err)
	}
	if it != 1 {
		t.Errorf("completed passes = %d, want 1", it)
	}
	if rep.ramFound == 0 {
		t.Fatal("no structured finding reported")
	}
	g := rep.ramDetails[0]
	if g.Algorithm != "ram-dwell-v1" || g.Seed != 7 || g.Pass != 2 || g.Phase != "verify" {
		t.Errorf("finding identity wrong: %+v", g)
	}
	if g.Offset != 4 {
		t.Errorf("finding offset = %d, want 4", g.Offset)
	}
	want := ramAddressWord(7, 2, 1)
	if g.Expected != uint64(want) || g.Actual != uint64(want)^0x01 {
		t.Errorf("finding values = %08x/%08x, want %08x/%08x", g.Expected, g.Actual, want, want^0x01)
	}
}

// Cancel during the DWELL: the pause is cut short, but the verify
// still reads back everything that was written (no written byte
// goes unchecked) - and the pass is NOT counted, because the
// promised pause was not kept. Clean data + broken pause contract
// = honest partial, never a credited pass.
func TestRamDwellCancelDuringDwell(t *testing.T) {
	rep := dwellTestReporter(t, 50*time.Millisecond, 0) // 2 sleep slices per dwell
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 4 // first dwell slice beat (fill beats 1,2,3 precede it)

	it, err := runRamDwell(ctx, rep)
	if err != nil {
		t.Fatalf("a shortened dwell is not a fault: %v", err)
	}
	if it != 0 || rep.ramPasses != 0 || rep.verified != 0 || rep.adds != 0 {
		t.Errorf("interrupted-dwell pass was credited: it=%d passes=%d verified=%d adds=%d",
			it, rep.ramPasses, rep.verified, rep.adds)
	}
	if rep.ramFound != 0 {
		t.Errorf("the compensating verify saw %d mismatches on clean memory", rep.ramFound)
	}
	// Everything written WAS verified clean: the regions hold pass
	// 1's image everywhere (fill completed before the dwell began).
	for _, r := range rep.regions {
		v := r.Block.Bytes()
		base := r.Grant.Offset / 4
		for i := 0; i+4 <= len(v); i += 4 {
			want := ramAddressWord(7, 1, base+uint64(i)/4)
			if got := binary.LittleEndian.Uint32(v[i:]); got != want {
				t.Fatalf("region %d word %d = %08x, want pass-1 image %08x", r.Grant.Region, i/4, got, want)
			}
		}
	}
}

// The dwell is a REAL pause: with an 80 ms base and no jitter, each
// pass costs at least that much, so a 250 ms window fits at most
// three completed passes. This is a throttle check, not a timer
// precision claim - Windows timer resolution (~15.6 ms) inflates
// short sleeps, the Go monotonic clock advances in ~0.5 ms steps,
// and the OS scheduler owns the wake-up; the base is kept well
// above the timer quantum so the floor is unambiguous.
func TestRamDwellPauseThrottlesPasses(t *testing.T) {
	rep := dwellTestReporter(t, 80*time.Millisecond, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	it, err := runRamDwell(ctx, rep)
	if err != nil {
		t.Fatalf("runRamDwell: %v", err)
	}
	if it == 0 {
		t.Fatal("no passes completed even with 80ms dwells in a 250ms window")
	}
	if it > 3 {
		t.Errorf("completed passes = %d in 250ms with an 80ms pause: the dwell did not actually pause", it)
	}
}

// Cancel mid-fill: the filled prefix is verified (no dwell ran,
// but written bytes still must not go unchecked), and nothing is
// credited.
func TestRamDwellGracefulMidFill(t *testing.T) {
	rep := dwellTestReporter(t, 5*time.Millisecond, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rep.cancel = cancel
	rep.cancelAt = 2 // pass 1 fill: r0c1, r0c2 done; r1 never written

	it, err := runRamDwell(ctx, rep)
	if err != nil {
		t.Fatalf("a graceful mid-fill stop is not a fault: %v", err)
	}
	if it != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("mid-fill stop credited: it=%d passes=%d verified=%d", it, rep.ramPasses, rep.verified)
	}
	if rep.ramFound != 0 {
		t.Errorf("compensating verify saw %d mismatches on clean memory", rep.ramFound)
	}
	v0 := rep.regions[0].Block.Bytes()
	for i := 0; i+4 <= len(v0); i += 4 {
		want := ramAddressWord(7, 1, uint64(i)/4)
		if got := binary.LittleEndian.Uint32(v0[i:]); got != want {
			t.Fatalf("region 0 word %d = %08x, want pass-1 image %08x", i/4, got, want)
		}
	}
}

func TestRamDwellUpfrontCancel(t *testing.T) {
	rep := dwellTestReporter(t, 1*time.Millisecond, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it, err := runRamDwell(ctx, rep)
	if err != nil {
		t.Fatalf("upfront cancel: %v", err)
	}
	if it != 0 || rep.adds != 0 || rep.ramPasses != 0 || rep.verified != 0 {
		t.Errorf("cancelled run counted work: iters=%d adds=%d passes=%d verified=%d",
			it, rep.adds, rep.ramPasses, rep.verified)
	}
}

func TestRamDwellRefusesWithoutEngine(t *testing.T) {
	rep := &regionReporter{}
	_, err := runRamDwell(context.Background(), rep)
	if err == nil || !strings.Contains(err.Error(), "never runs on the Go heap") {
		t.Errorf("err = %v, want the no-engine refusal", err)
	}
}

func TestRamDwellRegistryEntry(t *testing.T) {
	w, ok := workloads["ram-dwell"]
	if !ok {
		t.Fatal("ram-dwell missing from the workload registry")
	}
	if w.KernelVersion != "ram-dwell-v1" || !w.RequiresMemory || w.FaultClass != FaultMemory {
		t.Errorf("registry entry wrong: %+v", w)
	}
	if got := KernelVersionOf("ram-dwell"); got != "ram-dwell-v1" {
		t.Errorf("KernelVersionOf = %q", got)
	}
	if strings.TrimSpace(w.Description) == "" {
		t.Error("registry description empty")
	}
}
