// The showcase harness's entry: the desktop app's React UI, connected to a Demo
// daemon over NATS WebSocket instead of to vlrecd through the Electrobun
// process. The socket URL comes from ?nats=, defaulting to the Demo daemon's.
import { setDaemonClient } from "@/lib/daemon";
import { ShowcaseDaemonClient } from "./showcase-client";

const params = new URLSearchParams(location.search);
setDaemonClient(
  new ShowcaseDaemonClient({
    servers: params.get("nats") ?? "ws://127.0.0.1:14223",
    appVersion: params.get("app_version") ?? "1.0.0",
  }),
);

// After the client is installed: some modules resolve it at module scope.
const { StrictMode } = await import("react");
const { createRoot } = await import("react-dom/client");
await import("@/index.css");
const { default: App } = await import("@/App");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
