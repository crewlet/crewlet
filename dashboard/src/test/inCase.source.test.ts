// @vitest-environment node

/**
 * A suite reaches `act`, and configures the testing library, only through
 * `inCase.ts`.
 *
 * The binding there ends every act scope, wait and event with the case that
 * made it — but only the ones that go through it. The library's own `act`,
 * imported anywhere else, is the bare one again: a `settle()` every case in a
 * file shares, or a line in a case body, that a case still running after its
 * time ran out opens beside the next case, with nothing to refuse it. And a
 * second `configure` of the library replaces the two wrappers its waits and
 * events are bound through, which unbinds every `findBy`, `waitFor` and
 * `fireEvent` in the run at once.
 *
 * There is no ESLint in this tree, so the rule is read off the source, as
 * `app/source.test.ts` reads its own: comments blanked, imports parsed, every
 * offender named by file. A bare `await act(` cannot be told from a bound one
 * by its text — both are spelled the same — so what is held is where `act`
 * COMES FROM: every file that calls it imports it from `inCase.ts`, and nothing
 * but `inCase.ts` imports the library's.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const SRC = fileURLToPath(new URL("..", import.meta.url));
/** The one file that may reach the library's own `act` and `configure`. */
const BINDING = join("test", "inCase.ts");

/** Every TypeScript file under `src/`, suites and helpers alike, comments blanked. */
function sources(): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(full)) continue;
      const text = readFileSync(full, "utf8")
        .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
        .replace(
          /(^|[^:])\/\/[^\n]*/g,
          (m, lead: string) => lead + " ".repeat(m.length - lead.length),
        );
      out.push({ path: relative(SRC, full), text });
    }
  })(SRC);
  return out;
}

interface Import {
  from: string;
  /** Imported name to local name, for `{ a, b as c }`. */
  named: Map<string, string>;
  /** A default or namespace binding (`React`, `* as RTL`), whose members are reached by `.`. */
  whole: string[];
}

function imports(text: string): Import[] {
  const out: Import[] = [];
  const statement = /import\s+(?!type\s)([\s\S]*?)\s+from\s+["']([^"']+)["']/g;
  for (const [, clause, from] of text.matchAll(statement)) {
    const named = new Map<string, string>();
    const whole: string[] = [];
    const braces = /\{([\s\S]*)\}/.exec(clause!);
    for (const spec of braces?.[1]?.split(",") ?? []) {
      const parts = spec
        .trim()
        .replace(/^type\s+/, "")
        .split(/\s+as\s+/);
      if (parts[0]) named.set(parts[0], parts[1] ?? parts[0]);
    }
    const outside = clause!.replace(/\{[\s\S]*\}/, "");
    for (const binding of outside.split(",")) {
      const name = binding.trim().replace(/^\*\s+as\s+/, "");
      if (/^[\w$]+$/.test(name)) whole.push(name);
    }
    out.push({ from: from!, named, whole });
  }
  return out;
}

/** The modules a bare `act` comes from. */
const ACTS = new Set([
  "@testing-library/react",
  "@testing-library/react/pure",
  "react",
  "react-dom/test-utils",
]);
/** The modules whose `configure` replaces the library's wrappers. */
const CONFIGURES = new Set([
  "@testing-library/react",
  "@testing-library/react/pure",
  "@testing-library/dom",
]);

/** Whether `from` names the case-bound module, by alias or relative path. */
const bound = (from: string) => from === "~/test/inCase.ts" || /(^|\/)inCase\.ts$/.test(from);

const files = sources();

/** What a file reaches of the library's that only the binding may. */
function reachesLibrary(text: string): string[] {
  const found: string[] = [];
  for (const imp of imports(text)) {
    if (ACTS.has(imp.from)) {
      if (imp.named.has("act")) found.push(`act from "${imp.from}"`);
      for (const whole of imp.whole) {
        if (new RegExp(`\\b${whole}\\s*\\.\\s*act\\b`).test(text)) found.push(`${whole}.act`);
      }
    }
    if (CONFIGURES.has(imp.from) && imp.named.has("configure")) {
      found.push(`configure from "${imp.from}"`);
    }
  }
  return found;
}

test("the walk reads the suites, and finds the binding reaching the library itself", () => {
  // A walk rooted in the wrong place, or a parser that sees no import, passes
  // the two rules below having checked nothing. The binding is the one file
  // that must import the library's `act` and `configure`, so finding both
  // there is the proof the reading works.
  expect(files.length).toBeGreaterThan(400);
  const binding = files.find((f) => f.path === BINDING);
  expect(binding, `no ${BINDING} under ${SRC}`).toBeDefined();
  expect(reachesLibrary(binding!.text).sort()).toEqual([
    `act from "@testing-library/react"`,
    `configure from "@testing-library/react"`,
  ]);
});

test("nothing but the binding imports the library's act or configures the library", () => {
  const offenders = files
    .filter((f) => f.path !== BINDING)
    .flatMap((f) => reachesLibrary(f.text).map((what) => `${f.path} — ${what}`));
  expect(
    offenders,
    `import \`act\` from "~/test/inCase.ts", whose \`act\` ends with the case that called it; the library's is opened beside the next case by a case that timed out, and a second \`configure\` unbinds every wait and event:\n${offenders.join("\n")}`,
  ).toEqual([]);
});

test("every file that calls act takes it from the binding", () => {
  const calls = files.filter((f) => f.path !== BINDING && /(?<![\w$.])act\s*\(/.test(f.text));
  // The suites that act are many; a pattern that matched none would pass the
  // rule below vacuously.
  expect(calls.length).toBeGreaterThan(50);
  const offenders = calls
    .filter(
      (f) => !imports(f.text).some((imp) => bound(imp.from) && imp.named.get("act") === "act"),
    )
    .map((f) => f.path);
  expect(
    offenders,
    `these call an \`act\` that is not the case-bound one from "~/test/inCase.ts":\n${offenders.join("\n")}`,
  ).toEqual([]);
});
