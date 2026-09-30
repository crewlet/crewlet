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
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// The notices for everything the built dashboard redistributes, served beside it
// at /static/dashboard/THIRD_PARTY_NOTICES.txt and shipped in every release
// archive and image.
//
// Two halves, one file. Vite's own `build.license` writes every npm package the
// bundle contains, each with its license text, sorted by package, so the output
// is as reproducible as the bundle the CI diff checks. It only sees what a
// MODULE GRAPH reaches, so `sourceNotices` appends what travels as files: the
// faces, and the Lucide drawings behind every glyph, both
// redistributed by a package under a license of their own that the package's
// own MIT text does not cover.
//
// A `.txt` name rather than Vite's default `.vite/license.md`: the engine serves
// this tree, and a notice under a dot directory with a Markdown type is one
// nobody finds and a browser downloads rather than shows.
const NOTICES = "THIRD_PARTY_NOTICES.txt";

// The design system's own packages, as paths rather than imports: what is
// taken from them here is files, not modules.
const ICONS = "./node_modules/@crewlethq/icons";

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

// brandAssets emits the product's mark from the package that owns it.
//
// Both files are @crewlethq/icons', so the tab icon, the rail lockup, the
// GitHub App landing page and the raster favicon a browser asks for unprompted
// are one drawing with one source, served from /static/dashboard/ like every
// other file the build writes. There is no second copy anywhere in the tree.
// They are emitted rather than imported because nothing in the module graph
// references them: the shell names the SVG in its head and the engine serves
// the .ico from a route of its own.
//
// AND THE DEV SERVER SERVES THEM TOO, from the same package files, under the
// same base: an emitted asset exists only in a build, so without this a
// reader of `npm run dev` gets a blank tab icon and an empty lockup — and the
// proxy entry that used to paper over that forwarded the path to whatever the
// running engine had embedded instead.
//
// THE TAB ICON'S <link> IS WRITTEN HERE rather than in index.html, because its
// URL is `base` plus the file and the two modes disagree about a literal: the
// dev server prefixes `base` to every root-relative URL in the shell, so
// `/static/dashboard/crewlet-icon.svg` became `/static/dashboard/static/
// dashboard/…` there, while a build leaves a URL that is not a public file
// exactly as written, so `/crewlet-icon.svg` stayed unprefixed in the
// artifact. A tag injected after Vite's own pass is written once, from the
// resolved base, and is the same in both.
const BRAND_ASSETS: readonly { from: string; to: string }[] = [
  { from: `${ICONS}/svg/crewlet-icon.svg`, to: "crewlet-icon.svg" },
  { from: `${ICONS}/favicon/crewlet.ico`, to: "favicon.ico" },
];

function brandAssets(): Plugin {
  const read = (from: string) => readFileSync(fileURLToPath(new URL(from, import.meta.url)));
  let base = "/";
  return {
    name: "crewlet:brand-assets",
    configResolved(config) {
      base = config.base;
    },
    transformIndexHtml: {
      order: "post",
      handler: () => [
        {
          tag: "link",
          attrs: { rel: "icon", type: "image/svg+xml", href: `${base}crewlet-icon.svg` },
          injectTo: "head",
        },
      ],
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const path = (req.url ?? "").split("?")[0];
        const asset = BRAND_ASSETS.find(({ to }) => path === `${base}${to}`);
        if (!asset) return next();
        res.setHeader("Content-Type", asset.to.endsWith(".svg") ? "image/svg+xml" : "image/x-icon");
        res.end(read(asset.from));
      });
    },
    generateBundle() {
      for (const { from, to } of BRAND_ASSETS) {
        this.emitFile({ type: "asset", fileName: to, source: read(from) });
      }
    },
  };
}

// SOURCE_NOTICES is the third-party material in the bundle that is not an npm
// package, in the order it is appended. Each license file sits beside what it
// covers, so the notice moves with the material it belongs to.
const SOURCE_NOTICES: readonly { heading: string; lead: string; file: string }[] = [
  {
    heading: "Fonts: Geist and Geist Mono (OFL-1.1)",
    lead: "The dashboard serves these font files from /static/dashboard/fonts/.",
    file: FONT_LICENCE,
  },
  {
    heading: "Icons: Lucide (ISC; portions Feather, MIT)",
    lead: "The dashboard's glyphs are Lucide drawings, redistributed by @crewlethq/icons.",
    file: fileURLToPath(new URL(`${ICONS}/glyphs/LICENSE`, import.meta.url)),
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

// designSystemSheet makes the design system's component stylesheets ONE
// sheet, loaded once, in its place in the cascade.
//
// Every uilet component — and the two drawings @crewlethq/icons ships with a
// stylesheet — imports its own sheet as a side effect of its module. That is
// right for a page built as one chunk: the sheets land in the entry's CSS in
// module order, above ours (main.tsx says why that order is the whole game —
// a one-class tie between a kit rule and ours goes to whichever is written
// later). A code-split build breaks it silently. A component used only by a
// lazy workspace takes its sheet INTO THAT WORKSPACE'S chunk, which a browser
// appends when the chunk loads — after our sheets — so every tie ours used to
// win flips on the first navigation there, and nothing in the source says so.
// Measured on the first split build: twelve lazy stylesheets carried kit
// rules, 140 KB of them.
//
// So main.tsx imports DESIGN_SYSTEM_SHEET, which this plugin answers with
// every component sheet the two packages ship — @crewlethq/ui's own
// single-sheet build, `styles.css`, which its README names as the supported
// way to take the whole set, then the icons' — and each per-component
// side-effect import is answered with an empty module. The rules are then in
// exactly one place, whichever chunk reaches a component first, and
// internal/api's TestTheDesignSystemCascadesInOrder fails a lazy stylesheet
// that carries one. `enforce: "pre"` so the answers are given before Vite's
// own resolver and loader see the files; both key on the RESOLVED path, so
// the dev server's pre-bundled copy of the kit is answered as the build is.
const DESIGN_SYSTEM_SHEET = "virtual:crewlet-design-system.css";
const DESIGN_SYSTEM_ID = `\0${DESIGN_SYSTEM_SHEET}`;
const EMPTIED_ID = "\0crewlet-design-system-sheet-already-loaded";
const UI_DIST = fileURLToPath(new URL("./node_modules/@crewlethq/ui/dist/", import.meta.url));
const ICONS_DIST = fileURLToPath(new URL(`${ICONS}/dist/`, import.meta.url));

function designSystemSheet(): Plugin {
  const componentSheet = (file: string) =>
    file.endsWith(".css") &&
    (file.startsWith(ICONS_DIST) || (file.startsWith(UI_DIST) && file !== `${UI_DIST}styles.css`));
  return {
    name: "crewlet:design-system-sheet",
    enforce: "pre",
    async resolveId(source, importer, options) {
      if (source === DESIGN_SYSTEM_SHEET) return DESIGN_SYSTEM_ID;
      if (!source.endsWith(".css") || !importer) return null;
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      return resolved && componentSheet(resolved.id.split("?")[0] ?? "") ? EMPTIED_ID : null;
    },
    load(id) {
      if (id === EMPTIED_ID) return "export {};";
      if (id !== DESIGN_SYSTEM_ID) return null;
      const icons = readdirSync(ICONS_DIST)
        .filter((f) => f.endsWith(".css"))
        .sort();
      return [`${UI_DIST}styles.css`, ...icons.map((f) => `${ICONS_DIST}${f}`)]
        .map((file) => {
          this.addWatchFile(file);
          return readFileSync(file, "utf-8");
        })
        .join("\n");
    },
  };
}

export default defineConfig({
  plugins: [react(), designSystemSheet(), fontLicence(), brandAssets(), sourceNotices()],
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
    // The build's own warning, at the RAW size of the lazy-chunk budget. The
    // authority is internal/api's TestTheDashboardFitsItsBudget, which holds
    // every chunk the page fetches later to 150 KiB GZIPPED, as the engine
    // sends it; this knob can only count raw kB (1,000 bytes). The chunks
    // this build writes compress between 2.86x (the most compressible) and
    // 3.48x, so 150 KiB x 1.024 x 2.86 = 439 kB is the raw size at which the
    // most compressible chunk reaches the budget: the warning fires at or
    // before the point the test fails, for every chunk, rather than at
    // Vite's default 500 kB, where a chunk could be 175 KiB on the wire and
    // the build still say nothing. Change the budget there, then this.
    chunkSizeWarningLimit: 439,
    // The faces keep the paths they have always had. They arrive from
    // @crewlethq/tokens through its stylesheet rather than from public/, and
    // Vite would otherwise content-hash them into assets/. Two things depend
    // on them staying at /static/dashboard/fonts/<name>.woff2: the published
    // notice says so in as many words (THIRD_PARTY_NOTICES.txt, written from
    // the `license` block above), and a reader who bookmarked one is a reader
    // a hash breaks for nothing. The engine's Go suite pins the DIRECTORY —
    // TestTheBuiltDashboardIsWhole reads each face out of the stylesheet that
    // asks for it and requires the path to be under fonts/ — rather than any
    // filename, which is the design system's to choose. Everything else keeps
    // the hashed default.
    //
    // AND EVERYTHING ROUTED TO assets/ MUST CARRY [hash]. The engine serves
    // that directory `immutable` for a year (internal/api/dashboard.go), which
    // is correct only because a changed file is a new name; a fixed name there
    // would pin a stale module in every reader's browser.
    // TestEveryFileUnderAssetsIsContentHashed reads the built tree and fails
    // on one. The entry and the chunks take Vite's default,
    // `assets/[name]-[hash].js`.
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
    // the dev loop is the real API rather than a fixture. EXACTLY the prefixes
    // the dashboard reaches, both ways, and src/protocol/proxy.test.ts reads
    // every URL in the source and the shell's markup to hold it: an unlisted
    // prefix is served by Vite itself, which answers 404 for a path it has no
    // file for, so the screen sees a refusal that looks like the engine's and
    // is not; a listed prefix nothing reaches is a dependency nobody has, which
    // outlives the screen that once needed it.
    proxy: {
      // The live socket, and the plain GET the socket makes to diagnose a
      // handshake the engine refused.
      "/ws/stream": { target: "ws://localhost:8000", ws: true },
      // The degraded-mode snapshot poll, for a browser that cannot upgrade.
      "/stream": { target: "http://localhost:8000" },
      "/config": { target: "http://localhost:8000" },
      "/secrets": { target: "http://localhost:8000" },
      "/setup": { target: "http://localhost:8000" },
      // The state log's two operator gates, evict and readmit, which the
      // Fleet screen's replication panels write. The retention document they
      // sit beside is read over the socket.
      "/work": { target: "http://localhost:8000" },
      // Every change a screen makes, as the signed-in person
      // (protocol/act.ts). Only the act route: /operator/mcp is a person's
      // assistant's surface, and nothing in the dashboard dials it.
      "/operator/act": { target: "http://localhost:8000" },
      // Taking a backup (Settings › Backups & retention). The record it
      // lands in is read over the socket.
      "/backup": { target: "http://localhost:8000" },
    },
  },
});
