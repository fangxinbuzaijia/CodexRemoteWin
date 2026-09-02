$ErrorActionPreference = 'Stop'

$outputDir = Join-Path $PSScriptRoot 'dist'
$outputFile = Join-Path $outputDir 'CodexRemoteWin.exe'
$resourceFile = Join-Path $PSScriptRoot 'rsrc_windows_amd64.syso'
$iconFile = Join-Path $PSScriptRoot 'assets\codex-remote-icon.ico'
$localRsrc = Join-Path $PSScriptRoot 'tools\rsrc.exe'

New-Item -ItemType Directory -Path $outputDir -Force | Out-Null

if (Test-Path -LiteralPath $localRsrc) {
    & $localRsrc -arch amd64 -ico $iconFile -o $resourceFile
} else {
    go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -ico $iconFile -o $resourceFile
}

go test ./...
go build -trimpath -ldflags '-H windowsgui -s -w' -o $outputFile .

$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $outputFile).Hash
Write-Host "Built: $outputFile"
Write-Host "SHA256: $hash"
