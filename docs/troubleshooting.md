# Troubleshooting

When something goes wrong, start with the logs: **About → Open logs folder**
in the app, or the paths `vod info` prints. To report a bug, use **About →
Report a bug**, and paste in what **Copy diagnostics** puts on your clipboard
(versions, system, and the recorder's state; no logins).

## A stream went live but wasn't recorded

- **The app wasn't running**, or the computer was asleep. Nothing is recorded
  while the recorder is stopped. Turn on **Start at login** in
  **Settings → General** so a reboot doesn't stop it.
- **The streamer's recording is disabled.** The Dashboard shows
  *Recording Disabled*, or *Live (recording disabled)*. Turn on its
  **Record** switch, or run `vod streamer enable`.
- **The stream needs an account.** The Dashboard shows *Sign-in required* or
  *Restricted*. [Sign in to the platform](platform-sign-in.md), if it is one
  that supports it.
- **The download folder is full or missing.** The Dashboard says so, and
  **Settings → Files** is marked.

`vod streamer check <url>` asks for a streamer's status right away.

## "Can't reach the VodLabs service"

The recorder learns that streamers are live from the VodLabs service. While it
can't reach it, recordings already running carry on, but nothing is checked
and no new recording starts. It keeps retrying on its own. If your own
connection works, the service may be having an outage; it recovers without you
doing anything.

## "Update required"

The service has stopped serving this version. Open **Update**, or run
`vod update` if you use the command-line tool.

## The app doesn't open

**Windows:** if SmartScreen shows "Windows protected your PC", click **More
info**, then **Run anyway**. The app isn't code-signed for Windows yet.

**Linux:** an error about `libwebkit2gtk-4.1.so.0` or the app indicator means
a library is missing:

```sh
sudo apt install libwebkit2gtk-4.1-0 libayatana-appindicator3-1
```

**macOS:** the app is signed and notarized. If macOS says it is damaged or
can't be checked, download it again from the
[releases page](https://github.com/vodlabs-io/recorder/releases/latest).

## The recorder won't start: port 4222 is in use

The recorder daemon listens on `127.0.0.1:4222`, on this machine only. If
another program already uses that port, often another NATS server, the daemon
can't start. Stop the other program. For the command-line tool alone, you can
instead move the daemon to another port with `daemon.port` in `config.toml`;
the desktop app always looks for it on 4222.

## A recording has several files

A new file starts whenever the stream's video format changes, for example
when the streamer's encoder switches resolution, and at the size or length
limits set in [Configuration](configuration.md). Use **Merge parts…** in the
VOD Archive, or `vod merge <folder>`, to join them into one `vod.mp4`.
