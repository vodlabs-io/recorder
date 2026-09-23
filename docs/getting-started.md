# Getting started

## The first launch

The first time the app opens, it asks where recordings should go (your
Downloads folder by default) and offers to start the app when you log in.
Starting at login is worth turning on: nothing is recorded while the app is
closed, so after a reboot, a stream that starts before you open the app again
is missed.

Closing the window keeps the app running in the system tray. To stop it,
choose **Quit** from the tray icon or the app menu.

## Add a streamer

On the **Dashboard**, click **Add Streamer** and paste a link to the
streamer's channel or live page, such as `https://www.twitch.tv/example`, or
pick the platform and type their username.

The Dashboard then shows whether each streamer is live. When one goes live,
recording starts on its own.

From the command line, the same thing is:

```sh
vod streamer add https://www.twitch.tv/example
vod status
```

`vod record <url>` adds a streamer and follows its status live, and
`vod record --once <url>` records a single broadcast and then stops following
the streamer.

## Find your recordings

Each broadcast is written to its own folder inside your download folder,
`<platform>/<username>/<date and time>` by default. While a stream is live it
is saved in parts. When the broadcast ends, the parts are turned into
`vod.mp4` (or `vod1.mp4`, `vod2.mp4`… when [splitting](configuration.md) is on),
and the parts are deleted.

The **VOD Archive** page lists every recording, newest first. Each one can be
opened in your video player or revealed in its folder. `vod vods list` prints
the same list.

If a broadcast was saved as several files, for example because the stream
changed resolution partway through, **Merge parts…** in its menu in the VOD
Archive (or `vod merge <folder>`) joins them into one `vod.mp4`. Merges run one at a time
on the **Encoding** page.

To move finished recordings off the download disk, set an **archive folder**
in **Settings → Files**. Each recording moves there once it is processed.

## Next

- [Sign in to a platform](platform-sign-in.md) to record streams that need an
  account.
- [Configuration](configuration.md) covers quality, splitting, folder layout
  and per-streamer options.
