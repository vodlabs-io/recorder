// Must precede the react-dom import: disables React's dev performance track,
// which leaks memory in the Electrobun WKWebView. See the module for details.
import "./lib/disable-react-perf-track";
// Install the Electrobun DaemonClient. Must precede the App import: some
// modules resolve the client at module scope (quit-dialog, dashboard columns).
import "./lib/daemon/electrobun-client";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import App from "./App";
import { reactErrorHandler } from "./lib/error-reports";

createRoot(document.getElementById("root")!, {
	onUncaughtError: reactErrorHandler(),
	onCaughtError: reactErrorHandler(),
	onRecoverableError: reactErrorHandler(),
}).render(
	<StrictMode>
		<App />
	</StrictMode>,
);
