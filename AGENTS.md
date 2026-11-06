# VodLabs Recorder

**Using the recorder to record a stream?** Read
[docs/agents.md](docs/agents.md). In short: install a release, then run

```sh
curl -fsSL https://vodlabs.io/install.sh | sh
vod platforms --json
vod record --once --wait 30m --duration 1h --json <url>
```

Don't build this repository to record something. A plain `go build`, a source
archive or a shallow clone has no version number, and the VodLabs service refuses
those builds.

**Changing the code?** Read [CONTRIBUTING.md](CONTRIBUTING.md). Run
`./scripts/build.sh` from a full clone, so the build carries the last release's
version.
