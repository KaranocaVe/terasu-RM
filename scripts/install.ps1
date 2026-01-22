$ErrorActionPreference = "Stop"

param(
  [string]$Version = "",
  [string]$BinDir = "$env:USERPROFILE\\bin",
  [ValidateSet("rmirror", "rmirrord", "both")]
  [string]$Component = "both",
  [switch]$SkipDocker,
  [string]$DockerConfig = "$env:USERPROFILE\\.config\\rmirror\\docker.json"
)

$repo = "KaranocaVe/terasu-RM"
if (-not $Version) {
  $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
  $Version = $release.tag_name
}
if (-not $Version) {
  throw "Failed to determine latest version."
}
if (-not $Version.StartsWith("v")) {
  $Version = "v$Version"
}

$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
switch ($arch) {
  "X64" { $goarch = "amd64" }
  "Arm64" { $goarch = "arm64" }
  default { throw "Unsupported architecture: $arch" }
}

$os = "windows"
$ext = ".exe"
$baseUrl = "https://github.com/$repo/releases/download/$Version"

switch ($Component) {
  "rmirror" { $bins = @("rmirror") }
  "rmirrord" { $bins = @("rmirrord") }
  "both" { $bins = @("rmirror", "rmirrord") }
}

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null

foreach ($bin in $bins) {
  $asset = "$bin-$Version-$os-$goarch$ext"
  $url = "$baseUrl/$asset"
  $dest = Join-Path $BinDir "$bin$ext"
  Write-Host "Downloading $url"
  Invoke-WebRequest -Uri $url -OutFile $dest
  Write-Host "Installed $dest"
}

function Write-DockerConfig {
  param(
    [string]$Path
  )
  $dir = Split-Path -Parent $Path
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  if (Test-Path $Path) {
    $length = (Get-Item $Path).Length
    if ($length -gt 0) {
      return
    }
  }
  $rawUrl = "https://raw.githubusercontent.com/$repo/$Version/examples/docker.json"
  try {
    Invoke-WebRequest -Uri $rawUrl -OutFile $Path
    return
  } catch {
  }
  $content = @'
{
  "listen": "127.0.0.1:5000",
  "access_log": true,
  "transport": {
    "first_fragment_len": 3
  },
  "routes": [
    {
      "name": "docker-registry",
      "public_prefix": "/",
      "upstream": "https://registry-1.docker.io"
    },
    {
      "name": "docker-auth",
      "public_prefix": "/_auth",
      "upstream": "https://auth.docker.io"
    },
    {
      "name": "docker-blob",
      "public_prefix": "/_blob",
      "upstream": "https://production.cloudflare.docker.com"
    }
  ]
}
'@
  Set-Content -Path $Path -Value $content
}

function Start-DockerMirror {
  if ($Component -eq "rmirrord") {
    Write-Host "rmirror not installed; skip docker mirror start"
    return
  }

  Write-DockerConfig -Path $DockerConfig
  $cfgDir = Split-Path -Parent $DockerConfig
  $pidPath = Join-Path $cfgDir "rmirror-docker.pid"
  $logPath = Join-Path $cfgDir "rmirror-docker.log"

  if (Test-Path $pidPath) {
    $pid = Get-Content $pidPath | Select-Object -First 1
    if ($pid) {
      $proc = Get-Process -Id $pid -ErrorAction SilentlyContinue
      if ($proc) {
        Write-Host "rmirror already running (pid $pid)"
        return
      }
    }
  }

  $rmirrorPath = Join-Path $BinDir "rmirror$ext"
  if (-not (Test-Path $rmirrorPath)) {
    $cmd = Get-Command rmirror -ErrorAction SilentlyContinue
    if ($cmd) {
      $rmirrorPath = $cmd.Path
    }
  }
  if (-not (Test-Path $rmirrorPath)) {
    Write-Host "rmirror binary not found; skip docker mirror start"
    return
  }

  $proc = Start-Process -FilePath $rmirrorPath `
    -ArgumentList "-config", $DockerConfig `
    -WindowStyle Hidden `
    -RedirectStandardOutput $logPath `
    -RedirectStandardError $logPath `
    -PassThru
  $proc.Id | Set-Content -Path $pidPath
  Write-Host "rmirror started (pid $($proc.Id))"
  Write-Host "config: $DockerConfig"
  Write-Host "log: $logPath"
}

function Print-DockerInstructions {
  Write-Host ""
  Write-Host "Docker Desktop -> Settings -> Docker Engine"
  Write-Host "Add:"
  Write-Host '  {'
  Write-Host '    "registry-mirrors": ["http://127.0.0.1:5000"],'
  Write-Host '    "insecure-registries": ["127.0.0.1:5000"]'
  Write-Host '  }'
  Write-Host ""
}

if (-not $SkipDocker) {
  Start-DockerMirror
  Print-DockerInstructions
}
