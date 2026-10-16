# What the recorder sends to VodLabs

## The short version

- Recording happens on your computer. Video goes straight from the platform to
  your disk, and VodLabs never receives it.
- Your platform logins stay on your computer, encrypted.
- The VodLabs service tells your recorder when the streamers you follow go
  live. To do that, it knows which streamers you follow.
- The service is free. In return, your recorder runs some live checks for
  other users, from your IP address.

The rest of this page says exactly what that means, with links to the code
that does it, so you can check it for yourself.

## What stays on your device

- **Your recordings**, and the names and paths of your folders.
- **Your platform logins.** They are encrypted with a key kept in your
  system's keychain (the macOS Keychain, Windows Credential Manager, or the
  Secret Service on Linux). On a system without a keychain, the key is derived
  from your machine's ID and your home folder, plus a value published in this
  code. That keeps the file from being read after it's copied to another
  machine, but it does not protect against someone who can run programs as
  you. ([internal/platformauth/store.go](../internal/platformauth/store.go))
- **Your settings, disk usage and logs.** The logs stay in your cache folder
  until you send them to someone yourself.

## What the service receives, and how long it keeps it

| What | Why | Kept for |
| --- | --- | --- |
| A client ID: the public half of a key your recorder generates for itself. It isn't linked to your name or email. | To tell recorders apart, and to verify that requests are really from yours. | Until your recorder has been out of touch for 90 days. |
| The streamers you follow, as platform and username. | To tell you when they go live. ([subscribe.go](../internal/cloud/subscribe.go), [status.go](../internal/cloud/status.go)) | The same. |
| Your app version. | To tell you about updates, and to stop serving versions that no longer work. ([heartbeat.go](../internal/cloud/heartbeat.go)) | The same. |
| Your IP address. | To limit how much one address can use the free service, and for shared checks (below). | 30 days after your recorder was last connected. |
| That a recording of a streamer stalled. | So the service can check the stream again and give your recorder a fresh link. ([lag.go](../internal/cloud/lag.go)) | Not stored with your client. |

The service's logs, which can include IP addresses and streamer names, are
overwritten as they fill up. Encrypted backups of the service's database are
kept for up to three months. The service runs behind Cloudflare, which
handles the connection to it.

Whether a streamer is live, and their profile picture, are shared with
everyone who follows that streamer: that is public information on the
platform.

## Requests your recorder makes for the service

Some checks can only be done from your computer:

- **Stream links are tied to the address that asks for them.** A link the
  service fetched from its own address often would not play from yours. So
  when your recorder asks about a streamer, for example when you add one or a
  recording is about to start, the service usually has your recorder ask the
  platform directly.
- **Signed-in checks need your login**, which only your computer has.

For these, the service sends your recorder a request to make, and your
recorder sends back the result. The recorder enforces these rules itself, so
they hold whatever the service asks
([scrape_guard.go](../internal/cloud/scrape_guard.go),
[scraper.go](../internal/cloud/scraper.go)):

- **Only the platforms' own sites.** Each platform has a fixed list of hosts,
  built into the app. A request to any other host is refused, and so is a
  redirect that leads off the list.
- **Never your own network.** Addresses on your computer or local network
  (loopback, private, link-local and carrier-grade NAT ranges) are refused,
  including names that resolve to them, checked at the moment of connecting.
- **Only GET and POST requests**, at most 130 a minute, one at a time.
- **Only what the check needs comes back.** The reply carries just the
  response headers checks read, never `Set-Cookie`. For a signed-in check, the
  request must name the fields it needs, and your recorder extracts them
  before replying, so the rest of the page, including your own account
  details, stays on your computer.
- **Your login is attached only to its own platform.** The recorder adds your
  cookies only to requests for that platform's own domains. The service asks
  for a signed-in check only when your recorder asks about a streamer you
  follow, and sends the result only to you.

Stream links and headers from checks your recorder made are never shared with
other users.

## Shared checks: what the free plan asks in return

Knowing when thousands of streamers go live takes a great many requests to
the platforms. So that the free service stays affordable, your recorder also
runs some of these checks for streamers that other users follow. In practice:

- Platforms see those requests coming from your IP address, the same as the
  checks for your own streamers.
- The same rules apply to them as above: platform sites only, never your
  network, and a limit on how many.
- Your logins are never used for them.
- While your recorder is connected, the service keeps your IP address in the
  list of addresses it can run checks from, and removes it when you
  disconnect.

A paid plan will be able to turn shared checks off. The free service refuses
recorders that turn them off.

## Verify it yourself

- **Read and build the code.** Everything on this page is in this repository,
  and the [README](../README.md) shows how to build the app from source.
- **Check a download.** Every release file comes with a build provenance
  attestation, which proves it was built from this repository by its release
  workflow: `gh attestation verify <file> --repo vodlabs-io/recorder`.
- **Run the tests that guard each rule.**
  - Only platform sites, never your network:
    [scrape_guard_test.go](../internal/cloud/scrape_guard_test.go)
  - No `Set-Cookie`, only the headers checks read:
    [reply_headers_test.go](../internal/cloud/reply_headers_test.go)
  - Signed-in pages are reduced to the fields asked for:
    [browser_body_test.go](../internal/cloud/browser_body_test.go)
  - Logins are attached only when asked, and only to their platform's
    domains: [scraper_auth_test.go](../internal/cloud/scraper_auth_test.go),
    [platforms_login_test.go](../internal/vlconfig/platforms_login_test.go)
  - The request budget: [scraper_ratelimit_test.go](../internal/cloud/scraper_ratelimit_test.go)
  - Logins are encrypted on disk:
    [store_test.go](../internal/platformauth/store_test.go)
  - The client key is generated on your computer:
    [register_key_test.go](../internal/cloud/register_key_test.go)

## Deleting your data

When you stop using the recorder, the service deletes your client record, and
the streamer list that goes with it, once your recorder has been out of touch
for 90 days. IP addresses go after 30. A button to delete it all at once is
planned. For anything else about your data, email dev@vodlabs.io.
