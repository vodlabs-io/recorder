# recorder

A tool for automatically monitoring and recording live streams from various social media platforms. Includes a desktop app and a CLI, both driving the same recording daemon.

## Install the CLI

macOS and Linux:

```sh
curl -fsSL https://vodlabs.io/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://vodlabs.io/install.ps1 | iex
```

Both install `vod` for your user only, so no admin rights are needed: to
`~/.local/bin` on macOS and Linux, or `%LOCALAPPDATA%\Programs\VodLabs\bin` on
Windows, which the installer adds to your Path. `vod` downloads the recorder
daemon matching its version the first time it needs one, and `vod update`
upgrades the CLI. To choose the version or the directory, set `VOD_VERSION` or
`VOD_INSTALL_DIR`:

```sh
curl -fsSL https://vodlabs.io/install.sh | VOD_VERSION=0.1.2 VOD_INSTALL_DIR="$HOME/bin" sh
```

```powershell
$env:VOD_VERSION = '0.1.2'; irm https://vodlabs.io/install.ps1 | iex
```

## Build from source

### Prerequisites

* Go 1.25.4 or newer

### Steps

```sh
# Clone the repository
git clone https://github.com/vodlabs-io/recorder.git

# Build the daemon and cli binaries
cd recorder
./scripts/build.sh

# Run the CLI
./vod --help
```

## CLI Usage

```sh
vod [global options] [command [command options]]
```

### Example Commands

```sh
# Record a TikTok live stream
vod record https://www.tiktok.com/@example/live

# Show current recording status
vod status

# Stop all recordings and end the daemon process
vod quit
```

## Desktop app

The desktop app (Electrobun + React) is a front end for the daemon, which it
talks to over NATS on the loopback interface. It is built from the root
`package.json`.

### JS Prerequisites

* [Bun](https://bun.sh)
* The daemon and CLI built (see [Build from source](#build-from-source))

### Desktop app

```sh
bun install

# Recommended for UI work: hot-reloads React changes instantly
bun run dev:hmr

# One-off build + launch, no watching
bun run start

# Watch mode: auto-rebuilds and relaunches on changes to the Bun main-process
# code (desktop/src/bun/**). Does not pick up ui/mainview (React) changes.
bun run dev

# Release build (stable channel; CI signs and notarizes the macOS one)
bun run build:stable
```

`bun run dev:hmr` runs a Vite dev server on `http://localhost:5173` (`bun run
hmr`) alongside `bun run start`. The Electrobun window detects the running
Vite server and loads the UI from it instead of the bundled assets, so React
edits appear instantly with no rebuild or relaunch.

#### Debugging the desktop app

A dev build (`bun run dev:hmr`, `dev`, `start`) never starts a daemon: it waits,
banner showing, until one is serving, so the daemon under test is always the one
you built and launched yourself (`./scripts/build.sh && ./vlrecd --logger.mode
console`). Set
`VLREC_DEV_SPAWN_DAEMON=1` to exercise the release behaviour instead — the app
connects to any daemon already running, and otherwise spawns one, checking
`VLREC_DAEMON_PATH`/`VLREC_DAEMON_BIN`, then `vlrecd` on `PATH`, then its own
download cache, before downloading the latest release as a last resort. Confirm
the connection with socket evidence, e.g.
`lsof -nP -iTCP:4222 -sTCP:ESTABLISHED`.

