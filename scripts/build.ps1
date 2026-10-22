# Binaries auto-update from UpdateRepo in updater.go; set $env:LANFORGE_UPDATE_REPO = 'owner/name' to override it.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')
Push-Location ui
npm ci
if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
npm run build
if ($LASTEXITCODE -ne 0) { throw 'UI build failed' }
Pop-Location
go test -buildvcs=false ./...
if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED='0'
$flags = "-s -w"
if ($env:LANFORGE_UPDATE_REPO) { $flags += " -X main.UpdateRepo=$env:LANFORGE_UPDATE_REPO" }
foreach ($t in @(@('windows','lanforge-windows-amd64.exe'), @('linux','lanforge-linux-amd64'))) {
  $env:GOOS=$t[0]; $env:GOARCH='amd64'
  go build -buildvcs=false -trimpath -ldflags $flags -o "dist/$($t[1])" .
  if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
}
Remove-Item Env:GOOS, Env:GOARCH
# Upload both binaries and SHA256SUMS.txt to a GitHub release tagged vX.Y.Z (matching Version in model.go).
Get-ChildItem dist -Filter 'lanforge-*' | ForEach-Object { "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  $($_.Name)" } | Set-Content -Encoding ascii dist/SHA256SUMS.txt
