# Architecture

```text
your computer                                             VodLabs service
┌──────────────────────────────────┐   HTTPS: register,   ┌──────────────────────────────┐
│ desktop app ─┐                   │   subscribe, status  │ API: identity, subscriptions,│
│              ├─► vlrecd daemon ──┼─────────────────────►│      on-demand checks        │
│ vod CLI ─────┘   (127.0.0.1:4222)│                      │                              │
│   • records and processes video  │   WebSocket (NATS):  │ Notifications: tells your    │
│   • embedded FFmpeg              │◄─ go-live notices,   │   recorder who went live     │
│   • encrypted platform logins    │   relayed requests   │                              │
│   • runs relayed requests ───────┼─► and their replies ►│ Checkers: poll the streamers │
└───────────────┬──────────────────┘                      │   people follow              │
                │ video straight from the platform        └──────────────────────────────┘
                ▼
     TikTok · YouTube · Twitch · Kick · SOOP · CHZZK · PandaLive · Bigo · Douyin
```

## On your computer

**`vlrecd`, the daemon,** does all the work: it keeps the list of streamers,
talks to the VodLabs service, records, and processes recordings. It runs an
embedded [NATS](https://nats.io) server on `127.0.0.1:4222`, reachable only
from your own machine, which is how the other two parts talk to it. It embeds
a minimal [FFmpeg](https://github.com/vodlabs-io/custom-ffmpeg) build for
remuxing and merging.

**`vod`, the command-line tool,** and **the desktop app** are front ends to the
daemon. The desktop app is built with [Electrobun](https://electrobun.dev): a
Bun process that finds or starts the daemon, updates the app, and runs the
platform sign-in windows, plus a React interface. If no daemon is running,
either one starts it.

## The VodLabs service

The recorder can't tell on its own when a streamer goes live. The service
polls the platforms for every streamer its users follow and notifies each
recorder over a WebSocket when one of its streamers goes live, with a link to
the stream. The recorder then records straight from the platform. Video never
passes through VodLabs.

Some checks run on your recorder instead: signed-in checks, which need your
login, and most checks your recorder asks for itself, since stream links are
often tied to the IP address that requested them. The service sends those
requests over the same WebSocket, and your recorder makes them within fixed
rules and replies with only what the check needs. Your recorder also runs
some checks for other users, which is what the free plan asks in return.
[What the recorder sends to VodLabs](privacy.md) covers the rules and the
data.

## Where the code is

| Path | What it holds |
| --- | --- |
| `cmd/daemon`, `cmd/cli` | The `vlrecd` and `vod` entry points. |
| `internal/app` | Daemon start-up and the CLI's commands. |
| `internal/vlapi` | The daemon's local API, served over NATS to the app and `vod`. |
| `internal/cloud` | Everything that talks to the VodLabs service, including the rules for relayed requests (`scrape_guard.go`). |
| `internal/session`, `internal/recording`, `internal/writer`, `internal/stream` | Recording: following a live stream and writing it to disk. |
| `internal/flvfix` | Repairing unstable FLV streams while they record. |
| `internal/processing`, `internal/encoding`, `internal/ffmpeg` | Turning recordings into `vod.mp4`, and the merge queue. |
| `internal/vodindex`, `internal/vodmove`, `internal/vodmeta` | The VOD Archive, moving recordings to an archive folder, and what a platform said about each broadcast. |
| `internal/platformauth` | The encrypted store of platform logins. |
| `internal/vlconfig` | Settings, the streamers file, and the built-in platform list and rules. |
| `desktop/src/bun` | The desktop app's Bun process. |
| `ui/mainview` | The desktop app's React interface. |
| `shared` | TypeScript shared by the interface and the Bun process. |
| `scripts` | Build, install and maintenance scripts. |
| `patches` | Fixes applied to Electrobun, and why. |
