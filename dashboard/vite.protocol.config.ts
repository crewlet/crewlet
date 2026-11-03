// A second, tiny build: the wire protocol on its own, as plain ESM.
//
// internal/e2e/golden_test.go runs a real company, captures every frame its
// WebSocket pushed, and replays those exact bytes through THE DASHBOARD'S OWN
// client (internal/e2e/replay.mjs). That gate is what caught a server
// sending `agents` as an object while the client guarded on Array.isArray:
// both sides' own suites passed and the seat rendered idle for a whole turn.
//
// The app bundle cannot serve it — it is minified, code-split and boots React
// against a DOM. So the protocol layer, which imports nothing from React by
// construction, is emitted a second time: one file, unminified, no hashing,
// importable by plain `node`. It is built from the SAME source the app
// bundles, so there is no second implementation to drift.
//
// INTO static/protocol, BESIDE THE EMBEDDED TREES AND NOT IN THEM. No page
// imports this module and the engine serves it to nobody: internal/e2e reads
// it from disk. Written into static/dashboard, it rode the embed into every
// engine binary and image, 124 KiB of a test's module beside the screens
// every reader may open, which internal/api's tree budget measures. It is
// committed and diffed with the bundle all the same (`make dashboard-check`),
// so the replay runs the source the bundle was built from.
import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";

export default defineConfig({
  build: {
    outDir: fileURLToPath(new URL("../static/protocol", import.meta.url)),
    // The directory is this build's alone, so it starts empty: a file a
    // renamed entry left behind would be committed and replayed by nobody.
    emptyOutDir: true,
    sourcemap: false,
    target: "es2022",
    minify: false,
    lib: {
      entry: fileURLToPath(new URL("./src/protocol/index.ts", import.meta.url)),
      formats: ["es"],
      fileName: () => "protocol.js",
    },
  },
});
