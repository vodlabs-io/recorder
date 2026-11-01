// Browser-context request executor (desktop only).
//
// Some login-restricted streams (TikTok 18+/private) only return stream data to
// requests carrying the platform's own JS-computed signature (webmssdk's
// X-Gnarly). The daemon asks us (over LOCAL NATS, PLATFORM_BROWSERFETCH_SUBJECT)
// to run the request inside the platform's login webview, where the platform's
// own JS signs it and the browser attaches its own cookies.
//
// PRIVACY: the request runs with the webview's own cookies; nothing here talks to
// the vodlabs cloud, and no cookie value is read out.
//
// Result channel: executeJavascript is fire-and-forget, so the injected code
// stashes the response in short-lived COOKIE_PREFIX cookies that bun reads and
// deletes. captureCookies() in platform-login.ts skips that prefix.
//
// Lifecycle: one hidden window + view per partition, reused and parked on
// about:blank between requests, which are serialised. The background login
// refresh borrows the same view via withLoginView.

import { BrowserWindow, BrowserView, Session } from "electrobun/main";
import type { DaemonSession } from "../../../shared/daemon/session";
import {
  PLATFORM_BROWSERFETCH_READY_SUBJECT,
  PLATFORM_BROWSERFETCH_SUBJECT,
  Subjects,
} from "../../../shared/daemon/subjects";
import type { PlatformInfo } from "../../../shared/types";
import { createPartitionedWindow } from "./partitioned-window";
import { MUTE_MEDIA, muteWebview } from "./webview-mute";

type BrowserFetchRequest = {
  platform: string;
  url: string;
  method?: string;
  body?: string;
  timeout_ms?: number;
  // extractor_regex, when set, is applied to the response body IN-PAGE before
  // relaying, so a large HTML page is trimmed to just the matched block (capture
  // group 1 if present) instead of chunking the whole page through the cookie
  // relay. Supplied by the cloud; compiled here by the page's JS engine.
  extractor_regex?: string;
};
type BrowserFetchResponse = {
  status: number;
  body?: string;
  error?: string;
};

// COOKIE_PREFIX names the transient result cookies. It is deliberately shared
// with platform-login.ts captureCookies(), which skips this prefix so these
// never end up in the stored login jar.
export const COOKIE_PREFIX = "__vlbf_";

const LOGIN_WINDOW = { width: 480, height: 760 };
// Bound the whole operation: page load + webmssdk init + fetch + relay.
const DEFAULT_TIMEOUT_MS = 25000;
const POLL_INTERVAL_MS = 150;
// Cookie values are capped (~4KB) per cookie; chunk well under that.
const COOKIE_CHUNK = 3000;
// Where the reused view parks between requests: no JS, no media, no network.
const BLANK_URL = "about:blank";
// Share of a request's budget that may be spent waiting for a free view; the
// rest funds the page load and relay.
const ACQUIRE_BUDGET = 0.4;
// Backstop for armIdle: a view's own navigation commits in ~160ms, but a commit
// event that never arrives must not wedge the context forever.
const IDLE_FALLBACK_MS = 3000;

function partitionFor(platform: string): string {
  return `persist:${platform}`;
}

function eventUrl(e: unknown): string {
  const detail = (e as { data?: { detail?: unknown } })?.data?.detail;
  return typeof detail === "string" ? detail : "";
}

// pageURLFor returns a real platform page (with the signing JS) to host the
// fetch. Using the target URL's own origin keeps same-origin API calls (e.g.
// TikTok's api-live/user/room on www.tiktok.com) truly same-origin.
function pageURLFor(targetURL: string): string {
  return new URL(targetURL).origin + "/";
}

// buildInjectedScript returns main-world JS that waits for the platform's signing
// SDK to initialise, performs the fetch (so the SDK signs it), and writes the
// response into COOKIE_PREFIX cookies for bun to read. Runs entirely in-page.
function buildInjectedScript(
  id: string,
  target: string,
  method: string,
  body: string,
  extractorRegex: string,
): string {
  const targetJSON = JSON.stringify(target);
  const methodJSON = JSON.stringify(method || "GET");
  const bodyJSON = JSON.stringify(body || "");
  const regexJSON = JSON.stringify(extractorRegex || "");
  const prefix = COOKIE_PREFIX + id + "_";
  const prefixJSON = JSON.stringify(prefix);
  return `(async () => {
  var PREFIX = ${prefixJSON};
  function store(obj) {
    try {
      var b64 = btoa(unescape(encodeURIComponent(JSON.stringify(obj))));
      var n = Math.ceil(b64.length / ${COOKIE_CHUNK}) || 1;
      for (var i = 0; i < n; i++) {
        document.cookie = PREFIX + i + "=" + b64.slice(i * ${COOKIE_CHUNK}, (i + 1) * ${COOKIE_CHUNK}) + "; path=/";
      }
      document.cookie = PREFIX + "n=" + n + "; path=/";
    } catch (e) {
      document.cookie = PREFIX + "0=" + btoa('{"status":0,"error":"store failed"}') + "; path=/";
      document.cookie = PREFIX + "n=1; path=/";
    }
  }
  try {
    // Wait (≤6s) for the signing SDK (webmssdk) to patch fetch. Checks the
    // patch itself (a native fetch reports [native code]) rather than only a
    // global the SDK happens to set, which could be renamed.
    function fetchPatched() {
      try { return String(window.fetch).indexOf("[native code]") === -1; }
      catch (e) { return false; }
    }
    var waited = 0;
    for (var i = 0; i < 60; i++) {
      if (window.byted_acrawler && fetchPatched()) break;
      await new Promise(function (r) { setTimeout(r, 100); });
      waited += 100;
    }
    var ready = !!(window.byted_acrawler && fetchPatched());

    var opts = { credentials: "include", method: ${methodJSON} };
    if (${bodyJSON}) opts.body = ${bodyJSON};

    // The SDK's wrapper can transiently return undefined instead of a Response
    // even after the wait above, so retry once and report it plainly.
    var resp = await fetch(${targetJSON}, opts);
    if (!resp || typeof resp.text !== "function") {
      await new Promise(function (r) { setTimeout(r, 500); });
      resp = await fetch(${targetJSON}, opts);
    }
    if (!resp || typeof resp.text !== "function") {
      store({
        status: 0,
        error: "signing fetch returned no response (sdk_ready=" + ready +
               ", waited_ms=" + waited + ")",
      });
      return;
    }
    var text = await resp.text();
    // Trim to just the requested block IN-PAGE so a big HTML page doesn't get
    // chunked whole through the cookie relay. The cloud supplies the pattern.
    var out = text;
    var RX = ${regexJSON};
    if (RX) {
      try {
        var m = text.match(new RegExp(RX));
        out = m ? (m[1] != null ? m[1] : m[0]) : "";
      } catch (e) { out = ""; }
    }
    store({ status: resp.status, body: out });
  } catch (e) {
    store({ status: 0, error: String(e) });
  }
})();`;
}

// readResult polls the partition's cookies for a complete COOKIE_PREFIX result,
// reassembles and decodes it, then deletes the transient cookies. Returns null
// until the result is complete.
function readResult(partition: string, id: string, host: string): BrowserFetchResponse | null {
  const s = Session.fromPartition(partition);
  const prefix = COOKIE_PREFIX + id + "_";
  const all = s.cookies.get({});
  const nCookie = all.find((c) => c.name === prefix + "n");
  if (!nCookie) return null;
  const n = parseInt(nCookie.value, 10);
  if (!Number.isFinite(n) || n < 1) return null;

  let b64 = "";
  for (let i = 0; i < n; i++) {
    const chunk = all.find((c) => c.name === prefix + i);
    if (!chunk) return null; // not all chunks landed yet
    b64 += chunk.value;
  }
  // Clean up every transient cookie for this id (chunks + marker).
  for (const c of all) {
    if (c.name.startsWith(prefix)) {
      try {
        s.cookies.remove(`https://${host}/`, c.name);
      } catch {
        /* best-effort */
      }
    }
  }
  try {
    const json = decodeURIComponent(escape(atob(b64)));
    return JSON.parse(json) as BrowserFetchResponse;
  } catch (e) {
    return { status: 0, error: "decode failed: " + String(e) };
  }
}

async function loginPartition(session: DaemonSession, platform: string): Promise<string> {
  const platforms = await session.request<PlatformInfo[]>(Subjects.platforms);
  const info = platforms.find((p) => p.name === platform);
  if (!info?.login) throw new Error(`platform "${platform}" does not support login`);
  return partitionFor(platform);
}

// FetchContext is one long-lived hidden window + view, leased by one request at
// a time. onReady holds the current navigation's one-shot resolver, fired when
// the target document is ready to run script; it is null while the view sits
// parked on about:blank.
type FetchContext = {
  win: BrowserWindow;
  view: BrowserView;
  onReady: (() => void) | null;
  // idle resolves once the navigation the view started on its own has committed,
  // so navigate() never issues a loadURL that load would overtake. See armIdle.
  idle: Promise<void>;
  markIdle: () => void;
};

// armIdle re-arms ctx.idle ahead of a navigation the view starts on its own: the
// constructor's initial load, or park()'s. Until that commits, any loadURL we
// issue is silently overtaken by it, so navigate() awaits this first.
function armIdle(ctx: FetchContext): void {
  ctx.idle = new Promise<void>((resolve) => {
    const timer = setTimeout(resolve, IDLE_FALLBACK_MS);
    ctx.markIdle = () => {
      clearTimeout(timer);
      resolve();
    };
  });
}

// One login partition: its single hidden window + view, and any requests waiting
// their turn on it. A view hosts one page at a time, so requests are serialised.
// Waiters are rare, since subscribeRequest already hands requests over one at a
// time (shared/daemon/session.ts).
type Partition = {
  ctx: FetchContext | null;
  busy: boolean;
  waiters: ((ctx: FetchContext) => void)[];
};

const partitions = new Map<string, Partition>();

function partitionState(partition: string): Partition {
  let p = partitions.get(partition);
  if (!p) {
    p = { ctx: null, busy: false, waiters: [] };
    partitions.set(partition, p);
  }
  return p;
}

// createContext builds one hidden window + view, parked on about:blank. Event
// handlers are attached exactly once, here — electrobun's `on()` has no `off()`,
// so per-request handlers would accumulate in the global emitter forever.
function createContext(partition: string, state: Partition): FetchContext {
  const frame = { x: 0, y: 0, width: LOGIN_WINDOW.width, height: LOGIN_WINDOW.height };
  // The page is the window's OWN webview: a BrowserView added on top of one
  // is never painted on Windows (see partitioned-window.ts). This view is
  // hidden either way, but there is no reason to keep the broken shape.
  const { win, view } = createPartitionedWindow(
    { title: "…", frame, url: BLANK_URL, sandbox: true, preload: MUTE_MEDIA },
    partition,
  );
  // Never steal the user's focus: this window exists only to host the view.
  win.hide();
  // Platform pages autoplay with sound; nobody is watching this one.
  muteWebview(view);

  const ctx: FetchContext = {
    win,
    view,
    onReady: null,
    idle: Promise.resolve(),
    markIdle: () => {},
  };
  // The constructor above already queued a load; nothing may navigate until it
  // lands.
  armIdle(ctx);

  // Two signals, two jobs (see the lifecycle note at the top of the file). A
  // navigation event means the view's own pending load has landed, so a loadURL
  // of ours can no longer be overtaken by it...
  const onNavigation = (): void => ctx.markIdle();
  // ...while dom-ready is what a request waits for: the point the target
  // document exists and can run the injected script.
  const onDomReady = (e: unknown): void => {
    // Ignore the parking navigations; only a real platform page counts.
    if (!eventUrl(e).startsWith("http")) return;
    const fire = ctx.onReady;
    if (!fire) return;
    ctx.onReady = null;
    fire();
  };
  view.on("did-navigate", onNavigation);
  view.on("did-commit-navigation", onNavigation);
  view.on("dom-ready", onDomReady);
  // Should the window ever go away, drop the context so the next request
  // builds a replacement instead of driving a dead view.
  win.on("close", () => {
    if (state.ctx === ctx) state.ctx = null;
  });

  state.ctx = ctx;
  return ctx;
}

// acquire leases the partition's view, building it on first use. While a request
// holds it the next one waits, bounded by timeoutMs — the daemon abandons the
// request at its own NATS timeout, so an unbounded wait would leave us doing
// work for a reply nobody reads.
function acquire(partition: string, timeoutMs: number): Promise<FetchContext | null> {
  const state = partitionState(partition);
  // Drop a view that died under us so this lease builds a replacement.
  if (state.ctx?.view.isRemoved) state.ctx = null;

  if (!state.busy) {
    state.busy = true;
    try {
      return Promise.resolve(state.ctx ?? createContext(partition, state));
    } catch (err) {
      // Never hold the lease for a view that failed to build: busy would stay
      // true forever and every later request (and refresh) would time out
      // waiting on a context that does not exist.
      state.busy = false;
      throw err;
    }
  }
  return new Promise<FetchContext | null>((resolve) => {
    const waiter = (ctx: FetchContext): void => {
      clearTimeout(timer);
      resolve(ctx);
    };
    const timer = setTimeout(() => {
      state.waiters = state.waiters.filter((w) => w !== waiter);
      resolve(null);
    }, timeoutMs);
    state.waiters.push(waiter);
  });
}

// release hands the view to the next waiter, or parks it until the next request.
function release(partition: string, ctx: FetchContext): void {
  const state = partitionState(partition);
  if (ctx.view.isRemoved && state.ctx === ctx) state.ctx = null;

  const next = state.waiters.shift();
  if (next) {
    // The lease passes straight on (stays busy). A live view is handed over
    // WITHOUT parking: the waiter navigates in the same tick, and a parking load
    // racing that navigation could blank the page out from under it.
    let handoff: FetchContext | null = null;
    try {
      handoff = state.ctx ?? createContext(partition, state);
    } catch (err) {
      console.error("[browser-context] failed to rebuild view for waiter", err);
    }
    if (handoff) {
      next(handoff);
      return;
    }
    // Nothing to hand over. Fall through and free the lease rather than hold it
    // for a waiter that can only time out.
  }
  if (state.ctx === ctx) park(ctx);
  state.busy = false;
}

// navigate drives the reused view to url and resolves once that page's document
// is ready to run script.
async function navigate(ctx: FetchContext, url: string, timeoutMs: number): Promise<void> {
  // Let the view's own pending load land first, or it would overtake ours.
  await ctx.idle;
  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      ctx.onReady = null;
      reject(new Error("page load timed out"));
    }, timeoutMs);
    ctx.onReady = () => {
      clearTimeout(timer);
      resolve();
    };
    ctx.view.loadURL(url);
  });
}

// park returns the view to about:blank so no platform page is left running JS,
// polling the network, or decoding video between requests.
function park(ctx: FetchContext): void {
  ctx.onReady = null;
  // Parking is a navigation of the view's own: the next request must wait it out
  // rather than race it (see armIdle).
  armIdle(ctx);
  try {
    ctx.view.loadURL(BLANK_URL);
  } catch {
    ctx.markIdle(); // nothing in flight to wait for
  }
}

// awaitResult polls the partition's cookies until the injected script's result
// lands, or the deadline passes.
function awaitResult(
  partition: string,
  id: string,
  host: string,
  timeoutMs: number,
): Promise<BrowserFetchResponse> {
  return new Promise<BrowserFetchResponse>((resolve) => {
    const done = (res: BrowserFetchResponse): void => {
      clearInterval(pollTimer);
      clearTimeout(timeoutTimer);
      resolve(res);
    };
    const pollTimer = setInterval(() => {
      const result = readResult(partition, id, host);
      if (result) done(result);
    }, POLL_INTERVAL_MS);
    const timeoutTimer = setTimeout(
      () => done({ status: 0, error: "browser fetch timed out" }),
      timeoutMs,
    );
  });
}

// performFetch runs one request on a leased context. The caller holds the lease
// for the whole duration and releases (and parks) it afterwards.
async function performFetch(
  ctx: FetchContext,
  partition: string,
  req: BrowserFetchRequest,
  timeoutMs: number,
): Promise<BrowserFetchResponse> {
  const pageURL = pageURLFor(req.url);
  const host = new URL(req.url).host;
  const id = Math.random().toString(36).slice(2) + Date.now().toString(36);
  const deadline = Date.now() + timeoutMs;
  console.log(`[browser-context] request platform=${req.platform} host=${host} page=${pageURL}`);

  try {
    // The whole budget covers page load + webmssdk init + fetch + relay, so the
    // load gets the full remaining time and whatever is left funds the relay.
    await navigate(ctx, pageURL, timeoutMs);
  } catch (err) {
    return { status: 0, error: String(err) };
  }

  console.log(`[browser-context] page ready; injecting fetch (id=${id})`);
  // The document is ready; webmssdk loads shortly after. Inject the fetch (it
  // waits for the SDK internally), then poll cookies for the result.
  ctx.view.executeJavascript(
    buildInjectedScript(id, req.url, req.method || "GET", req.body || "", req.extractor_regex || ""),
  );
  return await awaitResult(partition, id, host, Math.max(deadline - Date.now(), POLL_INTERVAL_MS));
}

// runBrowserFetch executes one request inside the partition's hidden platform
// webview and returns the platform's response (or an error the daemon relays as
// a failed check, leaving the stream restricted).
async function runBrowserFetch(
  session: DaemonSession,
  req: BrowserFetchRequest,
): Promise<BrowserFetchResponse> {
  if (!req?.url || !req?.platform) return { status: 0, error: "invalid request" };

  let partition: string;
  try {
    partition = await loginPartition(session, req.platform);
  } catch (err) {
    return { status: 0, error: String(err) };
  }

  // One budget covers waiting for a view AND the fetch itself — the daemon
  // abandons us at its own timeout, so the two must share, not stack. Waiting is
  // capped at a fraction of it so a lease that arrives late still has time to be
  // worth something.
  const timeoutMs = req.timeout_ms && req.timeout_ms > 0 ? req.timeout_ms : DEFAULT_TIMEOUT_MS;
  const deadline = Date.now() + timeoutMs;

  let ctx: FetchContext | null;
  try {
    ctx = await acquire(partition, Math.floor(timeoutMs * ACQUIRE_BUDGET));
  } catch (err) {
    return { status: 0, error: "browser context unavailable: " + String(err) };
  }
  if (!ctx) return { status: 0, error: "browser context busy" };

  let res: BrowserFetchResponse;
  try {
    res = await performFetch(ctx, partition, req, deadline - Date.now());
  } catch (err) {
    res = { status: 0, error: String(err) };
  } finally {
    release(partition, ctx);
  }
  console.log(
    `[browser-context] finish status=${res.status} bodyLen=${res.body?.length ?? 0}` +
      (res.error ? ` error=${res.error}` : ""),
  );
  return res;
}

// LoginView is the slice of a leased context another module may drive. It
// exposes navigation and fire-and-forget script only: the lease owner parks the
// view afterwards, and nothing outside this file should touch the BrowserView's
// lifecycle.
export type LoginView = {
  // navigate drives the view to url, resolving once the page can run script.
  navigate: (url: string, timeoutMs: number) => Promise<void>;
  // run executes js in the loaded page, without a result.
  run: (js: string) => void;
};

// withLoginView leases the platform's pooled hidden webview, runs fn on it, and
// parks it afterwards, so the background login refresh (platform-login.ts)
// shares the view the signed fetches above use rather than creating a native
// window + view of its own each time.
//
// Returns null when the view could not be leased within acquireMs, i.e. a
// signed fetch holds it, which loads a platform page in this partition anyway.
export async function withLoginView<T>(
  platform: string,
  acquireMs: number,
  fn: (view: LoginView) => Promise<T>,
): Promise<T | null> {
  const partition = partitionFor(platform);
  const ctx = await acquire(partition, acquireMs);
  if (!ctx) return null;
  try {
    return await fn({
      navigate: (url, timeoutMs) => navigate(ctx, url, timeoutMs),
      run: (js) => ctx.view.executeJavascript(js),
    });
  } finally {
    release(partition, ctx);
  }
}

// subscribeBrowserFetch wires the LOCAL NATS request-reply handler. Call on every
// connect (subscriptions live on the current connection), like startLoginRefresh.
export function subscribeBrowserFetch(session: DaemonSession): () => void {
  const unsubscribe = session.subscribeRequest<BrowserFetchRequest, BrowserFetchResponse>(
    PLATFORM_BROWSERFETCH_SUBJECT,
    (reqMsg) => runBrowserFetch(session, reqMsg),
  );
  // Announce only after the handler is wired, so a check released by this can
  // never arrive before there is something to serve it. The daemon sets those
  // checks aside rather than retrying them, so without this they wait for the
  // streamer's next ordinary poll.
  session.publish(PLATFORM_BROWSERFETCH_READY_SUBJECT);
  return unsubscribe;
}
