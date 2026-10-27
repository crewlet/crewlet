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
  // NO NUMBER, NO NUMBER SAID: an unknown wait is "later", one over is "now".
  expect(fmtWait(Number.NaN)).toBe("later");
  expect(fmtWait(Number.POSITIVE_INFINITY)).toBe("later");
  expect(fmtWait(0)).toBe("now");
  expect(fmtWait(-5)).toBe("now");
});

/**
 * THE FILES ALLOWED TO SPELL A WAIT THEMSELVES: the formatters, and the
 * protocol layer's timeout sentence, which is built from constants of a minute
 * or two, never from a count the engine sent, already switches to minutes past
 * two (`protocol/rest.ts`'s `waitWords`), and cannot import the app's
 * formatters because the protocol is also built alone.
 */
const ALLOWED = new Set(["lib/format.ts", "protocol/rest.ts"]);

/** Every non-test source file under `src/`, as [path, text]. */
function sources(): [string, string][] {
  const root = join(dirname(fileURLToPath(import.meta.url)), "..");
  const out: [string, string][] = [];
  (function walk(abs: string): void {
    for (const entry of readdirSync(abs)) {
      const full = join(abs, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.(ts|tsx)$/.test(entry) || /\.test\.(ts|tsx)$/.test(entry)) continue;
      out.push([relative(root, full), readFileSync(full, "utf8")]);
    }
  })(root);
  return out;
}

/**
 * THE SHAPES A COUNT OF SECONDS TAKES ON A SCREEN, each of which has shipped
 * or was found in review: a number interpolated before "seconds" ("ask again
 * in 81155 seconds"), a plural of "second" ("Try again in 81,155 seconds"),
 * a bare `s` after a wait word ("Try again in 900s", "wait 30s", "every 5s",
 * in a template or in JSX), a number before "sec", and concatenation.
 *
 * SCOPED TO TEXT AFTER A WORD: the bare-`s` and "sec" shapes match only after
 * a word a sentence says a wait or a period with (in, again, after, within,
 * wait, every, for) or, for "sec", any word, so a CSS duration
 * (`animation: spin ${ms}ms`) or an identifier is not a sentence.
 */
const WAIT_WORDS = "in|again|after|within|wait|every|for";
const RAW_SECONDS: [string, RegExp][] = [
  ["interpolated before seconds", /\$\{[^}]+\}\s*(seconds|secs)\b/],
  ["plural of second", /plural\([^)]*,\s*["'`]second["'`]\s*\)/],
  ["interpolated wait with a bare s", new RegExp(`\\b(${WAIT_WORDS}) \\$\\{[^}]+\\}s\\b`)],
  ["JSX wait with a bare s", new RegExp(`\\b(${WAIT_WORDS}) \\{[^{}]+\\}s\\b`)],
  ["interpolated before sec", /\b[A-Za-z]+ \$\{[^}]+\} ?sec\b/],
  ["JSX number before seconds", /\{[^{}]+\}\s*(seconds|secs|sec)\b/],
  ["concatenated seconds", /\+\s*["'`]\s*(seconds|secs|sec)\b/],
];

/**
 * EACH SHAPE IS PROVEN BOTH WAYS on fixtures: red on the text that shipped or
 * was found, green on text that is not a wait (a CSS duration, a setting's
 * unit name, an identifier), so a pattern that stopped matching, or started
 * matching everything, fails here before it fails the scan.
 */
const FIXTURES: Record<string, { red: string[]; green: string[] }> = {
  "interpolated before seconds": {
    red: ["`You can ask again in ${waitSeconds} seconds`", "`${n}seconds`"],
    green: ["`${n} minutes`", "timeout_seconds"],
  },
  "plural of second": {
    red: ['plural(err.retryAfterSeconds, "second")', "plural(n, 'second')"],
    green: ['plural(n, "step")', 'plural(n, "seconds-long")'],
  },
  "interpolated wait with a bare s": {
    red: ["`Try again in ${left}s`", "`wait ${n}s`", "`every ${x}s`", "`for ${n}s`"],
    green: ["`animation: spin ${ms}s`", "`${count}s`", "`items ${n}s`"],
  },
  "JSX wait with a bare s": {
    red: ["Try again in {left}s", "Refreshes every {n}s", "wait {n}s"],
    green: ["spin {ms}s", "{count}s", "rows {n}s"],
  },
  "interpolated before sec": {
    red: ["`after ${n} sec`", "`timeout ${n}sec`"],
    green: ["`${n} sec`", "`after ${n} seconds-long`", "`${n} section`"],
  },
  "JSX number before seconds": {
    red: ["in {n} seconds", "{left} sec", "{n} secs"],
    green: ["{n} sections", "{n} minutes"],
  },
  "concatenated seconds": {
    red: ['"in " + n + " seconds"', "n + ' sec'"],
    green: ['n + " minutes"', 'n + " section"'],
  },
};

test("every raw-seconds shape is red on what shipped and green on what is not a wait", () => {
  expect(Object.keys(FIXTURES).sort()).toEqual(RAW_SECONDS.map(([shape]) => shape).sort());
  for (const [shape, pattern] of RAW_SECONDS) {
    const { red, green } = FIXTURES[shape]!;
    for (const text of red)
      expect(`${shape} red ${text}: ${pattern.test(text)}`).toBe(`${shape} red ${text}: true`);
    for (const text of green)
      expect(`${shape} green ${text}: ${pattern.test(text)}`).toBe(`${shape} green ${text}: false`);
  }
});

test("no screen prints a count of seconds", () => {
  const offenders: string[] = [];
  for (const [path, text] of sources()) {
    if (ALLOWED.has(path)) continue;
    for (const [shape, pattern] of RAW_SECONDS) {
      if (pattern.test(text)) offenders.push(`${path}: ${shape}`);
    }
  }
  expect(offenders).toEqual([]);
});

/**
 * AND STRUCTURALLY: a wait the engine sent (`retryAfterSeconds`) reaches a
 * person only through [fmtWait]. Every place it is interpolated into text,
 * outside the protocol layer that parses it, wraps it in `fmtWait(...)`;
 * every other read is arithmetic for a timer.
 */
test("a wait the engine sent is said only through fmtWait", () => {
  // What is left of an expression once its sanctioned reads are taken out:
  // the wait said through `fmtWait(...)`, and a null check before it.
  const unsaid = (expr: string) =>
    expr
      .replace(/fmtWait\([^()]*(\([^()]*\))?[^()]*\)/g, "")
      .replace(/retryAfterSeconds\s*[!=]==?\s*null/g, "")
      .includes("retryAfterSeconds");
  const offenders: string[] = [];
  for (const [path, text] of sources()) {
    if (path.startsWith("protocol/") || ALLOWED.has(path)) continue;
    // A template interpolation, and a JSX child expression (an element's `>`,
    // never an arrow's `=>`).
    for (const [, inside] of text.matchAll(/\$\{([^{}]*retryAfterSeconds[^{}]*)\}/g)) {
      if (unsaid(inside!)) offenders.push(`${path}: \${${inside}}`);
    }
    for (const [, inside] of text.matchAll(/(?<!=)>\s*\{([^{}]*retryAfterSeconds[^{}]*)\}/g)) {
      if (unsaid(inside!)) offenders.push(`${path}: {${inside}}`);
    }
  }
  expect(offenders).toEqual([]);
  // The guard sees the call sites it guards.
  const said = sources()
    .filter(([, text]) => /fmtWait\([^)]*retryAfterSeconds/.test(text))
    .map(([path]) => path);
  expect(said).toEqual(
    expect.arrayContaining(["app/assistant/chunk/Thread.tsx", "lib/refusal.ts"]),
  );
});
