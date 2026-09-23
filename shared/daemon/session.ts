import type { ConnectionOptions } from "@nats-io/nats-core";
import type { ConnectionInfo, StreamersSnapshot } from "../types";
import { DaemonConnection, type ConnectFn, type KvWatcher } from "./connection";
import { StreamerState } from "./streamer-state";
import { VODS_REV_KEY } from "./subjects";

// DaemonSession is the platform-free heart of a GUI client's daemon
// connection: it owns the NATS connection, mirrors the daemon's KV stream
// (view docs, session stats, daemon counters) into a StreamerState, keeps the
// mirror correct across daemon restarts (reconnect resync + key pruning), and
// delivers debounced snapshots. The Electrobun bun process and the Web UI both
// drive their UIs from it; only the transport (node connect vs wsconnect) and
// the event sinks differ.
//
// This module must stay runtime-agnostic: no Bun, node, electrobun, or DOM
// APIs.

export interface DaemonSessionEvents {
  // Debounced, deduped snapshot delivery. Return false when the snapshot was
  // NOT delivered (e.g. the webview transport isn't ready yet) so the same
  // snapshot is re-sent on the next change instead of being skipped as a
  // duplicate.
  onSnapshot?: (snap: StreamersSnapshot) => boolean | void;
  onConnectionChanged?: (info: ConnectionInfo) => void;
  // Fired for every KV entry that actually changed state, from the watch loop
  // itself — independent of visibility gating (the desktop's native update
  // notification must fire even while the window is closed to the tray).
  onKvEntryChanged?: (key: string) => void;
  // Raw NATS disconnect. Fires even after beginShutdown() — the desktop quit
  // flow exits the app on the disconnect that follows a graceful shutdown.
  onDisconnect?: () => void;
}

// shouldNotifyKvChange decides whether a KV watch entry is dispatched as a
// change event. Normally only entries that actually changed state qualify: the
// daemon re-writes counter and heartbeat keys with their existing values, and
// waking every listener for those is pure noise.
//
// The VOD Archive rev is the exception, because it is written as a signal
// rather than as a value. A scan that finishes having found nothing re-writes
// the same rev deliberately — that write is the only thing telling a client its
// walk is over. Deduping it by value swallows the one message it exists to
// send, and the Scanning indicator stays up until something unrelated (a window
// focus) happens to refresh.
export function shouldNotifyKvChange(key: string, stateChanged: boolean): boolean {
  return stateChanged || key === VODS_REV_KEY;
}

export interface DaemonSessionOptions {
  // Transport-specific NATS connect function (node connect / browser wsconnect).
  // Ignored when `connection` is provided.
  connect?: ConnectFn;
  // Pre-built connection variant (e.g. RemoteDaemonConnection for the hosted
  // dashboard's cloud path). When omitted, a direct DaemonConnection is built
  // from `connect`.
  connection?: DaemonConnection;
  // Merged over the connection defaults (client name, future authenticator…).
  connectOptions?: Partial<ConnectionOptions>;
  events?: DaemonSessionEvents;
  // KV key-spaces the snapshot is built from. The same set drives the live
  // watches and the reconnect keyset reconciliation.
  kvFilters?: string[];
  pushDebounceMs?: number;
}

// platformlogin.> carries the local platform-login status for the Accounts UI,
// and encode.> the encode queue's rev/progress keys.
// PRIVACY: this filter only affects LOCAL clients (desktop bun / Web UI). The
// Dashboard bridge has its own filter (streamer.*/daemon.* only, in
// internal/cloud/bridge.go), so widening this does NOT expose login or encode
// state to the cloud.
const DEFAULT_KV_FILTERS = ["streamer.>", "daemon.>", "platformlogin.>", "encode.>"];
const DEFAULT_PUSH_DEBOUNCE_MS = 150;

export class DaemonSession {
  private readonly conn: DaemonConnection;
  private readonly state = new StreamerState();
  private readonly events: DaemonSessionEvents;
  private readonly connectOptions?: Partial<ConnectionOptions>;
  private readonly kvFilters: string[];
  private readonly pushDebounceMs: number;

  // Active KV watchers, tracked so they can be torn down and re-established
  // when the daemon restarts (its store — and our watch consumers — are
  // recreated).
  private kvWatchers: KvWatcher[] = [];
  // The connection-status loop runs once for the connection's lifetime.
  private statusLoopStarted = false;
  private pushTimer: ReturnType<typeof setTimeout> | null = null;
  // Serialized form of the last snapshot successfully delivered. Used to skip
  // no-op pushes so a busy-but-idle daemon doesn't re-render the UI.
  private lastPushedJson: string | null = null;
  // Whether the consumer can currently render (e.g. the desktop webview's Page
  // Visibility). While hidden, snapshot delivery pauses and the latest state is
  // flushed once on becoming visible.
  private visible = true;
  // Latched by beginShutdown(): the status loop stops driving the reconnect
  // banner/resync, but onDisconnect still fires.
  private shuttingDown = false;

  private resyncing = false;
  private resyncPending = false;

  constructor(opts: DaemonSessionOptions) {
    if (!opts.connection && !opts.connect) {
      throw new Error("DaemonSession needs a connection or a connect function");
    }
    this.conn = opts.connection ?? new DaemonConnection(opts.connect!);
    this.connectOptions = opts.connectOptions;
    this.events = opts.events ?? {};
    this.kvFilters = opts.kvFilters ?? DEFAULT_KV_FILTERS;
    this.pushDebounceMs = opts.pushDebounceMs ?? DEFAULT_PUSH_DEBOUNCE_MS;
  }

  // connect makes a single dial attempt and throws on failure. Callers own the
  // retry/spawn policy (the desktop may spawn a daemon; the Web UI just
  // reports disconnected).
  connect(servers: string | string[]): Promise<void> {
    return this.conn.connect(servers, this.connectOptions);
  }

  // start marks the connection live and begins mirroring: emits the connected
  // state, establishes the KV watches, and starts the status loop. Call once
  // after each successful connect().
  async start(): Promise<void> {
    this.emitConnection({ state: "connected" });
    await this.startKvWatches();
    this.startStatusLoop();
  }

  // reset tears the session down for a fresh connect() (the retry path):
  // closes the connection, stops the watches, and drops the KV mirror so
  // stale rows don't linger. Also clears shutdown mode — a reset is by
  // definition the prelude to reconnecting, and the daemon-update restart
  // shuts the daemon down and then comes back on the same session.
  async reset(): Promise<void> {
    await this.conn.close();
    this.stopKvWatches();
    this.statusLoopStarted = false;
    this.shuttingDown = false;
    this.state.clearKv();
  }

  // beginShutdown latches shutdown mode: subsequent disconnects no longer
  // drive the reconnect banner or resync (onDisconnect still fires so the
  // caller can exit). Call before requesting the daemon shutdown so an
  // instant disconnect can't race into the reconnect path. Cleared by reset().
  beginShutdown(): void {
    this.shuttingDown = true;
  }

  snapshot(): StreamersSnapshot {
    return this.state.snapshot();
  }

  /** Sends a micro request and returns the unwrapped data, throwing on error. */
  request<T>(subject: string, payload?: unknown, timeoutMs?: number): Promise<T> {
    return this.conn.request<T>(subject, payload, timeoutMs);
  }

  // publish sends one fire-and-forget message on a plain LOCAL NATS subject
  // (not a micro endpoint). Nothing waits for it and nothing replies, so a
  // subject with no subscriber is not an error — it is the ordinary case for an
  // announcement the daemon may or may not be listening for.
  publish(subject: string, payload?: unknown): void {
    this.conn.connection.publish(
      subject,
      payload === undefined ? undefined : JSON.stringify(payload),
    );
  }

  // subscribeJSON subscribes to a plain local NATS subject (not a micro
  // endpoint) and invokes cb with each JSON-decoded message. Returns an
  // unsubscribe function. The subscription lives on the CURRENT connection:
  // after reset()+connect() (daemon-restart recovery) callers must subscribe
  // again — do it wherever start() is called.
  subscribeJSON<T>(subject: string, cb: (msg: T) => void): () => void {
    const sub = this.conn.connection.subscribe(subject);
    void (async () => {
      for await (const m of sub) {
        try {
          cb(m.json<T>());
        } catch (err) {
          console.error(`subscription (${subject}) handler failed:`, err);
        }
      }
    })();
    return () => sub.unsubscribe();
  }

  // subscribeRequest subscribes to a request-reply LOCAL NATS subject: it
  // JSON-decodes each request, awaits the handler, and replies with the handler's
  // JSON result (or an { error } object on throw). Like subscribeJSON the
  // subscription lives on the CURRENT connection, so re-subscribe after a
  // reset()+connect(). Returns an unsubscribe function.
  subscribeRequest<Req, Res>(
    subject: string,
    handler: (req: Req) => Promise<Res> | Res,
  ): () => void {
    const sub = this.conn.connection.subscribe(subject);
    void (async () => {
      for await (const m of sub) {
        let reply: unknown;
        try {
          reply = await handler(m.json<Req>());
        } catch (err) {
          console.error(`request handler (${subject}) failed:`, err);
          reply = { error: err instanceof Error ? err.message : String(err) };
        }
        if (m.reply) {
          try {
            m.respond(JSON.stringify(reply));
          } catch (err) {
            console.error(`request reply (${subject}) failed:`, err);
          }
        }
      }
    })();
    return () => sub.unsubscribe();
  }

  // setVisible reacts to the consumer's visibility transitions: on becoming
  // visible it flushes the state accumulated while hidden in a single
  // delivery; on becoming hidden it cancels any queued push so nothing
  // buffers.
  setVisible(visible: boolean): void {
    if (this.visible === visible) return;
    this.visible = visible;
    if (visible) {
      this.pushSnapshot();
    } else if (this.pushTimer) {
      clearTimeout(this.pushTimer);
      this.pushTimer = null;
    }
  }

  private emitConnection(info: ConnectionInfo): void {
    this.events.onConnectionChanged?.(info);
  }

  // pushSnapshot delivers the current snapshot, skipping no-op pushes. The
  // dedupe key only latches when the consumer reports successful delivery, so
  // a snapshot dropped by a not-yet-ready transport is re-sent later.
  private pushSnapshot(): void {
    const snap = this.state.snapshot();
    const json = JSON.stringify(snap);
    if (json === this.lastPushedJson) return; // nothing user-visible changed
    if (this.events.onSnapshot?.(snap) !== false) {
      this.lastPushedJson = json;
    }
  }

  private schedulePush(): void {
    // Don't buffer pushes a hidden consumer can't render; setVisible flushes
    // the latest snapshot once it becomes visible again.
    if (!this.visible || this.pushTimer) return;
    this.pushTimer = setTimeout(() => {
      this.pushTimer = null;
      this.pushSnapshot();
    }, this.pushDebounceMs);
  }

  private stopKvWatches(): void {
    for (const w of this.kvWatchers) {
      try {
        w.stop();
      } catch {
        /* already stopped */
      }
    }
    this.kvWatchers = [];
  }

  // startKvWatches (re)establishes the KV watchers. Any existing watchers are
  // stopped first, so a consumer left invalid by a daemon restart doesn't
  // linger. Each new watcher replays the bucket's current values (as PUTs), so
  // the cached KV self-heals; a reconnect caller follows with
  // resyncAfterReconnect() to also prune keys the rebuilt store no longer has.
  private async startKvWatches(): Promise<void> {
    this.stopKvWatches();
    const kv = await this.conn.openKV();
    for (const key of this.kvFilters) {
      const watcher = await kv.watch({ key });
      this.kvWatchers.push(watcher);
      void (async () => {
        for await (const e of watcher) {
          const value = e.operation === "PUT" ? safeString(e) : undefined;
          // Only push a snapshot when the entry actually changed state; the
          // daemon re-writes counter/heartbeat keys with unchanged values.
          const changed = this.state.applyKvEntry(e.key, e.operation, value);
          if (changed) {
            this.schedulePush();
          }
          if (shouldNotifyKvChange(e.key, changed)) {
            this.events.onKvEntryChanged?.(e.key);
          }
        }
      })().catch((err) => console.error(`KV watch (${key}) ended:`, err));
    }
  }

  // startStatusLoop drives connection-state transitions from NATS status
  // events. It runs once for the connection's lifetime (until conn.close()).
  private startStatusLoop(): void {
    if (this.statusLoopStarted) return;
    this.statusLoopStarted = true;
    void (async () => {
      for await (const s of this.conn.status()) {
        if (s.type === "disconnect") this.events.onDisconnect?.();
        // During a graceful shutdown the reconnect machinery must stay quiet:
        // the caller decides what a disconnect means (the desktop exits).
        if (this.shuttingDown) continue;
        if (s.type === "disconnect") {
          this.emitConnection({
            state: "connecting",
            message: "Reconnecting to daemon…",
          });
        } else if (s.type === "reconnect") {
          this.emitConnection({ state: "connected" });
          this.resyncAfterReconnect();
        }
      }
    })().catch(() => {});
  }

  // resyncAfterReconnect recovers from a daemon restart. The old watch
  // consumers die with the daemon's JetStream store, and the cached KV may
  // name keys the rebuilt store no longer has. It re-establishes the watchers
  // (replaying current values into the cache), prunes any cached key that's
  // now gone, and lets the debounced push surface the result. Pruning against
  // the live keyset — rather than clearing first — keeps the existing rows
  // visible throughout, so the table doesn't flash empty.
  //
  // Concurrent reconnect events are coalesced into a single trailing run so
  // racing resyncs can't fight over the watcher set.
  private resyncAfterReconnect(): void {
    this.resyncPending = true;
    if (this.resyncing) return;
    this.resyncing = true;
    void (async () => {
      try {
        while (this.resyncPending) {
          this.resyncPending = false;
          await this.runResync();
        }
      } finally {
        this.resyncing = false;
      }
    })();
  }

  // runResync performs one resync attempt, retrying with a short backoff. A
  // reconnect can fire before the restarted daemon has recreated its KV bucket
  // or API service, so the first few attempts may fail; keep trying for a
  // while.
  private async runResync(): Promise<void> {
    for (let attempt = 0; attempt < 20; attempt++) {
      try {
        await this.startKvWatches();
        if (this.state.retainKv(await this.currentKvKeys())) {
          this.schedulePush();
        }
        return;
      } catch (err) {
        console.error("KV resync after reconnect failed; retrying:", err);
        await sleep(500);
      }
    }
    console.error("KV resync after reconnect gave up after repeated failures");
  }

  // currentKvKeys returns the daemon's current key set across the KV filters —
  // the authoritative membership used to prune stale cached keys after a
  // reconnect.
  private async currentKvKeys(): Promise<Set<string>> {
    const kv = await this.conn.openKV();
    const keys = new Set<string>();
    for (const filter of this.kvFilters) {
      const iter = await kv.keys(filter);
      for await (const k of iter) keys.add(k);
    }
    return keys;
  }
}

function safeString(e: { string(): string }): string | undefined {
  try {
    return e.string();
  } catch {
    return undefined;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
