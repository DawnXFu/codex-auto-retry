[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$pluginRoot = Split-Path -Parent $PSScriptRoot
$sourceDir = Join-Path $PSScriptRoot 'source'
$binDir = Join-Path $PSScriptRoot 'bin'
$watchdogOutput = Join-Path $binDir 'codex-auto-resume.exe'
. (Join-Path $PSScriptRoot 'build-provenance.ps1')

New-Item -ItemType Directory -Force -Path $binDir | Out-Null

Push-Location $sourceDir
try {
    & go fmt ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go formatting failed.' }
    & go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go vet failed.' }
    $sourceHash = Get-CodexBuildSourceHash -Root $pluginRoot
    $buildFlags = '-s -w -H=windowsgui -X codex-auto-retry/internal/watchdog.buildSourceHash=' + $sourceHash
    & go build -trimpath -ldflags $buildFlags -o $watchdogOutput ./cmd/auto-resume
    if ($LASTEXITCODE -ne 0) { throw 'Watchdog build failed.' }
}
finally {
    Pop-Location
}

# The desktop client lives next to the watchdog binary in the installed
# layout. It needs npm to build frontend/dist before the Go embed compiles,
# so a toolchain without npm still produces a working watchdog-only build —
# the miss is loud, not silent, and install.ps1 installs the client whenever
# this step produced it.
$desktopOutput = Join-Path $binDir 'codex-auto-resume-desktop.exe'
$desktopBuilt = $false
if (Get-Command npm -ErrorAction SilentlyContinue) {
    & (Join-Path $pluginRoot 'desktop\build.ps1') -FrontendOnly
    if ($LASTEXITCODE -ne 0) { throw 'Desktop frontend build failed.' }
    Push-Location (Join-Path $pluginRoot 'desktop')
    try {
        & go build -trimpath -ldflags '-s -w -H=windowsgui' -o $desktopOutput .
        if ($LASTEXITCODE -ne 0) { throw 'Desktop client build failed.' }
    }
    finally {
        Pop-Location
    }
    $desktopBuilt = $true
}
else {
    Write-Warning 'npm is not on PATH — skipping the desktop client build. The watchdog installs alone, but the tray has no settings window until the client is built.'
}

$watchdogBinary = Get-Item -LiteralPath $watchdogOutput
. (Join-Path $PSScriptRoot 'build-provenance.ps1')
Write-CodexBuildProvenance -Root $pluginRoot
[pscustomobject]@{
    PluginRoot = $pluginRoot
    WatchdogBinary = $watchdogBinary.FullName
    WatchdogBytes = $watchdogBinary.Length
    DesktopBinary = if ($desktopBuilt) { $desktopOutput } else { $null }
    Status = 'built'
}
