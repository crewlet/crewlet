/**
 * The suite's temporary files land in a directory the runner removes.
 *
 * Vitest's forks pool writes every transformed module under
 * `join(os.tmpdir(), nanoid())` and never removes the root project's copy, so
 * each `vitest run` used to leave 16-48 MB behind — 11.9 GB over one day of
 * gate runs, on the volume whose free space the engine's embedded broker
 * sizes itself from. `scripts/owned-tmpdir.mjs` runs the suite with TMPDIR
 * pointed at a directory it creates and removes; these cases hold both halves
 * of that by what they produce: that this very run is inside such a
 * directory, and that the wrapper really does remove one, whatever the exit.
 */

import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { basename, dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { expect, test } from "vitest";

// `process.cwd()` for the reason `./source.ts` gives: the dashboard root.
const wrapper = join(process.cwd(), "scripts", "owned-tmpdir.mjs");

/** Runs a node one-liner under the wrapper and returns what it printed and how it ended. */
function underWrapper(script: string) {
  const run = spawnSync(process.execPath, [wrapper, process.execPath, "-e", script], {
    encoding: "utf8",
  });
  return { status: run.status, printed: run.stdout.trim() };
}

test("this run's temporary directory is one the runner owns and removes", () => {
  // Fails when the suite is started without the wrapper — `npm test` and
  // `npm run test:watch` both go through it; a bare `npx vitest` does not,
  // and leaks exactly the directory this file exists to stop leaking.
  expect(basename(tmpdir())).toMatch(/^crewlet-dashboard-/);
});

test("the wrapper removes everything the command left in its temporary directory", () => {
  const { status, printed } = underWrapper(`
    const fs = require("node:fs"), os = require("node:os"), path = require("node:path");
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "leak-"));
    fs.writeFileSync(path.join(dir, "module.js"), "x".repeat(1024));
    console.log(dir);
  `);
  expect(status).toBe(0);
  expect(basename(dirname(printed))).toMatch(/^crewlet-dashboard-/);
  expect(existsSync(dirname(printed))).toBe(false);
});

test("a failing command still has its directory removed, and its exit status is the wrapper's", () => {
  const { status, printed } = underWrapper(`
    console.log(require("node:os").tmpdir());
    process.exit(3);
  `);
  expect(status).toBe(3);
  expect(basename(printed)).toMatch(/^crewlet-dashboard-/);
  expect(existsSync(printed)).toBe(false);
});
