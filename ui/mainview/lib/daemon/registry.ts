import type { DaemonClient } from "./client";

// The process-wide DaemonClient, injected by the entry point before the app
// renders: the desktop entry installs the Electrobun client
// (electrobun-client.ts), the web entry installs the WebDaemonClient. Keeping
// the choice in the entry point means neither implementation is pulled into
// the other bundle.
let client: DaemonClient | null = null;

export function setDaemonClient(c: DaemonClient): void {
  client = c;
}

export function getDaemonClient(): DaemonClient {
  if (!client) {
    throw new Error(
      "DaemonClient not installed — the entry point must import its client " +
        "module (e.g. lib/daemon/electrobun-client) before the App module",
    );
  }
  return client;
}
