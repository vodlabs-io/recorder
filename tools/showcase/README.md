# Showcase

Screenshots and recordings of the recorder, rendered from invented data so they
can be regenerated whenever the UI changes. Nothing here ships in a build.

| Piece | What it is |
|---|---|
| `demodaemon/` | The **Demo daemon**: a stand-in daemon that serves a Demo dataset over the daemon's own API (the real `vlapi` service and `vlstate` store, over dataset-backed controllers). The desktop app, `vod` and the harness connect to it as they would to `vlrecd`. All its state lives in a temp dir; it never touches a real daemon's config, streamers, logins or keychain. |
| `datasets/<name>/` | A **Demo dataset**: `dataset.toml` plus `images/avatars/` and `images/thumbs/`. |
| `harness/` | Renders **Shot lists** in Playwright's WebKit: the desktop app's React UI with the Demo daemon behind it, framed in drawn **Window chrome**. |
| `shots/*.yaml` | Shot lists: which **Scenes** to shoot, and how to compose them. |
| `terminal/` | The `vod status` recording (VHS). |
| `fetch-images.ts` | Downloads a dataset's images, which are kept out of git. |

Why a browser build and not the real app: the desktop app's WKWebView exposes
no DOM, so reaching a UI state would mean synthetic clicks at screen points,
which cannot run unattended. The real app stays the manual path, against the
same Demo daemon.

## Rendering

```sh
cd tools/showcase/harness && bun install && bunx playwright install webkit   # once
cd ../../..
bun tools/showcase/fetch-images.ts                                          # dataset images (needs B2 credentials, below)
bun tools/showcase/harness/render.ts tools/showcase/shots/marketing.yaml    # → tools/showcase/out/marketing/
tools/showcase/terminal/render-terminal.sh                                  # → tools/showcase/out/terminal/ (needs `brew install vhs`)
```

`render.ts` starts its own Demo daemon (frozen at the Shot list's `now`) and
dev server on private ports, so it runs beside anything else. Per Scene and
theme it writes `<scene>-<theme>.png` (window, chrome, shadow, transparent) and
`<scene>-<theme>.content.png` (the window's content alone); per composition,
`<name>-<theme>.png` and, with `backdrop: true`, `<name>-<theme>-backdrop.png`.
`--only a,b` renders a subset.

## Manual Shots

Run the Demo daemon and point the real app, or `vod`, at it:

```sh
go run ./tools/showcase/demodaemon                  # nats :14222, ws :14223, avatars :14224
VLREC_NATS_URL=nats://127.0.0.1:14222 bun run dev   # the desktop app (quit any running copy first)
printf '[daemon]\nport = 14222\nauto_start = false\n' > /tmp/vod-demo.toml
vod --config /tmp/vod-demo.toml status
```

Or open the harness in any browser: `cd tools/showcase/harness && bunx vite`,
then http://127.0.0.1:5175/#/dashboard.

Flags: `--dataset DIR`, `--now RFC3339` (what the dataset's offsets count back
from), `--freeze` (hold recording stats still), `--port`/`--ws-port`/`--http-port`.

## Shot lists

```yaml
dataset: default                 # datasets/<name>
now: 2026-09-24T20:30:00-07:00   # frozen clock for the page and the daemon
timezone: America/Los_Angeles
defaults:
  size: [1280, 800]              # whole window in points, title bar included
  scale: 2
  themes: [light, dark]
  prefs: { dashboard_page_size: 20 }   # the app's localStorage preferences
scenes:
  - name: dashboard
    route: /dashboard            # the app's hash route
    steps:                       # run in order before the Shot
      - wait: text=lanternfox_tv # Playwright selector, or milliseconds
      - click: role=button[name="Status"]
      - hover: text=Juniper
      - press: Escape
      - scroll: { selector: main, y: 400 }
      - eval: document.querySelector("main").scrollTo(0, 0)
      - script: ./my-step.ts     # default export: async (page) => {}
compositions:
  - name: hero
    windows:                     # back to front; `at` is the top-left in points
      - { scene: dashboard, at: [0, 0] }
      - { scene: vod-archive, at: [440, 350] }
    backdrop: true
```

## Demo datasets

Every time in `dataset.toml` is an offset back from "now" (`3d4h`, `36m`), so a
dataset always looks current. Sizes are binary (`4.9GB` is 4.9 GiB).

Top level: `version` (daemon and app version shown), `uptime`, `download_dir`,
`archive_dir`, and `[[root]]` Storage roots (`paths`, `download`, `total`,
`free`).

`[[streamer]]`: `platform`, `username`, `display_name`, `status` (a LiveStatus;
default `offline`), `record` (default true), `bitrate` (`6Mbps`; how fast a
recording grows and what its VODs weigh), `added`, `avatar_prompt`, `verified`.
Live only: `live_for`, `title`, `category`, `session` (`recording`, `lagging`,
`paused`). Offline: `last_live`, `last_live_duration`.

`[[vod]]`: `id` (stable: it names the thumbnails and seeds the session ID),
`streamer` (`platform/username`), `ago`, `duration`, `size` (default: the
Streamer's bitrate over the duration), `files` (1 → `vod.mp4`, n →
`vod1…n.mp4`), `state` (`ready`, `processing`, `moving`, `encoding`), `reason`
(a stalled move's cause), `percent` (encode progress), `folder` (`download`,
`archive`), `titles`.

Images: `images/avatars/<platform>-<username>.jpg` (512²) and
`images/thumbs/<id>.jpg`, `<id>-2.jpg`, … (1024×576; a file without its own
thumbnail falls back to the first). A missing avatar shows initials; a missing
thumbnail, the placeholder.

The images themselves are not in git. `images.lock` lists each one with its
SHA-256, and `fetch-images.ts` downloads them from a private B2 bucket, where
each is stored under its hash. It reads `B2_APPLICATION_KEY_ID` and
`B2_APPLICATION_KEY` from the environment (or `--key-file`, a file of
`KEY=VALUE` lines). Without them it only warns, and Shots render with initials
and placeholders.

Rules for a dataset:

- Usernames are invented and confirmed absent from their Platform; `verified`
  records when.
- Avatars are generated and stylized, not photoreal; `avatar_prompt` records
  the prompt.
- Thumbnails are live-stream frames that show no identifiable face:
  full-screen games without a face cam, animal and nature cams, hands-only
  cooking, driving and flight sims, science feeds. Each VOD's titles fit its
  frame.
