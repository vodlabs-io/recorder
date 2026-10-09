# Installing

## The desktop app

Download the installer for your system from the
[latest release](https://github.com/vodlabs-io/recorder/releases/latest).
After that, the app updates itself: the **Update** page shows when a new
version is ready.

On first launch the app downloads the latest recorder daemon (`vlrecd`), so
it needs an internet connection the first time it starts. If you installed
the command-line tool, the app uses its `vlrecd` when that is newer.

### macOS

Download `VodLabs-Recorder-macOS-arm64.dmg` (Apple Silicon, M1 and later,
macOS 14 or later), open it, and drag **VodLabs Recorder** into
**Applications**. There is no desktop app for Intel Macs; the command-line
tool still runs on them.

The app is signed and notarized by Apple, so it opens without a warning.

### Windows

Requires 64-bit Windows 11. Windows 10 should also work, with the Microsoft
Edge WebView2 runtime installed, but is not yet tested. Extract
`VodLabs-Recorder-Windows-x64-Setup.zip` and run the Setup program inside it;
it needs the `.installer` folder next to it.

The installer is not yet code-signed, so Windows SmartScreen may show
"Windows protected your PC". Click **More info**, then **Run anyway**.

### Linux

Built for x64. Ubuntu 24.04 and later are supported (glibc 2.38 or newer). The app needs WebKitGTK
4.1 and the Ayatana app indicator library for its tray icon. On Ubuntu or
Debian:

```sh
sudo apt install libwebkit2gtk-4.1-0 libayatana-appindicator3-1
```

Then extract the installer and run it:

```sh
tar -xzf VodLabs-Recorder-Linux-x64-Setup.tar.gz
./installer
```

It installs the app under `~/.local/share/` and adds a desktop shortcut.

## The command-line tool

`vod` drives the same recorder as the desktop app, so you can use either, or
both. On macOS and Linux:

```sh
curl -fsSL https://vodlabs.io/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://vodlabs.io/install.ps1 | iex
```

Both install `vod` for your user only, with no admin rights needed: to
`~/.local/bin` on macOS and Linux, or to `%LOCALAPPDATA%\Programs\VodLabs\bin`
on Windows, which the installer adds to your `Path`. `vod` downloads the
recorder daemon the first time it needs it, and `vod update` updates both.

To choose the version or the folder, set `VOD_VERSION` or `VOD_INSTALL_DIR`:

```sh
curl -fsSL https://vodlabs.io/install.sh | VOD_VERSION=0.1.0 VOD_INSTALL_DIR="$HOME/bin" sh
```

```powershell
$env:VOD_VERSION = '0.1.0'; irm https://vodlabs.io/install.ps1 | iex
```

You can also download `cli_<os>_<arch>` and `daemon_<os>_<arch>` archives
from a release and put `vod` and `vlrecd` on your `PATH` yourself.

## Checking a download

Every release file is built by this repository's release workflow on GitHub,
which publishes a signed build provenance attestation for it. With the
[GitHub CLI](https://cli.github.com), you can confirm a file came from that
workflow and this source:

```sh
gh attestation verify VodLabs-Recorder-macOS-arm64.dmg --repo vodlabs-io/recorder
```

`checksums.txt` in each release lists the SHA-256 checksums of the
command-line archives.
