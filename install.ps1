# Install gocode on Windows from its GitHub releases.
#
#   irm https://raw.githubusercontent.com/umairge66/gocode/main/install.ps1 | iex
#
# Environment:
#   GOCODE_VERSION       release to install, e.g. 0.1.1 (default: the latest)
#   GOCODE_INSTALL_DIR   where to put gocode.exe (default: %LOCALAPPDATA%\Programs\gocode)
#
# The download is checked against the release's checksums.txt before it is
# installed, and the folder is added to your user PATH. Nothing else changes.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'umairge66/gocode'
$dir = if ($env:GOCODE_INSTALL_DIR) { $env:GOCODE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\gocode' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    'x86' { if ($env:PROCESSOR_ARCHITEW6432 -eq 'AMD64') { 'amd64' } elseif ($env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { '386' } }
    default { throw "gocode install: unsupported processor: $env:PROCESSOR_ARCHITECTURE" }
}

$base = if ($env:GOCODE_VERSION) {
    "https://github.com/$repo/releases/download/v$($env:GOCODE_VERSION.TrimStart('v'))"
} else {
    "https://github.com/$repo/releases/latest/download"
}
$archive = "gocode_windows_$arch.zip"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("gocode-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading gocode for windows/$arch..."
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

    $want = $null
    foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $archive) { $want = $parts[0].ToLower() }
    }
    if (-not $want) { throw "gocode install: $archive is not listed in checksums.txt" }
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
    if ($got -ne $want) { throw "gocode install: checksum mismatch for $archive (expected $want, got $got)" }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x') -Force
    $exe = Get-ChildItem -Path (Join-Path $tmp 'x') -Recurse -Filter 'gocode.exe' | Select-Object -First 1
    if (-not $exe) { throw 'gocode install: the archive holds no gocode.exe' }

    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $target = Join-Path $dir 'gocode.exe'
    # A running gocode.exe cannot be overwritten, but it can be renamed.
    if (Test-Path $target) {
        $old = "$target.old"
        Remove-Item -Force $old -ErrorAction SilentlyContinue
        Move-Item -Force $target $old
    }
    Copy-Item $exe.FullName $target
    Remove-Item -Force "$target.old" -ErrorAction SilentlyContinue
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath = ($userPath -split ';') -contains $dir
if (-not $onPath) {
    $newPath = if ($userPath) { "$userPath;$dir" } else { $dir }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    $env:Path = "$env:Path;$dir"
}

$version = & $target --version 2>$null
Write-Host "Installed $version to $target"
if (-not $onPath) { Write-Host "Added $dir to your PATH. Open a new terminal, then run: gocode" } else { Write-Host 'Run: gocode' }
