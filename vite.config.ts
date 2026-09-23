import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import { fileURLToPath } from "url";
import { dirname } from "path";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

export default defineConfig({
	plugins: [react(), tailwindcss()],
	root: "./ui/mainview",
	resolve: {
		alias: {
			"@": path.resolve(__dirname, "./ui/mainview"),
			// Wire types + daemon engine shared across desktop, webview and web.
			"@shared": path.resolve(__dirname, "./shared"),
		},
	},
	build: {
		outDir: "./dist",
		emptyOutDir: true,
	},
	server: {
		port: 5173,
		strictPort: true,
		// Allow importing @shared (top-level shared/) from outside the ui root.
		fs: {
			allow: [path.resolve(__dirname)],
		},
	},
});
