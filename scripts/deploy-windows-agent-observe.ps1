[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$StagedExecutable,
    [string]$Executable = "$env:LOCALAPPDATA\ArenaTradingAgent\windows-agent.exe",
    [string]$Controller = "ws://localhost:8787/ws/agent",
    [string]$AgentId = "windows-live"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$methodCtx = "scripts.deploy-windows-agent-observe"

$staged = (Resolve-Path -LiteralPath $StagedExecutable).Path
$targetDirectory = Split-Path -Parent $Executable
if (-not (Test-Path -LiteralPath $targetDirectory -PathType Container)) {
    New-Item -ItemType Directory -Path $targetDirectory | Out-Null
}

$running = @(
    Get-Process -Name "windows-agent" -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $Executable }
)
if ($running.Count -gt 1) {
    throw "${methodCtx}: найдено несколько процессов windows-agent из $Executable"
}
if ($running.Count -eq 1) {
    Stop-Process -Id $running[0].Id
    if (-not $running[0].WaitForExit(10000)) {
        throw "${methodCtx}: процесс windows-agent с PID $($running[0].Id) не завершился за 10 секунд"
    }
}

$backup = $null
if (Test-Path -LiteralPath $Executable -PathType Leaf) {
    $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $backup = Join-Path $targetDirectory "windows-agent.$stamp.previous.exe"
    Move-Item -LiteralPath $Executable -Destination $backup
}

try {
    Move-Item -LiteralPath $staged -Destination $Executable
    $process = Start-Process -FilePath $Executable -ArgumentList @(
        "-controller", $Controller,
        "-agent-id", $AgentId
    ) -PassThru
} catch {
    $failureMessage = $_.Exception.Message
    if (Test-Path -LiteralPath $Executable -PathType Leaf) {
        Move-Item -LiteralPath $Executable -Destination $staged
    }
    if ($null -ne $backup) {
        Move-Item -LiteralPath $backup -Destination $Executable
    }
    throw "${methodCtx}: не удалось развернуть и запустить windows-agent: $failureMessage"
}

[pscustomobject]@{
    Method = $methodCtx
    Message = "windows-agent развёрнут и запущен только для наблюдения"
    ProcessId = $process.Id
    Executable = $Executable
    Backup = $backup
    AllowInput = $false
} | ConvertTo-Json -Compress
