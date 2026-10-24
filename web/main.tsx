// Install the web DaemonClient. Must precede the App import: some modules
// resolve the client at module scope (quit-dialog, dashboard columns).
// (The desktop entry's disable-react-perf-track shim is a WKWebView-specific
// workaround and isn't needed in a real browser.)
import "./install-daemon-client";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@/index.css";
import App from "@/App";

createRoot(document.getElementById("root")!).render(
	<StrictMode>
		<App />
	</StrictMode>,
);
