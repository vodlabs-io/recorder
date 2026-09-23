# Security policy

## Reporting a vulnerability

Please report security problems privately, not in a public issue:

- on GitHub, with **Report a vulnerability** on this repository's
  [Security tab](https://github.com/vodlabs-io/recorder/security/advisories/new);
- or by email to dev@vodlabs.io.

Say what the problem is, how to reproduce it, and which version you tested.
We aim to reply within a week, and we'll tell you when a fix ships. We credit
reporters in the release notes unless you'd rather we didn't.

## In scope

The recorder runs requests on behalf of the VodLabs service and keeps your
platform logins, so these matter most:

- **Requests relayed from the service.** Anything that gets your recorder to
  reach a host outside a platform's list, to reach your own machine or local
  network, to send more than [docs/privacy.md](docs/privacy.md) describes, or
  to use your login on the wrong site.
- **Platform logins.** Reading them from disk or memory in a way the
  encryption should prevent, or sending them anywhere.
- **The daemon's local API** on `127.0.0.1:4222`, for example if another
  website or another user on the machine can drive it.
- **Updates and releases.** Anything that could get the app or the
  command-line tool to install something other than a release built here.

Problems in the VodLabs service itself are welcome through the same channels.

## Supported versions

Fixes go into the latest release. The app updates itself, so please check
that a problem still happens in the latest version before reporting it.
