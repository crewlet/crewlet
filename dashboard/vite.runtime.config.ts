// A third build: what the artifact origin serves beside a page.
//
// Two trees under ../static/artifacts, the directory package `static` embeds
// for the api.artifacts listener (internal/artifactserve), committed for the
// reason the dashboard is: `go build ./...` must work with no Node on the
// machine, and an embed directive cannot run a bundler.
//
//   runtime/<contract>/runtime.js    `window.crewlet`, built from
//                                    src/runtime/runtime.ts, one classic
//                                    script the origin places first in every
//                                    page whose version pins the contract
//   kit/<version>/…                  the design system's artifact kit,
//                                    copied unchanged from the installed
//                                    @crewlethq/artifact-kit, at the version
//                                    package.json pins
//
// NAMES ARE VERSIONS, so the origin serves each file `immutable` for a year:
// nothing here is content-hashed, and a changed byte under an existing name
// would pin the old one in every browser that saw it. A runtime change that a
// page could tell apart is a new contract directory; a kit change is a new
// kit version, which the release of @crewlethq/artifact-kit already is.
//
// THE OUTPUT IS NOT EMPTIED WHOLE: a contract any published version pins is
// served for as long as one does, so an older contract directory a later
// build no longer writes stays committed. What this build writes it owns
// outright: its own contract directory and its own kit version directory are
// emptied first, so a file renamed between builds leaves nothing behind.
//
// CI rebuilds and diffs the tree (`make dashboard-check`), as it does the
// dashboard's.
import { defineConfig, type Plugin } from "vite";
import { cpSync, readFileSync, readdirSync, rmSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { RUNTIME_CONTRACT } from "./src/contract/artifacts.ts";

const OUT = fileURLToPath(new URL("../static/artifacts", import.meta.url));
const KIT = fileURLToPath(new URL("./node_modules/@crewlethq/artifact-kit", import.meta.url));

/** The kit version the dashboard pins, read from what is installed, which `npm ci` holds to the lockfile. */
const KIT_VERSION = (
  JSON.parse(readFileSync(join(KIT, "package.json"), "utf-8")) as { version: string }
).version;

const RUNTIME_DIR = `runtime/${RUNTIME_CONTRACT}`;
const KIT_DIR = `kit/${KIT_VERSION}`;

/** Every file under `dir`, as paths relative to it, sorted so a build lists them the same way every time. */
function filesUnder(dir: string, prefix = ""): string[] {
  return readdirSync(dir)
    .sort()
    .flatMap((name) => {
      const path = join(dir, name);
      return statSync(path).isDirectory()
        ? filesUnder(path, `${prefix}${name}/`)
        : [`${prefix}${name}`];
    });
}

// artifactTrees empties the two directories this build owns, then copies the
// kit's built files in beside the runtime the bundler writes. The kit is its
// package's own output (`dist/`), its notices included, so nothing of the
// design system is restated or rebuilt here.
function artifactTrees(): Plugin {
  return {
    name: "crewlet:artifact-trees",
    apply: "build",
    buildStart() {
      rmSync(join(OUT, RUNTIME_DIR), { recursive: true, force: true });
      rmSync(join(OUT, KIT_DIR), { recursive: true, force: true });
    },
    writeBundle() {
      const dist = join(KIT, "dist");
      for (const file of filesUnder(dist)) {
        cpSync(join(dist, file), join(OUT, KIT_DIR, file));
      }
    },
  };
}

export default defineConfig({
  plugins: [artifactTrees()],
  build: {
    outDir: OUT,
    emptyOutDir: false,
    sourcemap: false,
    target: "es2022",
    // The notices for what runtime.js redistributes, beside it.
    license: { fileName: `${RUNTIME_DIR}/THIRD_PARTY_NOTICES.txt` },
    lib: {
      entry: fileURLToPath(new URL("./src/runtime/runtime.ts", import.meta.url)),
      // A CLASSIC SCRIPT, not a module: the origin places it as the page's
      // first `<script>`, which runs synchronously before anything the page
      // ships, so `window.crewlet` exists by the time page code asks for it.
      formats: ["iife"],
      name: "crewletRuntime",
      fileName: () => `${RUNTIME_DIR}/runtime.js`,
    },
  },
});
