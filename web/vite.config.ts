import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import fs from "fs";
import path from "path";
import { fileURLToPath } from "url";
import { dirname } from "path";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const repoRoot = path.resolve(__dirname, "..");

// Web UI build. Serves the same React app as the desktop webview
// (ui/mainview) from this directory's entry, which installs the WebDaemonClient
// (NATS over the daemon's WebSocket bridge) instead of the Electrobun client.
// Unlike the desktop config it needs no Electrobun aliases: nothing it bundles
// imports electrobun/*.
//
// The daemon embeds web/dist with //go:embed, which fails to compile when the
// directory has no files. emptyOutDir wipes the tracked placeholder on every
// build, so put it back and keep a fresh checkout buildable without bun.
const keepEmbedPlaceholder = {
	name: "keep-embed-placeholder",
	closeBundle() {
		const dist = path.resolve(__dirname, "dist");
		fs.mkdirSync(dist, { recursive: true });
		fs.writeFileSync(
			path.join(dist, ".gitkeep"),
			"Keeps web/dist in git so //go:embed in ../embed.go always matches something.\n" +
				"`bun run web:build` overwrites this directory with the real build.\n",
		);
	},
};

// The daemon's Web UI listener, which the dev server proxies to.
const daemon = process.env.VLREC_WEB_DEV_TARGET ?? "http://127.0.0.1:8080";

export default defineConfig({
	plugins: [react(), tailwindcss(), keepEmbedPlaceholder],
	root: __dirname,
	// Relative asset URLs, so the build works under a reverse proxy's subpath.
	base: "./",
	resolve: {
		alias: {
			"@": path.resolve(repoRoot, "ui/mainview"),
			// Wire types + daemon engine shared across desktop, webview and web.
			"@shared": path.resolve(repoRoot, "shared"),
		},
	},
	build: {
		// web/dist — never touches ui/mainview/dist (the desktop package input).
		outDir: "./dist",
		emptyOutDir: true,
	},
	server: {
		host: "127.0.0.1",
		// 5174 so it runs alongside the desktop HMR dev server (5173).
		port: 5174,
		strictPort: true,
		// The daemon's own routes. The Host header is passed through, so the
		// daemon's same-origin checks see the dev server's origin as its own.
		proxy: {
			"/nats": { target: daemon, ws: true },
			"/api": { target: daemon },
			"/signin": { target: daemon },
		},
		// Allow importing @/ (ui/mainview) and @shared from outside the web root.
		fs: {
			allow: [repoRoot],
		},
	},
});
