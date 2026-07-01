import path from "node:path";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// The dev server proxies /api to a locally running dashboard binary
// (ORKANO_ADDR defaults to :8080), so the SPA talks to the real Go API in
// development. Production has no Node at all: `make web` builds dist/ and the
// webdist-tagged Go binary embeds it (dashboard/web/web_dist.go).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "./src") },
  },
  server: {
    proxy: { "/api": "http://localhost:8080" },
  },
});
