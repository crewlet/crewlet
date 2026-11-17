import { describe, expect, test } from "vitest";
import { parsePatch } from "~/lib/patch.ts";
import { EXPAND_STEP, expandHunks, gapsOf } from "./expand.ts";

/** A newer blob of 40 lines, "n<line>". */
const newer = (n: number) => (n >= 1 && n <= 40 ? `n${n}` : undefined);

// Lines 1-13 hidden, a hunk 14-16 that adds a line (14-17 newer), lines 17-19 hidden, a hunk 20-21.
const PATCH = [
  "@@ -14,3 +14,4 @@ func New()",
  " a",
  "+b",
  " c",
  " d",
  "@@ -20,2 +21,2 @@",
  "-e",
  "+E",
  " f",
].join("\n");

describe("expanding a patch's gaps", () => {
  test("says what each gap hides, and after the last hunk once the newer length is known", () => {
    const hunks = parsePatch(PATCH);
    expect(gapsOf(hunks, null)).toEqual({ above: [13, 3], trailing: null });
    expect(gapsOf(hunks, 30)).toEqual({ above: [13, 3], trailing: 8 });
  });

  // BOTH SIDES NUMBERED, the older shifted by what the hunks above added.
  // Mutation: number the revealed lines on the newer side only, and this fails.
  test("numbers revealed lines on both sides through a shifted hunk", () => {
    const { hunks, gaps } = expandHunks(parsePatch(PATCH), new Map([[1, 2]]), newer);
    expect(gaps).toEqual([0, 1]);
    expect(hunks[1]!.skipped).toBe(1);
    expect(hunks[1]!.lines.slice(0, 2)).toEqual([
      { kind: "same", text: "n19", before: 18, after: 19 },
      { kind: "same", text: "n20", before: 19, after: 20 },
    ]);
    expect(hunks[1]!.header).toBe("@@ -18,4 +19,4 @@");
  });

  test("joins a gap revealed whole into the hunk above, and never draws a line twice", () => {
    const { hunks, gaps } = expandHunks(parsePatch(PATCH), new Map([[1, 3]]), newer);
    expect(hunks).toHaveLength(1);
    expect(gaps).toEqual([0]);
    const afters = hunks[0]!.lines.map((l) => l.after).filter((n) => n !== null);
    expect(new Set(afters).size).toBe(afters.length);
    expect(afters).toEqual([14, 15, 16, 17, 18, 19, 20, 21, 22]);
  });

  test("reveals the gap above the first hunk from its bottom and the trailing one from its top", () => {
    const { hunks } = expandHunks(
      parsePatch(PATCH),
      new Map([
        [0, EXPAND_STEP],
        [2, 2],
      ]),
      newer,
    );
    expect(hunks[0]!.skipped).toBe(0);
    expect(hunks[0]!.lines[0]).toEqual({ kind: "same", text: "n1", before: 1, after: 1 });
    expect(hunks.at(-1)!.lines.slice(-2)).toEqual([
      { kind: "same", text: "n23", before: 22, after: 23 },
      { kind: "same", text: "n24", before: 23, after: 24 },
    ]);
  });

  // GIT'S SECTION WORDS NAME THE HUNK'S OWN START: lines revealed above it drop
  // them, lines after it keep them. Mutation: always keep them, and this fails.
  test("drops git's section words once lines are revealed above a hunk", () => {
    const above = expandHunks(parsePatch(PATCH), new Map([[0, 13]]), newer);
    expect(above.hunks[0]!.header).toBe("@@ -1,16 +1,17 @@");
    const one = parsePatch(PATCH.split("\n").slice(0, 5).join("\n"));
    const after = expandHunks(one, new Map([[1, 1]]), newer);
    expect(after.hunks[0]!.header).toBe("@@ -14,4 +14,5 @@ func New()");
  });

  // NEVER PAST THE BLOB'S END: a count past it reveals only what the blob holds.
  // Mutation: drop the cap, and lines 23 to 30 are drawn past a 22-line blob.
  test("reveals no line after the last hunk past the newer blob's length", () => {
    const { hunks } = expandHunks(parsePatch(PATCH), new Map([[2, 10]]), newer, 23);
    expect(hunks.at(-1)!.lines.at(-1)).toEqual({
      kind: "same",
      text: "n23",
      before: 22,
      after: 23,
    });
  });
});
