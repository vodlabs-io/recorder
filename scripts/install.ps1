# Installs the VodLabs Recorder CLI (vod) on Windows. In PowerShell:
#
#   irm https://vodlabs.io/install.ps1 | iex
#
# Options, set as environment variables before running it:
#   $env:VOD_VERSION = '0.1.2'         release to install (default: latest)
#   $env:VOD_INSTALL_DIR = 'C:\tools'  where to put vod.exe
#                                      (default: %LOCALAPPDATA%\Programs\VodLabs\bin)
#
# vod fetches the matching recorder daemon (vlrecd.exe) itself when it first
# needs one, and `vod update` replaces vod.exe in place, so the install
# directory has to stay writable by you.
#
# Runs inside a script block and never calls `exit`: under `iex` that would
# close the user's PowerShell window.

& {
    $ErrorActionPreference = 'Stop'
    # Windows PowerShell 5.1 slows downloads to a crawl drawing its progress bar.
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $releases = 'https://github.com/vodlabs-io/assets/releases'
    $base = if ($env:VOD_VERSION) {
        "$releases/download/v$($env:VOD_VERSION.TrimStart('v'))"
    } else {
        "$releases/latest/download"
    }
    # Windows builds are x64 only; Windows on Arm runs them under emulation.
    $archive = 'cli_windows_amd64.zip'
    $dir = if ($env:VOD_INSTALL_DIR) {
        $env:VOD_INSTALL_DIR
    } else {
        Join-Path $env:LOCALAPPDATA 'Programs\VodLabs\bin'
    }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) "vod-install-$([Guid]::NewGuid())"
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $archive..."
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile "$tmp\$archive"
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile "$tmp\checksums.txt"

        $want = Get-Content "$tmp\checksums.txt" |
            ForEach-Object { $f = $_ -split '\s+'; if ($f[1] -eq $archive) { $f[0] } } |
            Select-Object -First 1
        if (-not $want) { throw "$archive is not listed in checksums.txt" }
        # -ne ignores case, so Get-FileHash's upper-case hex compares fine.
        if ((Get-FileHash -Algorithm SHA256 "$tmp\$archive").Hash -ne $want) {
            throw "checksum mismatch for $archive"
        }

        Expand-Archive -Path "$tmp\$archive" -DestinationPath $tmp -Force
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
        Move-Item -Path "$tmp\vod.exe" -Destination "$dir\vod.exe" -Force
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    Write-Host "Installed $(& "$dir\vod.exe" --version) to $dir\vod.exe"

    # Append to the user's Path through the registry, keeping it an expandable
    # string so %VAR% entries other tools put there still expand.
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        $userPath = [string]$key.GetValue('Path', '',
            [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = @($userPath -split ';' | Where-Object { $_ -ne '' })
        $added = $entries -notcontains $dir
        if ($added) {
            $key.SetValue('Path', (($entries + $dir) -join ';'),
                [Microsoft.Win32.RegistryValueKind]::ExpandString)
        }
    } finally {
        $key.Close()
    }
    if ($added) {
        # Setting a user variable through .NET broadcasts WM_SETTINGCHANGE, so
        # terminals opened from now on see the new Path.
        [Environment]::SetEnvironmentVariable('VODLABS_INSTALL', '1', 'User')
        [Environment]::SetEnvironmentVariable('VODLABS_INSTALL', $null, 'User')
        $env:Path = "$env:Path;$dir"
        Write-Host "Added $dir to your Path. Terminals that were already open need a restart to see it."
    }

    Write-Host ''
    Write-Host 'Get started: vod --help'
}
