# Using the recorder from an AI agent or a script

This page is for anything that runs `vod` without a person at a terminal: an AI
agent, a cron job, a CI step. For every command, see the
[command-line reference](cli.md).

## What it needs

- **Outbound HTTPS and WebSockets** to `vodlabs.io` and `github.com` for the
  install, to `api.vodlabs.io` and `nats.vodlabs.io` for the service, and to the
  streaming platform and its video hosts. Status checks and recordings run from
  this machine, so a sandbox that filters outbound traffic often fails here, with
  errors such as `connection reset by peer`.
- **A machine that outlives the broadcast.** Recordings are ordinary files on
  this machine's disk. In a sandbox that is deleted after the task, copy them out
  before it ends.

## Install

```sh
curl -fsSL https://vodlabs.io/install.sh | sh
```

This puts `vod` in `~/.local/bin` with no admin rights. `vod` downloads and starts
the daemon the first time it needs it. No account is needed.

Use a release rather than `go build`, `go run`, a source archive or a shallow
clone. Those have no version number, and the VodLabs service refuses a build
without one.

## Check this machine can record

```sh
vod doctor --json twitch                     # the platform
vod doctor --json twitch example             # a streamer you track: a real check
```

This checks the daemon, its connection to the service, whether the service offers
the platform, whether this machine reaches the platform's site, and, given a
streamer this recorder tracks, runs a real status check from this machine.
`vod record --once` runs the same check for its own streamer as soon as it starts,
and ends with `unreachable` if this machine cannot reach the platform. It exits with 1 if any check
fails, and each failed check says why.

If the platform is not offered, it can't be recorded: `vod platforms --json` lists
the ones that are. Tell the user. Adding it to `streamers.toml` by hand doesn't
help, since the service rejects it.

A network that filters by host name can let the site through and still block the
hosts a check uses, so the status check is the one to trust.

## Record

```sh
vod record --once --wait 30m --duration 1h --json https://www.twitch.tv/example
```

| Flag | What it does |
| --- | --- |
| `--once` | Record one broadcast, then stop tracking the streamer. Required by the flags below. |
| `--wait 30m` | How long to wait for the stream to go live. 15 seconds by default. |
| `--duration 1h` | Stop the recording after this long. By default it runs until the stream ends. The file can run slightly longer, because some platforms start with footage they have buffered. |
| `--json` | Print one JSON result on stdout when it is over. Progress goes to stderr. |

The command blocks until the recording's files are final, so run it in the
background if your shell tool has a time limit:

```sh
vod record --once --duration 1h --json <url> > result.json 2> progress.log &
```

Sending it `SIGINT` or `SIGTERM` stops the recording and still reports what was
saved, waiting up to 30 seconds for the files.

### The result

```json
{
  "outcome": "recorded",
  "platform": "twitch",
  "username": "example",
  "live_status": "live",
  "session_id": "abc123",
  "state": "finished",
  "ready": true,
  "dir": "/home/me/Downloads/recordings/twitch/example/2026-11-05_12-00-00",
  "paths": ["/home/me/Downloads/recordings/twitch/example/2026-11-05_12-00-00/vod.mp4"],
  "files": [{ "name": "vod.mp4", "bytes": 104857600, "has_thumb": true }],
  "total_bytes": 104857600,
  "duration_sec": 600
}
```

| `outcome` | Exit code | Meaning |
| --- | --- | --- |
| `recorded` | 0 | The files are in `paths`. If `ready` is false, they are still being processed. |
| `not_live` | 2 | The stream did not go live within `--wait`. Nothing was recorded. |
| `unavailable` | 1 | The account is gone, restricted or otherwise can't be recorded; see `live_status`. |
| `unreachable` | 1 | This machine's own request to the platform failed, for example `connection reset by peer`; see `error`. Retrying from the same network won't help. |
| `failed` | 1 | The recording started and failed; see `error`. `paths` lists anything it kept. |
| `error` | 1 | It never started, for example the service refused the streamer; see `error`. |

`stopped` is true when `--duration` or a signal ended the recording. If `vod`
can't reach a daemon at all, it prints an error on stderr and exits with 1, with
no JSON.

### If it is killed

`SIGKILL` can't be handled. A `--wait` longer than 15 seconds tracks the streamer
until the command ends, so after a kill the rule stays, and the daemon keeps
recording that streamer. Remove it with:

```sh
vod streamer remove <url>
```

A streamer you already track is refused with an error, so this command never
changes your existing rules. Its recordings show up in `vod vods list`.

## Look at what's running

None of these block or redraw when stdout is not a terminal:

```sh
vod status --json            # what is recording now
vod streamer list --json     # tracked streamers and their status
vod vods list --json         # finished recordings, with paths
vod health                   # exit 0 if the daemon is answering, 1 if not
vod quit                     # stop the daemon before the machine goes away
```

## Tell the user

- **Shared checks are on.** On the free plan, this recorder also checks streamers
  other people follow, from this machine's IP address. Platform logins are never
  used for those. See [what the recorder sends to VodLabs](data.md).
- **Recordings are other people's broadcasts.** They should respect the creators'
  rights and each platform's terms when they keep or share them.
- **Some streams need an account.** `vod login <platform> --cookies-file
  cookies.txt` stores a login, on this machine only. Use it only with cookies the
  user gave you for this. See [signing in to platforms](platform-sign-in.md).
