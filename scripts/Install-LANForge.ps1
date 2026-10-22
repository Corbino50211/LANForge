param([switch]$AutoStart)
$ErrorActionPreference = 'Stop'
$destination = Join-Path $env:LOCALAPPDATA 'Programs\LANForge'
New-Item -ItemType Directory -Force -Path $destination | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'lanforge.exe') -Destination (Join-Path $destination 'lanforge.exe') -Force
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Desktop')) 'LANForge.lnk'))
$shortcut.TargetPath = Join-Path $destination 'lanforge.exe'
$shortcut.WorkingDirectory = $destination
$shortcut.Save()
if ($AutoStart) {
  $startup = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Startup')) 'LANForge.lnk'))
  $startup.TargetPath = Join-Path $destination 'lanforge.exe'
  $startup.Arguments = 'run --no-browser'
  $startup.WorkingDirectory = $destination
  $startup.Save()
}
Write-Host 'Installed. Open the LANForge desktop shortcut to complete setup.'
