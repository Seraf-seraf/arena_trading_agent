[CmdletBinding()]
param(
    [string]$Executable = "$env:LOCALAPPDATA\ArenaTradingAgent\windows-agent.exe",
    [string]$Controller = "ws://localhost:8787/ws/agent",
    [string]$AgentId = "windows-local",
    [string]$ScreenConfig = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$methodCtx = "scripts.start-windows-agent-observe"

if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) {
    throw "${methodCtx}: файл windows-agent.exe не найден: $Executable"
}

$agentArguments = @(
    "-controller", $Controller,
    "-agent-id", $AgentId,
    "-process", "UAGame.exe",
    "-window-title", "Arena Breakout Infinite"
)

if ($ScreenConfig) {
    if (-not (Test-Path -LiteralPath $ScreenConfig -PathType Leaf)) {
        throw "${methodCtx}: файл конфигурации экранов не найден: $ScreenConfig"
    }
    $agentArguments += @("-screen-config", $ScreenConfig)
}

Write-Host "ИНФО метод=${methodCtx} сообщение=windows-agent запускается в режиме наблюдения; параметр -allow-input не передаётся"
Write-Host "ИНФО метод=${methodCtx} сообщение=горячая клавиша аварийной остановки: Ctrl+Alt+F12"
& $Executable @agentArguments
