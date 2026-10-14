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

$watchdogBinary = Get-Item -LiteralPath $watchdogOutput
. (Join-Path $PSScriptRoot 'build-provenance.ps1')
Write-CodexBuildProvenance -Root $pluginRoot
[pscustomobject]@{
    PluginRoot = $pluginRoot
    WatchdogBinary = $watchdogBinary.FullName
    WatchdogBytes = $watchdogBinary.Length
    Status = 'built'
}
