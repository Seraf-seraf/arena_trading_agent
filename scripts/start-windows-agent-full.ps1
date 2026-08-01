[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$StagedExecutable,
    [string]$Executable = "$env:LOCALAPPDATA\ArenaTradingAgent\windows-agent.exe",
    [string]$Controller = "ws://localhost:8787/ws/agent",
    [string]$AgentId = "windows-local",
    [string]$ProcessName = "UAGame.exe",
    [string]$WindowTitle = "Arena Breakout Infinite",
    [Parameter(Mandatory = $true)]
    [string]$ScreenConfig
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$methodCtx = "scripts.start-windows-agent-full"

if (-not (Test-Path -LiteralPath $StagedExecutable -PathType Leaf)) {
    throw "${methodCtx}: собранный windows-agent.exe не найден: $StagedExecutable"
}
if (-not (Test-Path -LiteralPath $ScreenConfig -PathType Leaf)) {
    throw "${methodCtx}: конфигурация экранов не найдена: $ScreenConfig"
}

$targetDirectory = Split-Path -Parent $Executable
$logDirectory = Join-Path $targetDirectory "logs"
New-Item -ItemType Directory -Path $targetDirectory -Force | Out-Null
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null

$installedPath = [System.IO.Path]::GetFullPath($Executable)
$existing = @(
    Get-CimInstance Win32_Process -Filter "Name = 'windows-agent.exe'" |
        Where-Object {
            $_.ExecutablePath -and
            [System.StringComparer]::OrdinalIgnoreCase.Equals(
                [System.IO.Path]::GetFullPath($_.ExecutablePath),
                $installedPath
            )
        }
)
foreach ($process in $existing) {
    Stop-Process -Id $process.ProcessId -Force
    Write-Host "ИНФО метод=${methodCtx}.stop_existing сообщение=остановлен ранее установленный Windows Agent pid=$($process.ProcessId)"
}

Copy-Item -LiteralPath $StagedExecutable -Destination $Executable -Force

$arguments = @(
    "-controller", $Controller,
    "-agent-id", $AgentId,
    "-process", $ProcessName,
    "-window-title", ('"' + $WindowTitle + '"'),
    "-screen-config", $ScreenConfig,
    "-allow-input"
)
$stdout = Join-Path $logDirectory "windows-agent.stdout.log"
$stderr = Join-Path $logDirectory "windows-agent.stderr.log"
$process = Start-Process `
    -FilePath $Executable `
    -ArgumentList $arguments `
    -RedirectStandardOutput $stdout `
    -RedirectStandardError $stderr `
    -PassThru

Start-Sleep -Milliseconds 750
if ($process.HasExited) {
    $errorText = ""
    if (Test-Path -LiteralPath $stderr) {
        $errorText = (Get-Content -LiteralPath $stderr -Raw).Trim()
    }
    throw "${methodCtx}: Windows Agent завершился сразу после запуска, код=$($process.ExitCode), ошибка=$errorText"
}

[pscustomobject]@{
    Method = $methodCtx
    Message = "Windows Agent запущен с полным доступом к SendInput"
    ProcessId = $process.Id
    Executable = $Executable
    Controller = $Controller
    ScreenConfig = $ScreenConfig
    AllowInput = $true
    StandardOutput = $stdout
    StandardError = $stderr
} | ConvertTo-Json -Compress
