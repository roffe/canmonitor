$ErrorActionPreference = "Stop"
$root = (Get-Location).Path
$env:PATH += ";$root\llvm-mingw\bin"
$env:CGO_ENABLED = "1"
$env:GOOS = "windows"

# Kvaser ships both arches as "canlib32.dll" in layouts that have moved between
# SDK versions, so pick by PE machine type instead of trusting a path.
function Get-PEMachine([string]$path) {
    $bytes = [IO.File]::ReadAllBytes($path)
    $peOffset = [BitConverter]::ToInt32($bytes, 0x3C)
    return [BitConverter]::ToUInt16($bytes, $peOffset + 4)
}

$canlibDlls = Get-ChildItem 'C:\Program Files (x86)\Kvaser\Canlib' -Recurse -Filter canlib32.dll |
    Where-Object { $_.Directory.Name -ne 'Debug' }
Write-Host "canlib32.dll candidates:"
$canlibDlls | ForEach-Object { Write-Host ("  {0} machine=0x{1:x}" -f $_.FullName, (Get-PEMachine $_.FullName)) }

$targets = @(
    @{ goarch = "amd64"; cc = "x86_64-w64-mingw32-clang"; triplet = "x64-windows"; canusb = "canusb\dll64\canusbdrv64.dll"; machine = 0x8664; zip = "canmonitor_win64.zip" }
    @{ goarch = "386";   cc = "i686-w64-mingw32-clang";   triplet = "x86-windows"; canusb = "canusb\dll\canusbdrv.dll";     machine = 0x014c; zip = "canmonitor_win32.zip" }
)

foreach ($t in $targets) {
    Write-Host "=== Building $($t.goarch) ==="
    $vcpkg = "$root\vcpkg\packages\libusb_$($t.triplet)"

    $env:GOARCH = $t.goarch
    $env:CC = "$($t.cc).exe"
    $env:CXX = "$($t.cc)++.exe"
    $env:PKG_CONFIG_PATH = "$vcpkg\lib\pkgconfig"
    $env:CGO_CFLAGS = "-I$vcpkg\include\libusb-1.0"

    Remove-Item .\canmonitor.exe -ErrorAction SilentlyContinue
    fyne package -tags="canlib,canusb,combi,ftdi,j2534,pcan,rcan" --release
    if ($LASTEXITCODE -ne 0) { throw "fyne package failed for $($t.goarch)" }

    $canlib = $canlibDlls | Where-Object { (Get-PEMachine $_.FullName) -eq $t.machine } | Select-Object -First 1
    if (-not $canlib) { throw "no canlib32.dll for $($t.goarch)" }

    # Stage so everything lands flat at the zip root.
    $stage = "$root\dist\$($t.goarch)"
    Remove-Item $stage -Recurse -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Path $stage -Force | Out-Null
    Copy-Item .\canmonitor.exe, $canlib.FullName, $t.canusb, "$vcpkg\bin\libusb-1.0.dll" -Destination $stage

    7z a $t.zip "$stage\*"
    if ($LASTEXITCODE -ne 0) { throw "7z failed for $($t.goarch)" }
}
