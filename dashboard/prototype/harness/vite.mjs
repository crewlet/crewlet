// @ts-check
/**
 * How a prototype variant becomes a build: the dashboard's OWN Vite config,
 * loaded from `vite.config.ts` and merged with what a prototype changes, plus
 * the plugins that make a variant a variant.
 *
 * Read by `build.mjs`, which bundles a topic's variants for publishing, and by
 * `check/vitest.config.mjs`, which mounts the same module graph in jsdom to
 * prove every route renders. One module, so the swap a check exercised is the
 * swap the published page runs.
 *
 * What a prototype build changes, and nothing else:
 *
 *  - THE ENTRY. The shell's `index.html` is the dashboard's own; its one
 *    module script is pointed at a generated entry that boots the prototype
 *    (`harness/boot.tsx`) instead of `src/main.tsx`, so the page shell, its
 *    metas and its brand icon are the shipped ones.
 *  - THE SWAPS. A topic names dashboard modules and their replacements; every
 *    importer of the original gets the replacement, matched on the resolved
 *    absolute path. `@original/<path under src>` (or a `?original` suffix)
 *    reaches the shipped module past every swap, so a proposal can wrap
 *    rather than copy.
 *  - THE DESIGN SYSTEM, when a variant names a local uilet checkout: every
 *    `@crewlethq/*` import, every component stylesheet and the design-system
 *    sheet the shipped config assembles from `node_modules` are read from
 *    that checkout's built packages instead.
 *  - THE OUTPUT SHAPE. One JS file and one stylesheet per variant, relative
 *    URLs throughout, and the faces moved to one folder every variant shares.
 */

import { createHash } from "node:crypto";
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer, loadConfigFromFile, mergeConfig } from "vite";

/** `dashboard/prototype`. */
export const PROTOTYPE = dirname(dirname(fileURLToPath(import.meta.url)));
/** `dashboard`. */
export const DASHBOARD = dirname(PROTOTYPE);
/** `dashboard/src`, which `~/` and `@original/` both name. */
export const SRC = join(DASHBOARD, "src");
/** The module every prototype page boots through. */
const BOOT = join(PROTOTYPE, "harness", "boot.tsx");

/** The shell's own module script, which a prototype entry replaces. */
const MAIN_SCRIPT = 'src="/src/main.tsx"';
/** What the shell's script is pointed at instead, and the id it resolves to. */
const ENTRY = "/@crewlet-prototype/entry";
const ENTRY_ID = "\0crewlet-prototype-entry";

/** The escapes past every swap. */
const ORIGINAL_PREFIX = "@original/";
const ORIGINAL_SUFFIX = "?original";

/** The design system's sheet, as `src/main.tsx` imports it and the shipped config answers it. */
const DESIGN_SYSTEM_SHEET = "virtual:crewlet-design-system.css";
const UILET_SHEET_ID = "\0crewlet-prototype-uilet-design-system.css";
const UILET_EMPTIED_ID = "\0crewlet-prototype-uilet-sheet-already-loaded";
const UILET_PACKAGES = /** @type {const} */ (["ui", "icons", "tokens"]);
const UILET_IMPORT = /^@crewlethq\/(ui|icons|tokens)(\/|$)/;

/**
 * @typedef {import("./defineTopic.ts").Topic} Topic
 * @typedef {import("./defineTopic.ts").Variant} Variant
 * @typedef {{ topic: Topic, file: string, dir: string }} LoadedTopic
 * @typedef {{ root: string, anchor: string, uiDist: string, iconsDist: string }} UiletLayout
 * @typedef {{
 *   topicId: string,
 *   variantId: string,
 *   topicFile: string,
 *   swaps: Map<string, string>,
 *   css: string[],
 *   uilet: UiletLayout | null,
 *   literals: string[],
 * }} VariantPlan
 */

/**
 * Load `topics/<id>/topic.ts` in Node, through Vite so it may import the
 * fixtures and the dashboard's own modules by `~/`.
 *
 * @param {string} id
 * @returns {Promise<LoadedTopic>}
 */
export async function loadTopic(id) {
  if (!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(id)) {
    throw new Error(
      `"${id}" is not a topic id: a topic is a lowercase slug under prototype/topics/.`,
    );
  }
  const file = join(PROTOTYPE, "topics", id, "topic.ts");
  if (!existsSync(file)) {
    throw new Error(`There is no topic "${id}": expected ${relative(DASHBOARD, file)}.`);
  }
  // THROUGH A VITE SERVER'S OWN SSR LOADER rather than `runnerImport`, which
  // externalises every package: a topic's scenarios reach the dashboard's own
  // helpers, some of those import the kit, and the kit's modules import their
  // stylesheets, which Node has no loader for. Here the kit goes through Vite,
  // which reads a stylesheet as nothing on the server.
  // A dev server sets NODE_ENV to development for the whole process, and a
  // build that follows would then bundle React's development runtime, file
  // names and all. The value the caller had is put back.
  const environment = process.env.NODE_ENV;
  const server = await createServer({
    configFile: false,
    root: DASHBOARD,
    logLevel: "error",
    appType: "custom",
    server: { middlewareMode: true, hmr: false, ws: false },
    optimizeDeps: { noDiscovery: true, include: [] },
    resolve: { alias: { "~": SRC } },
    ssr: { noExternal: [/^@crewlethq\//] },
  });
  let module;
  try {
    module = await server.ssrLoadModule(file);
  } finally {
    await server.close();
    if (environment === undefined) delete process.env.NODE_ENV;
    else process.env.NODE_ENV = environment;
  }
  const topic = /** @type {{ default?: Topic }} */ (module).default;
  if (!topic || typeof topic !== "object" || !Array.isArray(topic.variants)) {
    throw new Error(`${relative(DASHBOARD, file)} has no default export made by defineTopic().`);
  }
  if (topic.id !== id) {
    throw new Error(
      `${relative(DASHBOARD, file)} declares the id "${topic.id}", not its folder's "${id}".`,
    );
  }
  return { topic, file, dir: dirname(file) };
}

/**
 * Everything one variant's build needs, resolved and checked, so a typo is a
 * sentence naming the path before Vite starts rather than a bundle that
 * silently ships the original.
 *
 * @param {LoadedTopic} loaded
 * @param {string} variantId
 * @returns {VariantPlan}
 */
export function planVariant(loaded, variantId) {
  const variant = loaded.topic.variants.find((v) => v.id === variantId);
  if (!variant) {
    const known = loaded.topic.variants.map((v) => v.id).join(", ");
    throw new Error(
      `The topic "${loaded.topic.id}" has no variant "${variantId}" (it has ${known}).`,
    );
  }
  const where = `topic "${loaded.topic.id}", variant "${variant.id}"`;

  const swaps = new Map();
  for (const [from, to] of Object.entries(variant.modules ?? {})) {
    const original = from.startsWith("~/") ? join(SRC, from.slice(2)) : resolve(DASHBOARD, from);
    if (!isUnder(original, SRC) || !existsSync(original)) {
      throw new Error(`${where}: "${from}" names no module under dashboard/src.`);
    }
    const replacement = resolve(loaded.dir, to);
    if (!isUnder(replacement, PROTOTYPE) || !existsSync(replacement)) {
      throw new Error(
        `${where}: the replacement "${to}" for "${from}" does not exist under prototype/.`,
      );
    }
    swaps.set(realpathSync(original), realpathSync(replacement));
  }

  const css = (variant.css ?? []).map((sheet) => {
    const file = resolve(loaded.dir, sheet);
    if (!file.endsWith(".css") || !existsSync(file)) {
      throw new Error(`${where}: the stylesheet "${sheet}" does not exist.`);
    }
    return realpathSync(file);
  });

  const uilet = variant.uilet
    ? uiletLayout(
        isAbsolute(variant.uilet) ? variant.uilet : resolve(loaded.dir, variant.uilet),
        where,
      )
    : null;

  // Every uilet path the topic WRITES, as written: the bundle carries the
  // topic module, and a path on somebody's machine is not something a
  // published page should.
  const literals = loaded.topic.variants.flatMap((v) => (v.uilet ? [v.uilet] : []));
  return {
    topicId: loaded.topic.id,
    variantId: variant.id,
    topicFile: loaded.file,
    swaps,
    css,
    uilet,
    literals,
  };
}

/** The code that builds and reads a demo company, whose digest is the company's format. */
const COMPANY_CODE = [join(PROTOTYPE, "harness", "engine"), join(PROTOTYPE, "fixtures")];

/**
 * The demo company's format: a digest of every file that builds or reads
 * one (`harness/engine/`, `fixtures/`). A tab keeps its company across a
 * variant switch only while the format matches, so a build whose company
 * changed shape starts the tab over instead of misreading what it stored,
 * and nobody has to remember to say the shape moved. Every variant of a
 * topic has the same one, since a variant swaps only dashboard modules.
 *
 * @returns {string}
 */
export function companyFormat() {
  const hash = createHash("sha256");
  /** @param {string} dir */
  const walk = (dir) => {
    for (const name of readdirSync(dir).sort()) {
      const path = join(dir, name);
      if (statSync(path).isDirectory()) {
        walk(path);
        continue;
      }
      hash.update(`${relative(PROTOTYPE, path)}\0`);
      hash.update(readFileSync(path));
      hash.update("\0");
    }
  };
  for (const dir of COMPANY_CODE) walk(dir);
  return hash.digest("hex").slice(0, 16);
}

/**
 * The shipped modules a plan swaps out, as paths under `dashboard/` written
 * with forward slashes (`src/routes/settings/Integrations.tsx`).
 *
 * @param {VariantPlan} plan
 * @returns {string[]}
 */
export function swappedModules(plan) {
  const root = realpathSync(DASHBOARD);
  return [...plan.swaps.keys()].map((file) => relative(root, file).split(sep).join("/"));
}

/**
 * A uilet checkout's built packages, or a sentence saying what to run first.
 *
 * @param {string} dir
 * @param {string} where
 * @returns {UiletLayout}
 */
function uiletLayout(dir, where) {
  if (!existsSync(dir)) throw new Error(`${where}: the uilet checkout ${dir} does not exist.`);
  const root = realpathSync(dir);
  for (const name of UILET_PACKAGES) {
    const link = join(root, "node_modules", "@crewlethq", name);
    const pkg = join(root, "packages", name);
    if (!existsSync(link) || realpathSync(link) !== pkg) {
      throw new Error(
        `${where}: ${root} has no workspace link for @crewlethq/${name}. Run \`npm ci --ignore-scripts\` there first.`,
      );
    }
    if (!existsSync(join(pkg, "dist"))) {
      throw new Error(`${where}: ${pkg} is not built. Run \`npm run build\` in ${root} first.`);
    }
  }
  return {
    root,
    anchor: join(root, "package.json"),
    uiDist: join(root, "packages", "ui", "dist") + sep,
    iconsDist: join(root, "packages", "icons", "dist") + sep,
  };
}

/**
 * @param {string} file
 * @param {string} dir
 */
function isUnder(file, dir) {
  const rel = relative(dir, file);
  return rel !== "" && !rel.startsWith("..") && !isAbsolute(rel);
}

/** @param {string} id */
const cleanId = (id) => id.split("?")[0] ?? id;

/**
 * The shell's module script pointed at the prototype's entry, and the entry.
 *
 * @param {VariantPlan} plan
 * @returns {import("vite").Plugin}
 */
function entryPlugin(plan) {
  return {
    name: "crewlet-prototype:entry",
    enforce: "pre",
    transformIndexHtml: {
      order: "pre",
      handler(html) {
        if (!html.includes(MAIN_SCRIPT)) {
          throw new Error(
            `dashboard/index.html no longer loads ${MAIN_SCRIPT}, so the prototype cannot replace its entry. Update prototype/harness/vite.mjs.`,
          );
        }
        return html.replace(MAIN_SCRIPT, `src="${ENTRY}"`);
      },
    },
    resolveId(source) {
      return source === ENTRY ? ENTRY_ID : null;
    },
    load(id) {
      if (id !== ENTRY_ID) return null;
      // THE VARIANT'S STYLESHEETS ARE IMPORTED AFTER THE BOOT MODULE, which
      // imports every stylesheet the app ships, so they land after all of
      // them in the one sheet this build writes.
      return [
        `import { bootPrototype } from ${JSON.stringify(BOOT)};`,
        `import topic from ${JSON.stringify(plan.topicFile)};`,
        ...plan.css.map((file) => `import ${JSON.stringify(file)};`),
        `bootPrototype(topic, ${JSON.stringify(plan.variantId)});`,
      ].join("\n");
    },
    transform(code, id) {
      // THE TOPIC'S UILET PATHS STAY ON THIS MACHINE. The browser reads only
      // the labels, the summaries and the scenarios off the topic.
      if (cleanId(id) !== plan.topicFile || plan.literals.length === 0) return null;
      let out = code;
      for (const literal of plan.literals) {
        for (const quoted of [JSON.stringify(literal), `'${literal}'`]) {
          out = out.split(quoted).join(JSON.stringify("(a local uilet build)"));
        }
      }
      return out === code ? null : { code: out, map: null };
    },
  };
}

/**
 * Every importer of a swapped module gets its replacement; the escapes reach
 * the original.
 *
 * @param {Map<string, string>} swaps original absolute path to replacement
 * @returns {import("vite").Plugin}
 */
export function swapPlugin(swaps) {
  /** The replacements some importer was actually handed. */
  const used = new Set();
  return {
    name: "crewlet-prototype:swap",
    enforce: "pre",
    async resolveId(source, importer, options) {
      if (source.startsWith(ORIGINAL_PREFIX)) {
        const file = join(SRC, source.slice(ORIGINAL_PREFIX.length));
        if (!existsSync(file)) {
          this.error(`${source} names no module under dashboard/src (imported by ${importer}).`);
        }
        return realpathSync(file);
      }
      if (source.endsWith(ORIGINAL_SUFFIX)) {
        const resolved = await this.resolve(source.slice(0, -ORIGINAL_SUFFIX.length), importer, {
          ...options,
          skipSelf: true,
        });
        return resolved ? cleanId(resolved.id) : null;
      }
      if (swaps.size === 0 || source.startsWith("\0")) return null;
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      if (!resolved) return null;
      const replacement = swaps.get(cleanId(resolved.id));
      // A replacement importing the shipped module by its ordinary name would
      // import itself; the escapes above are how it reaches the original.
      if (replacement && importer && cleanId(importer) === replacement) {
        this.error(
          `${replacement} imports the module it replaces by its ordinary name. Import it as ${ORIGINAL_PREFIX}<path under src> instead.`,
        );
      }
      if (!replacement) return resolved;
      used.add(replacement);
      return replacement;
    },
    // A SWAP NOTHING IMPORTED IS A VARIANT THAT LOOKS LIKE TODAY: a module
    // the app reaches by another path, or one no screen imports at all. The
    // build stops rather than publish the shipped screen under a proposal's
    // name.
    buildEnd(error) {
      if (error) return;
      const unused = [...swaps].filter(([, replacement]) => !used.has(replacement));
      if (unused.length > 0) {
        this.error(
          `No module imported ${unused.map(([original]) => relative(DASHBOARD, original)).join(", ")}, so its replacement was never used. Swap the module the screen actually imports.`,
        );
      }
    },
  };
}

/**
 * The design system read from a local uilet checkout: the packages, their
 * component stylesheets emptied as the shipped config empties them, and the
 * one sheet assembled from the checkout's own `styles.css` and the icons'
 * sheets, exactly as `vite.config.ts`'s `designSystemSheet` assembles it from
 * `node_modules`.
 *
 * ITS OWN PLUGIN RATHER THAN THE SHIPPED ONE POINTED ELSEWHERE, because the
 * shipped one fixes its directories when `vite.config.ts` is evaluated and
 * exports nothing to re-point. It runs FIRST, so it answers the sheet before
 * the shipped plugin does, and the shipped plugin never meets a uilet file.
 *
 * @param {UiletLayout} layout
 * @returns {import("vite").Plugin}
 */
export function uiletPlugin(layout) {
  /** @param {string} file */
  const componentSheet = (file) =>
    file.endsWith(".css") &&
    (file.startsWith(layout.iconsDist) ||
      (file.startsWith(layout.uiDist) && file !== `${layout.uiDist}styles.css`));
  return {
    name: "crewlet-prototype:uilet",
    enforce: "pre",
    async resolveId(source, importer, options) {
      if (source === DESIGN_SYSTEM_SHEET) return UILET_SHEET_ID;
      if (UILET_IMPORT.test(source)) {
        const resolved = await this.resolve(source, layout.anchor, { ...options, skipSelf: true });
        if (!resolved)
          this.error(`${source} does not resolve in the uilet checkout ${layout.root}.`);
        return componentSheet(cleanId(resolved.id)) ? UILET_EMPTIED_ID : resolved;
      }
      if (!source.endsWith(".css") || !importer || !cleanId(importer).startsWith(layout.root)) {
        return null;
      }
      const resolved = await this.resolve(source, importer, { ...options, skipSelf: true });
      return resolved && componentSheet(cleanId(resolved.id)) ? UILET_EMPTIED_ID : null;
    },
    load(id) {
      if (id === UILET_EMPTIED_ID) return "export {};";
      if (id !== UILET_SHEET_ID) return null;
      const icons = readdirSync(layout.iconsDist)
        .filter((file) => file.endsWith(".css"))
        .sort();
      return [`${layout.uiDist}styles.css`, ...icons.map((file) => `${layout.iconsDist}${file}`)]
        .map((file) => {
          this.addWatchFile(file);
          return readFileSync(file, "utf-8");
        })
        .join("\n");
    },
  };
}

/** Where the engine serves the dashboard, which one module still spells out. */
export const ENGINE_BASE = '"/static/dashboard/';

/** Every topic's own files, a variant's replacement modules among them. */
const TOPICS = join(PROTOTYPE, "topics");

/**
 * The dashboard's own URLs, made relative to the variant's page.
 *
 * The shipped sidebar draws the brand mark from `/static/dashboard/`, the path
 * the engine serves the dashboard at, written out rather than read from
 * `import.meta.env.BASE_URL`; on any other host that is a broken image. This
 * rewrites that literal to the page's own folder, where the build emits the
 * mark, in a shipped module and in a topic's replacement for one, which is a
 * copy of it.
 *
 * It insists the SHIPPED source still spells it: once the shipped code reads
 * its base from the build, the build stops here, and this plugin is deleted.
 * That is judged from the shipped modules this variant's graph holds and from
 * the ones it swapped out, so a variant replacing the one module that spells
 * the literal is not mistaken for the literal having gone.
 *
 * @param {Map<string, string>} swaps the swapped originals' paths, to their replacements
 * @returns {import("vite").Plugin}
 */
function relativeBasePlugin(swaps) {
  let shipped = 0;
  return {
    name: "crewlet-prototype:relative-base",
    enforce: "pre",
    apply: "build",
    transform(code, id) {
      const file = cleanId(id);
      const own = file.startsWith(SRC + sep);
      if ((!own && !file.startsWith(TOPICS + sep)) || !code.includes(ENGINE_BASE)) return null;
      if (own) shipped += code.split(ENGINE_BASE).length - 1;
      return { code: code.split(ENGINE_BASE).join('"./'), map: null };
    },
    buildEnd(error) {
      if (error || shipped > 0) return;
      const swappedOut = [...swaps.keys()].some(
        (file) => file.startsWith(SRC + sep) && readFileSync(file, "utf-8").includes(ENGINE_BASE),
      );
      if (!swappedOut) {
        this.error(
          `No dashboard module spells out ${ENGINE_BASE.slice(1)} any more, so the prototype's rewrite of it is stale. Delete relativeBasePlugin from prototype/harness/vite.mjs.`,
        );
      }
    },
  };
}

/**
 * Fails the build if an emitted file names a directory on this machine.
 *
 * @param {string[]} forbidden
 * @returns {import("vite").Plugin}
 */
function noLocalPathsPlugin(forbidden) {
  return {
    name: "crewlet-prototype:no-local-paths",
    apply: "build",
    generateBundle: {
      order: "post",
      handler(_options, bundle) {
        for (const file of Object.values(bundle)) {
          const text =
            file.type === "chunk"
              ? file.code
              : typeof file.source === "string"
                ? file.source
                : file.fileName.endsWith(".woff2") || file.fileName.endsWith(".ico")
                  ? ""
                  : Buffer.from(file.source).toString("utf-8");
          const found = forbidden.find((path) => text.includes(path));
          if (found) {
            const at = text.indexOf(found);
            const context = text.slice(Math.max(0, at - 120), at + found.length + 60);
            this.error(
              `${file.fileName} contains the local path ${found}, which a published page must not: …${context}…`,
            );
          }
        }
      },
    },
  };
}

/**
 * The directories a published page must never name: this checkout, the
 * reader's home and every uilet checkout the plan reads, each as written and
 * as resolved.
 *
 * @param {VariantPlan} plan
 */
function localPaths(plan) {
  const roots = [dirname(DASHBOARD), homedir(), ...(plan.uilet ? [plan.uilet.root] : [])];
  const all = new Set();
  for (const root of roots) {
    all.add(root);
    if (existsSync(root)) all.add(realpathSync(root));
  }
  return [...all].filter((path) => path.length > 1);
}

/**
 * The plugins that make a module graph this variant's, in the order they must
 * run: ahead of every plugin the shipped config declares.
 *
 * @param {VariantPlan} plan
 * @returns {import("vite").Plugin[]}
 */
export function variantPlugins(plan) {
  return [
    entryPlugin(plan),
    ...(plan.uilet ? [uiletPlugin(plan.uilet)] : []),
    swapPlugin(plan.swaps),
  ];
}

/** The design system's sheet as nothing: jsdom lays nothing out, so a check needs no styles. */
const EMPTY_SHEET = "\0crewlet-prototype-empty-sheet.css";

/**
 * The plugins a CHECK runs a variant's module graph with: the same swaps and
 * the same uilet resolution as its build, and the design system's sheet as an
 * empty one (the shipped config, which answers it in a build, is not loaded
 * by a test runner).
 *
 * @param {VariantPlan} plan
 * @returns {import("vite").Plugin[]}
 */
export function checkPlugins(plan) {
  return [
    {
      name: "crewlet-prototype:empty-sheet",
      enforce: "pre",
      resolveId(source) {
        return source === DESIGN_SYSTEM_SHEET ? EMPTY_SHEET : null;
      },
      load(id) {
        return id === EMPTY_SHEET ? "" : null;
      },
    },
    ...(plan.uilet ? [uiletPlugin(plan.uilet)] : []),
    swapPlugin(plan.swaps),
  ];
}

/**
 * The shipped config with a prototype's changes, ready for `build()`.
 *
 * @param {VariantPlan} plan
 * @param {string} outDir
 * @returns {Promise<import("vite").InlineConfig>}
 */
export async function buildConfig(plan, outDir) {
  const loaded = await loadConfigFromFile(
    { command: "build", mode: "production" },
    join(DASHBOARD, "vite.config.ts"),
    DASHBOARD,
    "error",
  );
  if (!loaded) throw new Error("dashboard/vite.config.ts could not be loaded.");
  /** @type {import("vite").InlineConfig} */
  const changes = {
    configFile: false,
    root: DASHBOARD,
    // RELATIVE, so a variant loads from wherever its folder is published.
    base: "./",
    define: { __PROTOTYPE_COMPANY_FORMAT__: JSON.stringify(companyFormat()) },
    logLevel: "warn",
    resolve: {
      // ONE REACT, whatever a uilet checkout holds in its own node_modules: a
      // second copy is a hook call against the wrong dispatcher.
      dedupe: ["react", "react-dom"],
    },
    build: {
      outDir,
      emptyOutDir: true,
      // A prototype is one chunk by design (see below), so the shipped
      // lazy-chunk budget this warns at does not apply to it.
      chunkSizeWarningLimit: 100_000,
      rollupOptions: {
        output: {
          // ONE JS FILE PER VARIANT: every lazy workspace inlined, which keeps
          // a topic's file count far below a publish's limit.
          codeSplitting: false,
          // The faces carry their content hash here, unlike the shipped
          // build's fixed names, so `build.mjs` can move them into ONE folder
          // every variant shares: an identical face is one file, and a uilet
          // build that changed a face is a second one rather than a clash.
          assetFileNames: (asset) =>
            asset.names?.some((name) => name.endsWith(".woff2"))
              ? "fonts/[name]-[hash][extname]"
              : "assets/[name]-[hash][extname]",
        },
      },
    },
    experimental: {
      // THE SHARED FONT FOLDER sits beside the variant folders, two levels
      // above the stylesheet that names the faces (`<variant>/assets/`).
      renderBuiltUrl(filename, { hostType }) {
        if (hostType === "css" && filename.startsWith("fonts/")) return `../../${filename}`;
        return { relative: true };
      },
    },
  };
  const merged = mergeConfig(loaded.config, changes);
  merged.plugins = [
    ...variantPlugins(plan),
    relativeBasePlugin(plan.swaps),
    ...(merged.plugins ?? []),
    noLocalPathsPlugin(localPaths(plan)),
  ];
  return merged;
}
