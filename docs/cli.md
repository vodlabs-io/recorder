# Command-line reference

`vod` controls the recorder daemon, `vlrecd`, which does the recording. If no
daemon is running, `vod` starts one. The desktop app talks to the same daemon,
so changes made with `vod` show up in the app and the other way round.

Every command takes `--help`. Most listing commands take `--json` for output
you can script against.

```text
vod [--config <file>] <command> [options]
```

`--config`, or `-c`, points at a config file other than the
[default one](configuration.md).

## Recording

| Command | What it does |
| --- | --- |
| `vod record <url>`<br>`vod record <platform> <username>` | Track a streamer, record them whenever they are live, and follow their status. Aliases: `rec`, `r`. |
| `vod record --once <url>` | Record one broadcast, then stop tracking the streamer. |
| `vod record --variant 720p <url>` | Record at a given quality instead of the default. |
| `vod status` | Show what is being recorded, with live statistics. |
| `vod status filter -p <platform> [-u <username>]` | The same, for one platform or streamer. |

## Streamers

| Command | What it does |
| --- | --- |
| `vod streamer add <url>`<br>`vod streamer add <platform> <username>` | Track a streamer without following its status. Takes `--variant`. Alias: `str`. |
| `vod streamer list` | List tracked streamers, their status and settings. `-p <platform>` filters. |
| `vod streamer remove <url>` | Stop tracking a streamer. Alias: `rm`. |
| `vod streamer disable <url>` | Stop recording a streamer, but keep watching whether they are live. |
| `vod streamer enable <url>` | Record a disabled streamer again. |
| `vod streamer check <url>` | Check a streamer's status now. |
| `vod streamer variant <url> [variant]` | Set a streamer's quality, or clear it when no variant is given. |
| `vod streamer config path`<br>`vod streamer config edit` | Print or edit the streamers file. |

Every command that takes `<url>` also accepts `<platform> <username>`.

## Recordings

| Command | What it does |
| --- | --- |
| `vod vods list` | List recordings, newest first. Filter with `-p <platform>` and `-u <username>`; `-n` sets how many (50 by default); `--rescan` re-reads the folders first; `--paths` prints only folder paths. Alias: `recordings`. |
| `vod merge <folder>` | Join a recording's files into one `vod.mp4`, or `vod.webm` when WebM parts can be joined without re-encoding. It is queued; `--keep-parts` keeps the originals as `.bak` files. |
| `vod queue list` | List merge jobs. |
| `vod queue cancel <job_id>`<br>`vod queue retry <job_id>`<br>`vod queue rm <job_id>` | Cancel, retry, or remove a job. |
| `vod queue pause`<br>`vod queue resume` | Pause the queue after the running job, or resume it. |
| `vod flv-fix <file.ts>…` | Repair raw FLV captures into clean files that convert to MP4 without re-encoding. `-o` names the output; `-v` logs each repair. |

## Platforms and sign-in

| Command | What it does |
| --- | --- |
| `vod platforms` | List the platforms the recorder supports. |
| `vod login <platform>` | Store a platform login from a pasted Cookie header, or from `--cookies-file cookies.txt`. See [Signing in to platforms](platform-sign-in.md). |
| `vod login list` | List stored logins. |
| `vod login remove <platform>` | Delete a stored login. |
| `vod cloud` | Show how often the VodLabs service is checking each platform. |

## Settings and the daemon

| Command | What it does |
| --- | --- |
| `vod config path`<br>`vod config edit` | Print or edit the config file. Alias: `cfg`. |
| `vod info` | Print the daemon's settings and the paths of its files and logs. |
| `vod health` | Exit with 0 if the daemon is running and answering, 1 if not. Never starts a daemon. For scripts and container health checks. |
| `vod web url`<br>`vod web token [--rotate]` | Print a sign-in link for the [Web UI](docker.md#signing-in), or its token. `--rotate` replaces the token and signs every browser out. |
| `vod update` | Update the daemon and `vod` to the latest release. |
| `vod restart` | Restart the daemon. |
| `vod quit` | Stop all recordings and the daemon. Finished recordings are processed first unless you pass `--force`. |
