[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

# Smoke-test coverage matrix for scripts\*-smoke-test.ps1:
#   runner        = executed by this script
#   CI            = executed by .github/workflows/ci.yml (directly or via release-test.ps1)
#   manual-only   = not scheduled; run by hand
#
#   app-server-protocol-smoke-test.ps1        manual-only  needs an installed standalone Codex CLI (@openai/codex) and drives a real local app-server
#   build-provenance-smoke-test.ps1           manual-only  self-contained fixtures; provenance write/assert is already exercised by build.ps1 in CI
#   empty-response-protocol-smoke-test.ps1    manual-only  needs standalone Codex CLI + node.exe and a local fake provider
#   environment-smoke-test.ps1                runner
#   install-routing-smoke-test.ps1            manual-only  static installer invariants; overlapping asserts run in CI via release-test.ps1
#   installer-cli-smoke-test.ps1              CI           powershell.exe 5.1 step in ci.yml
#   installer-close-smoke-test.ps1            CI           powershell.exe 5.1 step in ci.yml
#   launch-codex-smoke-test.ps1               manual-only  mock-based launcher checks; not scheduled by any runner
#   path-safety-smoke-test.ps1                CI           executed against the payload by release-test.ps1
#   safe-disable-smoke-test.ps1               runner
#   shared-app-server-smoke-test.ps1          runner
#   shared-server-status-smoke-test.ps1       runner
#   startup-approval-smoke-test.ps1           runner + CI  also in the ci.yml protocol step
#   startup-fail-open-smoke-test.ps1          runner
#   startup-manager-smoke-test.ps1            CI           executed against the payload by release-test.ps1
#   status-smoke-test.ps1                     manual-only  needs a live installed watchdog under %LOCALAPPDATA%\CodexAutoRetry
#   supervisor-smoke-test.ps1                 runner + CI  also in the ci.yml protocol step
#   tray-smoke-test.ps1                       manual-only  needs an interactive desktop/tray; starts a real watchdog and checks the systray window
#   upgrade-rollback-smoke-test.ps1           CI           powershell.exe 5.1 step in ci.yml

$startupApprovalResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'startup-approval-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Startup approval smoke test failed.' }

$startupResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'startup-fail-open-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Startup fail-open smoke test failed.' }

$sharedResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'shared-app-server-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Shared app-server recovery smoke test failed.' }

$sharedStatusResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'shared-server-status-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Shared-server status smoke test failed.' }

$environmentResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'environment-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Environment ownership smoke test failed.' }

$safeDisableResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'safe-disable-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Safe-disable smoke test failed.' }

$supervisorResult = & powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'supervisor-smoke-test.ps1')
if ($LASTEXITCODE -ne 0) { throw 'Supervisor smoke test failed.' }

[pscustomobject]@{
    Status = 'passed'
    StartupApproval = ($startupApprovalResult -join [Environment]::NewLine)
    StartupFailOpen = ($startupResult -join [Environment]::NewLine)
    SharedAppServer = ($sharedResult -join [Environment]::NewLine)
    SharedServerStatus = ($sharedStatusResult -join [Environment]::NewLine)
    EnvironmentOwnership = ($environmentResult -join [Environment]::NewLine)
    SafeDisable = ($safeDisableResult -join [Environment]::NewLine)
    Supervisor = ($supervisorResult -join [Environment]::NewLine)
    RealProviderUsed = $false
}
