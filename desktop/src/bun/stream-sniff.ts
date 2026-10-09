// Stream sniffer (desktop only).
//
// The daemon asks us (over LOCAL NATS, PLATFORM_BROWSERSNIFF_SUBJECT) to load a
// page the user asked to record on a site VodLabs has no support for, and to
// report the stream requests its player makes: what a person would read off
// the browser's network panel. Most such pages build their player in
// JavaScript, so the stream's address appears nowhere in the HTML.
//
// A script injected at document start into every frame (embedded players are
// usually another site's iframe) watches fetch, XHR, media element sources and
// the page's resource timing, nudges paused videos to play, and posts what it
// sees over Electrobun's event bridge. WebKit plays HLS natively, so there the
// playlist never passes through fetch; the media element hooks are what catch
// it.
//
// PRIVACY: the page loads in an in-memory partition of its own, wiped before
// every sniff, never in a platform login partition. Only stream addresses, the
// frames that requested them and the page title leave this file.
//
// Lifecycle: a window of its own, never the app's, reused and parked on
// about:blank between sniffs, which subscribeRequest already hands over one at
// a time, and closed once idle. It is shown but placed beyond every display:
// WebKit treats a hidden window's page as hidden, and embedded players in it
// never start. It is built hidden and borderless at that spot, so it never
// appears on any screen.
//
// SECURITY: the page is one the user pasted, but it is still arbitrary web
// content running unseen. The view is sandboxed (no RPC bridge, events only),
// so all it can do is report hits, which rank() checks. WebKit saves any
// download a page starts straight into ~/Downloads, unasked; the sniff ends,
// and the file is removed, the moment one starts.

import { unlink } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { BrowserWindow, BrowserView, Screen, Session } from "electrobun/main";
import type { DaemonSession } from "../../../shared/daemon/session";
import { PLATFORM_BROWSERSNIFF_SUBJECT } from "../../../shared/daemon/subjects";
import { addAllFramesPreload } from "./all-frames-preload";
import { createPartitionedWindow } from "./partitioned-window";
import { type Candidate, type Hit, isLiveEnough, rank } from "./stream-sniff-rank";
import { NUDGE, SNIFFER } from "./stream-sniff-script";

type SniffRequest = { url: string; timeout_ms?: number };
type SniffResponse = { title?: string; candidates: Candidate[]; error?: string };

// No "persist:" prefix: Electrobun keeps such a partition in memory only.
const PARTITION = "vlsniff";
const BLANK_URL = "about:blank";
const FRAME = { x: 0, y: 0, width: 1280, height: 720 };
// The longest a sniff watches a page. The longest a page took to request its
// stream in testing was 12.6s from a cold view.
const MAX_WATCH_MS = 15_000;
// How long a sniff keeps watching once a live stream turned up, for the master
// playlist or a better candidate to follow it.
const SETTLE_MS = 2_000;
// When the host itself pokes the page's players. Calls from the host count as
// a user's gesture, which a page script's own play() does not.
const FIRST_NUDGE_MS = 2_500;
// Cap on hits kept per sniff, against a page that floods the bridge.
const MAX_HITS = 400;
// How long the window outlives its last sniff. An add's preflight and its
// first check arrive within seconds of each other; nothing else needs it.
const IDLE_CLOSE_MS = 60_000;

// The sniff in progress, fed by the view's host-message handler.
type Watch = { hits: Hit[]; title: string; changed: () => void; aborted: string };

// ready settles once SNIFFER reaches every frame the view loads next.
type SniffContext = { win: BrowserWindow; view: BrowserView; watch: Watch | null; ready: Promise<unknown> };

let ctx: SniffContext | null = null;
let idleTimer: ReturnType<typeof setTimeout> | null = null;

// offScreenFrame places the window left of every display, so it is on none.
function offScreenFrame(): typeof FRAME {
  let left = 0;
  let top = 0;
  try {
    for (const d of Screen.getAllDisplays()) {
      left = Math.min(left, d.bounds.x);
      top = Math.min(top, d.bounds.y);
    }
  } catch {
    /* no display list: left of the primary display's origin still works */
  }
  return { ...FRAME, x: left - FRAME.width - 200, y: top + 100 };
}

// context returns the sniffing view, building it on first use. Handlers are
// attached once, here: electrobun's on() has no off().
function context(): SniffContext {
  if (idleTimer) clearTimeout(idleTimer);
  idleTimer = null;
  if (ctx && !ctx.view.isRemoved) return ctx;
  const frame = offScreenFrame();
  const { win, view } = createPartitionedWindow(
    {
      title: "VodLabs page check",
      frame,
      url: BLANK_URL,
      sandbox: true,
      preload: SNIFFER,
      hidden: true,
      // Borderless: AppKit pulls a titled window back onto a screen when it is
      // first shown, and only a titled one can join a tab group.
      styleMask: { Borderless: true, Titled: false, Closable: false, Miniaturizable: false, Resizable: false },
    },
    PARTITION,
  );
  // Placed before it is first shown, and shown without taking focus.
  win.setPosition(frame.x, frame.y);
  win.showInactive();
  const c: SniffContext = { win, view, watch: null, ready: addAllFramesPreload(view, SNIFFER) };
  const onDownload = (e: unknown) => {
    const path = (e as { data?: { detail?: unknown } })?.data?.detail;
    const file = typeof path === "string" ? safeParse(path)?.path : (path as { path?: string })?.path;
    console.warn(`[stream-sniff] page started a download; removing it file=${file ?? "?"}`);
    if (c.watch) c.watch.aborted = "the page tried to download a file";
    try {
      c.view.loadURL(BLANK_URL);
    } catch {
      /* the view went away */
    }
    if (typeof file === "string" && file) removeDownload(file);
  };
  for (const name of ["download-started", "download-completed", "download-failed"]) {
    (view.on as (name: string, handler: (e: unknown) => void) => void)(name, onDownload);
  }
  (view.on as (name: string, handler: (e: unknown) => void) => void)("host-message", (e) => {
    const detail = (e as { data?: { detail?: unknown } })?.data?.detail as
      | { vlSniff?: number; hit?: Hit & { title?: string } }
      | undefined;
    const w = c.watch;
    if (!w || !detail?.vlSniff || !detail.hit) return;
    const hit = detail.hit;
    if (hit.kind === "title") {
      w.title = String(hit.title ?? "").slice(0, 300);
      return;
    }
    if (typeof hit.url !== "string" || w.hits.length >= MAX_HITS) return;
    w.hits.push({
      kind: String(hit.kind),
      url: hit.url,
      frame: String(hit.frame ?? ""),
      master: hit.master === true,
      ended: hit.ended === true,
      invalid: hit.invalid === true,
    });
    w.changed();
  });
  win.on("close", () => {
    if (ctx === c) ctx = null;
  });
  ctx = c;
  return c;
}

// closeWhenIdle closes the window once no sniff has used it for a while.
function closeWhenIdle(): void {
  if (idleTimer) clearTimeout(idleTimer);
  idleTimer = setTimeout(() => {
    idleTimer = null;
    const c = ctx;
    if (!c || c.watch) return;
    ctx = null;
    try {
      c.win.close();
    } catch {
      /* already gone */
    }
  }, IDLE_CLOSE_MS);
}

function safeParse(s: string): { path?: string } | null {
  try {
    return JSON.parse(s);
  } catch {
    return null;
  }
}

// removeDownload deletes a file a sniffed page downloaded. The path comes from
// native code (a page cannot forge the event; see patches/README.md), and
// WebKit names each download afresh rather than overwrite, so it is only ever
// that file.
// It is removed again shortly after, in case it was not written yet.
function removeDownload(file: string): void {
  if (dirname(resolve(file)) !== join(homedir(), "Downloads")) {
    console.warn(`[stream-sniff] not removing a download outside ~/Downloads file=${file}`);
    return;
  }
  const attempt = () => unlink(file).catch(() => {});
  void attempt();
  setTimeout(attempt, 2_000);
}

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

// sniff watches one page until a live stream has settled or the time is up,
// and returns what it found, best first.
async function sniff(req: SniffRequest): Promise<SniffResponse> {
  let page: URL;
  try {
    page = new URL(req.url);
  } catch {
    return { candidates: [], error: "invalid url" };
  }
  if (page.protocol !== "http:" && page.protocol !== "https:") {
    return { candidates: [], error: "invalid url" };
  }
  // The daemon's own deadline is the request's; finish inside it.
  const budget = Math.min(MAX_WATCH_MS, Math.max((req.timeout_ms ?? 20_000) - 3_000, 5_000));

  let c: SniffContext;
  try {
    c = context();
  } catch (err) {
    return { candidates: [], error: "browser unavailable: " + String(err) };
  }
  try {
    Session.fromPartition(PARTITION).clearStorageData("all");
  } catch {
    /* a fresh partition has nothing to clear */
  }

  await c.ready;
  const start = Date.now();
  let liveAt = 0;
  let wake: () => void = () => {};
  const watch: Watch = {
    hits: [],
    title: "",
    aborted: "",
    changed: () => {
      if (!liveAt && isLiveEnough(watch.hits)) {
        liveAt = Date.now();
        wake();
      }
    },
  };
  c.watch = watch;
  console.log(`[stream-sniff] sniffing host=${page.host}`);
  try {
    c.view.loadURL(page.href);
    let nudged = 0;
    const halfway = budget / 2;
    for (;;) {
      const elapsed = Date.now() - start;
      if (watch.aborted) break;
      if (liveAt && Date.now() - liveAt >= SETTLE_MS) break;
      if (elapsed >= budget) break;
      if ((nudged === 0 && elapsed >= FIRST_NUDGE_MS) || (nudged === 1 && elapsed >= halfway && !liveAt)) {
        nudged++;
        c.view.executeJavascript(NUDGE);
      }
      await Promise.race([sleep(250), new Promise<void>((r) => (wake = r))]);
    }
    c.view.executeJavascript("window.__vlSniffFinal && window.__vlSniffFinal()");
    await sleep(500);
  } finally {
    c.watch = null;
    try {
      c.view.loadURL(BLANK_URL);
    } catch {
      /* the view went away; context() builds another next time */
    }
    closeWhenIdle();
  }
  if (watch.aborted) {
    console.warn(`[stream-sniff] aborted host=${page.host} reason=${watch.aborted}`);
    return { candidates: [], error: watch.aborted };
  }
  const candidates = rank(watch.hits);
  console.log(
    `[stream-sniff] done host=${page.host} hits=${watch.hits.length} candidates=${candidates.length}` +
      ` live=${liveAt ? liveAt - start : -1}ms elapsed=${Date.now() - start}ms`,
  );
  return { title: watch.title, candidates };
}

// subscribeStreamSniff wires the LOCAL request-reply handler. Call on every
// connect: subscriptions live on the current connection.
export function subscribeStreamSniff(session: DaemonSession): () => void {
  return session.subscribeRequest<SniffRequest, SniffResponse>(PLATFORM_BROWSERSNIFF_SUBJECT, (req) =>
    sniff(req).catch((err) => ({ candidates: [], error: String(err) })),
  );
}
