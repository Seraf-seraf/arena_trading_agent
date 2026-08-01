param(
    [string]$ProcessName = "UAGame"
)

$ErrorActionPreference = "Stop"
$methodCtx = "scripts.focus-game"

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

public static class ArenaWindowFocus
{
    [DllImport("user32.dll")]
    public static extern bool ShowWindow(IntPtr window, int command);

    [DllImport("user32.dll")]
    public static extern bool SetForegroundWindow(IntPtr window);
}
"@

$process = Get-Process -Name $ProcessName |
    Where-Object { $_.MainWindowHandle -ne [IntPtr]::Zero } |
    Select-Object -First 1

if ($null -eq $process) {
    throw "${methodCtx}: не найдено главное окно процесса $ProcessName"
}

[ArenaWindowFocus]::ShowWindow($process.MainWindowHandle, 9) | Out-Null
$focused = [ArenaWindowFocus]::SetForegroundWindow($process.MainWindowHandle)

[pscustomobject]@{
    Method = $methodCtx
    Message = "окно игры явно переведено на передний план"
    ProcessId = $process.Id
    Handle = $process.MainWindowHandle
    Foreground = $focused
} | ConvertTo-Json -Compress
