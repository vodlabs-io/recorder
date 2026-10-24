# Running in Docker

The Docker image runs the recorder on a server or NAS with no desktop. It is
the same daemon the desktop app uses, with the app's interface served as a
web page, the **Web UI**, that you open from any browser on your network.

The image is `ghcr.io/vodlabs-io/recorder`, for `linux/amd64` and
`linux/arm64`.

## Quick start

Make a folder for the recorder, and put this repository's
[`compose.yaml`](../compose.yaml) in it. Then:

```sh
docker compose up -d
docker compose exec recorder vod web url
```

The second command prints a sign-in link, such as
`http://localhost:8080/signin#token=…`. Open it in a browser on the same
machine, or replace `localhost` with the machine's address on your network
(`http://192.168.1.20:8080/…`) to open it from another computer or a phone.
The browser stays signed in for 30 days of use.

Recordings appear in `./recordings`, and the settings in `./config`.

Without Compose:

```sh
docker run -d --name vodlabs-recorder --restart unless-stopped \
  --user "$(id -u):$(id -g)" -p 8080:8080 -e TZ=Europe/Berlin \
  -v "$PWD/config:/config" -v "$PWD/recordings:/recordings" -v vodlabs-cache:/cache \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  ghcr.io/vodlabs-io/recorder:latest
docker exec vodlabs-recorder vod web url
```

Create `config` and `recordings` before the first start (`mkdir config
recordings`). A folder Docker has to create belongs to root, which the
recorder can't write to; see [Permissions](#permissions).

## Signing in

The Web UI asks for a sign-in token, which the recorder generates on its first
start and keeps in `./config/web-token`. It is never written to the logs.

| Command | What it does |
| --- | --- |
| `docker compose exec recorder vod web url` | Print a sign-in link. |
| `docker compose exec recorder vod web token` | Print the token, to paste into the sign-in page. |
| `docker compose exec recorder vod web token --rotate` | Replace the token. Every browser is signed out within a few seconds. |

The token in a link sits after `#`, which the browser never sends anywhere;
the sign-in page removes it from the address bar as soon as it has read it.
**Sign out** at the bottom of the sidebar signs that browser out.

To set the token yourself, for example from a secret manager, use
`VLREC_WEB_TOKEN` or, better, a file such as a Docker secret:

```yaml
services:
  recorder:
    environment:
      VLREC_WEB_TOKEN_FILE: /run/secrets/web_token
    secrets: [web_token]
secrets:
  web_token:
    file: ./web_token.txt   # at least 32 characters, e.g. `openssl rand -base64 32`
```

A token you set this way is changed where you set it; `--rotate` refuses it.

Anyone with a session controls the recorder fully: its streamers, its
recordings, its settings and your stored platform logins. Treat the token like
a password.

## Who can reach it

`compose.yaml` publishes port 8080 on every network the machine is on, so
your home network can reach the Web UI, behind the sign-in.

**Never forward the port from the internet** on your router. Note that
Docker's published ports bypass host firewalls such as `ufw`: a rule there
won't close them. To narrow who can connect, publish on one address instead:

```yaml
    ports:
      - "127.0.0.1:8080:8080"     # this machine only (or through a reverse proxy)
      - "192.168.1.20:8080:8080"  # one network interface
```

To reach it from outside your home, use a VPN such as
[Tailscale](https://tailscale.com) rather than an open port.

After 10 failed sign-ins from one address in 15 minutes, that address must
wait; after 100 failed sign-ins in total, everyone must. Each failure is
logged with the address it came from, which [fail2ban](#fail2ban) can act on.

## HTTPS

The Web UI itself speaks plain HTTP. On your own network that is usually
fine, but anyone on the network can read plain HTTP, including a platform
login you paste into the Accounts page, which warns about it. For HTTPS, put
a reverse proxy in front, or use `tailscale serve`. Over HTTPS the session
cookie is marked `Secure` and named `__Host-vlrec_session`.

The recorder only believes a proxy's `X-Forwarded-Proto` and
`X-Forwarded-For` headers when the request comes from an address listed in
`web.trusted_proxies`. To find the address your proxy reaches the container
from, sign in through it once and look at `client_ip` on the
`Web UI sign-in` line in `docker compose logs recorder`. Then:

```yaml
    environment:
      VLREC_WEB_TRUSTED_PROXIES: 172.16.0.0/12   # Docker's default networks on Linux
```

The proxy must pass the original `Host` header through, which Caddy and
Traefik do by default: the recorder refuses requests whose `Origin` doesn't
match it. If the proxy serves the Web UI under another name than the one the
browser uses, add that origin to `VLREC_WEB_ALLOWED_ORIGINS`
(`https://recorder.example.com`).

With a proxy in front, publish the recorder's port on `127.0.0.1` only, so the
proxy is the one way in.

### Caddy

```caddyfile
recorder.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Under a subpath, with the prefix stripped:

```caddyfile
example.com {
	redir /recorder /recorder/
	handle_path /recorder/* {
		reverse_proxy 127.0.0.1:8080
	}
}
```

### nginx

The Web UI keeps a WebSocket open, which nginx closes after a minute of
silence unless told otherwise:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 1h;
    proxy_buffering off;
}
```

### Traefik

```yaml
    labels:
      - traefik.enable=true
      - traefik.http.routers.recorder.rule=Host(`recorder.example.com`)
      - traefik.http.routers.recorder.tls.certresolver=letsencrypt
      - traefik.http.services.recorder.loadbalancer.server.port=8080
```

Leave out `ports:` when Traefik reaches the container over a shared Docker
network.

### Tailscale

On the Docker host, `tailscale serve --bg 8080` serves the Web UI at
`https://<machine>.<tailnet>.ts.net` to your tailnet only, with a certificate.
Trust the address it connects from, as above.

## Permissions

The recorder never runs as root. It runs as the user `compose.yaml` names,
`1000:1000` unless you set `PUID` and `PGID`, and the files it writes belong
to that user. On most single-user Linux systems that is you already. If not,
put your own ids in a `.env` file beside `compose.yaml`:

```sh
echo "PUID=$(id -u)" > .env
echo "PGID=$(id -g)" >> .env
```

If `./config` or `./recordings` belongs to someone else, the recorder stops at
start-up and says which folder and which fix:

```text
/config is not writable by the container's user (uid 1000, gid 1000)
```

Fix it with `sudo chown -R 1000:1000 config recordings` (your ids), or set
`PUID` and `PGID` to the folders' owner. On a NAS, give that user write access
to the shared folder.

## Time zone

Recording folder names use the local time. Set `TZ` to yours, in `.env` or
in `compose.yaml`, for example `TZ=America/New_York`. The default is UTC.

## Folders

| In the container | Holds | In `compose.yaml` |
| --- | --- | --- |
| `/config` | Settings, the streamer list, the queue databases, your platform logins (encrypted), the Web UI's token and session key, and the recorder's identity with the VodLabs service. Back it up. | `./config` |
| `/recordings` | Recordings: the download folder. | `./recordings` |
| `/cache` | Logs, thumbnails, the extracted FFmpeg, and scratch space for merges, which can reach several GB. Safe to delete. | the `cache` volume |

Everything else in the container is read-only and lost when it is recreated.
Any other folder the recorder writes to, such as an **archive folder**, must
be a volume you mount, and you set the path *inside* the container:

```yaml
    volumes:
      - /mnt/nas/vods:/archive
```

then set **Settings → Files → Archive folder** to `/archive`. The Web UI
suggests the folders mounted into the container. The same goes for scan
folders and per-streamer archive folders.

## Settings

Settings made in the Web UI are saved to `./config/config.toml` and survive
restarts and updates. [Configuration](configuration.md) lists the settings;
environment variables override the file, which suits Compose. In addition to
the ones listed there, the container reads:

| Variable | Default | What it does |
| --- | --- | --- |
| `VLREC_WEB_ENABLED` | `true` | Serve the Web UI. |
| `VLREC_WEB_HOST` | `0.0.0.0` | The address the Web UI listens on, inside the container. |
| `VLREC_WEB_PORT` | `8080` | Its port, inside the container. |
| `VLREC_WEB_TOKEN` | generated | Pin the sign-in token; at least 32 characters. |
| `VLREC_WEB_TOKEN_FILE` | none | Read the token from this file, such as a Docker secret. Wins over `VLREC_WEB_TOKEN`. |
| `VLREC_WEB_ALLOWED_ORIGINS` | none | Extra origins (`https://host[:port]`), comma-separated, allowed to sign in and connect. |
| `VLREC_WEB_TRUSTED_PROXIES` | none | Addresses or CIDRs, comma-separated, whose forwarding headers are believed. |
| `TZ` | `UTC` | The time zone. |

The same settings go in `config.toml` under `[web]`, as `enabled`, `host`,
`port`, `token`, `token_file`, `allowed_origins` and `trusted_proxies`.

In a container the defaults differ from a desktop install: recordings go to
`/recordings`, logs go to the console for `docker compose logs`, and `vod`
never starts a daemon of its own.

## The command-line tool

The image includes `vod`, which talks to the daemon in the container:

```sh
docker compose exec recorder vod status
docker compose exec recorder vod record https://www.twitch.tv/example
docker compose exec recorder vod info
```

Run it without `-u root`: files it creates must stay readable by the
recorder. [Command-line reference](cli.md) lists the commands. `vod update`
and `vod restart` don't apply in a container; Docker does both.

## What a headless recorder can't do

The desktop app signs in to platforms in a window of its own and keeps those
logins fresh. Without it:

- **Platform logins are pasted.** On the Accounts page, paste a `Cookie`
  header from a browser where you are signed in
  ([Signing in to platforms](platform-sign-in.md)). Nothing renews it: paste a
  new one when it expires.
- **Some checks need a browser.** Age-restricted TikTok rooms, and the few
  platform checks that run inside the desktop app, are skipped.
- **Recordings play in the browser.** The Web UI plays and downloads them;
  there is no OS player or file manager to open them in.

## Updating

```sh
docker compose pull
docker compose up -d
```

Your settings, streamers, logins and recordings are in the volumes and carry
over. The Web UI's **Update** page says when a newer version is out.
[Watchtower](https://containrrr.dev/watchtower/) can update the container
for you, or [Diun](https://crazymax.dev/diun/) can tell you when an update is
out. To stay on a version, use its tag: `ghcr.io/vodlabs-io/recorder:0.1.2`,
or `0.1` for its fixes.

To check that an image was built from this repository by its release
workflow, with the [GitHub CLI](https://cli.github.com):

```sh
gh attestation verify oci://ghcr.io/vodlabs-io/recorder:latest --repo vodlabs-io/recorder
```

## Hardening

`compose.yaml` already runs the container as an unprivileged user with a
read-only filesystem (`read_only: true`), every Linux capability dropped and
`no-new-privileges`, and only the Web UI's port published. The daemon's own
API, on port 4222, stays on the container's loopback interface: it has no
authentication of its own, so never publish it.

### fail2ban

Each failed sign-in logs a line such as:

```text
level=WARN msg="Web UI sign-in failed" service=daemon component=webui client_ip=192.168.1.50
```

Behind a reverse proxy, `client_ip` is the browser's address only when the
proxy is in `web.trusted_proxies`. A filter, `/etc/fail2ban/filter.d/vodlabs-recorder.conf`:

```ini
[Definition]
failregex = Web UI sign-in failed.* client_ip=<HOST>
```

and a jail reading the container's log; `DOCKER-USER` is the chain Docker's
published ports pass through:

```ini
[vodlabs-recorder]
enabled  = true
filter   = vodlabs-recorder
logpath  = /var/lib/docker/containers/*/*-json.log
chain    = DOCKER-USER
maxretry = 5
bantime  = 1h
```

## Building the image yourself

```sh
git clone https://github.com/vodlabs-io/recorder.git
cd recorder
scripts/docker-build.sh
```

It builds for your machine and tags the result
`ghcr.io/vodlabs-io/recorder:latest`, which `compose.yaml` then runs.
`--platform linux/arm64` builds for another architecture.

## Troubleshooting

- **The container keeps restarting.** `docker compose logs recorder` says
  why, usually a folder it can't write to ([Permissions](#permissions)) or a
  pinned token that is too short.
- **The page loads but stays on "connecting".** A proxy is dropping the
  WebSocket; see the nginx settings above.
- **Sign-in says the token isn't right.** It was rotated, or the token was
  pinned; run `vod web url` again.
- **"Too many failed sign-ins."** Wait the time the page names.
