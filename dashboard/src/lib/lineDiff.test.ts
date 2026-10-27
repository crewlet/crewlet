/**
 * Two texts compared line by line: what the one line diff says, and when it
 * says it did not compare them.
 */

import { describe, expect, test } from "vitest";
import { collapseDiff, type DiffLine, type DiffSection } from "@crewlethq/ui";
import { kitModule, kitReaches, spelled } from "~/test/kit.ts";
import { lineOf, modules, parse, stringValue, walk, type Lang, type Node } from "~/test/source.ts";
import {
  MAX_COMPARE_LINES,
  MAX_EDITS,
  compareLines,
  distanceFloor,
  linesOf,
  notComparedReason,
  type LineComparison,
} from "./lineDiff.ts";

/** The runs of a comparison that must have been made. */
function hunksOf(got: LineComparison): DiffSection[] {
  if (got.kind !== "compared") throw new Error(`not compared: ${got.kind}`);
  return got.hunks;
}

/** A section's lines as `kind before|after text`, the way a reader scans them. */
const said = (hunks: readonly DiffSection[]) =>
  hunks.map((h) => ({
    skipped: h.skipped,
    lines: h.lines.map((l) => `${l.kind} ${l.before ?? ""}|${l.after ?? ""} ${l.text}`),
  }));

/** `n` distinct numbered lines, from `from`. */
const numbered = (n: number, from = 0, prefix = "line") =>
  Array.from({ length: n }, (_, i) => `${prefix} ${from + i}`);

/** Lines as a text that ends each with a newline: no lines is the empty text, not one empty line. */
const text = (lines: readonly string[]) => (lines.length === 0 ? "" : lines.join("\n") + "\n");

/** A deterministic pseudo-random source, so a failing case is the same case every run. */
function random(seed: number) {
  return () => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return seed / 2147483648;
  };
}

/** The length of the longest common subsequence, by the textbook table. */
function lcs(a: readonly string[], b: readonly string[]): number {
  const t = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = 1; i <= a.length; i++) {
    for (let j = 1; j <= b.length; j++) {
      const row = t[i] ?? [];
      row[j] =
        a[i - 1] === b[j - 1]
          ? (t[i - 1]?.[j - 1] ?? 0) + 1
          : Math.max(t[i - 1]?.[j] ?? 0, row[j - 1] ?? 0);
    }
  }
  return t[a.length]?.[b.length] ?? 0;
}

/**
 * Every line of a comparison, unchanged ones included: one section, since a
 * context wider than either text keeps them all.
 */
function everyLine(a: readonly string[], b: readonly string[]): DiffLine[] {
  return hunksOf(compareLines(text(a), text(b), Infinity))[0]?.lines ?? [];
}

/**
 * Holds one comparison of `a` with `b` to what a shortest edit is: every line
 * numbered where it stands, the older text turned into the newer by it, no
 * longer than the longest common subsequence allows, and each change's removed
 * lines before its added ones.
 */
function holdsTheShortestEdit(a: readonly string[], b: readonly string[], label: string) {
  const all = everyLine(a, b);
  const replayed: string[] = [];
  let x = 0;
  let y = 0;
  let edits = 0;
  let adding = false;
  for (const line of all) {
    if (line.kind === "same") {
      expect([line.text, line.before, line.after], label).toEqual([a[x], x + 1, y + 1]);
      replayed.push(line.text);
      x++;
      y++;
      adding = false;
      continue;
    }
    edits++;
    if (line.kind === "remove") {
      expect(adding, `${label}: a removed line after an added one in one change`).toBe(false);
      expect([line.text, line.before, line.after], label).toEqual([a[x], x + 1, null]);
      x++;
      continue;
    }
    adding = true;
    expect([line.text, line.before, line.after], label).toEqual([b[y], null, y + 1]);
    replayed.push(line.text);
    y++;
  }
  if (all.length === 0) replayed.push(...a);
  expect(replayed, label).toEqual(b);
  expect(edits, label).toBe(a.length + b.length - 2 * lcs(a, b));
}

describe("two texts line by line", () => {
  test("split on every line ending, a final newline ending the last line", () => {
    expect(linesOf("a\nb\r\nc\n")).toEqual(["a", "b", "c"]);
    expect(linesOf("a\rb")).toEqual(["a", "b"]);
    expect(linesOf("")).toEqual([]);
    expect(linesOf("\n")).toEqual([""]);
    expect(linesOf("one")).toEqual(["one"]);
  });

  test("that are the same lines have no runs, at any length", () => {
    expect(compareLines("a\nb\n", "a\nb\n")).toEqual({ kind: "compared", hunks: [] });
    // A LINE ENDING IS NOT A LINE: the same lines in other endings are the same.
    expect(compareLines("a\r\nb\r\n", "a\nb")).toEqual({ kind: "compared", hunks: [] });
    // Nothing changed, so nothing is past the bound.
    const long = text(numbered(MAX_COMPARE_LINES + 1));
    expect(compareLines(long, long)).toEqual({ kind: "compared", hunks: [] });
  });

  test("name a changed line where both versions hold it, with the lines either side", () => {
    expect(said(hunksOf(compareLines("a\nb\nc\n", "a\nB\nc\n")))).toEqual([
      { skipped: 0, lines: ["same 1|1 a", "remove 2| b", "add |2 B", "same 3|3 c"] },
    ]);
  });

  test("name an addition and a removal apart", () => {
    expect(said(hunksOf(compareLines("a\nb\nc\nd\n", "a\nc\nd\ne\n", 0)))).toEqual([
      { skipped: 1, lines: ["remove 2| b"] },
      { skipped: 2, lines: ["add |4 e"] },
    ]);
  });

  // THE GAP BEFORE EACH RUN IS COUNTED IN UNCHANGED LINES: after two lines
  // replaced from line 3, a line added after line 9 skips the five unchanged
  // lines between them, and each line keeps its own number in its own text.
  test("skip the unchanged lines between runs, each line at its number", () => {
    const older = ["a", "b", "c", "d", "e", "f", "g", "h", "i"];
    const newer = ["a", "b", "C", "e", "f", "g", "h", "i", "new"];
    expect(said(hunksOf(compareLines(text(older), text(newer), 0)))).toEqual([
      { skipped: 2, lines: ["remove 3| c", "remove 4| d", "add |3 C"] },
      { skipped: 5, lines: ["add |9 new"] },
    ]);
  });

  test("are the shortest edit between them, whatever the texts", () => {
    const rand = random(7);
    for (let round = 0; round < 200; round++) {
      const draw = () =>
        Array.from({ length: Math.floor(rand() * 30) }, () => "abcde"[Math.floor(rand() * 5)]!);
      holdsTheShortestEdit(draw(), draw(), `round ${round}`);
    }
  });

  // AN EDIT OF A REAL TEXT: a few hundred lines, most kept, some removed,
  // rewritten and added, with repeated lines (a blank, a closing brace)
  // throughout, which is what sends a comparison down its deeper cuts.
  test("are the shortest edit of a long text edited in many places", () => {
    const rand = random(11);
    const pool = ["", "}", "  return x;", ...numbered(40, 0, "text")];
    for (let round = 0; round < 20; round++) {
      const older = Array.from(
        { length: 100 + Math.floor(rand() * 300) },
        () => pool[Math.floor(rand() * pool.length)]!,
      );
      const newer: string[] = [];
      for (const line of older) {
        const roll = rand();
        if (roll < 0.06) continue;
        if (roll < 0.12) newer.push(`edited ${Math.floor(rand() * 1000)}`);
        else newer.push(line);
        if (rand() < 0.05) newer.push(pool[Math.floor(rand() * pool.length)]!);
      }
      holdsTheShortestEdit(older, newer, `round ${round}`);
    }
  });

  // THE SECTIONS ARE THE DESIGN SYSTEM'S: built without the unchanged lines
  // they leave out, and exactly what `collapseDiff` makes of every line.
  test("keep the context collapseDiff keeps, at every width", () => {
    const rand = random(3);
    for (let round = 0; round < 100; round++) {
      const draw = () =>
        Array.from({ length: Math.floor(rand() * 40) }, () => "abcdefg"[Math.floor(rand() * 7)]!);
      const a = draw();
      const b = draw();
      for (const context of [0, 1, 2, 3, 5]) {
        expect(hunksOf(compareLines(text(a), text(b), context)), `round ${round}`).toEqual(
          collapseDiff(everyLine(a, b), context),
        );
      }
    }
  });

  // THE LINES A COMPARISON BUILDS FOLLOW WHAT CHANGED: a long text edited on
  // one line is that line and its context, and the bound is not reached by
  // the lines the two share at their ends.
  test("of a long text edited on one line are that line and its context", () => {
    const older = numbered(MAX_COMPARE_LINES + 5_000);
    const newer = [...older];
    newer[10_000] = "line 10000, edited";
    expect(said(hunksOf(compareLines(text(older), text(newer))))).toEqual([
      {
        skipped: 9_997,
        lines: [
          "same 9998|9998 line 9997",
          "same 9999|9999 line 9998",
          "same 10000|10000 line 9999",
          "remove 10001| line 10000",
          "add |10001 line 10000, edited",
          "same 10002|10002 line 10001",
          "same 10003|10003 line 10002",
          "same 10004|10004 line 10003",
        ],
      },
    ]);
  });
});

describe("two texts not compared", () => {
  test("past the line bound in what changed", () => {
    const at = numbered(MAX_COMPARE_LINES);
    // Every line of it is between the first and the last, which both changed.
    const ends = (lines: string[]) => ["first, edited", ...lines.slice(1, -1), "last, edited"];
    expect(compareLines(text(at), text(ends(at))).kind).toBe("compared");
    const past = numbered(MAX_COMPARE_LINES + 1);
    expect(compareLines(text(past), text(ends(past)))).toEqual({
      kind: "too-large",
      lines: MAX_COMPARE_LINES + 1,
    });
    expect(compareLines(text(past), "x")).toEqual({
      kind: "too-large",
      lines: MAX_COMPARE_LINES + 1,
    });
  });

  // ONE SIDE EMPTIED: a removal of every line is as many edits as lines.
  test("past the edit bound when one side holds nothing of the other", () => {
    expect(compareLines(text(numbered(MAX_EDITS)), "").kind).toBe("compared");
    expect(compareLines(text(numbered(MAX_EDITS + 1)), "")).toEqual({ kind: "too-different" });
    expect(compareLines("", text(numbered(MAX_EDITS + 1)))).toEqual({ kind: "too-different" });
  });

  test("with nothing in common past the edit bound", () => {
    const a = text(numbered(3_000, 0, "a"));
    const b = text(numbered(3_000, 0, "b"));
    expect(compareLines(a, b)).toEqual({ kind: "too-different" });
  });

  // THE BOUND IS EXACT, AND HELD BY THE ROUNDS THEMSELVES: one text and the
  // same lines in reverse share every line as often, so no count of the
  // lines can tell, and their shortest edit keeps one line of the k and
  // removes and adds every other: 2k - 2 lines.
  test("at one line more than the edit bound, and compared at it", () => {
    const k = MAX_EDITS / 2 + 1;
    const ascending = numbered(k);
    const compared = everyLine(ascending, [...ascending].reverse());
    expect(compared.filter((l) => l.kind === "same")).toHaveLength(1);
    expect(compared.filter((l) => l.kind !== "same")).toHaveLength(MAX_EDITS);
    const more = numbered(k + 1);
    expect(compareLines(text(more), text([...more].reverse()))).toEqual({
      kind: "too-different",
    });
  });

  test("say why, in the clause a host's sentence ends with", () => {
    expect(notComparedReason({ kind: "too-large", lines: 25_000 })).toBe(
      "what changed spans 25,000 lines, more than the 20,000 a comparison reads",
    );
    expect(notComparedReason({ kind: "too-different" })).toBe(
      "more than 4,000 lines were removed or added",
    );
  });
});

/** The design system's line diff, the one name this gate is about. */
const KIT_DIFF = ["diffLines"];

/**
 * Where a module reaches the design system's `diffLines`, by every way a
 * module can:
 *
 * - an import of it by name, from the package's root or any module of it,
 *   under whatever local name, or a re-export of it by name;
 * - a namespace import, a dynamic import or a whole re-export (`export *`) of
 *   a module the package says hands it on (`test/kit.ts`) — its root,
 *   `@crewlethq/ui/generated` and `@crewlethq/ui/DiffView` today — each of
 *   which reaches it under a name this gate cannot follow.
 */
function kitDiffSites(path: string, text: string, lang: Lang): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  const at = (node: Node) => `${path}:${line(node.start)}`;
  walk(parse(text, lang), (node: Node) => {
    switch (node.type) {
      case "ImportDeclaration": {
        const source = (node.source as Node).value;
        const module = kitModule(source);
        if (module === null) return false;
        for (const spec of node.specifiers as Node[]) {
          if (spec.type === "ImportSpecifier") {
            if (spelled(spec.imported as Node) === "diffLines") {
              out.push(`${at(spec)} imports diffLines`);
            }
          } else if (spec.type === "ImportNamespaceSpecifier" && kitReaches(module, KIT_DIFF)) {
            const local = String((spec.local as Node).name);
            out.push(`${at(spec)} imports ${String(source)} whole, as ${local}`);
          }
        }
        return false;
      }
      case "ImportExpression": {
        const source = stringValue(node.source as Node);
        if (kitReaches(kitModule(source), KIT_DIFF)) {
          out.push(`${at(node)} imports ${String(source)} at run time`);
        }
        return;
      }
      case "ExportNamedDeclaration": {
        if (kitModule((node.source as Node | null)?.value) === null) return;
        for (const spec of node.specifiers as Node[]) {
          if (spelled(spec.local as Node) === "diffLines") {
            out.push(`${at(spec)} re-exports diffLines`);
          }
        }
        return false;
      }
      case "ExportAllDeclaration": {
        const source = (node.source as Node).value;
        if (kitReaches(kitModule(source), KIT_DIFF)) {
          out.push(`${at(node)} re-exports ${String(source)} whole`);
        }
        return false;
      }
    }
  });
  return out;
}

// ONE LINE DIFF: the design system's `diffLines` stops comparing at 2,000
// lines a side and answers a whole replacement past them, which the
// dashboard drew as a page rewritten and merged as one conflict.
describe("the dashboard's texts", () => {
  test("are compared here and nowhere else", () => {
    const sites = modules().flatMap((m) => kitDiffSites(m.path, m.text, m.lang));
    expect(sites, "compare two texts with compareLines from lib/lineDiff.ts").toEqual([]);
  });

  // EVERY WAY TO IT IS REPORTED: by name from any module of the package, and
  // through a namespace, a dynamic import or a re-export of a module that
  // hands it on. The last four lines are not ways to it: this tree's own
  // line diff, a name the package's diff module exports beside it, and a
  // module of the package that does not hand it on, imported and re-exported
  // whole.
  test("it can tell — the package's line diff is reported however it is reached", () => {
    const ways = [
      'import { collapseDiff, diffLines } from "@crewlethq/ui";',
      'import { diffLines as compare } from "@crewlethq/ui/DiffView";',
      'import * as ui from "@crewlethq/ui";',
      'import * as all from "@crewlethq/ui/generated";',
      'export const d = async () => (await import("@crewlethq/ui/DiffView")).diffLines;',
      'export { diffLines as lines } from "@crewlethq/ui";',
      'export * from "@crewlethq/ui/generated";',
      'import { compareLines } from "~/lib/lineDiff.ts";',
      'import { splitLines } from "@crewlethq/ui/DiffView";',
      'import * as buttons from "@crewlethq/ui/Button";',
      'export * from "@crewlethq/ui/Button";',
    ].join("\n");
    expect(kitDiffSites("ways.ts", ways, "ts")).toEqual([
      "ways.ts:1 imports diffLines",
      "ways.ts:2 imports diffLines",
      "ways.ts:3 imports @crewlethq/ui whole, as ui",
      "ways.ts:4 imports @crewlethq/ui/generated whole, as all",
      "ways.ts:5 imports @crewlethq/ui/DiffView at run time",
      "ways.ts:6 re-exports diffLines",
      "ways.ts:7 re-exports @crewlethq/ui/generated whole",
    ]);
  });
});

describe("the floor under a distance", () => {
  // A FLOOR THAT EVER PASSED THE DISTANCE would call a pair within the bound
  // too different: it is held under the shortest edit of every pair.
  test("is never more than the shortest edit", () => {
    const rand = random(5);
    for (let round = 0; round < 200; round++) {
      const draw = () =>
        Array.from({ length: Math.floor(rand() * 30) }, () => Math.floor(rand() * 6));
      const a = draw();
      const b = draw();
      const distance = a.length + b.length - 2 * lcs(a.map(String), b.map(String));
      expect(distanceFloor(a, b), `round ${round}`).toBeLessThanOrEqual(distance);
    }
  });

  test("is every line of two texts that share none, and nothing of one", () => {
    expect(distanceFloor([1, 2, 3], [4, 5])).toBe(5);
    expect(distanceFloor([1, 2, 2, 3], [3, 2, 2, 1])).toBe(0);
    // A REPEAT COUNTS AS OFTEN AS BOTH HOLD IT: two of three kept.
    expect(distanceFloor([7, 7, 7], [7, 7])).toBe(1);
  });
});
