# Downloads self-contained ffmpeg, ffprobe, and yt-dlp into ./bin (plus the
# Datastar client bundle into static/vendor) so the app needs nothing installed
# on the host. Re-run to update the binaries.
#
#   pwsh ./scripts/fetch-tools.ps1
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$bin  = Join-Path $root 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null

# Datastar client bundle. MUST match the datastar-go SDK in go.mod (v1.x).
# Datastar parses keyed attributes with a colon (data-on:click), so the
# templates and this bundle version have to stay in lockstep.
$datastarVersion = 'v1.0.2'
$vendor = Join-Path $root 'static/vendor'
New-Item -ItemType Directory -Force -Path $vendor | Out-Null
Write-Host "==> datastar $datastarVersion"
Invoke-WebRequest -Uri "https://cdn.jsdelivr.net/gh/starfederation/datastar@$datastarVersion/bundles/datastar.js" `
    -OutFile (Join-Path $vendor 'datastar.js')

Write-Host '==> yt-dlp'
Invoke-WebRequest -Uri 'https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe' `
    -OutFile (Join-Path $bin 'yt-dlp.exe')

Write-Host '==> ffmpeg + ffprobe (gyan.dev essentials build)'
$zip = Join-Path $env:TEMP 'ffmpeg-release-essentials.zip'
$ext = Join-Path $env:TEMP 'ffmpeg-extract'
Invoke-WebRequest -Uri 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip' -OutFile $zip
if (Test-Path $ext) { Remove-Item -Recurse -Force $ext }
Expand-Archive -Path $zip -DestinationPath $ext -Force

foreach ($exe in @('ffmpeg.exe', 'ffprobe.exe')) {
    $src = Get-ChildItem -Path $ext -Recurse -Filter $exe | Select-Object -First 1
    if (-not $src) { throw "Could not find $exe in the downloaded archive" }
    Copy-Item $src.FullName (Join-Path $bin $exe) -Force
}

Remove-Item $zip -Force
Remove-Item -Recurse -Force $ext

# MediaMTX relay (local dev only — lets you watch the stream on-box and, like the
# Docker `dev` image, gives the encoder a reliable localhost RTMP sink that
# absorbs upstream network blips). Pinned to the Docker image's MEDIAMTX_VERSION.
# The production image ships no relay and pushes straight to an external ingest.
$mediamtxVersion = 'v1.9.3'
Write-Host "==> mediamtx $mediamtxVersion"
$mmZip = Join-Path $env:TEMP 'mediamtx.zip'
$mmExt = Join-Path $env:TEMP 'mediamtx-extract'
Invoke-WebRequest -Uri "https://github.com/bluenviron/mediamtx/releases/download/$mediamtxVersion/mediamtx_${mediamtxVersion}_windows_amd64.zip" `
    -OutFile $mmZip
if (Test-Path $mmExt) { Remove-Item -Recurse -Force $mmExt }
Expand-Archive -Path $mmZip -DestinationPath $mmExt -Force
# Take only the binary; the repo ships its own mediamtx.yml.
$mmSrc = Get-ChildItem -Path $mmExt -Recurse -Filter 'mediamtx.exe' | Select-Object -First 1
if (-not $mmSrc) { throw 'Could not find mediamtx.exe in the downloaded archive' }
Copy-Item $mmSrc.FullName (Join-Path $bin 'mediamtx.exe') -Force
Remove-Item $mmZip -Force
Remove-Item -Recurse -Force $mmExt

Write-Host ''
Write-Host "Done. Bundled tools in $bin :"
Get-ChildItem $bin | Where-Object { $_.Name -ne '.gitkeep' } | ForEach-Object { Write-Host "  $($_.Name)" }
