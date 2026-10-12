$ErrorActionPreference = "Stop"

$dest = if ($env:WR_INSTALL_DIR) { $env:WR_INSTALL_DIR } else { Join-Path $HOME ".local\bin" }
$binary = Join-Path $dest "wr.exe"
if (Test-Path $binary -PathType Leaf) {
    Remove-Item -Force $binary
    Write-Output "removed $binary"
} else {
    Write-Output "wr is not installed at $binary"
}
