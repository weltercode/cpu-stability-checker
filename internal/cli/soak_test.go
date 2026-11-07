package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
)

func TestSoakVerdict(t *testing.T) {
	s := func(g, h int) soakSample { return soakSample{goroutines: g, handles: h, haveHandle: true} }

	cases := []struct {
		name    string
		samples []soakSample
		warmup  int
		wantOK  bool
		wantMsg string
	}{
		{
			name:    "flat is fine",
			samples: []soakSample{s(5, 100), s(5, 100), s(6, 105), s(6, 110), s(6, 120)},
			warmup:  1,
			wantOK:  true,
			wantMsg: "soak OK",
		},
		{
			name:    "warmup spike is not a leak",
			samples: []soakSample{s(5, 100), s(50, 500), s(6, 105), s(6, 106)},
			warmup:  1,
			wantOK:  true,
			wantMsg: "soak OK",
		},
		{
			name:    "goroutine growth is a leak",
			samples: []soakSample{s(5, 100), s(5, 100), s(10, 100), s(30, 100), s(60, 100)},
			warmup:  1,
			wantOK:  false,
			wantMsg: "soak LEAK",
		},
		{
			name:    "handle growth is a leak",
			samples: []soakSample{s(5, 100), s(5, 100), s(5, 200), s(5, 300), s(5, 400)},
			warmup:  1,
			wantOK:  false,
			wantMsg: "soak LEAK",
		},
		{
			name: "without handle data the verdict still works",
			samples: []soakSample{
				{goroutines: 5}, {goroutines: 5}, {goroutines: 6}, {goroutines: 6},
			},
			warmup:  1,
			wantOK:  true,
			wantMsg: "handle counting unavailable",
		},
		{
			name:    "too few cycles cannot verdict",
			samples: []soakSample{s(5, 100), s(5, 100)},
			warmup:  3,
			wantOK:  false,
			wantMsg: "need more than",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			msg, ok := soakVerdict(tt.samples, tt.warmup, 16, 64)
			if ok != tt.wantOK {
				t.Errorf("soakVerdict ok = %v, want %v (msg %q)", ok, tt.wantOK, msg)
			}
			if !strings.Contains(msg, tt.wantMsg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tt.wantMsg)
			}
		})
	}
}

// The cycle loop must police both directions of the fault expectation:
// a clean phase that faults, and a fault phase that stops faulting (the
// fault pipeline itself broke), are both soak failures.
func TestSoakRun(t *testing.T) {
	seq := []plan.Options{
		{Phase: "inproc", A: "int"},
		{Phase: "fault", A: "fault"},
	}

	run := func(execute func(plan.Options) plan.Result, sample func() soakSample) (int, string) {
		var out bytes.Buffer
		code := soakRun(6, 2, seq, execute, sample, &out)
		return code, out.String()
	}

	flat := func() soakSample { return soakSample{goroutines: 5, handles: 50, haveHandle: true} }

	t.Run("expected faults and flat metrics pass", func(t *testing.T) {
		execute := func(opt plan.Options) plan.Result {
			if opt.A == "fault" {
				return plan.Result{Exit: 1, Faults: map[int]string{0: "injected"}}
			}
			return plan.Result{}
		}
		code, out := run(execute, flat)
		if code != 0 {
			t.Fatalf("soakRun exit = %d, want 0; out:\n%s", code, out)
		}
		if !strings.Contains(out, "soak OK") {
			t.Errorf("out = %q, want soak OK", out)
		}
	})

	t.Run("a fault phase that stops faulting fails", func(t *testing.T) {
		execute := func(plan.Options) plan.Result { return plan.Result{} }
		code, out := run(execute, flat)
		if code != 1 {
			t.Fatalf("soakRun exit = %d, want 1 when the fault phase does not fault", code)
		}
		if !strings.Contains(out, "expected fault=true") {
			t.Errorf("out = %q, want the mismatch to be reported", out)
		}
	})

	t.Run("an unexpected fault fails", func(t *testing.T) {
		execute := func(opt plan.Options) plan.Result {
			if opt.A == "int" {
				return plan.Result{Exit: 1, Faults: map[int]string{0: "boom"}}
			}
			return plan.Result{Exit: 1, Faults: map[int]string{0: "injected"}}
		}
		code, _ := run(execute, flat)
		if code != 1 {
			t.Fatalf("soakRun exit = %d, want 1 for an unexpected fault", code)
		}
	})

	t.Run("growing metrics fail the verdict", func(t *testing.T) {
		n := 0
		grow := func() soakSample {
			n += 20
			return soakSample{goroutines: 5 + n, handles: 50 + n, haveHandle: true}
		}
		execute := func(opt plan.Options) plan.Result {
			if opt.A == "fault" {
				return plan.Result{Exit: 1, Faults: map[int]string{0: "injected"}}
			}
			return plan.Result{}
		}
		code, out := run(execute, grow)
		if code != 1 {
			t.Fatalf("soakRun exit = %d, want 1 for growing metrics; out:\n%s", code, out)
		}
		if !strings.Contains(out, "soak LEAK") {
			t.Errorf("out = %q, want soak LEAK", out)
		}
	})
}

func TestSoakCmdGuards(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"too few cycles", []string{"-cycles", "2"}},
		{"warmup eats the run", []string{"-cycles", "5", "-warmup", "4"}},
		{"bad duration", []string{"-duration", "nope"}},
		{"bad cores", []string{"-cores", "x"}},
		{"one cpu cannot rotate", []string{"-cores", "0"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if code := soakCmd(tt.args); code != 2 {
				t.Errorf("soakCmd(%v) exit = %d, want 2", tt.args, code)
			}
		})
	}
}
