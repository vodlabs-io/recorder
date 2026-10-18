# Privacy policy

Last updated 18 October 2026

This policy covers the vodlabs.io website, the VodLabs Recorder app and its
`vod` command-line tool, the VodLabs service the recorder connects to,
VodLabs accounts (auth.vodlabs.io), and the hosted dashboard
(dashboard.vodlabs.io). They are run by VODLABS LLC ("VodLabs", "we").

It is published from [docs/privacy.md](privacy.md) in the recorder's source
repository, and vodlabs.io/privacy shows the same text.

## The short version

- Recording happens on your computer. Video goes straight from the platform to
  your disk, and VodLabs never receives it.
- Your platform logins stay on your computer, encrypted. We never receive them.
- The VodLabs service tells your recorder when the streamers you follow go
  live. To do that, it knows which streamers you follow.
- The service is free. In return, your recorder runs some live checks for
  other users, from your IP address.
- A VodLabs account is optional. If you make one, with an email address or by
  signing in with Google or Apple, we keep your email address and name, and use
  them only to run your account.
- We don't sell your data, and we don't use it for advertising.

The recorder sections below say exactly what the recorder sends, with links to
the code that does it, so you can check it for yourself.

## What stays on your device

- **Your recordings**, and the names and paths of your folders. If you link
  your recorder to an account, the dashboard shows you your recording list and
  settings (see [Linking your recorder](#linking-your-recorder)), but the
  recordings themselves stay on your computer.
- **Your platform logins.** They are encrypted with a key kept in your
  system's keychain (the macOS Keychain, Windows Credential Manager, or the
  Secret Service on Linux). On a system without a keychain, the key is derived
  from your machine's ID and your home folder, plus a value published in this
  code. That keeps the file from being read after it's copied to another
  machine, but it does not protect against someone who can run programs as
  you. ([internal/platformauth/store.go](../internal/platformauth/store.go))
- **Your settings, disk usage and logs.** The logs stay in your cache folder
  until you send them to someone yourself.

## What the service receives from your recorder

| What | Why | Kept for |
| --- | --- | --- |
| A client ID: the public half of a key your recorder generates for itself. Unless you link the recorder to an account, it isn't linked to your name or email. | To tell recorders apart, and to verify that requests are really from yours. | Until your recorder has been out of touch for 90 days. |
| The streamers you follow, as platform and username, and any link you paste to add one. | To tell you when they go live, and to work out which streamer a link names. ([subscribe.go](../internal/cloud/subscribe.go), [status.go](../internal/cloud/status.go), [preflight.go](../internal/cloud/preflight.go)) | The same. |
| Your app version, operating system and CPU type, and whether the app or `vod` started the recorder. | To tell you about updates, and to stop serving versions that no longer work. ([heartbeat.go](../internal/cloud/heartbeat.go), [useragent.go](../internal/cloud/useragent.go)) | The same. |
| Your IP address. | To limit how much one address can use the free service, and for shared checks (below). | 30 days after your recorder was last connected. |
| That a recording of a streamer stalled. | So the service can check the stream again and give your recorder a fresh link. ([lag.go](../internal/cloud/lag.go)) | Not stored with your client. |

The service's logs, which can include IP addresses and streamer names, are
overwritten as they fill up. Encrypted backups of the service's database are
kept for up to three months. The service runs behind Cloudflare, which
handles the connection to it.

Whether a streamer is live, and their profile picture, are shared with
everyone who follows that streamer: that is public information on the
platform.

## Other connections the recorder makes

- **The platforms**, to record. On some platforms the recorder joins a
  broadcast as a guest viewer, from your IP address.
- **GitHub**, to download the recorder daemon and updates.
- **VodLabs' image server**, for streamers' profile pictures.

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

All live status checks, including the shared ones, are recorded in scraper.log.

## VodLabs accounts

You don't need an account to use the recorder. An account lets you link your
recorder and control it from a web browser at dashboard.vodlabs.io. You can
create one at auth.vodlabs.io with an emailed code, an email address and
password, Google, or Apple.

For your account, we keep:

- your email address, and whether you have confirmed it;
- your name and profile picture, if you give them or your Google or Apple
  account provides them;
- a hash of your password, if you set one (never the password itself);
- which sign-in methods you use, and the ID Google or Apple gives your account
  there;
- your signed-in sessions, each with the IP address and browser it was started
  from;
- when the account was created and last changed.

We use this to sign you in, keep your account secure, show which account
you're signed in as, and send you emails about your account, such as sign-in
codes. We don't send marketing email.

To protect accounts, the sign-up and sign-in forms use Cloudflare Turnstile
to tell people from bots. When you set a password, we check it against
[Have I Been Pwned](https://haveibeenpwned.com/Passwords)'s list of leaked
passwords: only the first five characters of the password's hash leave our
service, so the password itself is never sent.

### Signing in with Google

When you sign in with Google, we ask Google only for your basic profile: your
name, email address, and profile picture (the `openid`, `email` and `profile`
scopes). We don't ask for access to your Gmail, Drive, contacts, calendar, or
any other Google data.

We use what Google provides only to create your account, sign you in, and
show who you're signed in as. The tokens Google issues at sign-in are stored
encrypted and used for nothing else. We don't sell this data, share it with
anyone except the service providers listed below that help us run VodLabs,
use it for advertising, or use it to train AI models. People at VodLabs read
it only when you ask us to, to keep VodLabs secure, or when the law requires.

VodLabs' use and transfer of information received from Google APIs adheres to
the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy),
including the Limited Use requirements.

You can remove VodLabs' access at any time from your Google Account's
[third-party connections](https://myaccount.google.com/connections) page.
That stops Google sign-in for VodLabs; to delete the account itself, see
[Deleting your data](#deleting-your-data).

### Signing in with Apple

When you sign in with Apple, Apple gives us your email address and, the first
time, the name you choose to share. If you choose Hide My Email, we receive an
address at privaterelay.appleid.com, and emails we send you go through Apple's
relay; we never see your real address. We use what Apple provides the same way
as Google's above, and nothing else.

You can stop using Sign in with Apple for VodLabs in your Apple Account
settings, under Sign in with Apple. To delete the account itself, see
[Deleting your data](#deleting-your-data).

### Linking your recorder

To link, your recorder shows a code, and you confirm it on the dashboard while
signed in. For the few minutes the code is valid, the service holds it with
your recorder's client ID, computer name and app version, so the dashboard can
show you which recorder you are linking. Once linked, the service keeps:

- the link between your account and your recorder's client ID;
- your account's email address, to show in the recorder which account it is
  linked to;
- if you have the service record streamers in the cloud for you, the list of
  those streamers.

While you use the dashboard, it shows what the recorder's own screens show:
your streamers, your recording list, your settings and the recorder's status.
That passes from your recorder through the service to your browser as you use
it, and the service doesn't keep it. Your recordings and platform logins are
never sent: the dashboard lists recordings but can't play or download them.

You can unlink at any time, from the recorder or the dashboard.

## This website, the dashboard and sign-in pages

vodlabs.io sets no cookies and runs no analytics. auth.vodlabs.io and
dashboard.vodlabs.io set only the cookies and browser storage needed to keep
you signed in. All three are served through Cloudflare, which processes your
IP address and browser details to deliver them and protect them from attacks.
If you email us, we keep your message and address to reply.

## How long we keep it

- Your recorder's client record and streamer list: until your recorder has
  been out of touch for 90 days.
- IP addresses the service sees from your recorder: 30 days after your
  recorder was last connected.
- Your account: until you delete it.
- Signed-in sessions, with their IP address and browser: until you sign out,
  or 30 days after you last used them.
- Sign-in and link codes: until used, or a few minutes at most.
- Service logs, which can include IP addresses and streamer names: overwritten
  as they fill up.
- Backups: the account database's for 30 days, and encrypted backups of the
  service's database for up to three months.

## Who else handles it

We use these providers to run VodLabs. They process data only to provide
their services to us:

- **Cloudflare**, for our network, the website, the dashboard, the account
  service and its database, the emails we send, and Turnstile.
- **Hosting and backup storage providers**, for the VodLabs service.
- **Google and Apple**, if you sign in with them. Their own privacy policies
  cover what they do when you use them to sign in.

We don't sell or rent personal data, and we disclose it only when the law
requires.

## Legal bases

Where data protection law such as the GDPR applies, we process this data to
provide the service and account you ask for, and for our legitimate interest
in keeping them secure, fair and free of abuse.

## Your choices and rights

You can stop using the recorder at any time; its data is then deleted on the
schedule above. You may also ask us to access, correct or delete your data, or
object to how we use it, by emailing [dev@vodlabs.io](mailto:dev@vodlabs.io).
Without an account, the service doesn't know who you are, so we may ask for
details, such as the streamers you follow, to find your data. You may also
complain to your local data protection authority.

## Deleting your data

- **Without an account:** when you stop using the recorder, the service
  deletes your client record, and the streamer list that goes with it, once
  your recorder has been out of touch for 90 days. IP addresses go after 30.
- **With an account:** email [dev@vodlabs.io](mailto:dev@vodlabs.io) from the
  account's address, and we will delete the account, its sign-in methods and
  sessions, and unlink your recorder. Deleted data leaves our backups within
  the times above.

A button to delete it all at once, in the recorder and on the dashboard, is
planned.

## Age requirement

You must be at least 18 to use VodLabs Recorder, VodLabs accounts, or any
other VodLabs software or service. We don't knowingly collect data from anyone
under 18, and we delete it if we learn we have.

## Changes

If we change this policy, we'll update the date at the top. If a change is
significant, we'll say so in the app or on this site. Every past version is in
this file's history in the recorder's source repository.

## Verify it yourself

- **Read and build the code.** Everything this policy says about the recorder
  is in its repository, and the [README](../README.md) shows how to build the
  app from source.
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

## Contact

VODLABS LLC, [dev@vodlabs.io](mailto:dev@vodlabs.io).
