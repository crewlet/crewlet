// @vitest-environment node
/**
 * The builder core stays pure.
 *
 * Everything in this directory is meant to run under a test with a fake clock,
 * fake storage and a scripted engine, and to behave identically when React
 * runs a reducer twice. That holds only while no module here reaches for
 * React, the DOM, the network, the real clock or real randomness, and a single
 * `Date.now()` in a reducer would break it in a way no test of the reducer
 * notices. So the rule is read off the source text, in the idiom of
 * `ui/boundary.test.ts`:
 *
 * - runtime imports name a file in this directory, `~/lib/format.ts` (pure
 *   formatting), `~/lib/storage.ts` (the table of storage KEYS — the storage
 *   itself is still injected) or `~/protocol/retry.ts` (pure arithmetic over
 *   the engine's retry hint, which the check waits out exactly as the
 *   socket's questions do, so it is one reading rather than a copy); the last
 *   two are DOORS held below to being as pure as this directory, and the rest
 *   of `~/protocol` is imported for TYPES only, because its runtime half is
 *   the socket and `fetch`;
 * - no module names a browser or time global.
 */

import { describe, expect, test } from "vitest";

import { modules } from "~/test/source.ts";

/** This directory, relative to `src/`. */
const MODEL = "routes/org/builder/model/";

/** Pure formatting: the one module outside this directory and its doors the core may import. */
const FORMAT = "~/lib/format.ts";

/**
 * The DOORS: the modules the core may import at runtime from where the
 * network lives, by the specifier it imports them under and their path under
 * `src/`, each with the runtime imports it may make itself — `protocol/retry.ts`
 * reads the engine's two waits out of the contract, which is data and nothing
 * else (the contract's own suite holds that), and nothing else may reach
 * further.
 */
const DOORS: readonly { spec: string; path: string; reaches: readonly string[] }[] = [
  { spec: "~/lib/storage.ts", path: "lib/storage.ts", reaches: [] },
  { spec: "~/protocol/retry.ts", path: "protocol/retry.ts", reaches: ["../contract/retry.ts"] },
];

/** The modules of this directory, tests excluded: a test may use whatever it needs. */
const sources = modules()
  .map(({ path, text }) => ({ name: path.slice(MODEL.length), path, text }))
  .filter(({ path, name }) => path.startsWith(MODEL) && !name.includes("/"));

/** Source without block comments, line comments or string contents, so prose cannot match. */
function code(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "")
    .replace(/`(?:\\[\s\S]|[^`\\])*`/g, "``")
    .replace(/"(?:\\.|[^"\\\n])*"/g, '""')
    .replace(/'(?:\\.|[^'\\\n])*'/g, "''");
}

/** Every import statement: its specifier and whether it is type-only. */
function imports(text: string): { spec: string; typeOnly: boolean }[] {
  const stripped = text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
  const out: { spec: string; typeOnly: boolean }[] = [];
  for (const match of stripped.matchAll(
    /(?:^|[;\s])(import|export)(\s+type)?\s[^;"']*?\sfrom\s*["']([^"']+)["']/g,
  )) {
    out.push({ spec: match[3]!, typeOnly: match[2] !== undefined });
  }
  for (const match of stripped.matchAll(/(?:^|[;\s])import\s*["']([^"']+)["']/g)) {
    out.push({ spec: match[1]!, typeOnly: false });
  }
  for (const match of stripped.matchAll(/\bimport\(\s*["']([^"']+)["']/g)) {
    out.push({ spec: match[1]!, typeOnly: false });
  }
  return out;
}

const FORBIDDEN_GLOBALS: readonly [string, RegExp][] = [
  ["fetch", /\bfetch\s*\(/],
  ["window", /\bwindow\b/],
  ["globalThis", /\bglobalThis\b/],
  [
    "the DOM document",
    /\bdocument\s*\.\s*(?:getElement|querySelector|createElement|body|documentElement|addEventListener|activeElement)/,
  ],
  ["location", /\blocation\s*\./],
  ["navigator", /\bnavigator\b/],
  ["localStorage", /\blocalStorage\b/],
  ["sessionStorage", /\bsessionStorage\b/],
  ["setTimeout", /\bsetTimeout\b/],
  ["setInterval", /\bsetInterval\b/],
  ["Date.now", /\bDate\s*\.\s*now\b/],
  ["new Date", /\bnew\s+Date\b/],
  ["performance", /\bperformance\s*\./],
  ["Math.random", /\bMath\s*\.\s*random\b/],
  ["crypto", /\bcrypto\s*\./],
];

describe("the builder core", () => {
  test("has modules to check", () => {
    // A directory rename that left this suite reading nothing would pass
    // every assertion below.
    expect(sources.map((s) => s.name)).toEqual(expect.arrayContaining(["keys.ts", "document.ts"]));
  });

  test("imports at runtime only its own files and pure formatting, and the protocol for types only", () => {
    const offending: string[] = [];
    for (const { name, text } of sources) {
      for (const { spec, typeOnly } of imports(text)) {
        const allowed =
          spec.startsWith("./") ||
          spec === FORMAT ||
          DOORS.some((door) => door.spec === spec) ||
          (typeOnly && spec.startsWith("~/protocol/"));
        if (!allowed) offending.push(`${name}: ${typeOnly ? "import type" : "import"} "${spec}"`);
      }
    }
    expect(offending).toEqual([]);
  });

  // A DOOR IS ONLY AS PURE AS WHAT IT IMPORTS: one that reached for `rest.ts`
  // or the socket would bring the network in with it, past every rule above.
  // So each imports at runtime only what it is listed as reaching, and names
  // no global either.
  test.each(DOORS)(
    "$spec imports only what it is listed as reaching and names no global",
    (door) => {
      const found = modules().find(({ path }) => path === door.path);
      expect(found, `${door.path} is gone`).toBeDefined();
      const runtime = imports(found!.text)
        .filter(({ typeOnly }) => !typeOnly)
        .map(({ spec }) => spec);
      expect(runtime).toEqual(door.reaches);
      const body = code(found!.text);
      expect(FORBIDDEN_GLOBALS.filter(([, pattern]) => pattern.test(body)).map(([l]) => l)).toEqual(
        [],
      );
    },
  );

  test("names no browser, network, clock or randomness global", () => {
    const offending: string[] = [];
    for (const { name, text } of sources) {
      const body = code(text);
      for (const [label, pattern] of FORBIDDEN_GLOBALS) {
        if (pattern.test(body)) offending.push(`${name}: ${label}`);
      }
    }
    expect(offending).toEqual([]);
  });

  test("the scan catches what it forbids", () => {
    // The guards above are regular expressions over text; prove each one
    // matches the thing it names, or a typo in a pattern passes silently.
    for (const [label, pattern] of FORBIDDEN_GLOBALS) {
      const sample: Record<string, string> = {
        fetch: "fetch(url)",
        window: "window.x",
        globalThis: "globalThis.x",
        "the DOM document": "document.querySelector('a')",
        location: "location.origin",
        navigator: "navigator.onLine",
        localStorage: "localStorage.getItem",
        sessionStorage: "sessionStorage.getItem",
        setTimeout: "setTimeout(f, 1)",
        setInterval: "setInterval(f, 1)",
        "Date.now": "Date.now()",
        "new Date": "new Date()",
        performance: "performance.now()",
        "Math.random": "Math.random()",
        crypto: "crypto.randomUUID()",
      };
      expect(pattern.test(code(sample[label]!)), label).toBe(true);
    }
    expect(imports('import { useState } from "react";')).toEqual([
      { spec: "react", typeOnly: false },
    ]);
    expect(imports('import type { Derived } from "~/protocol/index.ts";')).toEqual([
      { spec: "~/protocol/index.ts", typeOnly: true },
    ]);
  });
});
