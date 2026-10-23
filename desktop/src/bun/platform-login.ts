// Third-party platform login capture (desktop only).
//
// PRIVACY — READ THIS:
// This module opens a webview to a streaming platform's own login page, lets the
// user sign in, and captures the resulting session cookies (including HttpOnly
// ones, via the native webview cookie store) so the LOCAL daemon can record
// login-restricted streams. The cookies are handed to the daemon over local
// NATS and are NEVER sent to the vodlabs cloud. Only the bun process can do this
// — the React webview cannot read another site's cookies.

import { Session } from "electrobun/main";
import type { DaemonSession } from "../../../shared/daemon/session";
import { Subjects } from "../../../shared/daemon/subjects";
import type { PlatformInfo, LoginFlow, StorageCookie } from "../../../shared/types";
import { COOKIE_PREFIX, withLoginView } from "./browser-context";
import { enableLoginPopups } from "./login-popups";
import { createPartitionedWindow } from "./partitioned-window";

// DaemonCookie is the cookie shape the daemon's platformlogin.set endpoint
// expects (snake_case, matching internal/platformauth.Cookie's JSON tags).
type DaemonCookie = {
  name: string;
  value: string;
  domain?: string;
  path?: string;
  secure?: boolean;
  http_only?: boolean;
  expires?: number;
};

const LOGIN_WINDOW = { width: 480, height: 760 };

// How long to let the signed-in page settle before capturing its cookies.
// Platforms rotate short-lived auxiliary tokens (e.g. TikTok's msToken) via
// page JS shortly after load, so capturing on the first navigation would
// re-store the previous, stale jar.
const CAPTURE_SETTLE_MS = 3000;

// How often an open login window checks for the session cookie. A sign-in that
// finishes without a navigation (a popup sign-in, an XHR login on a single-page
// app) fires no navigation event to capture on.
const CAPTURE_POLL_MS = 2000;

// Background-refresh tuning. The settle is longer than the interactive one
// because the hidden page loads cold (no user interaction warmed it up) and the
// whole point is waiting for token rotation. The timeout bounds a page that
// never navigates (offline, platform down) — that outcome changes nothing.
const REFRESH_SETTLE_MS = 5000;
const REFRESH_TIMEOUT_MS = 20000;
// A refresh shares one hidden webview per platform with the browser-context
// executor (see withLoginView), so it may have to wait its turn. It waits only
// briefly: a signed fetch holding the view is loading a platform page in this
// same partition, so it freshens the jar anyway, and the next sweep is minutes
// away — yielding beats making a status check wait on a background chore.
const REFRESH_ACQUIRE_MS = 2000;
// The page load's share of REFRESH_TIMEOUT_MS; the settle below runs on top of
// it, so the two together keep a refresh inside that budget. Every step that
// follows is time-bounded too (local NATS requests time out at 5s), which caps
// how long a refresh can hold the shared view at ~30s worst case — the reason
// this path must never grow an unbounded await.
const REFRESH_NAV_TIMEOUT_MS = REFRESH_TIMEOUT_MS - REFRESH_SETTLE_MS;
// Ignore refresh requests for a platform this soon after the previous run.
// Deliberately shorter than the daemon's own 3-min request cooldown; this one
// exists to coalesce the periodic sweep with on-demand daemon requests.
const REFRESH_MIN_INTERVAL_MS = 2 * 60 * 1000;

// Platforms with an interactive login window open right now. Background
// refreshes skip these: the user is mid-sign-in and the interactive capture
// will store a fresher jar than any silent reload could.
const interactiveLogins = new Set<string>();
// Platforms with a background refresh in flight, and when each last ran.
const refreshesInFlight = new Set<string>();
const lastRefreshAt = new Map<string, number>();

function partitionFor(platform: string): string {
  return `persist:${platform}`;
}

async function loginDescriptor(
  session: DaemonSession,
  platform: string,
): Promise<{ login: LoginFlow; label: string }> {
  const platforms = await session.request<PlatformInfo[]>(Subjects.platforms);
  const info = platforms.find((p) => p.name === platform);
  if (!info?.login) {
    throw new Error(`platform "${platform}" does not support login`);
  }
  return { login: info.login, label: info.display_name || platform };
}

// hostAllowed mirrors vlconfig.loginFlow.AllowsHost: a cookie is kept only if
// its domain belongs to the descriptor's cookie_domains. A sign-in can span
// more than one registrable domain (YouTube's runs on accounts.google.com), and
// the stored jar is replayed WHOLE to every allowed host, so a cookie without an
// attributable domain is dropped. Both native cookie APIs report a domain for
// every cookie.
export function hostAllowed(domains: string[] | undefined, cookieDomain?: string): boolean {
  const allowed = domains ?? [];
  if (allowed.length === 0) return true; // no allowlist declared: nothing to enforce
  if (!cookieDomain) return false; // unattributable, and the jar is not per-host
  const host = cookieDomain.replace(/^\./, "").toLowerCase();
  return allowed.some((d) => {
    const dd = d.replace(/^\./, "").toLowerCase();
    return dd !== "" && (host === dd || host.endsWith("." + dd));
  });
}

// captureCookies reads the login partition's cookies and returns them in the
// daemon's shape, or null when the user isn't signed in yet (no session cookie).
function captureCookies(login: LoginFlow, partition: string): DaemonCookie[] | null {
  const keep = Session.fromPartition(partition)
    .cookies.get({})
    .filter((c) => hostAllowed(login.cookie_domains, c.domain))
    // Never persist the browser-context executor's transient result cookies:
    // a refresh racing a signed fetch must not capture them into the stored
    // login jar.
    .filter((c) => !c.name.startsWith(COOKIE_PREFIX));

  // "Signed in" is decided on the jar we would actually store, not on
  // everything the partition holds. A sign-in that spans two domains leaves
  // cookies we deliberately drop, and judging by those would report a login the
  // daemon then rejects for having no session cookie in it.
  const signedIn = login.session_cookie
    ? keep.some((c) => c.name === login.session_cookie && !!c.value)
    : keep.length > 0;
  if (!signedIn) return null;

  return keep
    .map((c) => ({
      name: c.name,
      value: c.value,
      domain: c.domain,
      path: c.path,
      secure: c.secure,
      http_only: c.httpOnly,
      expires: c.expirationDate ? Math.floor(c.expirationDate) : undefined,
    }));
}

// How long a background refresh waits after copying storage values into
// cookies (see storageCookieScript) before it captures the jar.
const STORAGE_COPY_SETTLE_MS = 500;

// storageCookieScript returns page JS that keeps each of the descriptor's
// local-storage values (Fansly's session token) in a first-party cookie, so the
// native cookie capture picks it up with the rest of the jar; a value that is
// gone (the user signed out) removes its cookie, which is how a refresh sees
// the session end. Empty when the descriptor names none.
//
// PRIVACY: the script runs in the platform's own page and writes the value
// only into that site's own cookie jar; nothing is read out here.
export function storageCookieScript(entries: StorageCookie[] | undefined): string {
  const list = (entries ?? []).filter((e) => e.key && /^[A-Za-z0-9_]+$/.test(e.cookie));
  if (list.length === 0) return "";
  return `(function () {
  var entries = ${JSON.stringify(list)};
  var domain = location.hostname.split(".").slice(-2).join(".");
  entries.forEach(function (e) {
    var v = "";
    try {
      var raw = localStorage.getItem(e.key);
      if (raw && e.field) {
        var obj = JSON.parse(raw);
        v = obj && typeof obj[e.field] === "string" ? obj[e.field] : "";
      } else if (raw) {
        v = raw;
      }
    } catch (err) { v = ""; }
    var attrs = "; domain=" + domain + "; path=/; secure; samesite=lax";
    if (v) {
      document.cookie = e.cookie + "=" + encodeURIComponent(v) + attrs + "; max-age=31536000";
    } else if (document.cookie.indexOf(e.cookie + "=") !== -1) {
      document.cookie = e.cookie + "=" + attrs + "; max-age=0";
    }
  });
})();`;
}

function eventUrl(e: unknown): string {
  const detail = (e as { data?: { detail?: unknown } })?.data?.detail;
  return typeof detail === "string" ? detail : "";
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// daemonHasLogin reports whether the daemon currently stores a login for the
// platform. Distinguishes "Sign in again" (login exists — silent re-capture of
// the same account is wanted) from a fresh sign-in (no login — the user is
// choosing an account and must get a real login page).
async function daemonHasLogin(
  session: DaemonSession,
  platform: string,
): Promise<boolean> {
  try {
    const logins = await session.request<{ platform: string }[] | null>(
      Subjects.platformLoginList,
    );
    return (logins ?? []).some((l) => l.platform === platform);
  } catch {
    return false;
  }
}

// wipePartition clears the login partition's web data. clearStorageData alone
// doesn't reliably clear cookies, so this also clears the cookie jar
// explicitly, then removes any survivors one by one. Returns whether the jar
// ended up empty.
function wipePartition(partition: string): boolean {
  const s = Session.fromPartition(partition);
  try {
    s.clearStorageData("all");
  } catch (err) {
    console.error(`platform login: clearStorageData failed (${partition})`, err);
  }
  try {
    s.cookies.clear();
  } catch (err) {
    console.error(`platform login: cookie clear failed (${partition})`, err);
  }
  try {
    for (const c of s.cookies.get({})) {
      const host = (c.domain ?? "").replace(/^\./, "");
      if (!host) continue;
      s.cookies.remove(`https://${host}${c.path ?? "/"}`, c.name);
    }
    const left = s.cookies.get({}).length;
    if (left > 0) {
      console.error(
        `platform login: ${left} cookie(s) survived the wipe (${partition})`,
      );
      return false;
    }
    return true;
  } catch (err) {
    console.error(`platform login: wipe verification failed (${partition})`, err);
    return false;
  }
}

// runPlatformLogin opens the platform's login page in an isolated webview,
// watches for a completed sign-in, captures the cookies, and stores them on the
// daemon. Resolves true once a login was captured, false if the user closed the
// window first.
export async function runPlatformLogin(
  session: DaemonSession,
  platform: string,
): Promise<boolean> {
  const { login, label } = await loginDescriptor(session, platform);
  const partition = partitionFor(platform);
  const successRe = login.success_url_pattern
    ? new RegExp(login.success_url_pattern)
    : null;

  // Fresh sign-in (first time, or right after sign-out): the user is choosing
  // an account, so a leftover webview session must not be insta-captured — that
  // would silently re-store the account they just signed out of and make
  // switching accounts impossible. Wipe the partition so the platform serves a
  // real login page. When a stored login EXISTS ("Sign in again"), the leftover
  // session IS that account and the insta-capture below is the wanted refresh.
  if (!(await daemonHasLogin(session, platform))) {
    wipePartition(partition);
  }

  interactiveLogins.add(platform);
  return await new Promise<boolean>((resolve) => {
    let settled = false;
    const frame = { x: 0, y: 0, width: LOGIN_WINDOW.width, height: LOGIN_WINDOW.height };

    // Dedicated partition so the login's own web data is isolated and sign-out
    // can wipe exactly it. sandbox: true — untrusted remote content, no RPC.
    // The login page is the window's OWN webview: a partitioned view added on
    // top of one is never painted on Windows (see partitioned-window.ts).
    const { win, view } = createPartitionedWindow(
      {
        title: `Sign in to ${label}`,
        frame,
        url: login.url,
        sandbox: true,
      },
      partition,
    );
    enableLoginPopups(view);

    // Set when the window is already gone because the user closed it. Tearing
    // it down again from here is a no-op at best: the native side reports the
    // handle as invalid and falls back to destroying an already-destroyed
    // window.
    let windowGone = false;

    const finish = (ok: boolean): void => {
      if (settled) return;
      settled = true;
      interactiveLogins.delete(platform);
      clearInterval(poll);
      if (!windowGone) {
        try {
          view.remove();
        } catch {
          /* already gone */
        }
        try {
          win.close();
        } catch {
          /* already closed */
        }
      }
      resolve(ok);
    };

    // Platforms that keep their session in local storage get it copied into a
    // cookie on every check; the copy lands by the next poll.
    const storageScript = storageCookieScript(login.storage_cookies);

    let capturePending = false;
    const tryCapture = (url: string): void => {
      if (settled || capturePending) return;
      if (storageScript) {
        try {
          view.executeJavascript(storageScript);
        } catch {
          /* page not ready yet; the next poll retries */
        }
      }
      // A sign-in is detected when a session cookie is present, or (fallback)
      // the post-login URL matches the descriptor's success pattern.
      let cookies = captureCookies(login, partition);
      if (!cookies && successRe?.test(url)) {
        cookies = captureCookies(login, partition);
      }
      if (!cookies || cookies.length === 0) return;

      // Signed in — but don't store this jar yet: let the page settle and
      // re-capture, so freshly rotated tokens are included (see
      // CAPTURE_SETTLE_MS). Especially matters for the already-signed-in
      // "refresh" case, where the first capture is the stale previous jar.
      capturePending = true;
      setTimeout(() => {
        void (async () => {
          if (settled) return; // user closed the window during the settle
          const fresh = captureCookies(login, partition) ?? cookies;
          try {
            // PRIVACY: cookies go ONLY to the local daemon over local NATS.
            await session.request(Subjects.platformLoginSet, {
              platform,
              cookies: fresh,
            });
            finish(true);
          } catch (err) {
            console.error("platform login: failed to store cookies", err);
            finish(false);
          }
        })();
      }, CAPTURE_SETTLE_MS);
    };

    view.on("did-navigate", (e) => tryCapture(eventUrl(e)));
    view.on("did-navigate-in-page", (e) => tryCapture(eventUrl(e)));
    view.on("did-commit-navigation", (e) => tryCapture(eventUrl(e)));
    const poll = setInterval(() => tryCapture(""), CAPTURE_POLL_MS);
    // User closed the window before signing in.
    win.on("close", () => {
      windowGone = true;
      finish(false);
    });
  });
}

export type RefreshResult = "refreshed" | "logged_out" | "skipped" | "failed";

// refreshPlatformLogin silently reloads the platform's page in the pooled
// hidden login webview (withLoginView) and re-stores the captured jar on the
// daemon. Captured jars go stale within tens of minutes (TikTok's msToken stops
// rotating), and only a webview page load can freshen them. Called by the
// periodic scheduler and by daemon "platformlogin.refresh" messages (see
// index.ts).
//
// Outcomes:
//   "refreshed"  — fresh jar captured and stored (platformlogin.set also nudges
//                  the daemon's walled streamers, closing the unlock loop);
//   "logged_out" — the page loaded but the session cookie is gone: the platform
//                  ended the session. This is the ONE unambiguous logged-out
//                  signal TikTok gives us, so it flips the Accounts row to
//                  "Expired" via platformlogin.health;
//   "skipped"    — interactive login open, refresh already running, ran too
//                  recently, or the shared login view was busy with a signed
//                  fetch;
//   "failed"     — the page never loaded (offline, platform down). Changes
//                  nothing: a network failure is not a login failure.
//
// PRIVACY: like the interactive path, captured cookies go ONLY to the local
// daemon over local NATS. Nothing here talks to the vodlabs cloud.
export async function refreshPlatformLogin(
  session: DaemonSession,
  platform: string,
): Promise<RefreshResult> {
  if (interactiveLogins.has(platform) || refreshesInFlight.has(platform)) {
    return "skipped";
  }
  const last = lastRefreshAt.get(platform);
  if (last !== undefined && Date.now() - last < REFRESH_MIN_INTERVAL_MS) {
    return "skipped";
  }
  // Only refresh logins the daemon still holds. Without this, a refresh racing
  // a sign-out would capture the not-yet-wiped partition and re-store the login
  // the user just removed.
  if (!(await daemonHasLogin(session, platform))) {
    return "skipped";
  }

  let login: LoginFlow;
  try {
    ({ login } = await loginDescriptor(session, platform));
  } catch (err) {
    console.error(`login refresh (${platform}): no descriptor`, err);
    return "failed";
  }

  refreshesInFlight.add(platform);
  const partition = partitionFor(platform);

  try {
    const result = await withLoginView<RefreshResult>(
      platform,
      REFRESH_ACQUIRE_MS,
      async (view) => {
        // Only a run that got the view counts against the interval. Stamping it
        // earlier would let a refresh that never loaded a page suppress the next
        // sweep, so a busy stretch could starve the jar of refreshes entirely.
        lastRefreshAt.set(platform, Date.now());

        try {
          // The platform's own page, not necessarily the sign-in page — see
          // LoginFlow.refresh_url.
          await view.navigate(login.refresh_url || login.url, REFRESH_NAV_TIMEOUT_MS);
        } catch (err) {
          // Offline, or the platform is down. Not a login failure.
          console.error(`login refresh (${platform}): page did not load`, err);
          return "failed";
        }

        // The page is up. Let its JS settle (token rotation) before capturing.
        await sleep(REFRESH_SETTLE_MS);

        // Bring local-storage values into the jar (or drop the ones the
        // platform cleared) before it is captured.
        const storageScript = storageCookieScript(login.storage_cookies);
        if (storageScript) {
          view.run(storageScript);
          await sleep(STORAGE_COPY_SETTLE_MS);
        }

        // The user may have signed out while this page was loading — never
        // re-store (or health-flag) a login that no longer exists.
        if (!(await daemonHasLogin(session, platform))) return "skipped";

        const cookies = captureCookies(login, partition);
        try {
          if (cookies && cookies.length > 0) {
            // PRIVACY: cookies go ONLY to the local daemon over local NATS.
            await session.request(Subjects.platformLoginSet, { platform, cookies });
            return "refreshed";
          }
          // The page loaded but the session cookie is gone from the jar the
          // platform itself just rewrote — the session is dead. Tell the daemon
          // so the Accounts UI shows "Expired — sign in again".
          await session.request(Subjects.platformLoginHealth, {
            platform,
            healthy: false,
          });
          return "logged_out";
        } catch (err) {
          console.error(`login refresh (${platform}): daemon request failed`, err);
          return "failed";
        }
      },
    );
    // null: a signed fetch held the view for the whole acquire window.
    return result ?? "skipped";
  } catch (err) {
    console.error(`login refresh (${platform}): no login view`, err);
    return "failed";
  } finally {
    refreshesInFlight.delete(platform);
  }
}

// signOutPlatform removes the stored login from the daemon AND wipes the login
// webview's own session data, so signing out leaves nothing behind on-device
// and the next sign-in gets a real login page (account switching). A failed
// wipe is logged and self-heals: runPlatformLogin re-wipes before a fresh
// sign-in.
export async function signOutPlatform(
  session: DaemonSession,
  platform: string,
): Promise<void> {
  await session.request(Subjects.platformLoginRemove, { platform });
  wipePartition(partitionFor(platform));
}
