[CmdletBinding()]
param(
    [string]$ProcessName = "UAGame"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$methodCtx = "scripts.inspect-game-window"

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

public static class ArenaWindowInspection
{
    [StructLayout(LayoutKind.Sequential)]
    public struct Rect
    {
        public int Left;
        public int Top;
        public int Right;
        public int Bottom;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct Point
    {
        public int X;
        public int Y;
    }

    [DllImport("user32.dll", SetLastError = true)]
    public static extern bool GetClientRect(IntPtr window, out Rect rect);

    [DllImport("user32.dll", SetLastError = true)]
    public static extern bool ClientToScreen(IntPtr window, ref Point point);

    [DllImport("user32.dll")]
    public static extern uint GetDpiForWindow(IntPtr window);

    [DllImport("user32.dll", SetLastError = true)]
    public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr context);
}
"@

$windows = @(
    Get-Process -Name $ProcessName -ErrorAction SilentlyContinue |
        Where-Object { $_.MainWindowHandle -ne [IntPtr]::Zero }
)
if ($windows.Count -eq 0) {
    throw "${methodCtx}: не найдено видимое главное окно процесса $ProcessName"
}
if ($windows.Count -ne 1) {
    $identifiers = ($windows | ForEach-Object { $_.Id }) -join ","
    throw "${methodCtx}: найдено несколько игровых окон; PID: $identifiers"
}

$process = $windows[0]
$perMonitorAwareV2 = [IntPtr](-4)
$previousDpiContext = [ArenaWindowInspection]::SetThreadDpiAwarenessContext($perMonitorAwareV2)
if ($previousDpiContext -eq [IntPtr]::Zero) {
    $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
    throw "${methodCtx}: не удалось включить точное чтение DPI, ошибка Win32 $code"
}
try {
    $rect = New-Object ArenaWindowInspection+Rect
    if (-not [ArenaWindowInspection]::GetClientRect($process.MainWindowHandle, [ref]$rect)) {
        $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
        throw "${methodCtx}: GetClientRect завершился ошибкой Win32 $code"
    }
    $origin = New-Object ArenaWindowInspection+Point
    if (-not [ArenaWindowInspection]::ClientToScreen($process.MainWindowHandle, [ref]$origin)) {
        $code = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
        throw "${methodCtx}: ClientToScreen завершился ошибкой Win32 $code"
    }
    $dpi = [ArenaWindowInspection]::GetDpiForWindow($process.MainWindowHandle)
} finally {
    [ArenaWindowInspection]::SetThreadDpiAwarenessContext($previousDpiContext) | Out-Null
}
if ($dpi -eq 0) {
    throw "${methodCtx}: GetDpiForWindow вернул нулевой DPI"
}

$width = $rect.Right - $rect.Left
$height = $rect.Bottom - $rect.Top
$dpiPercent = [int][Math]::Round(($dpi * 100.0) / 96.0)
if ($width -le 0 -or $height -le 0 -or $dpiPercent -le 0) {
    throw "${methodCtx}: получена некорректная клиентская область ${width}x${height} при DPI ${dpiPercent}%"
}

[pscustomobject]@{
    Method = $methodCtx
    Message = "параметры клиентской области прочитаны без фокуса и без пользовательского ввода"
    ProcessId = $process.Id
    WindowTitle = $process.MainWindowTitle
    ClientLeft = $origin.X
    ClientTop = $origin.Y
    ARENA_EXPECTED_WIDTH = $width
    ARENA_EXPECTED_HEIGHT = $height
    ARENA_EXPECTED_DPI = $dpiPercent
} | ConvertTo-Json -Compress
