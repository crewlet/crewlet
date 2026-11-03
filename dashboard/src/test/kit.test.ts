// @vitest-environment node

/**
 * The design system's modules as the gates read them (`kit.ts`), held to what
 * the gates rely on it for: an import names the package as the package
 * resolves it, and a module hands on every name it exports, through every
 * module it re-exports whole.
 */

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, expect, test } from "vitest";
import { kitExports, kitModule, kitReaches, moduleExports } from "./kit.ts";

test("an import names the package's root, a module of it, or nothing of it", () => {
  expect(kitModule("@crewlethq/ui")).toBe("");
  expect(kitModule("@crewlethq/ui/DiffView")).toBe("DiffView");
  expect(kitModule("@crewlethq/uikit")).toBeNull();
  expect(kitModule("@crewlethq/tokens")).toBeNull();
  expect(kitModule("~/components/DiffView.tsx")).toBeNull();
  expect(kitModule(null)).toBeNull();
});

// THE PACKAGE AS INSTALLED, read from the modules a browser loads: what the
// gates over the diffs judge an import by.
test("the installed package says what each of its modules hands an importer", () => {
  const diff = kitExports("DiffView");
  expect(["DiffView", "DiffSummary", "diffLines"].filter((name) => !diff.has(name))).toEqual([]);
  expect(diff.has("DiffList")).toBe(false);
  expect(kitReaches("DiffList", ["DiffList"])).toBe(true);
  // THE ROOT AND `generated` hand on every module, the diffs among them.
  for (const whole of ["", "generated"]) {
    expect(kitReaches(whole, ["DiffList"]), whole || "the root").toBe(true);
    expect(kitReaches(whole, ["diffLines"]), whole || "the root").toBe(true);
  }
  expect(kitReaches("Button", ["DiffView", "DiffSummary", "DiffList", "diffLines"])).toBe(false);
  // A module the package does not have hands nothing on, and neither does
  // what is not the package.
  expect(kitExports("NoSuchModule").size).toBe(0);
  expect(kitReaches(null, ["DiffList"])).toBe(false);
});

// A PACKAGE OF ITS OWN, since the installed one names every export of every
// module and so never re-exports one whole: a release that did must not hide
// a name behind it.
const root = mkdtempSync(join(tmpdir(), "kit-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

function write(path: string, text: string): string {
  mkdirSync(join(root, path, ".."), { recursive: true });
  writeFileSync(join(root, path), text);
  return join(root, path);
}

test("a module re-exported whole hands on every name it has, however deep", () => {
  const index = write(
    "index.js",
    ['export * from "./generated/index.js";', 'export { cx } from "./chunk.js";'].join("\n"),
  );
  write(
    "generated/index.js",
    ['export * from "../DiffList/index.js";', 'export * as kit from "../index.js";'].join("\n"),
  );
  const list = write(
    "DiffList/index.js",
    [
      'export { DiffList, DIFF_LIST_LABELS as LABELS } from "../chunk.js";',
      'export * from "../generated/index.js";',
      "export const MAX = 1, MIN = 0;",
      "export function draw() {}",
      "export class Board {}",
    ].join("\n"),
  );
  const own = ["Board", "DiffList", "LABELS", "MAX", "MIN", "draw", "kit"];
  expect([...moduleExports(index)].sort()).toEqual([...own, "cx"].sort());
  // THE CHAIN LEADS BACK INTO ITSELF (the list hands `generated` on, which
  // hands the list on), and a module read from inside it is as complete as
  // one read from outside.
  expect([...moduleExports(list)].sort()).toEqual([...own].sort());
});

test("an export it cannot name fails the reading, every time, rather than leaving it out", () => {
  const destructured = write("pattern/index.js", "export const { DiffList } = kit;");
  const why = /cannot name what an export binds \(ObjectPattern\)/;
  expect(() => moduleExports(destructured)).toThrow(why);
  expect(() => moduleExports(destructured)).toThrow(why);
});
