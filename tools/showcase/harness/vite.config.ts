import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import { fileURLToPath } from "url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "../../..");

// The desktop app's UI (ui/mainview) served from this directory's entry, which
// connects it to a Demo daemon. Port 5175, beside the desktop HMR server (5173).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  root: here,
  resolve: {
    alias: {
      "@": path.resolve(repoRoot, "ui/mainview"),
      "@shared": path.resolve(repoRoot, "shared"),
    },
  },
  build: {
    outDir: "./dist",
    emptyOutDir: true,
    target: "esnext",
  },
  server: {
    host: "127.0.0.1",
    port: 5175,
    strictPort: true,
    fs: { allow: [repoRoot] },
  },
});
