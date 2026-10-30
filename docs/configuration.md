# Configuration

Most settings are in the desktop app under **Settings**. They are saved to a
config file you can also edit by hand, and the command-line tool uses the same
files.

## Where everything is stored

| | macOS | Linux | Windows |
| --- | --- | --- | --- |
| Settings, streamers, logins | `~/Library/Application Support/vodlabs/recorder/` | `~/.config/vodlabs/recorder/` | `%AppData%\vodlabs\recorder\` |
| Logs and downloaded daemons | `~/Library/Caches/vodlabs/recorder/` | `~/.cache/vodlabs/recorder/` | `%LocalAppData%\vodlabs\recorder\` |
| Recordings | `~/Downloads` unless you choose another folder | the same | the same |

The settings folder holds:

- `config.toml`, the settings;
- `streamers.toml`, the streamers you track and their options;
- `processing.db`, the queue of recordings waiting to be processed or merged;
- your platform logins, encrypted.

In Docker, these are the `/config`, `/cache` and `/recordings` volumes; see
[Running in Docker](docker.md#folders).

`vod info`, and **About** in the app, print the exact paths on your machine.
To keep everything somewhere else, set `VLREC_CONFIG_DIR` and
`VLREC_CACHE_DIR` to the folders to use.

The recorder's identity with the VodLabs service, a key it generated for
itself, is kept in your system keychain, or in `~/.vodlabs-creds.json` where
there is no keychain.

## config.toml

The file is created on first run, with a comment above every setting. Open it
with `vod config edit`. The main settings:

| Setting | Default | What it does |
| --- | --- | --- |
| `session.download_dir` | your Downloads folder | Where recordings are written while they are captured. A local disk works best. |
| `session.output_template` | `{platform}/{username}/{datetime}` | The folder layout for each recording, inside the download folder. Other placeholders include `{date}`, `{time}`, `{year}` and `{timestamp}`. |
| `session.variant` | `best` | The quality to record: `best`, `worst`, or a named one such as `720p`. |
| `session.split_size`<br>`session.split_duration` | off | Start a new file at a size (`"2GB"`) or a length (`"1h30m"`). |
| `session.sanitize_flv` | `true` | Repair unstable FLV streams while recording. |
| `processing.archive_dir` | off | Move each finished recording to this folder, for example an external drive. |
| `processing.scan_dirs` | none | Extra folders, such as an old drive, to list in the VOD Archive. They are only read. |
| `processing.delete_part_files` | `true` | Delete a recording's parts once they are turned into the finished video. |
| `processing.encoder` | `auto` | The video encoder for merges: `auto` picks a working hardware encoder, `software` uses x264. |
| `processing.low_space_warn_gb` | `20` | Warn when a recording folder has less free space than this, in GiB. |
| `logger.level` | `info` | How much to log: `debug`, `info`, `warn` or `error`. |
| `error_reports.enabled` | not set (off) | Send error reports to VodLabs. The app asks once; change it in Settings › General or with `vod config error-reports on\|off`. See [What the recorder sends](data.md#error-reports-and-bug-reports). |

Environment variables override the file, for the daemon and for `vod` alike:
`VLREC_DOWNLOAD_DIR`, `VLREC_ARCHIVE_DIR`, `VLREC_OUTPUT_TEMPLATE`,
`VLREC_VARIANT`, `VLREC_SPLIT_SIZE`, `VLREC_SPLIT_DURATION`,
`VLREC_DELETE_PART_FILES`, `VLREC_ENCODER`, `VLREC_LOG_LEVEL`,
`VLREC_LOG_MODE`, `VLREC_DAEMON_HOST` and `VLREC_DAEMON_PORT`. The Web UI's
settings, under `[web]`, are described in [Running in Docker](docker.md#settings).

## streamers.toml

The app and `vod streamer` keep this file up to date, and it is safe to edit.
Streamers are listed by platform, or by URL:

```toml
[streamers]
twitch = ["example"]
urls = ["https://www.tiktok.com/@example/live"]
```

Rules give some streamers their own options:

```toml
[[rules]]
twitch = ["example"]
variant = "720p"
split_duration = "2h"
```

A rule can set `variant`, `output_template`, `split_size`, `split_duration`,
`archive_dir` and `delete_part_files`, plus:

- `record = false`, to keep watching whether a streamer is live but record
  nothing;
- `monitor = false`, to record the next broadcast only, then remove the rule.

Use `"off"` for `split_size`, `split_duration` or `archive_dir` to turn that
setting off for those streamers only.
