import { setDaemonClient } from "@/lib/daemon";
import { WebDaemonClient } from "@/lib/daemon/web-client";

// Install the browser DaemonClient (side effect on import).
//
// The daemon serves this page and bridges its API on the relative `nats`
// path, so the socket follows whatever address and subpath the page was opened
// on (localhost, a LAN address, a tailnet name, a reverse proxy's /recorder/)
// with nothing to configure. The dev server proxies the same path (see
// web/vite.config.ts).
setDaemonClient(new WebDaemonClient({ base: location.href }));
