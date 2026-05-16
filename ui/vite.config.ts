import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Dev: vite serves on :5173 and proxies /api to Go on :7777.
// Prod: `npm run build` writes to ../cmd/zeno/ui-dist/ where Go's embed.FS
// picks it up at compile time.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: path.resolve(__dirname, "../cmd/zeno/ui-dist"),
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:7777",
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
