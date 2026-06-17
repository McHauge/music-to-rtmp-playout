# Downloads self-contained ffmpeg, ffprobe, and yt-dlp into ./bin so the app
# needs nothing installed on the host. Re-run to update the binaries.
#
#   pwsh ./scripts/fetch-tools.ps1
$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$bin  = Join-Path $root 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null

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

Write-Host ''
Write-Host "Done. Bundled tools in $bin :"
Get-ChildItem $bin | Where-Object { $_.Name -ne '.gitkeep' } | ForEach-Object { Write-Host "  $($_.Name)" }
