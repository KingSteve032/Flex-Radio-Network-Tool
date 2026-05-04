param(
  [string]$Version = "latest",
  [string]$Repo = "KingSteve032/Flex-Radio-Network-Tool"
)

$ErrorActionPreference = "Stop"

$installDir = Join-Path $env:ProgramFiles "Flex Radio Network Tool"
$exePath = Join-Path $installDir "frnt.exe"
$iconPath = Join-Path $installDir "icon.png"

if (!(Test-Path $installDir)) {
  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}

if ($Version -eq "latest") {
  $binUrl = "https://github.com/$Repo/releases/latest/download/frnt-windows-amd64.exe"
} else {
  $binUrl = "https://github.com/$Repo/releases/download/$Version/frnt-windows-amd64.exe"
}

$iconUrl = "https://raw.githubusercontent.com/$Repo/main/assets/icon.png"

Write-Host "Downloading $binUrl"
Invoke-WebRequest -UseBasicParsing -Uri $binUrl -OutFile $exePath

Write-Host "Downloading $iconUrl"
Invoke-WebRequest -UseBasicParsing -Uri $iconUrl -OutFile $iconPath

Write-Host "Installed:"
& $exePath --version
Write-Host "Path: $exePath"

