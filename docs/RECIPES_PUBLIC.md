# Recipes — practical runs for CPU/RAM stability checking

These are worked examples, not requirements. Every core selection shown
here is a placeholder: always derive cores/CCX/SMT groups from what
`topology` reports on YOUR machine, never copy numbers from someone
else's machine.

Binary naming: the released Windows zip runs as `cpu-stability-checker`;
the examples below use `cpu-stability`, the name `go build -o` produces.
Same program either way.

## 1. Read the machine before loading it

```powershell
.\cpu-stability.exe caps        # which telemetry sources are alive here
.\cpu-stability.exe topology    # physical cores, SMT siblings, CPU Sets
.\cpu-stability.exe config      # the settings this machine will actually use, and from where
```

`config` names the real path of the config file, its state, and every
value's meaning: defaults are said out loud when there is no file, an
unreadable file is refused with the reason (it holds a deliberate plan,
answering from defaults would describe a run that never was), and a
broken value is shown as written with what the runner will do about it -
never silently repaired.

What to look at in `caps`: each telemetry source is named or explicitly
absent. `WHEA ?` means the monitor has not completed its first full
query cycle yet — it is NOT "zero errors", and it never silently
becomes zero. A missing temperature provider is named as monitoring
state, not reported as a fault. Record what `caps` said before a run:
a verdict computed with degraded telemetry is worth differently than
one computed with full telemetry.

`topology` prints physical cores, which logical CPUs are SMT siblings
of one core, and CPU Sets (the parking engine's own view). Later
recipes refer to these numbers.

## 2. The smallest honest CPU test

```powershell
.\cpu-stability.exe run -test general -a int -cores 0-3 -duration 15s
```

Or take the built-in sanity window in one command:

```powershell
.\cpu-stability.exe quick       # all-core int 10s + all-core float 10s + three idle->load bursts
```

`quick` is CPU-only by design: no RAM verification, no per-core walk, no
soak - a clean word there is a sanity signal for that exact window, not
a stability verdict, and the command says so in its own closing scope
line.

The run writes a text log, an append-only JSONL journal, and a final
result file. The terminal verdict is one of exactly four words:

- `PASS` — all required load completed and verified;
- `FAIL` — a confirmed fault (workload mismatch or a WHEA hardware record);
- `INCOMPLETE` — stopped by run control (monitor, tool, setup, or you);
- `UNVERIFIED` — the load finished but its correctness contract was not
  confirmed (this includes zero verified checks).

Exit codes answer a different question — did the program run to its
declared end: `0` clean end, `1` fault/abort/interruption, `2` bad
setup/configuration, `130` forced signal end. A clean exit `0` can and
often does carry `UNVERIFIED`: scripts deciding pass/fail must read the
result file's verdict and validation fields, not the exit code alone.

To aggregate a journal after the fact:

```powershell
.\cpu-stability.exe report <journal-file>
.\cpu-stability.exe journal <journal-file>   # recover outcomes after a crash
```

## 3. Memory tests, with the machine's own limits respected

Memory runs are configured through `config.json`, key `"memory"`:
`"auto"`, an exact size such as `"256MiB"`, or empty (engine off).
Start small and grow deliberately: `"256MiB"` → `"1GiB"` → `"2GiB"`.

The program refuses to ask for more than the machine can honestly take.
If it refuses, you get exit `2` and the exact arithmetic — byte counts
as they were measured, deliberately not rounded into friendly GiB, for
example:

```text
memory engine: memtest: fixed request 214748364800 B refused:
available physical 10982555648 B cannot cover it plus the OS reserve 10051786343 B
```

Read that as: at this moment, with that much free RAM, a run of that
size cannot start without endangering the OS itself. That is the
engine protecting your machine, not a bug — close memory users and
rerun, or lower the budget. `"auto"` derives a budget from what is
free right now and states which constraint bound it — honest, but on a
busy desktop it will be small; an exact size is what a repeatable
recipe should name.

## 4. Core parking: what you may claim

Idle, wake and parking are three different facts. This tool proves the
schedule it commanded (idle requested, work acknowledged); it can
OBSERVE the OS's parking view when asked:

```powershell
.\cpu-stability.exe parkwatch 10s
```

and, in prepared transition runs, via `"parking_policy"` in
`config.json`: `off` (no observation — the historical behavior), `try`
(one `parking_obs` record per core with sampling counts and coverage
holes stated out loud), or `required` (a run whose parking source
cannot answer is refused before starting, and without observed
evidence the full verification claim is withheld — the run still ends
honestly as `partial`). `observed`, `unconfirmed` and `unavailable` are
three different states. Where observation is off, the tool still says
`idle`/`settle`, never "parked": only observation may speak about
parking.

## 5. Pair and switch interactions

Scenario generators build phases from what `topology` reported:

```powershell
# fill config.json phases (config init starts a sample; it will not
# overwrite an existing config without -force), then preview:
.\cpu-stability.exe scenario -dry-run
```

- `pairs` — every aggressor/observer pair gets its own phase with
  fresh processes (slower, cleanly isolated).
- `switch_*` phase fields — prepared groups alternate the load inside
  one session with no process restarts; name the groups by hand
  (`switch_pairs`) or generate them from detected topology
  (`switch_scope`: `ccx`/`smt`/`all` + `switch_cores` pool). Never
  paste another machine's pair list: on AMD numbering, the same-CCX
  shape is a property of the detected topology, and the generator
  derives it.

## 6. External tools (Prime95, y-cruncher, ryzen-smu-cli)

A fresh checkout has no external tools registered, and every entry
point says so:

```powershell
.\cpu-stability.exe tools status
.\cpu-stability.exe tools detect C:\Tools\StressTools
.\cpu-stability.exe tools install prime95   # explicit download of the pinned build
```

Nothing is downloaded during a test or a run; `tools install` is a
separate deliberate act that fetches the pinned archive from the
tool's own page and verifies its SHA-256. Missing registered tools are
refused by name before any load starts (exit 2) — never silently
skipped. Each external tool keeps its own license; this project's
public-domain dedication covers only this program and its sources.

## 7. When something goes wrong

- `FAIL` with a WHEA record names the record type and time; treat the
  settings, not the verdict, as the suspect.
- `UNVERIFIED` after completed load usually means the correctness
  contract never got evidence (zero verified checks). That is NOT a
  pass, and NOT a failure either — read the structured reason.
- `INCOMPLETE` after a machine reset mid-run: recover with
  `journal <file>`; partial evidence is preserved by design, never
  padded.
- unrecognized WHEA records block `PASS` until adjudicated — the run
  will say exactly how many.
- Every finished run ends by naming its journal file(s) (a refusal that
  journaled nothing is stated as nothing, never pointed at a phantom), the
  durable result file, and the sharing routes: `report <journal>` renders
  the full story from the journal; `bundle <journal>` builds an
  identity-scrubbed folder (journal, report, config, last result,
  topology/caps snapshots, with a self-check that says so and exits 1 if
  an identity string survived) — that folder is what is safe to post.
- Phases whose WHEA window came back `degraded`, `unavailable` or
  `initializing` are named there per phase, with the reason the run
  recorded: zero WHEA records inside such a window means "could not
  look", not "nothing happened". On Windows, permissions are the lever
  that can make the observation possible — named as a lever, never as a
  diagnosis of any window or of the machine.

## 8. Repeatable scripted runs

```powershell
.\cpu-stability.exe run -request C:\path\to\request.json   # saved shape, replays exactly
.\cpu-stability.exe repeat                                # re-run the saved request
```

A saved request replays with the same plan unless you change it
explicitly; `plan` shows the resolved run before it starts, and any
topology drift between plan and start is refused by name rather than
silently adapted.

## Sanity rules

1. `caps` and `topology` before, not after.
2. Grow durations and memory in steps; a 15s result is 15s of evidence.
3. Read verdicts from the result file, exit codes are a different axis.
4. Never claim parking that was not observed.
5. Derive every core/CCX/SMT set from YOUR `topology` output.
