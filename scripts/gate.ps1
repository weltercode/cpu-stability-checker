# gate.ps1 - the canonical development gate of this repository.
#
# Why a script exists (ROADMAP R12): the gate recipe used to live in
# the operator's memory, and memory once "helpfully" merged the heavy
# subprocess package into the cli audit batch without -p 1, after
# which a memory-tight machine turned capability limits into hard
# failures. The recipe is an artifact now: run this script, do not
# improvise.
#
# Usage (repository root, PowerShell):
#   powershell -NoProfile -File scripts\gate.ps1
#   optional: -Mixed "<pkgs>" -Audit "<pkgs>" -Seed <n> -Log <file>
#
# Non-negotiables baked in below:
#   - strictly serial: -p 1 -parallel 1 everywhere (R11 lesson: heavy
#     worker packages sharing a batch without -p 1 squeeze the timing
#     out of light end-to-end tests);
#   - the audit seed is pinned (shuffle=1792760321706690100) so test
#     order is reproducible across runs;
#   - the mixed batch runs WITH -p 1: mixing packages is allowed only
#     fully serialized;
#   - the machine moment line is written first, so every green or red
#     result is attributable to a known machine state (available
#     physical + commit; R12 lesson);
#   - nothing is swallowed: every command's full output is in the log,
#     and the skip count of the -short suite is printed (skips must be
#     zero on an idle capable machine; any skip names its capability).
#
# Exit code: 0 = every step green; otherwise the first failing step's
# code. Machine precondition: idle. Do not run while large memory
# users are active - capability skips (R12) keep the suite honest
# under pressure, but the point of the gate is that they stay at zero.

param(
    [string]$Audit = "./internal/cli",
    [string]$Mixed = "./internal/cli ./internal/subprocess ./internal/wmisensor ./internal/hwinfo",
    [long]$Seed = 1792760321706690100,
    [string]$Log = ""
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

# 0. the machine moment, written before anything can be attributed.
$os = Get-CimInstance Win32_OperatingSystem
$commit = (Get-Process | Measure-Object PM -Sum).Sum / 1GB
$pf = (Get-CimInstance Win32_PageFileUsage -ErrorAction SilentlyContinue).AllocatedBaseSize
$pre = "pre-gate {0:HH:mm:ss}: free_phys={1:N1}GB, sum_private={2:N1}GB, pagefile={3}MB" -f (Get-Date), ($os.FreePhysicalMemory / 1MB), $commit, $pf
Write-Output $pre
Set-Content -Path $Log -Value $pre

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
