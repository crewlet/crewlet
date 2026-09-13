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
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// The notices for everything the built dashboard redistributes, served beside it
// at /static/dashboard/THIRD_PARTY_NOTICES.txt and shipped in every release
// archive and image.
//
// Two halves, one file. Vite's own `build.license` writes every npm package the
// bundle contains, each with its license text, sorted by package, so the output
// is as reproducible as the bundle the CI diff checks.
// The fonts are not bundled (they are copied from public/ untouched), so the
// license step never sees them, and `fontNotice` appends their SIL Open Font
// License from the same OFL.txt that travels in fonts/.
//
// A `.txt` name rather than Vite's default `.vite/license.md`: the engine serves
// this tree, and a notice under a dot directory with a Markdown type is one
// nobody finds and a browser downloads rather than shows.
const NOTICES = "THIRD_PARTY_NOTICES.txt";
const FONT_LICENSE = fileURLToPath(new URL("./public/fonts/OFL.txt", import.meta.url));

// fontNotice appends the font license to the notices the license step emitted.
//
// `order: "post"` is what makes the asset visible here at all: Vite registers
// its license step among its own post-build plugins, which run after every user
// plugin, so an ordinary generateBundle would look for the file before it
// exists. Ordered handlers run after every unordered one, whatever the plugin
// order. The second build (vite.protocol.config.ts) writes only protocol.js and
// leaves this file as the first build wrote it.
function fontNotice(): Plugin {
  return {
    name: "crewlet:font-notice",
    apply: "build",
    generateBundle: {
      order: "post",
      handler(_options, bundle) {
        const notices = bundle[NOTICES];
        if (notices?.type !== "asset") {
          this.error(
            `${NOTICES} was not emitted, so build.license no longer writes it and the ` +
              "font notice has nothing to join. Restore build.license.fileName.",
          );
        }
        const fonts = readFileSync(FONT_LICENSE, "utf-8").trim();
        notices.source =
          `${String(notices.source).trimEnd()}\n\n` +
          "## Fonts: Inter and JetBrains Mono (OFL-1.1)\n\n" +
          "The dashboard serves these font files from /static/dashboard/fonts/.\n\n" +
          `${fonts}\n`;
      },
    },
  };
}

export default defineConfig({
  plugins: [react(), fontNotice()],
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
    license: { fileName: NOTICES },
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
    // the dev loop is the real API rather than a fixture. Every prefix the
    // dashboard calls has to be listed: an unlisted one is served by Vite
    // itself, which answers 404 for a path it has no file for, so the screen
    // sees a refusal that looks like the engine's and is not.
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
      "/secrets": { target: "http://localhost:8000" },
      "/setup": { target: "http://localhost:8000" },
      "/stream": { target: "http://localhost:8000" },
    },
  },
});
