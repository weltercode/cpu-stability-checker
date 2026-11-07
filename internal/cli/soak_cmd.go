package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// soak runs the readiness-criteria stress: many short cycles over every
// execution mode (in-process, per-process, rotation) plus a deliberate
// fault phase, measuring the process's own lifecycle after each cycle.
// A leak is sustained growth — goroutines that never exit, OS handles
// left open by children we forgot to release — so the verdict compares
// endpoints with a slack instead of chasing single-sample noise.

// soakSample is one lifecycle measurement taken after a full cycle.
type soakSample struct {
	goroutines int
	handles    int
	haveHandle bool // false where the platform cannot count handles
}

// soakVerdict compares the last sample against the post-warmup baseline.
// Warmup is excluded because the first cycles pay one-time costs (caches,
// lazy OS state) that must not be mistaken for a leak.
func soakVerdict(samples []soakSample, warmup, goroutineSlack, handleSlack int) (string, bool) {
	if warmup < 1 || len(samples) <= warmup+1 {
		return fmt.Sprintf("soak: need more than %d cycles after a warmup of %d for a verdict", warmup+1, warmup), false
	}
	base, last := samples[warmup], samples[len(samples)-1]
	msg := fmt.Sprintf("goroutines %d -> %d (slack %d)", base.goroutines, last.goroutines, goroutineSlack)
	ok := last.goroutines <= base.goroutines+goroutineSlack
	if base.haveHandle && last.haveHandle {
		msg += fmt.Sprintf(", handles %d -> %d (slack %d)", base.handles, last.handles, handleSlack)
		ok = ok && last.handles <= base.handles+handleSlack
	} else {
		msg += ", handle counting unavailable here"
	}
	gmax, hmax := 0, 0
	for _, s := range samples {
		if s.goroutines > gmax {
			gmax = s.goroutines
		}
		if s.handles > hmax {
			hmax = s.handles
		}
	}
	msg += fmt.Sprintf(", peaks goroutines %d", gmax)
	if last.haveHandle {
		msg += fmt.Sprintf(", handles %d", hmax)
	}
	if ok {
		return "soak OK: " + msg, true
	}
	return "soak LEAK: " + msg, false
}

// soakRun drives the cycle loop. execute and sample are injected so the
// orchestration is testable without loading the machine; the fault phase
// (workload "fault") is expected to fault — a cycle where it does not
// means the fault pipeline itself broke, which is a soak failure too.
func soakRun(cycles, warmup int, seq []plan.Options, execute func(plan.Options) plan.Result, sample func() soakSample, out io.Writer) int {
	samples := make([]soakSample, 0, cycles)
	for c := 1; c <= cycles; c++ {
		for _, opt := range seq {
			res := execute(opt)
			wantFault := opt.A == "fault"
			faulted := res.Exit != 0 || len(res.Faults) > 0
			if wantFault != faulted {
				fmt.Fprintf(out, "soak: cycle %d phase %s: expected fault=%v, got fault=%v (exit %d)\n",
					c, opt.Phase, wantFault, faulted, res.Exit)
				return 1
			}
		}
		s := sample()
		samples = append(samples, s)
		if c%10 == 0 || c == cycles {
			fmt.Fprintf(out, "soak cycle %d/%d: goroutines %d", c, cycles, s.goroutines)
			if s.haveHandle {
				fmt.Fprintf(out, ", handles %d", s.handles)
			}
			fmt.Fprintln(out)
		}
	}
	msg, ok := soakVerdict(samples, warmup, 16, 64)
	fmt.Fprintln(out, msg)
	if !ok {
		return 1
	}
	return 0
}

// soakCmd is the user-facing entry: 'cpu-stability soak' (menu item 14).
func soakCmd(args []string) int {
	return soakCmdWithUI(args, nil)
}

func soakCmdWithUI(args []string, ui *screenUI) int {
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	cycles := fs.Int("cycles", 100, "lifecycle cycles to run")
	dur := fs.String("duration", "50ms", "load duration per phase")
	coresStr := fs.String("cores", "0,1", "logical CPUs to exercise")
	warmup := fs.Int("warmup", 3, "cycles excluded from the leak baseline")
	logFile := fs.String("log", "soak.log", "log/journal base file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cycles < 5 {
		fmt.Fprintln(os.Stderr, "soak: -cycles must be at least 5")
		return 2
	}
	if *warmup < 1 || *warmup+2 > *cycles {
		fmt.Fprintln(os.Stderr, "soak: -warmup must leave at least two measured cycles")
		return 2
	}
	d, err := time.ParseDuration(*dur)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "soak: -duration must be a positive duration: %v\n", err)
		return 2
	}
	cores, err := parseNumberSet(*coresStr, runtime.NumCPU())
	if err != nil {
		fmt.Fprintf(os.Stderr, "soak: -cores: %v\n", err)
		return 2
	}
	if len(cores) < 2 {
		fmt.Fprintln(os.Stderr, "soak: needs at least two CPUs (the rotation phase requires them)")
		return 2
	}

	seq := []plan.Options{
		{TestName: "general", Phase: "inproc", Cores: cores, Workers: 1, Duration: d, A: "int", B: "idle", ControlCore: -1, LogFile: *logFile},
		{TestName: "general", Phase: "perproc", Cores: cores, Workers: 1, Duration: d, A: "int", B: "idle", ControlCore: -1, LogFile: *logFile, PerProcess: true},
		{TestName: "general", Phase: "rotate", Cores: cores, Workers: 1, Duration: d, A: "int", B: "idle", ControlCore: -1, LogFile: *logFile, PerProcess: true, Rotate: true},
		{TestName: "general", Phase: "fault", Cores: cores, Workers: 1, Duration: d, A: "fault", B: "idle", ControlCore: -1, LogFile: *logFile, PerProcess: true},
	}

	execute, out := menuExecution(ui)
	sample := func() soakSample {
		runtime.GC() // measure the settled state, not the transient one
		s := soakSample{goroutines: runtime.NumGoroutine()}
		if h, ok := sysinfo.HandleCount(); ok {
			s.handles, s.haveHandle = h, true
		}
		return s
	}

	fmt.Fprintf(out, "soak: %d cycles x %d phases (inproc, perproc, rotate, fault) on CPUs %v, %s each\n",
		*cycles, len(seq), cores, d)
	return soakRun(*cycles, *warmup, seq, execute, sample, out)
}
