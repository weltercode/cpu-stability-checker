// Package cli is the command-line front end: command dispatch, usage text,
// the interactive menu and the session orchestration on top of the runner.
package cli

import (
	"fmt"
	"os"

	"github.com/weltercode/cpu-stability-checker/internal/plan"
	"github.com/weltercode/cpu-stability-checker/internal/subprocess"
	"github.com/weltercode/cpu-stability-checker/internal/workload"
)

// Run dispatches one command line (without the program name) and returns the
// process exit code. An empty argument list opens the interactive menu.
func Run(args []string) int {
	if len(args) == 0 {
		return runMenu()
	}
	switch args[0] {
	case "plan":
		return runConfigured(false)
	case "list":
		list()
		return 0
	case "topology":
		return topologyCmd(args[1:])
	case "run":
		return runCmd(args[1:])
	case "scenario":
		return scenarioCmd(args[1:])
	case "quick":
		return quickCmd(args[1:])
	case "repeat":
		return repeatCmd()
	case "catalog":
		return catalogCmd(args[1:])
	case "history":
		return historyCmd(args[1:])
	case "menu":
		return runMenu()
	case "config":
		return configCmd(args[1:])
	case "journal":
		return journalCmd(args[1:])
	case "report":
		return reportCmd(args[1:])
	case "bundle":
		return bundleCmd(args[1:])
	case "validate":
		return validateCmd(args[1:])
	case "parkwatch":
		return parkwatchCmd(args[1:])
	case "handoff":
		return handoffCmd(args[1:])
	case "caps", "capabilities":
		return capsCmd(args[1:])
	case "version":
		return versionCmd(args[1:])
	case "soak":
		return soakCmd(args[1:])
	case "whea":
		return wheaCmd(args[1:])
	case "tools":
		return toolsCmd(args[1:])
	case "co":
		return coCmd(args[1:])
	case "tune":
		return tuneCmd(args[1:])
	case "worker":
		return subprocess.WorkerCmd(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", args[0])
		printUsage()
		return 2
	}
}

// list prints every workload and test the tool knows about.
func list() {
	fmt.Println("Workloads (methods):")
	for _, name := range workload.Names() {
		w, _ := workload.Get(name)
		fmt.Printf("  %-8s %s\n", name, w.Description)
	}

	fmt.Println("\nTests:")
	last := ""
	for _, t := range plan.AllTests() {
		if t.Category != last {
			fmt.Printf("\n%s:\n", t.Category)
			last = t.Category
		}
		fmt.Printf("  %-8s %s\n", t.Name, t.Description)
	}
}

func printUsage() {
	fmt.Print(`cpu-stability-checker — CPU/RAM stability test utility

Usage:
  cpu-stability                open the main menu
  cpu-stability plan           explain the configured run without starting it
  cpu-stability list           list tests and workloads
  cpu-stability topology       show physical cores, SMT siblings, and CPU Sets
  cpu-stability run [options]  scripted run
  cpu-stability scenario       run the "phases" from config.json as one session
  cpu-stability quick [-yes]   bounded CPU sanity window (all-core int 10s +
                               all-core float 10s + three wake bursts): one
                               verdict word, evidence paths, next step. The
                               clean word covers that window ONLY - it is not
                               a stability verdict and checks no RAM engine.
                               (-continue-on-fault: exclude faulted cores, keep going)
  cpu-stability menu           open the main menu
  cpu-stability config           show this machine's effective settings: real path,
                               file state, every value with its meaning (R18 view;
                               replaces the old raw JSON dump)
  cpu-stability config init    write a sample config.json
  cpu-stability journal [file] recover run outcomes from a crash journal
  cpu-stability report  [file] per-core verdict aggregated over the journal
  cpu-stability bundle  [file] sanitized bug-report bundle (identity removed)
                               (-json: machine-readable output)
                               (-diff other.jsonl: per-core comparison of two
                                journals; put flags before [file])
                               (-anomalies: diagnostic clock dips vs each
                                phase's own median; never changes exit code)
  cpu-stability validate -request <abs path> [-json]
                                 answer whether that headless request would run
                                 here, right now, without starting any load
  cpu-stability run -request <abs path>
                                 headless run of a saved-shape request file
                                 (-events jsonl [-output dir]: managed mode,
                                  JSONL events on stdout, cancel on stdin)
  cpu-stability repeat           re-run the saved request with one action
  cpu-stability catalog [-json]  the profile catalog: ids, versions,
                                 availability and the reason when not
  cpu-stability history [-json] [-limit N]
                                 recorded sessions, newest first
  cpu-stability topology [-json] physical cores, SMT siblings, CPU Sets
  cpu-stability caps [-json]     which telemetry sources are alive here
  cpu-stability version [-json]  exact build identity (module version, VCS
                                 revision, modified state, build time) plus
                                 this machine's cpu/os and the firmware's own
                                 memory records, as written. Read-only: it
                                 starts no load and diagnoses nothing.
  cpu-stability parkwatch [dur] watch OS core-parking transitions (-interval)
  cpu-stability handoff        P07e block-ownership handoff between cores/LLC
                               domains over one shared section. The public form
                               (since S6) IS a tracked run: durable manifest
                               saved before the first load, session manifest,
                               attempt receipt, lastresult binding and journal
                               run_meta. The menu repeat, 'handoff -repeat' and
                               a kind=handoff request file replay the recorded
                               geometry and seed as a NEW attempt; machine or
                               topology drift is refused by name, never
                               re-picked. -helper keeps the legacy internal
                               test-helper form (safety lifecycle active, its
                               journals byte-identical to the pre-S6 records).
  cpu-stability soak           lifecycle stress: many cycles over every mode,
                               verdict on goroutine/handle leaks (-cycles)
  cpu-stability whea [n]       show recent WHEA hardware error records
  cpu-stability tools <cmd>    status / install / detect the external stress
                               tools (Prime95, y-cruncher) with checksum checks
   cpu-stability co <sub>       P10 Curve Optimizer: probe | topology | read
                                (read-only; -tool PATH or config co_tool; -json)
   cpu-stability tune <sub>     P11 CO auto-tuning on the FAKE machine only
                                 (plan-create | plan-show | trial | sweep | reconcile;
                                 -fake required to mutate; the real CO backend is
                                 never wired into tuning until P12)

Run options:
  -test string      test name (see 'list')
  -a string         workload slot A
  -b string         workload slot B
  -cores string     comma list of cores, e.g. 0,1,2 (empty = config / all)
  -workers int      workers per core
  -duration string  how long to run, e.g. 10s
  -log string       log file
  -stop-on-fault    abort the run on the first fault
  -per-process      one pinned subprocess per CPU (experimental)
  -rotate           with -per-process: freeze all cores but one, rotating windows
  -control int      CPU for helper threads (-1 = off, default = config / auto)
  -beat string      core_beat journal cadence, e.g. 10s (default 1s; live UI unaffected)

Examples:
  cpu-stability
  cpu-stability plan
  cpu-stability topology
  cpu-stability run -test general -a float -duration 15s
  cpu-stability run -test cyclic -cores 0,1,2,3 -duration 10s
  cpu-stability run -test abab -a int -b mem -workers 2
  cpu-stability run -test isolate -a sha256

This software is public domain, and support is never a condition of
use. If it helped and you feel like it: https://github.com/sponsors/weltercode
`)
}
