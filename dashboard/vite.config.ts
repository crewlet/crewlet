// How the dashboard becomes bytes the engine binary embeds.
//
// `outDir` is ../static/dashboard, the tree package `static` embeds with
// `//go:embed all:dashboard`, and the build output is COMMITTED. That is not
// laziness about a .gitignore: `go build ./...` and `go install ...@latest`
// must work on a clean checkout with no Node on the machine, and an embed
// directive cannot run a bundler. CI rebuilds and diffs the tree
// (.github/workflows/ci.yml, the `dashboard` job) so a committed bundle that
// does not match this source is a red build rather than a silent lie — the
// same idiom `go mod tidy -diff` and the generated `schema/` already use.
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath } from "node:url";

export default defineConfig({
  plugins: [react()],
  // The engine serves this tree from /static/dashboard/ and answers the shell
  // at both `/` and `/dashboard`. A relative base would resolve the shell's
  // own asset URLs against whichever of those the reader arrived at; an
  // absolute one resolves to the same files either way.
  base: "/static/dashboard/",
  resolve: {
    alias: { "~": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: fileURLToPath(new URL("../static/dashboard", import.meta.url)),
    emptyOutDir: true,
    // The tree is committed and diffed by CI, so the build has to be
    // reproducible. Content hashes already are; what is not is a sourcemap
    // carrying absolute paths from the machine that built it.
    sourcemap: false,
    target: "es2022",
    assetsDir: "assets",
    rollupOptions: {
      output: {
        // One vendor chunk, so a change to our own code does not invalidate
        // React in every reader's cache — and so the committed diff of an
        // ordinary UI change stays readable. (Vite 8 bundles with Rolldown,
        // whose chunking knob is `codeSplitting`, not Rollup's
        // `manualChunks`.)
        codeSplitting: {
          groups: [
            { name: "react", test: /[\\/]node_modules[\\/](react|react-dom|scheduler)[\\/]/ },
          ],
        },
      },
    },
  },
  server: {
    port: 5173,
    // `npm run dev` proxies the data plane to a locally running engine, so
    // the dev loop is the real API rather than a fixture.
    proxy: {
      "/ws/stream": { target: "ws://localhost:8000", ws: true },
      "/api": { target: "http://localhost:8000" },
      "/health": { target: "http://localhost:8000" },
      "/org": { target: "http://localhost:8000" },
      "/agents": { target: "http://localhost:8000" },
      "/events": { target: "http://localhost:8000" },
      "/tools": { target: "http://localhost:8000" },
      "/schedules": { target: "http://localhost:8000" },
      "/config": { target: "http://localhost:8000" },
    },
  },
});
