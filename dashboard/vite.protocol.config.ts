// A second, tiny build: the wire protocol on its own, as plain ESM.
//
// internal/e2e/golden_test.go runs a real company, captures every frame its
// WebSocket pushed, and replays those exact bytes through THE DASHBOARD'S OWN
// client (tests/dashboard/js/replay.mjs). That gate is what caught a server
// sending `agents` as an object while the client guarded on Array.isArray:
// both sides' own suites passed and the seat rendered idle for a whole turn.
//
// The app bundle cannot serve it — it is minified, code-split and boots React
// against a DOM. So the protocol layer, which imports nothing from React by
// construction, is emitted a second time: one file, unminified, no hashing,
// importable by plain `node`. It is built from the SAME source the app
// bundles, so there is no second implementation to drift.
import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";

export default defineConfig({
  build: {
    outDir: fileURLToPath(new URL("../static/dashboard", import.meta.url)),
    // The app build ran first and owns the directory; this one adds a file.
    emptyOutDir: false,
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
