# Builds the desktop client binary.
#
# The dashboard frontend lives in the repo-root frontend/ workspace, outside
# this module, so we build it with npm and stage frontend/dist into
# desktop/frontend/dist for the //go:embed in main.go.
#
# Usage:
#   ./build.ps1                 # frontend build + stage + compile binary
#   ./build.ps1 -FrontendOnly   # just build + stage frontend/dist
#   ./build.ps1 -Output foo.exe # override output name
#
# Compile step prefers `wails build` when the Wails CLI is on PATH, and falls
# back to plain `go build` — the app has no codegen dependencies (bindings
# are called through window.go.* directly), so a plain build is a working exe.
[CmdletBinding()]
param(
    [switch]$FrontendOnly,
    [string]$Output = "codex-auto-resume-desktop.exe"
)
$ErrorActionPreference = "Stop"

$desktop  = $PSScriptRoot
$frontend = Join-Path $desktop "..\frontend"
$src      = Join-Path $frontend "dist"
$target   = Join-Path $desktop "frontend\dist"

Push-Location $desktop
try {
    Write-Host ">> npm --prefix frontend run build"
    npm --prefix $frontend run build
    if ($LASTEXITCODE -ne 0) { throw "frontend build failed" }

    if (-not (Test-Path (Join-Path $src "index.html"))) {
        throw "frontend build produced no dist/index.html at $src"
    }
    if (Test-Path $target) { Remove-Item $target -Recurse -Force }
    New-Item -ItemType Directory -Force $target | Out-Null
    Copy-Item (Join-Path $src "*") $target -Recurse -Force
    # Keep the tracked placeholder present so `go build ./...` stays green
    # even if dist is later cleaned.
    New-Item -ItemType File -Force (Join-Path $target ".keep") | Out-Null
    Write-Host ">> staged frontend/dist -> desktop/frontend/dist"

    if ($FrontendOnly) { return }

    if (Get-Command wails -ErrorAction SilentlyContinue) {
        Write-Host ">> wails build -clean"
        wails build -clean
        if ($LASTEXITCODE -ne 0) { throw "wails build failed" }
    } else {
        Write-Host ">> go build -ldflags `"-H windowsgui`" -o $Output ."
        go build -ldflags "-H windowsgui" -o $Output .
        if ($LASTEXITCODE -ne 0) { throw "go build failed" }
        Write-Host ">> wrote $desktop\$Output"
    }
} finally {
    Pop-Location
}
