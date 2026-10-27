// @vitest-environment node

/**
 * A wait is said as a time to plan by, never as a count of seconds.
 *
 * The assistant printed the engine's own count, "ask again in 81155 seconds".
 * [fmtWait] is the one way a wait is said, and the scan below holds every
 * screen to it: a template that interpolates a number before the word
 * "seconds" is a count a person would have to divide.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";
import { fmtWait } from "./format.ts";

test("a wait is said in the largest two units, rounded up", () => {
  expect(fmtWait(81155)).toBe("in 22h 33m");
  expect(fmtWait(45)).toBe("in 45s");
  expect(fmtWait(0.2)).toBe("in 1s");
  expect(fmtWait(59.1)).toBe("in 1m");
  expect(fmtWait(4 * 60 + 29)).toBe("in 5m");
  expect(fmtWait(3600)).toBe("in 1h");
  expect(fmtWait(2 * 86_400 + 3 * 3600)).toBe("in 2d 3h");
  expect(fmtWait(3 * 86_400)).toBe("in 3d");
  expect(fmtWait(Number.NaN)).toBe("in 1s");
});

/**
 * THE PROTOCOL LAYER'S TIMEOUT SENTENCE is the one exception: it is built
 * from constants of a minute or two, never from a count the engine sent, and
 * it already switches to minutes past two (`protocol/rest.ts`'s `waitWords`).
 * It cannot import the app's formatters: the protocol is also built alone.
 */
const ALLOWED = new Set(["protocol/rest.ts"]);

test("no screen prints a count of seconds", () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), "..");
  const offenders: string[] = [];
  (function walk(abs: string): void {
    for (const entry of readdirSync(abs)) {
      const full = join(abs, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.(ts|tsx)$/.test(entry) || /\.test\.(ts|tsx)$/.test(entry)) continue;
      const rel = relative(root, full);
      if (ALLOWED.has(rel)) continue;
      const text = readFileSync(full, "utf8");
      if (/\$\{[^}]+\}\s*seconds\b/.test(text)) offenders.push(rel);
    }
  })(root);
  expect(offenders).toEqual([]);
});
