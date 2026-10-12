# Helpers shared by the protocol and shared-server smoke tests only.
# Keep this read-only: tests dot-source it and it must not touch machine state.

function Find-CodexBinary {
    foreach ($root in @(
        (Join-Path $env:LOCALAPPDATA 'OpenAI\Codex\bin'),
        (Join-Path $env:APPDATA 'npm\node_modules\@openai\codex')
    )) {
        if (-not (Test-Path -LiteralPath $root -PathType Container)) { continue }
        $candidate = Get-ChildItem -LiteralPath $root -Recurse -Filter 'codex.exe' -File -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending |
            Select-Object -First 1
        if ($candidate) { return $candidate.FullName }
    }
    throw 'Codex CLI was not found for the smoke test.'
}
