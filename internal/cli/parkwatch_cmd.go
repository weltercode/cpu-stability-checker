package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/weltercode/cpu-stability-checker/internal/sysinfo"
)

// parkwatchCmd is the park/wake telemetry spike: it samples the kernel's
// core-parking state (CPU Set "Parked" flag) and prints every transition,
// so we can see what the OS actually does under idle, load and our own
// rotation before designing park/wake phases on top of it.
func parkwatchCmd(args []string) int {
	fs := flag.NewFlagSet("parkwatch", flag.ExitOnError)
	interval := fs.String("interval", "200ms", "sampling interval")
	fs.Parse(args)

	dur := 10 * time.Second
	if fs.NArg() >= 1 {
		d, err := time.ParseDuration(fs.Arg(0))
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "parkwatch: bad duration %q\n", fs.Arg(0))
			return 2
		}
		dur = d
	}
	tickDur, err := time.ParseDuration(*interval)
	if err != nil || tickDur <= 0 {
		fmt.Fprintf(os.Stderr, "parkwatch: bad -interval %q\n", *interval)
		return 2
	}

	states, err := sysinfo.CpuSetStates()
	if err != nil {
		fmt.Fprintf(os.Stderr, "parkwatch: %v\n", err)
		fmt.Fprintln(os.Stderr, "parking state is unverified on this system")
		return 2
	}

	parked := map[int]bool{}
	since := map[int]time.Time{}
	parkedSec := map[int]float64{}
	start := time.Now()
	for _, s := range states {
		parked[s.LogicalCPU] = s.Parked
		since[s.LogicalCPU] = start
	}
	maxParked := sysinfo.ParkedCount(states)

	fmt.Printf("parkwatch: %d logical cpu(s), %d parked at start, sampling every %s for %s\n",
		len(states), maxParked, tickDur, dur)

	ticker := time.NewTicker(tickDur)
	defer ticker.Stop()
	deadline := time.NewTimer(dur)
	defer deadline.Stop()

	for {
		select {
		case <-deadline.C:
			end := time.Now()
			for cpu, p := range parked {
				if p {
					parkedSec[cpu] += end.Sub(since[cpu]).Seconds()
				}
			}

			fmt.Println("summary:")
			cpus := make([]int, 0, len(parkedSec))
			for cpu := range parked {
				cpus = append(cpus, cpu)
			}
			sort.Ints(cpus)
			for _, cpu := range cpus {
				if parkedSec[cpu] > 0 {
					fmt.Printf("  core %d: parked %.1fs\n", cpu, parkedSec[cpu])
				}
			}
			if len(cpus) == len(parkedSec) {
				fmt.Println("  (no core was parked during the window)")
			}
			fmt.Printf("max parked at any sample: %d of %d\n", maxParked, len(states))
			return 0

		case <-ticker.C:
			states, err := sysinfo.CpuSetStates()
			if err != nil {
				fmt.Fprintf(os.Stderr, "parkwatch: sample failed: %v\n", err)
				continue
			}
			now := time.Now()
			for _, s := range states {
				if parked[s.LogicalCPU] == s.Parked {
					continue
				}
				from, to := "active", "parked"
				if parked[s.LogicalCPU] {
					from, to = "parked", "active"
					parkedSec[s.LogicalCPU] += now.Sub(since[s.LogicalCPU]).Seconds()
				}
				fmt.Printf("  +%6.2fs core %2d: %s -> %s\n", now.Sub(start).Seconds(), s.LogicalCPU, from, to)
				parked[s.LogicalCPU] = s.Parked
				since[s.LogicalCPU] = now
			}
			if n := sysinfo.ParkedCount(states); n > maxParked {
				maxParked = n
			}
		}
	}
}
