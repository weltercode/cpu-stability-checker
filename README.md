# CPU Stability Checker

CPU Stability Checker is an experimental Windows-first diagnostic tool for
repeatable CPU and RAM stress tests. Its main target is AMD Ryzen on Windows 11
and newer. Linux support is best effort and may expose fewer topology,
telemetry, and control capabilities.

The project has two equal goals:

1. build a trustworthy stability checker for per-core, memory, transition, and
   pair-interaction testing;
2. teach its owner Go through small, reviewable lessons instead of hiding the
   design inside large generated changes.

The program can find errors. A clean run is evidence only for the exact
workload, duration, cores, memory range, and monitors recorded for that run. It
is never a universal guarantee that an overclock or undervolt is stable.

## Current state

The repository already contains:

- verified built-in CPU workloads and a memory-test engine;
- logical-CPU topology discovery and per-worker OS-thread pinning;
- one-process-per-core execution and Windows Job Object cleanup;
- multi-phase scenarios, repeatable requests, JSONL events, crash journals,
  history, and reports;
- `burst`, `pairs`, `curve`, and physical-core `cycle` generators;
- prepared idle/load sessions and shared-memory ownership handoff;
- WHEA and temperature monitoring with explicit degraded/unavailable states;
- adapters for external programs such as Prime95 and y-cruncher;
- a recoverable Curve Optimizer transaction layer and fake-only automatic
  tuning.

The application is not release-ready. The active blockers are:

- the park/wake hypothesis: prepared transitions can now OBSERVE and journal the
  OS parking view inside the measured idle windows when asked to
  (`parking_policy` in `config.json`, see “What park/wake means here”), but the
  first real-machine observation honestly came back `unconfirmed` — a confirmed
  parking-related failure scenario is still unproven on hardware, not refuted;
- external adapters point at tools you install yourself: a fresh checkout has no
  `config.json` at all, so every external workload starts out unavailable until
  you register its tool (`tools status`, `tools detect`, or `config init` plus
  a manual edit). This project never downloads anything for you.

The development journal — roadmap, dated audits, validation protocols, the
hardware-acceptance records behind the statements below — stays in the author's
private development archive. What lives here is the code, its tests and this
document; every verification claim is stated per run, in the journal that run
writes.

## Safety

Stress testing can expose unstable settings, overheat hardware, restart the
machine, or corrupt unrelated work. Before a real run:

- save open files;
- start with short runs and conservative memory budgets;
- verify temperature and WHEA monitoring with `caps`;
- preview the resolved plan;
- do not run real Curve Optimizer mutation as an incidental test action.

Automatic CO tuning remains fake-only until the diagnostic-sensitivity gates in
the roadmap are complete. Ordinary CPU/RAM tests must not change voltage,
frequency, BIOS, or CO settings.

## Build and start

Requirements: Go 1.22 or newer. On Windows PowerShell:

```powershell
go build -o cpu-stability.exe .
.\cpu-stability.exe
```

Useful non-interactive commands:

```powershell
.\cpu-stability.exe help
.\cpu-stability.exe list
.\cpu-stability.exe topology
.\cpu-stability.exe caps
.\cpu-stability.exe plan
.\cpu-stability.exe scenario -dry-run
```

The executable reads `config.json` from the working directory unless a command
selects another input. Use `config init` to generate an example rather than
copying machine-specific external-tool paths from another computer.

## Core selection

Core selectors accept comma-separated numbers and inclusive ranges:

```text
1, 2, 5-8, 12
```

The task-oriented menu interprets this selection as **physical core numbers**
and then asks whether to use one thread or all SMT siblings. The low-level
`run -cores` option and explicit scenario phases use **logical CPU numbers**.
Always check `topology` before translating between them: two logical CPUs can be
SMT siblings on one physical core.

Examples:

```powershell
.\cpu-stability.exe run -test general -a float -cores 0,1 -duration 15s
.\cpu-stability.exe run -test cyclic -cores 0,2,4,6 -duration 10s
.\cpu-stability.exe run -test abab -a int -b mem -cores 0,1 -per-process
```

Use `list` for the workloads and layouts available in the current build. Do not
infer data verification from the amount of load: for example, a bandwidth-only
workload can create heavy traffic without proving memory correctness.

Every finished scenario ends in one of four words - `PASS` (all required load
completed and verified), `FAIL` (a confirmed fault: workload mismatch or WHEA
hardware record), `INCOMPLETE` (stopped by run control: monitor, tool, setup or
the operator) and `UNVERIFIED` (the load finished but its correctness contract
was not confirmed, including zero verified checks) - each carrying its
structured reason. `UNKNOWN` belongs only to a missing, corrupt or foreign
result record. Monitoring states stay honest: WHEA claims coverage only after
completed query cycles ("`WHEA ?`" until then), and a missing temperature
provider is named as monitoring state, not as a fault.

When every logical CPU is a worker, the helper threads (watchdog, journal
writer, live panel) have no CPU of their own and compete with workers pinned
at 100%. `config.json` can opt in to headroom: `"reserve_control": "logical"`
takes the highest-numbered logical CPU out of the worker set for the helpers
whenever the automatic control core would otherwise find no free CPU. The
default (`""`/`"none"`) keeps every historical behavior byte-identical, and a
saved recipe replays exactly as recorded; pairing the policy with an explicit
`control_core` is a named refusal, not a guess. The coverage loss is stated
out loud in the plan, before the load starts, and in the journal - a 31-of-32
run is never mistaken for a full-coverage one.

## Layouts and scenario patterns

Simple layouts assign workloads to the selected CPU slots:

| Layout | Meaning |
|---|---|
| `general` | the same workload on every selected slot |
| `cyclic` | workloads rotate across slots |
| `abab` | A, B, A, B, ... |
| `aabb` | A, A, B, B, ... |
| `half` | A on the first half, B on the second half |
| `isolate` | one loaded slot while the other selected slots stay idle |

Scenario generators compile into ordinary phases before execution:

| Generator | Current behavior |
|---|---|
| `burst` | one selected CPU settles without project load, then receives a short load |
| `pairs` | ordered aggressor/observer pairs, optionally constrained to CCX or SMT siblings |
| `curve` | increases the number of simultaneously loaded CPUs step by step |
| `cycle` | walks physical-core groups in a reproducible order |
| `handoff` | transfers ownership of shared memory blocks between prepared participants |
| `switch_*` phase fields | prepared aggressor/observer groups alternate the load in one session, with no process restarts; groups are named by hand or generated from the detected topology |

`pairs` builds the pair matrix: every pair is its own phase with freshly
spawned processes. When you need a low-latency switch between prepared
participants, use a pair-switch phase instead: either name the groups by
hand (`switch_pairs`) or let the scope fields generate them from the
detected topology (`switch_scope`: `ccx`/`smt`/`all`, `switch_cores` pool),
plus `switch_rounds`, `switch_gap` and `switch_run`. Every participant is
spawned and PREPAREd before the first
switch, so the measured transition contains no spawn and no allocation; the
turn order is fixed or seeded, and the journal keeps the commanded turn
timeline, the parent's per-turn latencies (labeled upper bounds, IPC round
trip included) and each member's own view as separate records. The turn gaps
are measured idle windows, so the `parking_policy` described in “What
park/wake means here” applies to them too.

## What “park/wake” means here

The project must distinguish three facts:

1. **idle requested** — our worker stopped issuing workload instructions;
2. **parking observed** — the kernel's CPU Set enumeration reported this logical
   CPU's `Parked` flag inside a measured idle window; that flag is the parking
   engine's own report, and no single low-frequency sample proves a hardware
   C-state;
3. **wake/load acknowledged** — the prepared worker received the start command
   and began measured work.

Facts 1 and 3 are under direct application control and are journaled as
commands and acknowledgements. Fact 2 is observation, and it is integrated
into prepared transition runs by policy, not by default: `parking_policy` in
`config.json` (or per transition phase) is `off`/absent (no observation at
all — the historical behavior, byte-identical), `try` (one `parking_obs`
record per core: source, sampling cadence, sample counts inside the measured
idle windows, coverage holes stated out loud — without touching the verdict),
or `required` (a run whose source cannot answer is refused before it starts,
and without observed evidence the full verification claim is withheld — the
run still ends honestly, claiming only `partial`). `observed`, `unconfirmed`
and `unavailable` are three distinct states and never collapse into each
other. Where the policy is off, documentation and UI still say `idle` or
`settle`, never “parked”: the commands prove the schedule, only the
observation can speak about parking.

## Results and exit codes

The program keeps execution, validation, findings, coverage, and observation
quality separate. In particular, unavailable WHEA or temperature data must stay
`unknown` or `degraded`; it must never silently become “zero errors.”

Process exit codes:

| Code | Meaning |
|---:|---|
| `0` | the declared contract completed |
| `1` | fault, abort, interruption, or incomplete required validation |
| `2` | invalid configuration or setup/tool failure before a valid run |
| `130` | forced signal termination path |

Important output artifacts include the text log, append-only JSONL journal,
saved request/session manifest, and final result. Reports must preserve partial
evidence after cancellation or a worker crash.

## External workloads

External tools are optional modules, not trusted merely because an executable
exists. An adapter needs an explicit command, arguments, working directory,
accepted exit codes, result/fault evidence, and a version or checksum policy.

Check the local machine before a run:

```powershell
.\cpu-stability.exe tools status
.\cpu-stability.exe tools detect C:\Path\To\StressTools
```

No `config.json` is checked in: a fresh checkout has no external tools
registered, and `tools status` says so, entry by entry. A stale or wrong path is
visible, not silent: `tools status` marks the entry `MISSING` and names it, and
every entry point (the menu, repeats, scripted runs) refuses a missing
registered tool before any load starts, naming the path and exiting 2.
`config init` writes a portable sample that bakes in no machine-specific paths
at all; `tools detect` fills in what it can find on your machine. Repointing or
removing entries is the operator's call, not the tool's.

## Development checks

Start with deterministic, bounded checks:

```powershell
go test -short -p 1 -parallel 1 ./... -count=1 -timeout=120s
go vet -p 1 ./...
go build -p 1 ./...
```

The suite is levelled, and every level is selectable from the
command line. Skip messages carry the literal `test level N` plus the
missing capability, so a restricted machine reports honestly instead of
failing, while a machine that HAS the capability but broken behavior still
fails as it must.

| Level | What it may touch | How to select |
|---|---|---|
| L1 hermetic unit | process memory, test temp dirs, self-re-exec helper children, fakes | the floor of `go test -short -p 1 ./...` |
| L2 Windows integration | real console tools (`cmd`, `ping`), job objects, real process trees, the WMI service | full run (no `-short`), or by documented name: `-run 'TestExternalTree'`, `-run 'TestWMI'` |
| L3 hardware acceptance | live sensor providers (LHM/OHM), real topology, full-duration acceptance runs | explicit user runs on the target machine |
| L4 destructive / long | GiB memory commits, real stress tools, CO changes | explicit opt-in only, e.g. `CPU_TEST_SMOKE_GB=4 go test -run TestMultiGBSmokeOptIn ./internal/memtest/` |

External-adapter bookkeeping in `internal/runner` and `internal/subprocess`
uses hermetic self-re-exec helper children, not real system tools, so the
journal honesty contracts are covered even in the `-short` gate; real
`cmd`/`ping` process-tree behavior lives in `internal/workload` and
`internal/procjob` as L2. The marking is enforced mechanically: meta-tests
in those packages re-exec the test binary and assert that `-short` and
`-run 'TestWMI'` select exactly the documented level groups.

Do not enable `CPU_TEST_SMOKE_GB` during ordinary development. Hardware stress,
large allocations, SMU writes, and real CO changes require an explicit user
action.

## Project journal

The development journal — roadmap, dated audits, validation protocols, the
parking-observation experiment, the UI and external-adapter design notes — is
kept in the author's private development archive and is not part of this
release. Journal entries are never rewritten after the fact: dated records are
evidence, and an older document saying "done" does not override newer
measurements. Everything this README claims about verification describes what
the code's own tests and per-run journals prove, not what a plan promised.

## Contribution style

Changes are intentionally small. Each lesson or increment should state:

- the user-visible problem;
- the Go concept being exercised;
- the files and ownership boundaries involved;
- the smallest implementation that closes the problem;
- the regression test and commands used to verify it;
- remaining limitations and the next lesson.

Prefer one completed vertical change over several partially implemented
features. Do not rewrite working subsystems just to make the plan look cleaner.

## License

This is free and unencumbered software released into the public domain — see
the [UNLICENSE](UNLICENSE) file. There are no conditions on use, reuse or
redistribution; there is also no warranty of any kind, which this project
takes seriously: every verdict a run produces is scoped to the exact
workload, duration, cores and monitors recorded in that run's journal.

## Support

Support is entirely optional and never changes the software's rights or
behavior: the tool is public domain either way. If this project helped you,
you can sponsor the author on GitHub: <https://github.com/sponsors/weltercode>
(see [.github/FUNDING.yml](.github/FUNDING.yml)).
