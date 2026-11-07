// cpu-stability-checker is a CPU and RAM stability diagnostic application.
//
// It orchestrates repeatable CPU/RAM workloads, core cycling and idle/load
// transitions, records verification evidence and monitors system telemetry.
// Workloads declare their verification method; load-only workloads such as
// mem-bw do not establish correctness. Curve Optimizer uses a separate backend
// and recoverable transactions; automatic tuning is currently fake-only.
//
// The implementation lives in internal packages: cli (commands, menu),
// runner (execution), plan (tests/layouts), workload (load generators and
// external adapters), subprocess (per-core process mode), journal (crash
// recovery), config, sysinfo (affinity/topology/metrics), logging, human.
//
// Usage:
//
//	cpu-stability                # open the main menu
//	cpu-stability plan           # explain config without running
//	cpu-stability list           # list tests and workloads
//	cpu-stability topology       # show physical cores and logical CPUs
//	cpu-stability run -test ...  # scripted run
//	cpu-stability config init    # write a sample config.json
//	cpu-stability journal        # recover run outcomes from a crash journal
package main

import (
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
