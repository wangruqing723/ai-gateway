# ai-gateway Windows 自启动安装脚本（Task Scheduler）
# 以管理员身份运行 PowerShell，然后执行：
#   .\scripts\install-win.ps1 install    安装并启动
#   .\scripts\install-win.ps1 uninstall  停止并卸载
#   .\scripts\install-win.ps1 status     查看状态

param([string]$Action = "install")

$TaskName   = "ai-gateway"
$ScriptDir  = Split-Path -Parent $PSScriptRoot
$NodeBin    = (Get-Command node -ErrorAction Stop).Source
$GatewayJs  = Join-Path $ScriptDir "gateway.js"
$LogDir     = Join-Path $env:LOCALAPPDATA "ai-gateway"

function Install-Gateway {
    New-Item -ItemType Directory -Force -Path $LogDir | Out-Null

    $action  = New-ScheduledTaskAction `
        -Execute $NodeBin `
        -Argument "`"$GatewayJs`"" `
        -WorkingDirectory $ScriptDir

    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME

    $settings = New-ScheduledTaskSettingsSet `
        -ExecutionTimeLimit (New-TimeSpan -Hours 0) `
        -RestartCount 5 `
        -RestartInterval (New-TimeSpan -Minutes 1) `
        -StartWhenAvailable

    $principal = New-ScheduledTaskPrincipal `
        -UserId $env:USERNAME `
        -LogonType Interactive `
        -RunLevel Highest

    Register-ScheduledTask `
        -TaskName $TaskName `
        -Action   $action `
        -Trigger  $trigger `
        -Settings $settings `
        -Principal $principal `
        -Force | Out-Null

    Start-ScheduledTask -TaskName $TaskName

    Write-Host "✓ ai-gateway 已安装并启动"
    Write-Host "  日志目录: $LogDir"
    Write-Host "  停止命令: .\scripts\install-win.ps1 uninstall"
}

function Uninstall-Gateway {
    Stop-ScheduledTask  -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Write-Host "✓ ai-gateway 已停止并卸载"
}

function Get-GatewayStatus {
    $task = Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    if ($task) {
        $info = Get-ScheduledTaskInfo -TaskName $TaskName
        Write-Host "✓ ai-gateway 已安装，状态: $($task.State)"
        Write-Host "  上次运行: $($info.LastRunTime)"
    } else {
        Write-Host "✗ ai-gateway 未安装"
    }
}

switch ($Action) {
    "install"   { Install-Gateway   }
    "uninstall" { Uninstall-Gateway }
    "status"    { Get-GatewayStatus }
    default {
        Write-Host "用法: .\scripts\install-win.ps1 install | uninstall | status"
        exit 1
    }
}
