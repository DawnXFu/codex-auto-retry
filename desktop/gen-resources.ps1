# Generates rsrc_windows_amd64.syso next to the desktop sources so a plain
# `go build` links the icon + DPI manifest into the exe. Without it the
# binary ships no RT_ICON (generic title-bar icon) and no manifest (Windows
# treats the process as DPI-unaware and bitmap-stretches the UI).
#
# `wails build` does this itself — this script only matters for the go-build
# fallback path in build.ps1 / scripts/build.ps1. Re-run after changing
# build/windows/icon.ico or wails.exe.manifest (icons: tools/gen-icons.mjs).
[CmdletBinding()]
param()
$ErrorActionPreference = "Stop"

$desktop  = $PSScriptRoot
$icon     = Join-Path $desktop "build\windows\icon.ico"
$manifest = Join-Path $desktop "build\windows\wails.exe.manifest"
$syso     = Join-Path $desktop "rsrc_windows_amd64.syso"

if (-not (Test-Path $icon)) { throw "missing $icon — run: node desktop/tools/gen-icons.mjs" }
if (-not (Test-Path $manifest)) { throw "missing $manifest" }

Push-Location $desktop
try {
    if (Get-Command rsrc -ErrorAction SilentlyContinue) {
        & rsrc -manifest $manifest -ico $icon -o $syso
    } else {
        # rsrc rides as a go.mod `tool` directive — `go tool` resolves it from
        # the module cache offline (unlike `go run pkg@ver`, which always
        # queries the proxy for retractions). Fetch once with:
        #   go get -tool github.com/akavel/rsrc@v0.10.2
        & go tool rsrc -manifest $manifest -ico $icon -o $syso
    }
    if ($LASTEXITCODE -ne 0) { throw "rsrc failed" }
} finally {
    Pop-Location
}
Write-Host ">> embedded icon+manifest -> $syso"
