# gate.ps1 - the canonical development gate of this repository.
#
# Why a script exists (ROADMAP R12): the gate recipe used to live in
# the operator's memory, and memory once "helpfully" merged the heavy
# subprocess package into the cli audit batch without -p 1, after
# which a memory-tight machine turned capability limits into hard
# failures. The recipe is an artifact now: run this script, do not
# improvise.
#
# Non-negotiables baked in below:
#   - strictly serial: -p 1 -parallel 1 everywhere (R11 lesson: heavy
#     worker packages sharing a batch without -p 1 squeeze the timing
#     out of light end-to-end tests);
#   - the audit seed is pinned (shuffle=1792760321706690100) so test
#     order is reproducible across runs;
#   - the mixed batch runs WITH -p 1: mixing packages is allowed only
#     fully serialized;
#   - nothing is swallowed: every command's full output is in the log,
#     and the skip count of the -short suite is printed (skips must be
#     zero on an idle capable machine; any skip names its capability);
#   - the machine moment line is written first, so every green or red
#     result is attributable to a known machine state (available
#     physical + commit; R12 lesson);
#   - the R22 preflight REFUSES to start when this machine cannot host
#     the suite: exit 3, before any step ran, with the refusal written
#     to a named artifact. A machine moment must never be readable as
#     a code regression.
#
# R22 IN DETAIL (why the floors exist and what they are NOT): the
# 2026-11-09 gate started on a box that already held ~65 GB of another
# process's private memory; the suite was clean for ~4 minutes and then
# the box hard-locked (no dump, no WHEA, Event 6008). The post-reboot
# rerun came back RED with 37 named admission skips at 8.9 GB free
# physical. Both outcomes cost the operator a machine and looked like
# the code had broken - the same defect class R12 fixed for improvised
# gate recipes, one level up.
#
# The floors below are POLICY this repository chose, not a measured
# hardware requirement, and they say so in the log. Only one term is
# derived from evidence: the workload allowance W. The suite's largest
# DOCUMENTED memory request is 256 MiB (subprocess/run_end_exit_test.go,
# memtest/alloc_windows_test.go; handoff fixtures document 1 MiB
# bounds), and the gate's -p 1 -parallel 1 means at most one test loads
# at a time. W defaults to 0.5 GiB: that 256 MiB plus an explicitly
# stated allowance for the test binaries and their pinned children,
# which is an allowance, not a measurement. The multi-gigabyte smoke
# test is opt-in (CPU_TEST_SMOKE_GB) and is NOT part of this gate; if an
# operator enables it, they raise -WorkloadGiB to name that policy.
#
# Usage (repository root, PowerShell):
#   powershell -NoProfile -File scripts\gate.ps1
#   optional: -Mixed "<pkgs>" -Audit "<pkgs>" -Seed <n> -Log <file>
#             -PreflightOnly (evaluate the admission policy and stop)
#             -MinFreePhys / -MinFreeCommit / -WorkloadGiB (stated
#             policy overrides) -SkipPreflight "<reason>" (override,
#             never silent: the reason and the measurements stay in the log)
#
# Exit code: 0 = every step green; 3 = the R22 preflight refused before
# anything ran (a machine moment, not a code failure); otherwise the
# first failing step's code.

param(
    [string]$Audit = "./internal/cli",
    [string]$Mixed = "./internal/cli ./internal/subprocess ./internal/wmisensor ./internal/hwinfo",
    [long]$Seed = 1792760321706690100,
    [string]$Log = "",
    [double]$WorkloadGiB = 0,     # 0 = the derived default (0.5 GiB, see header)
    [double]$MinFreePhys = 0,     # 0 = derived floor; a stated override is echoed in the log
    [double]$MinFreeCommit = 0,   # 0 = derived floor
    [string]$SkipPreflight = "",  # run anyway, with a NAMED reason (never silent)
    [switch]$PreflightOnly        # evaluate the policy and stop: no step runs
)

if (-not $Log) { $Log = "gate-" + (Get-Date -Format "yyyyMMdd-HHmmss") + ".log" }
$script:fail = 0
$shuffleArg = @(("-shuffle=" + $Seed))
$auditPkgs = @($Audit -split " ")
$mixedPkgs = @($Mixed -split " ")

function Step([string]$name, [scriptblock]$body) {
    Write-Output ("==== {0} @ {1:HH:mm:ss} ====" -f $name, (Get-Date))
    & $body 2>&1 | ForEach-Object { "$((Get-Date).ToString('HH:mm:ss')) $_" } | Add-Content -Path $Log
    $code = $LASTEXITCODE
    Write-Output ("---- {0}: exit {1} ----" -f $name, $code)
    if ($code -ne 0 -and $script:fail -eq 0) { $script:fail = $code }
}

# 0. the machine moment + the R22 admission preflight, both written
#    before anything can be attributed to the code.
$os = Get-CimInstance Win32_OperatingSystem
$totalPhysGB = $os.TotalVisibleMemorySize / 1MB
$freePhysGB = $os.FreePhysicalMemory / 1MB
$totalCommitGB = $os.TotalVirtualMemorySize / 1MB
$freeCommitGB = $os.FreeVirtualMemory / 1MB
$sumPrivateGB = (Get-Process | Measure-Object PM -Sum).Sum / 1GB
$pf = (Get-CimInstance Win32_PageFileUsage -ErrorAction SilentlyContinue).AllocatedBaseSize
$workload = 0.5
if ($WorkloadGiB -gt 0) { $workload = $WorkloadGiB }
$floorPhys = [math]::Max(4.0, $totalPhysGB * 0.1) + $workload
$floorCommit = 2.0 + $workload
if ($MinFreePhys -gt 0) { $floorPhys = $MinFreePhys }
if ($MinFreeCommit -gt 0) { $floorCommit = $MinFreeCommit }
$top = @(Get-Process | Sort-Object PM -Descending | Select-Object -First 5 | ForEach-Object {
    "{0} {1:N1}GB" -f $_.ProcessName, ($_.PM / 1GB)
})

$lineMoment = "pre-gate {0:HH:mm:ss}: free_phys={1:N1}GB of {2:N1}GB total, free_commit={3:N1}GB of {4:N1}GB limit, sum_private={5:N1}GB, pagefile={6}MB" -f (Get-Date), $freePhysGB, $totalPhysGB, $freeCommitGB, $totalCommitGB, $sumPrivateGB, $pf
$linePolicy = "R22 preflight: policy floors free_phys>={0:N1}GB and free_commit>={1:N1}GB (W={2:N1}GiB; policy terms: 4GB base + 10 percent of total physical for RAM, 2GB for commit; W derives from the suite's documented 256MiB maximum request, and the rest of W is a STATED allowance for the test binaries and their pinned children, not a measurement)" -f $floorPhys, $floorCommit, $workload
$lineTop = "top private consumers now: " + ($top -join ", ")
$moment = @($lineMoment, $linePolicy, $lineTop)
$moment | ForEach-Object { Write-Output $_ }
Set-Content -Path $Log -Value $moment

$violated = @()
if ($freePhysGB -lt $floorPhys) { $violated += ("free physical {0:N1} GB is below the policy floor {1:N1} GB" -f $freePhysGB, $floorPhys) }
if ($freeCommitGB -lt $floorCommit) { $violated += ("free commit {0:N1} GB is below the policy floor {1:N1} GB" -f $freeCommitGB, $floorCommit) }

function Refuse([string[]]$why, [string[]]$context) {
    # The refusal is EVIDENCE, not a message: log + console + its own
    # file, and the file says plainly that no step ever started, so a
    # machine moment can never be read as a code regression.
    $lines = @("GATE REFUSED BEFORE ANY STEP RAN (exit 3: machine moment, not a code failure)") + $why + $context
    $lines | ForEach-Object { Write-Output $_ }
    Add-Content -Path $Log -Value $lines
    $artifact = ($Log -replace '\.log$', '') + "-preflight-refusal.txt"
    $note = @(
        "",
        "No gofmt, vet, test or build step was started: this file is the evidence that the machine, not the code, decided the outcome.",
        "To gate under a different stated policy: -MinFreePhys / -MinFreeCommit / -WorkloadGiB.",
        'To run anyway, name the reason: -SkipPreflight "<reason>" - the override and the measurements stay in the log.'
    )
    Set-Content -Path $artifact -Value ($lines + $note)
    Write-Output ("refusal evidence: " + $artifact)
    exit 3
}

if ($violated.Count -gt 0 -and -not $SkipPreflight) { Refuse $violated $moment }
if ($violated.Count -gt 0) {
    $note = 'PREFLIGHT OVERRIDE by operator ("' + $SkipPreflight + '"): running anyway despite ' + ($violated -join "; ")
    Write-Output $note
    Add-Content -Path $Log -Value $note
}
if ($PreflightOnly) {
    if ($violated.Count -eq 0) { Write-Output "preflight: this machine can host the gate under this policy"; exit 0 }
    Write-Output "preflight: would have refused; -PreflightOnly started no step"
    exit 3
}

# 1. format + vet
Step "gofmt" { $o = gofmt -l .; if ($o) { Write-Output $o; exit 1 } }
Step "vet"   { go vet ./... }

# 2. hermetic floor: the -short suite verbose into the log (not the
#    console), so package results and every capability skip are
#    countable afterwards.
Step "suite -short" { go test -short -p 1 -parallel 1 ./... -count=1 -timeout 400s -v }
$okp = (Select-String -Path $Log -Pattern "^\d\d:\d\d:\d\d ok\s+github" | Measure-Object).Count
$skips = (Select-String -Path $Log -Pattern "--- SKIP" | Measure-Object).Count
Write-Output ("suite: packages_ok=" + $okp + " skips=" + $skips)

# 3. pinned audit seed: solo, then the fully-serial mixed batch
#    (R11+R12: this is the shape that is allowed to mix packages).
Step "audit solo"  { go test @auditPkgs @shuffleArg -count=1 -p 1 -parallel 1 -timeout 300s }
Step "audit mixed" { go test @mixedPkgs @shuffleArg -count=1 -p 1 -parallel 1 -timeout 600s }

# 4. cross-builds: the tree must stay buildable for linux/darwin.
foreach ($osname in @("linux", "darwin", "windows")) {
    $env:GOOS = $osname
    Step ("cross " + $osname) { go build ./... }
}
$env:GOOS = "windows"

Write-Output ("==== gate result: " + $(if ($script:fail -eq 0) { "GREEN" } else { "RED exit " + $script:fail }) + " ; log: " + $Log + " ====")
exit $script:fail
