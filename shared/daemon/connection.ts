import type {
  ConnectionOptions,
  NatsConnection,
} from "@nats-io/nats-core";
import { Kvm } from "@nats-io/kv";

// SessionKV is the slice of the KV surface DaemonSession actually uses: live
// watches (replaying current values as PUTs) plus the authoritative keyset
// for reconnect pruning. The real @nats-io/kv KV satisfies it structurally;
// RemoteDaemonConnection substitutes a bridge-backed implementation (see
// remote.ts).
export interface SessionKvEntry {
  key: string;
  operation: "PUT" | "DEL" | "PURGE";
  string(): string;
}

export interface KvWatcher extends AsyncIterable<SessionKvEntry> {
  stop(): void;
}

export interface SessionKV {
  watch(opts: { key: string }): Promise<KvWatcher>;
  keys(filter: string): Promise<AsyncIterable<string>>;
}

// ConnectFn is the transport-specific NATS connect function: `connect` from
// @nats-io/transport-node in the bun process, `wsconnect` from
// @nats-io/nats-core in the browser. Injected so this module stays
// platform-free.
export type ConnectFn = (opts: ConnectionOptions) => Promise<NatsConnection>;

// DaemonError is a daemon's refusal, with the code it gave. The code does not
// survive the desktop app's RPC boundary (see request), so a handler on the bun
// side that needs it reads it here.
export class DaemonError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "DaemonError";
  }
}

type Envelope<T> = {
  ok: boolean;
  data?: T;
  error?: { code: string; message: string };
};

/**
 * DaemonConnection wraps the NATS connection to the daemon: typed micro requests
 * (unwrapping the {ok,data,error} envelope) and access to the streamers KV
 * bucket. The connection auto-reconnects; status events drive the UI's
 * connection indicator.
 */
export class DaemonConnection {
  private nc?: NatsConnection;

  constructor(private readonly connectFn: ConnectFn) {}

  // connect dials the daemon. Caller opts (client name, future cloud
  // authenticator, …) are merged over the defaults.
  async connect(
    servers: string | string[],
    opts?: Partial<ConnectionOptions>,
  ): Promise<void> {
    this.nc = await this.connectFn({
      servers,
      name: "vodlabs-desktop",
      maxReconnectAttempts: -1,
      reconnectTimeWait: 1000,
      timeout: 3000,
      ...opts,
    });
  }

  get connection(): NatsConnection {
    if (!this.nc) throw new Error("not connected to daemon");
    return this.nc;
  }

  /**
   * Sends a micro request and returns the unwrapped data, throwing on error.
   *
   * timeoutMs raises the default ceiling for the few endpoints that make a
   * cloud round-trip before answering. Their ceiling must
   * stay ABOVE the daemon's own, so a slow answer surfaces as the daemon's
   * message ("no such account") rather than a bare client-side timeout.
   */
  async request<T>(
    subject: string,
    payload?: unknown,
    timeoutMs = 5000,
  ): Promise<T> {
    const data = payload === undefined ? undefined : JSON.stringify(payload);
    const msg = await this.connection.request(subject, data, {
      timeout: timeoutMs,
    });
    const env = msg.json<Envelope<T>>();
    if (!env.ok) {
      // Message only: this crosses an Electrobun RPC boundary on the desktop
      // app, which does not carry custom Error properties, so anything a client
      // needs to SHOW has to already be in the message. The code stays on the
      // wire for logs and for callers on this side of that boundary.
      throw new DaemonError(
        env.error?.code ?? "",
        env.error?.message || env.error?.code || "request failed",
      );
    }
    return env.data as T;
  }

  openKV(bucket = "streamers"): Promise<SessionKV> {
    return new Kvm(this.connection).open(bucket);
  }

  /** Async iterable of connection status events (disconnect/reconnect/…). */
  status() {
    return this.connection.status();
  }

  async close(): Promise<void> {
    if (!this.nc) return;
    try {
      await this.nc.close();
    } catch {
      /* ignore */
    }
    this.nc = undefined;
  }
}
