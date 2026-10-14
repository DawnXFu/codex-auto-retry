[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$binary = Join-Path $PSScriptRoot 'bin\codex-auto-resume.exe'
$testRoot = Join-Path $env:TEMP ("codex-auto-retry-tray-" + [guid]::NewGuid().ToString('N'))
$dataDir = Join-Path $testRoot 'data'
$process = $null

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class CodexAutoRetryTrayProbe {
    private delegate bool EnumWindowsProc(IntPtr window, IntPtr state);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool EnumWindows(EnumWindowsProc callback, IntPtr state);
    [DllImport("user32.dll", SetLastError = true)]
    private static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern int GetClassName(IntPtr window, System.Text.StringBuilder className, int maxCount);

    // fyne.io/systray registers a hidden message window with the fixed class
    // name "SystrayClass" on Windows; other products use the same name, so the
    // probe matches by owning process id rather than by class alone.
    public static IntPtr FindProcessWindow(uint targetProcessId, string wantedClass) {
        IntPtr match = IntPtr.Zero;
        EnumWindows(delegate(IntPtr window, IntPtr state) {
            uint processId;
            GetWindowThreadProcessId(window, out processId);
            if (processId != targetProcessId) { return true; }
            var name = new System.Text.StringBuilder(256);
            if (GetClassName(window, name, name.Capacity) == 0) { return true; }
            if (string.Equals(name.ToString(), wantedClass, StringComparison.Ordinal)) {
                match = window;
                return false;
            }
            return true;
        }, IntPtr.Zero);
        return match;
    }
}
'@

try {
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
        throw "Built watchdog not found: $binary"
    }
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    $config = [ordered]@{
        config_version = 4
        poll_interval_seconds = 1
        initial_delay_seconds = 2
        max_delay_seconds = 30
        delay_strategy = 'exponential'
        max_consecutive_retries = 100
        max_recovery_attempts = 1000
        max_parallel_retries = 2
        start_ack_timeout_seconds = 10
        auth_max_attempts = 3
        unknown_max_attempts = 2
        session_roots = @()
        include_default_home = $false
        include_cockpit_homes = $false
        retry_prompt = 'Continue tray smoke test.'
        show_notifications = $false
    }
    [System.IO.File]::WriteAllText(
        (Join-Path $dataDir 'config.json'),
        ($config | ConvertTo-Json -Depth 5),
        [System.Text.UTF8Encoding]::new($false)
    )
    $process = Start-Process -FilePath $binary -ArgumentList @('run', '--data-dir', $dataDir) -WindowStyle Hidden -PassThru
    $deadline = (Get-Date).AddSeconds(15)
    $trayWindow = [IntPtr]::Zero
    do {
        Start-Sleep -Milliseconds 200
        $process.Refresh()
        if ($process.HasExited) { throw "Watchdog exited before creating the tray icon (exit=$($process.ExitCode))." }
        $trayWindow = [CodexAutoRetryTrayProbe]::FindProcessWindow([uint32]$process.Id, 'SystrayClass')
    } while ($trayWindow -eq [IntPtr]::Zero -and (Get-Date) -lt $deadline)
    if ($trayWindow -eq [IntPtr]::Zero) {
        throw 'Systray window was not created.'
    }
    $statusPath = Join-Path $dataDir 'status.json'
    $deadline = (Get-Date).AddSeconds(10)
    do {
        Start-Sleep -Milliseconds 200
        $status = $null
        if (Test-Path -LiteralPath $statusPath) {
            try { $status = Get-Content -Raw -Encoding UTF8 -LiteralPath $statusPath | ConvertFrom-Json } catch { }
        }
    } while ((-not $status -or -not $status.running -or $status.pid -ne $process.Id) -and (Get-Date) -lt $deadline)
    if (-not $status -or -not $status.running -or $status.pid -ne $process.Id) {
        throw 'Tray watchdog did not publish a valid heartbeat.'
    }
    $logPath = Join-Path $dataDir 'logs\daemon.log'
    $log = if (Test-Path -LiteralPath $logPath) { Get-Content -Raw -Encoding UTF8 -LiteralPath $logPath } else { '' }
    if ($log -match 'category=tray_error') {
        throw 'Tray initialization reported an error.'
    }
    New-Item -ItemType File -Force -Path (Join-Path $dataDir 'stop.signal') | Out-Null
    if (-not $process.WaitForExit(10000)) {
        throw 'Tray watchdog did not exit cleanly.'
    }
    $status = Get-Content -Raw -Encoding UTF8 -LiteralPath $statusPath | ConvertFrom-Json
    if ($status.running -or $status.pending_retries -ne 0 -or $status.active_retries -ne 0) {
        throw 'Final status retained stale running or retry state.'
    }
    [pscustomobject]@{
        Status = 'passed'
        SystrayWindowCreated = $true
        HeartbeatPublished = $true
        FinalStateClean = $true
    }
}
finally {
    if ($process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $testRoot) {
        $resolvedTestRoot = [System.IO.Path]::GetFullPath($testRoot)
        $resolvedTemp = [System.IO.Path]::GetFullPath($env:TEMP).TrimEnd('\') + '\'
        if (-not $resolvedTestRoot.StartsWith($resolvedTemp, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Refusing to remove unexpected tray smoke-test path: $resolvedTestRoot"
        }
        Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force
    }
}
