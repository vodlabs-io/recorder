# Signing in to platforms

Some streams can only be watched with an account: 18+ streams, subscriber-only
streams, or higher qualities that a platform keeps for signed-in viewers. To
record those, sign the recorder in to the platform with your own account.

[Supported platforms](platforms.md) marks the platforms that support sign-in.
You don't need it for anything else.

## What happens to your login

Your login stays on this device. The recorder stores it encrypted, in your
system keychain where one is available, and never sends it to VodLabs. It is
used only for requests your own recorder makes to that platform, and only for
the streamers you follow: the checks your recorder runs for other users never
use it. [What the recorder sends to VodLabs](data.md) explains how this is
enforced.

## In the desktop app

Open **Accounts** and click **Sign in** next to the platform. The platform's
own sign-in page opens in a window. Sign in as you normally would; the window
closes when you're done, and the platform shows as signed in.

The app keeps the login fresh in the background. If a platform signs you out
anyway, the account shows **Sign in again**.

**Sign out** deletes the stored login and the app's browsing data for that
platform.

## From the command line, or with cookies

If you can't use the sign-in window, you can give the recorder the cookies of
a browser that is signed in to the platform. In the app, use **Paste cookies**
on the Accounts page. With `vod`:

```sh
vod login tiktok                          # then paste a Cookie header
vod login youtube --cookies-file cookies.txt
vod login list
vod login remove tiktok
```

To copy a Cookie header, open the platform in your browser while signed in,
open the developer tools' **Network** tab, reload, select a request to the
platform's own site, and copy the value of its `Cookie` request header. A
`cookies.txt` file is the Netscape format that browser extensions for
exporting cookies produce.

A copied login signs the recorder in as you, and is as sensitive as your
password: don't share it or paste it anywhere else.
