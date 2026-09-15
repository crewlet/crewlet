// @vitest-environment node
/**
 * A recipe the design system owns is spelled nowhere else.
 *
 * The `.btn` class list has no owner in this tree at all now: it was
 * `ui/primitives.tsx`, and every button on every screen is uilet's. The
 * `<kbd>` element still has one, `ui/Kbd.tsx`. Each used to be written by
 * hand beside its primitive as well: a menu trigger and a list's Move buttons
 * spelled `btn ghost sm icon` themselves, three links spelled `btn primary`,
 * and the shell drew a bare `<kbd>` that told every reader not on a Mac to
 * press a key they do not have. A copy of a recipe is where the two drift
 * apart, and nothing but a scan notices one being added, so the rule in
 * `docs/reference/dashboard-design.md` is asserted here.
 *
 * Sources are read as TEXT, in the idiom of `ui/boundary.test.ts`, with
 * comments removed so a doc that quotes the old markup is not read as it.
 */

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const SRC = resolve(dirname(fileURLToPath(import.meta.url)), "..");

/** Every shipped .ts and .tsx file under src/. Tests may spell markup to assert on it. */
function sources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sources(path));
    else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) out.push(path);
  }
  return out;
}

function code(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
}

/** A string literal holding the `btn` class as a whole word. */
const BTN_CLASS = /(["'`])(?:[^"'`\n]*\s)?btn(?:\s[^"'`\n]*)?\1/;
/** A `<kbd>` element in JSX. */
const KBD_ELEMENT = /<kbd[\s>]/;

function offenders(pattern: RegExp, owner?: string): string[] {
  return sources(SRC)
    .map((file) => relative(SRC, file).split(sep).join("/"))
    .filter((file) => file !== owner)
    .filter((file) => pattern.test(code(readFileSync(join(SRC, file), "utf8"))));
}

test("the scan recognises the recipes it polices", () => {
  // A pattern that matched nothing would pass both tests below forever.
  expect(BTN_CLASS.test('<a className="btn sm primary" href="#/">')).toBe(true);
  expect(BTN_CLASS.test('cx("btn", "ghost")')).toBe(true);
  expect(BTN_CLASS.test("className={`btn ${tone}`}")).toBe(true);
  expect(BTN_CLASS.test('className="btn-group"')).toBe(false);
  expect(BTN_CLASS.test('className="subtn"')).toBe(false);
  expect(KBD_ELEMENT.test("<kbd>Command K</kbd>")).toBe(true);
  expect(code(readFileSync(join(SRC, "ui/Kbd.tsx"), "utf8"))).toMatch(KBD_ELEMENT);
});

test("nothing spells the button class list, which no longer has an owner", () => {
  expect(offenders(BTN_CLASS)).toEqual([]);
});

test("only ui/Kbd.tsx draws a kbd element", () => {
  expect(offenders(KBD_ELEMENT, "ui/Kbd.tsx")).toEqual([]);
});
