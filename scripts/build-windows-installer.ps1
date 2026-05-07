param(
  [string]$Version = "dev",
  [string]$InputExe = "dist/frnt-windows-amd64.exe",
  [string]$OutputDir = "dist/installer"
)

$ErrorActionPreference = "Stop"

if (!(Test-Path $InputExe)) {
  throw "Input executable not found: $InputExe"
}
if (!(Test-Path "assets/icon.png")) {
  throw "assets/icon.png not found"
}

New-Item -ItemType Directory -Force $OutputDir | Out-Null

$iconIco = Join-Path $OutputDir "icon.ico"

# Convert icon.png -> icon.ico (used by installer + shortcuts)
if (Get-Command magick -ErrorAction SilentlyContinue) {
  magick assets/icon.png -define icon:auto-resize=256,128,64,48,32,16 $iconIco
} else {
  throw "ImageMagick (magick) not found; required to generate icon.ico"
}

if (!(Test-Path $iconIco)) {
  throw "Failed generating icon file: $iconIco"
}

$iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe"
if (!(Test-Path $iscc)) {
  throw "Inno Setup compiler not found: $iscc"
}

$script = "packaging/windows/frnt-installer.iss"
$sourceExe = (Resolve-Path $InputExe).Path
$sourcePng = (Resolve-Path "assets/icon.png").Path
$sourceIco = (Resolve-Path $iconIco).Path
$outAbs = (Resolve-Path $OutputDir).Path

& $iscc `
  "/DMyAppVersion=$Version" `
  "/DMySourceExe=$sourceExe" `
  "/DMyIconPng=$sourcePng" `
  "/DMySetupIcon=$sourceIco" `
  "/DMyOutputDir=$outAbs" `
  $script

Write-Host "Windows installer build completed in $outAbs"
