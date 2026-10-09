import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import { fileURLToPath } from "url";
import { dirname } from "path";
import { electrobunViteAliases } from "./.hutch/devkit/api/config/electrobun-vite";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

export default defineConfig({
	plugins: [react(), tailwindcss()],
	root: "./ui/mainview",
	resolve: {
		alias: [
			{ find: "@", replacement: path.resolve(__dirname, "./ui/mainview") },
			// Wire types + daemon engine shared across desktop, webview and web.
			{ find: "@shared", replacement: path.resolve(__dirname, "./shared") },
			// electrobun/view, from the devkit Hutch projects (`electrobun prepare`).
			...electrobunViteAliases(path.resolve(__dirname, ".hutch/devkit")),
		],
	},
	build: {
		outDir: "./dist",
		emptyOutDir: true,
	},
	server: {
		// Bound to IPv4 only, where the app looks for it: on "localhost" another
		// server can hold 5173 on the other address family unnoticed by strictPort.
		host: "127.0.0.1",
		port: 5173,
		strictPort: true,
		// Allow importing @shared (top-level shared/) from outside the ui root.
		fs: {
			allow: [path.resolve(__dirname)],
		},
	},
});
