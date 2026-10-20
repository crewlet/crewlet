// @ts-check
/**
 * Build a design topic into a folder a static host can serve:
 *
 *     node prototype/build.mjs --topic <id> --out <dir> [--variant <id> ...]
 *
 * What lands in <dir>:
 *
 *     index.html            the launcher: opens today/ on the topic's start address
 *     fonts/                the faces, shared by every variant (one copy of each)
 *     <variant>/index.html  the dashboard shell
 *     <variant>/assets/     one script and one stylesheet
 *     <variant>/...         the tab icon and the third-party notices
 *
 * Every URL is relative, so the folder loads from any static host with no
 * server logic. `--variant` rebuilds only the named variants and keeps the
 * rest of an existing output. The run ends with each variant's file count and
 * size against the publishing limits.
 */

import {
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join, relative, resolve } from "node:path";
import { parseArgs } from "node:util";
import { build } from "vite";
import { themes } from "@crewlethq/tokens";
import { FONTS, filesUnder, finishVariant } from "./harness/publish.mjs";
import { buildConfig, loadTopic, planVariant } from "./harness/vite.mjs";

/** What every publish allows, from the artifact host's own limits. */
const LIMITS = { fileBytes: 16 * 1024 * 1024, totalBytes: 64 * 1024 * 1024, files: 255 };
/** Marks a folder this script wrote, so it never empties one it did not. */
const GENERATOR = '<meta name="generator" content="crewlet-prototype" />';
/** How many document types the build removed, which says whether removing them is still needed. */
let doctypesRemoved = 0;

const { values } = parseArgs({
  options: {
    topic: { type: "string" },
    out: { type: "string" },
    variant: { type: "string", multiple: true },
  },
});
if (!values.topic || !values.out) {
  process.stderr.write(
    "usage: node prototype/build.mjs --topic <id> --out <dir> [--variant <id> ...]\n",
  );
  process.exit(2);
}

const out = resolve(values.out);
const loaded = await loadTopic(values.topic);
const wanted = values.variant ?? loaded.topic.variants.map((v) => v.id);
const plans = wanted.map((id) => planVariant(loaded, id));

prepare(out, Boolean(values.variant));
// THE LAUNCHER FIRST: it is what marks the folder as this script's, so a
// build that fails part way leaves a folder the next run may empty.
writeFileSync(join(out, "index.html"), launcher(loaded.topic));
copyStatic(loaded, out);
for (const plan of plans) {
  const folder = join(out, plan.variantId);
  process.stdout.write(`building ${plan.topicId}/${plan.variantId}\n`);
  await build(await buildConfig(plan, folder));
  doctypesRemoved += finishVariant(folder).doctypesRemoved;
}
// THE REMOVAL INSISTS ON BEING NEEDED: once the shipped icon declares no
// document type, the build stops here and the removal is deleted.
if (doctypesRemoved === 0) {
  throw new Error(
    "No emitted file declares a document type any more, so the prototype's removal of one is stale. Delete it from publishableSvg in prototype/harness/publish.mjs.",
  );
}
report(
  out,
  loaded.topic.variants.map((v) => v.id),
);

/**
 * Empty `dir` for a whole build, or keep it for a partial one; refuse a
 * non-empty folder this script did not write.
 *
 * @param {string} dir
 * @param {boolean} partial
 */
function prepare(dir, partial) {
  if (existsSync(dir) && readdirSync(dir).length > 0) {
    const index = join(dir, "index.html");
    const ours = existsSync(index) && readFileSync(index, "utf-8").includes(GENERATOR);
    if (!ours) {
      throw new Error(
        `${dir} is not empty and was not written by this script; choose another --out.`,
      );
    }
    if (!partial) rmSync(dir, { recursive: true });
  }
  mkdirSync(join(dir, FONTS), { recursive: true });
}

/**
 * Copy each of the topic's static folders into the build, as it is. A source
 * that is missing fails the build, naming it, rather than leaving a frame
 * pointing at nothing.
 *
 * @param {import("./harness/vite.mjs").LoadedTopic} loaded
 * @param {string} dir
 */
function copyStatic(loaded, dir) {
  for (const [dest, source] of Object.entries(loaded.topic.static ?? {})) {
    const from = resolve(loaded.dir, source);
    if (!existsSync(from)) {
      throw new Error(`The static folder "${dest}" reads ${from}, which does not exist.`);
    }
    const to = join(dir, dest);
    rmSync(to, { recursive: true, force: true });
    mkdirSync(to, { recursive: true });
    cpSync(from, to, { recursive: true });
  }
}

/**
 * The topic's front page: it opens the shipped dashboard on the topic's start
 * address at once, and lists every variant for a reader whose browser does
 * not follow the redirect.
 *
 * Its colours are the design system's own tokens, read from
 * `@crewlethq/tokens` when it is written.
 *
 * @param {import("./harness/defineTopic.ts").Topic} topic
 */
function launcher(topic) {
  const today = `today/index.html${topic.start}`;
  const tokens = (/** @type {"light" | "dark"} */ mode) => {
    const c = themes[mode].color;
    return [
      `--page: ${c.surface.background};`,
      `--ink: ${c.text.primary};`,
      `--ink-quiet: ${c.text.secondary};`,
      `--link: ${c.brand.accentInk};`,
      `--rule: ${c.border.default};`,
    ].join(" ");
  };
  const items = topic.variants
    .map(
      (v) =>
        `<li><a href="${escape(`${v.id}/index.html${topic.start}`)}">${escape(v.label)}</a><span>${escape(v.summary)}</span></li>`,
    )
    .join("\n        ");
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    ${GENERATOR}
    <title>${escape(topic.title)}</title>
    <style>
      :root { color-scheme: light dark; ${tokens("light")} }
      @media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { ${tokens("dark")} } }
      :root[data-theme="dark"] { ${tokens("dark")} }
      body { margin: 0; padding: 48px 16px; background: var(--page); color: var(--ink); font: 15px/1.5 system-ui, sans-serif; }
      main { max-width: 640px; margin: 0 auto; }
      h1 { font-size: 22px; margin: 0 0 8px; }
      p { margin: 0 0 24px; color: var(--ink-quiet); }
      a { color: var(--link); }
      ul { list-style: none; margin: 0; padding: 0; border-top: 1px solid var(--rule); }
      li { display: flex; flex-wrap: wrap; gap: 4px 12px; padding: 12px 0; border-bottom: 1px solid var(--rule); }
      li span { color: var(--ink-quiet); overflow-wrap: anywhere; }
    </style>
    <script>
      // THE VARIANT THE READER LAST HAD OPEN, as harness/session.ts keeps it
      // (lastVariantKey), else Today.
      (function () {
        var ids = ${JSON.stringify(topic.variants.map((v) => v.id))};
        var to = ${JSON.stringify(today)};
        try {
          var last = localStorage.getItem(${JSON.stringify(`crewlet-prototype:${topic.id}:variant`)});
          if (last && ids.indexOf(last) >= 0) to = last + "/index.html" + ${JSON.stringify(topic.start)};
        } catch (e) {}
        location.replace(to);
      })();
    </script>
  </head>
  <body>
    <main>
      <h1>${escape(topic.title)}</h1>
      <p>Opening the dashboard as it ships. If nothing happens, <a href="${escape(today)}">open it here</a>, or choose a variant.</p>
      <ul>
        ${items}
      </ul>
    </main>
  </body>
</html>
`;
}

/** @param {string} text */
function escape(text) {
  return text.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

/**
 * Each variant's files and bytes, the shared ones, and the whole against the
 * publishing limits. Exits non-zero past any of them.
 *
 * @param {string} dir
 * @param {string[]} variants
 */
function report(dir, variants) {
  const files = filesUnder(dir);
  const mb = (/** @type {number} */ n) => `${(n / 1024 / 1024).toFixed(2)} MB`;
  const lines = [];
  for (const variant of variants) {
    const own = files.filter((f) => relative(dir, f.path).startsWith(`${variant}/`));
    lines.push(
      `  ${variant.padEnd(16)} ${String(own.length).padStart(3)} files  ${mb(own.reduce((n, f) => n + f.bytes, 0))}`,
    );
  }
  const shared = files.filter(
    (f) => !variants.some((v) => relative(dir, f.path).startsWith(`${v}/`)),
  );
  lines.push(
    `  ${"shared".padEnd(16)} ${String(shared.length).padStart(3)} files  ${mb(shared.reduce((n, f) => n + f.bytes, 0))}`,
  );
  const total = files.reduce((n, f) => n + f.bytes, 0);
  const largest = files.reduce((a, b) => (b.bytes > a.bytes ? b : a), { path: "", bytes: 0 });
  lines.push(`  ${"total".padEnd(16)} ${String(files.length).padStart(3)} files  ${mb(total)}`);
  lines.push(`  largest file     ${relative(dir, largest.path)} (${mb(largest.bytes)})`);
  process.stdout.write(`\n${dir}\n${lines.join("\n")}\n`);
  const over = [
    files.length > LIMITS.files &&
      `${files.length} files is over the ${LIMITS.files} a publish takes`,
    total > LIMITS.totalBytes &&
      `${mb(total)} is over the ${mb(LIMITS.totalBytes)} a publish takes`,
    largest.bytes > LIMITS.fileBytes &&
      `${relative(dir, largest.path)} is over ${mb(LIMITS.fileBytes)}`,
  ].filter(Boolean);
  if (over.length > 0) {
    process.stderr.write(`\nThis output cannot be published in one go: ${over.join("; ")}.\n`);
    process.exit(1);
  }
}
