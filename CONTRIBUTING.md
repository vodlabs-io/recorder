# Contributing

Thanks for helping. Bug reports, fixes and improvements are all welcome.

- **Found a bug?** Open an [issue](https://github.com/vodlabs-io/recorder/issues/new/choose).
  **About → Report a bug** in the app fills in your versions for you.
- **A platform stopped working?** Use the "A platform stopped working" form.
  Platform support lives in the VodLabs service as well as in the app, so most
  of these are fixed on the service side, without a new release.
- **Have a question or an idea?** Start a
  [discussion](https://github.com/vodlabs-io/recorder/discussions).
- **Found a security problem?** Please report it privately, as
  [SECURITY.md](SECURITY.md) describes, not in an issue.

For a larger change, open an issue or a discussion first, so we can agree on
the approach before you spend time on it.

## The contributor license agreement

Before your first pull request is merged, you sign the
[contributor license agreement](CLA.md). It lets VodLabs offer the recorder both under its
[noncommercial license](LICENSING.md) and under commercial licenses. A bot
asks you to sign it on the pull request; it takes a minute, once.

## What can't be contributed here

- **New platforms.** Checking whether a streamer is live happens in the
  VodLabs service, which is not open source, so a platform can't be added from
  this repository alone. Open an issue to ask for one.
- **Anything that sends more data to VodLabs**, or that weakens the rules in
  [docs/data.md](docs/data.md).

## Setting up

You need [Go](https://go.dev/dl/) (the toolchain `go.mod` names is fetched
automatically) and [Bun](https://bun.sh). On Windows, run the scripts from Git
Bash.

```sh
./scripts/build.sh          # downloads the pinned FFmpeg, builds ./vlrecd and ./vod
bun install
```

Builds from source talk to the production VodLabs service, the same as the
released app. Please be gentle with it: no load tests or scripted floods.

## Running the desktop app

A development build of the app never starts a daemon itself. It waits,
showing a banner, until one is running, so you always test the daemon you
built. In one terminal:

```sh
bun run daemon --logger.mode console   # rebuilds, then runs ./vlrecd, logging here
```

and in another:

```sh
bun run dev:hmr             # the app; interface changes appear instantly
bun run start               # a one-off build and launch, no watching
bun run dev                 # rebuilds and relaunches on changes to desktop/src/bun
bun run build:stable        # a release build, in ./artifacts
```

`bun run dev:hmr` serves the interface from Vite on `http://127.0.0.1:5173`,
and the app window loads it from there instead of from the bundle.

Only one daemon can run at a time, so quit the installed app first. The
daemon you build uses your normal settings, streamers and recordings. To keep
them apart, point it at other folders:

```sh
export VLREC_CONFIG_DIR=~/vodlabs-dev/config VLREC_CACHE_DIR=~/vodlabs-dev/cache
```

Set `VLREC_DEV_SPAWN_DAEMON=1` to test how a release build finds or starts its
daemon: it looks at `VLREC_DAEMON_PATH`, then `vlrecd` on your `PATH`, then its
download cache, and downloads the latest release as a last resort.

## Running the Web UI

The Web UI is the same interface, built for the browser, which the daemon
serves when `web.enabled` is on (the default only in Docker). With a daemon
running from the terminal as above:

```sh
VLREC_WEB_ENABLED=1 bun run daemon --logger.mode console
bun run web                 # Vite on http://127.0.0.1:5174, proxying the daemon on :8080
VLREC_WEB_ENABLED=1 ./vod web url   # a sign-in link; open it, then switch the port to 5174
```

`bun run web:build` builds `web/dist`, which `go build` embeds; a daemon
built without it serves a page saying so. `scripts/docker-build.sh` builds
the image, and `bun scripts/docker-smoke.ts <image>` runs the checks CI runs
on it.

## Before you open a pull request

Run the checks CI runs:

```sh
go vet ./... && go test ./...
bun test
bunx tsc --noEmit
```

Some tests exercise the network rules in `internal/cloud`; none need a live
stream or an account.

- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org):
  `fix(session): keep recording across a codec change`. Pull requests are
  squashed, so the pull request's title becomes the commit message.
- **Go code** is formatted with `gofmt`.
- **Comments** say what the code does and why, not what it used to do.
- **Dependencies:** after adding or updating one, run
  `bun scripts/third-party-notices.ts` and commit the updated
  `THIRD_PARTY_NOTICES.md`.
