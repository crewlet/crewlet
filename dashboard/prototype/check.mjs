// @ts-check
/**
 * Check a design topic in jsdom before anyone looks at it:
 *
 *     node prototype/check.mjs --topic <id> [--variant <id> ...]
 *
 * Every variant is booted as its page boots, with its own swaps and its own
 * uilet build, and every route the navigation offers is walked
 * (`check/routes.test.tsx`): a route fails on an error boundary, on "no such
 * screen", or on anything said to the console, which is where the demo engine
 * says it was asked something it does not model. The `today` run also runs the
 * harness's own checks: the derivation against the engine's golden files, the
 * modelled writes, the stylesheet order and the base company's fidelity to
 * the screenshots.
 *
 * Each run gets a temporary directory of its own, removed when it ends
 * (`scripts/owned-tmpdir.mjs` says why).
 *
 * Before the variants, the prototype's own files are typechecked with unused
 * declarations refused. The dashboard's settings do not refuse them (its own
 * source holds some, which a prototype may not touch), so the scan reads the
 * whole program and judges only the files under prototype/.
 */

import { spawnSync } from "node:child_process";
import { parseArgs } from "node:util";
import { DASHBOARD, loadTopic } from "./harness/vite.mjs";

const { values } = parseArgs({
  options: { topic: { type: "string" }, variant: { type: "string", multiple: true } },
});
if (!values.topic) {
  process.stderr.write("usage: node prototype/check.mjs --topic <id> [--variant <id> ...]\n");
  process.exit(2);
}

const loaded = await loadTopic(values.topic);

process.stdout.write("\ntypechecking prototype/, unused declarations included\n");
const typecheck = spawnSync(
  "npx",
  ["tsc", "-p", "prototype/tsconfig.json", "--noEmit", "--noUnusedLocals", "--noUnusedParameters"],
  { cwd: DASHBOARD, encoding: "utf8" },
);
if (typecheck.error) throw typecheck.error;
const own = `${typecheck.stdout}`
  .split("\n")
  .filter((line) => /^prototype\/\S+\(\d+,\d+\): error TS\d+/.test(line));
for (const line of own) process.stdout.write(`  ${line}\n`);
const variants = values.variant ?? loaded.topic.variants.map((v) => v.id);
const results = variants.map((variant) => {
  process.stdout.write(`\nchecking ${loaded.topic.id}/${variant}\n`);
  const run = spawnSync(
    process.execPath,
    [
      "scripts/owned-tmpdir.mjs",
      "npx",
      "vitest",
      "run",
      "--config",
      "prototype/check/vitest.config.mjs",
    ],
    {
      cwd: DASHBOARD,
      stdio: "inherit",
      env: { ...process.env, PROTO_TOPIC: loaded.topic.id, PROTO_VARIANT: variant },
    },
  );
  return { variant, ok: run.status === 0 };
});

process.stdout.write(`\n${loaded.topic.id}\n`);
process.stdout.write(`  ${own.length === 0 ? "pass" : "FAIL"}  typecheck of prototype/\n`);
for (const { variant, ok } of results)
  process.stdout.write(`  ${ok ? "pass" : "FAIL"}  ${variant}\n`);
process.exit(own.length === 0 && results.every((r) => r.ok) ? 0 : 1);
