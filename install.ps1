$ErrorActionPreference = "Stop"

$repo = if ($env:WR_REPO) { $env:WR_REPO } else { "stormovermars/wr" }
$dest = if ($env:WR_INSTALL_DIR) { $env:WR_INSTALL_DIR } else { Join-Path $HOME ".local\bin" }
$version = if ($env:WR_VERSION) { $env:WR_VERSION } else { "latest" }
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "x86_64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}
$asset = "wr_Windows_$arch.zip"
$base = if ($version -eq "latest") {
    "https://github.com/$repo/releases/latest/download"
} else {
    "https://github.com/$repo/releases/download/$version"
}
$temp = Join-Path ([IO.Path]::GetTempPath()) ("wr-" + [guid]::NewGuid())

try {
    New-Item -ItemType Directory -Force -Path $temp, $dest | Out-Null
    $archive = Join-Path $temp $asset
    $checksums = Join-Path $temp "checksums.txt"
    Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile $checksums
    $line = Get-Content $checksums | Where-Object { $_ -match "^[0-9a-fA-F]{64}\s+\*?$([regex]::Escape($asset))$" } | Select-Object -First 1
    if (-not $line) { throw "Release checksum not found" }
    $expected = ($line -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Release checksum mismatch" }
    Expand-Archive -Force $archive $temp
    Copy-Item -Force (Join-Path $temp "wr.exe") (Join-Path $dest "wr.exe")
    Write-Output "installed wr to $(Join-Path $dest 'wr.exe')"
    # Put $dest on the user PATH (persisted for new shells, and for this one).
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $parts = @($userPath -split ";" | Where-Object { $_ })
    if (-not $env:WR_NO_PATH -and ($parts | ForEach-Object { $_.TrimEnd("\") }) -notcontains $dest.TrimEnd("\")) {
        [Environment]::SetEnvironmentVariable("Path", (($parts + $dest) -join ";"), "User")
        Write-Output "added $dest to the user PATH (open a new terminal to use wr)"
    }
    if (($env:PATH -split ";") -notcontains $dest) { $env:PATH = "$env:PATH;$dest" }
} finally {
    if (Test-Path $temp) { Remove-Item -Recurse -Force $temp }
}
