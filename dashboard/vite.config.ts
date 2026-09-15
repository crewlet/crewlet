// How the dashboard becomes bytes the engine binary embeds.
//
// `outDir` is ../static/dashboard, the tree package `static` embeds with
// `//go:embed all:dashboard`, and the build output is COMMITTED. That is not
// laziness about a .gitignore: `go build ./...` and `go install ...@latest`
// must work on a clean checkout with no Node on the machine, and an embed
// directive cannot run a bundler. CI rebuilds and diffs the tree
// (.github/workflows/ci.yml, the `dashboard` job) so a committed bundle that
// does not match this source is a red build rather than a silent lie, the
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
// is as reproducible as the bundle the CI diff checks. It only sees what comes
// out of node_modules, so `sourceNotices` appends the rest: the fonts, which are
// copied from public/ untouched, and the icon paths adapted from Feather Icons,
// which live in our own source and lose their attribution comment to the
// minifier.
//
// A `.txt` name rather than Vite's default `.vite/license.md`: the engine serves
// this tree, and a notice under a dot directory with a Markdown type is one
// nobody finds and a browser downloads rather than shows.
const NOTICES = "THIRD_PARTY_NOTICES.txt";

// The faces come from @crewlethq/tokens now, and so does their licence. Both
// the notice below and the copy served beside the files read this one path, so
// a font bump cannot leave the two disagreeing.
const FONT_LICENCE = fileURLToPath(
  new URL("./node_modules/@crewlethq/tokens/fonts/OFL.txt", import.meta.url),
);

// fontLicence serves the OFL text beside the faces it covers.
//
// The four woff2 files reach the output through the tokens stylesheet, which
// Vite rewrites and emits; a plain text file beside them is referenced by
// nothing and has to be emitted by hand. The path is pinned rather than
// hashed because internal/api/dashboardjs_test.go asserts it, and because a
// licence a reader is told to find has to stay where it was found.
function fontLicence(): Plugin {
  return {
    name: "crewlet:font-licence",
    apply: "build",
    generateBundle() {
      this.emitFile({
        type: "asset",
        fileName: "fonts/OFL.txt",
        source: readFileSync(FONT_LICENCE, "utf-8"),
      });
    },
  };
}

// SOURCE_NOTICES is the third-party material in the bundle that is not an npm
// package, in the order it is appended. Each license file sits beside what it
// covers, so the notice moves with the material it belongs to.
const SOURCE_NOTICES: readonly { heading: string; lead: string; file: string }[] = [
  {
    heading: "Fonts: Inter and JetBrains Mono (OFL-1.1)",
    lead: "The dashboard serves these font files from /static/dashboard/fonts/.",
    file: FONT_LICENCE,
  },
  {
    heading: "Icons: Feather Icons (MIT)",
    lead: "The dashboard's icon set is drawn from paths adapted from Feather Icons.",
    file: fileURLToPath(new URL("./src/ui/Icon.LICENSE.txt", import.meta.url)),
  },
];

// sourceNotices appends SOURCE_NOTICES to the notices the license step emitted.
//
// `order: "post"` is what makes the asset visible here at all: Vite registers
// its license step among its own post-build plugins, which run after every user
// plugin, so an ordinary generateBundle would look for the file before it
// exists. Ordered handlers run after every unordered one, whatever the plugin
// order. The second build (vite.protocol.config.ts) writes only protocol.js and
// leaves this file as the first build wrote it.
function sourceNotices(): Plugin {
  return {
    name: "crewlet:source-notices",
    apply: "build",
    generateBundle: {
      order: "post",
      handler(_options, bundle) {
        const notices = bundle[NOTICES];
        if (notices?.type !== "asset") {
          this.error(
            `${NOTICES} was not emitted, so build.license no longer writes it and the ` +
              "notices for the fonts and icons have nothing to join. Restore build.license.fileName.",
          );
        }
        let text = String(notices.source).trimEnd();
        for (const { heading, lead, file } of SOURCE_NOTICES) {
          text += `\n\n## ${heading}\n\n${lead}\n\n${readFileSync(file, "utf-8").trim()}`;
        }
        notices.source = `${text}\n`;
      },
    },
  };
}

export default defineConfig({
  plugins: [react(), fontLicence(), sourceNotices()],
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
    // The faces keep the paths they have always had. They arrive from
    // @crewlethq/tokens through its stylesheet rather than from public/, and
    // Vite would otherwise content-hash them into assets/; the engine's own
    // Go suite pins /static/dashboard/fonts/<name>.woff2, and a reader who
    // bookmarked one is a reader a hash breaks for nothing. Everything else
    // keeps the hashed default.
    rollupOptions: {
      output: {
        assetFileNames: (asset) =>
          asset.names?.some((name) => name.endsWith(".woff2"))
            ? "fonts/[name][extname]"
            : "assets/[name]-[hash][extname]",
        // One vendor chunk, so a change to our own code does not invalidate
        // React in every reader's cache, and so the committed diff of an
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
