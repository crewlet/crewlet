// @vitest-environment node

/**
 * A file's patch is read as git wrote it: its hunks, each line numbered on
 * the side or sides it is on, and the unchanged lines between hunks counted.
 */

import { describe, expect, test } from "vitest";
import { lineSide, parsePatch } from "./patch.ts";

const PATCH = [
  "diff --git a/app.go b/app.go",
  "index 1111111..2222222 100644",
  "--- a/app.go",
  "+++ b/app.go",
  "@@ -2,3 +2,3 @@ package app",
  " import",
  "-old line",
  "+new line",
  " end",
  "@@ -10,2 +10,3 @@",
  " ten",
  "+added",
  " eleven",
  "\\ No newline at end of file",
  "",
].join("\n");

describe("a file's patch", () => {
  // EACH LINE ON THE SIDE IT IS ON, numbered from the hunk's header: a removed
  // line has no newer number, an added one no older one. Mutation: number
  // both sides of every line, and the removed line claims a newer number.
  test("numbers every line on the side it is on, from its hunk's header", () => {
    const hunks = parsePatch(PATCH);
    expect(hunks).toHaveLength(2);
    expect(hunks[0]!.lines).toEqual([
      { kind: "same", text: "import", before: 2, after: 2 },
      { kind: "remove", text: "old line", before: 3, after: null },
      { kind: "add", text: "new line", before: null, after: 3 },
      { kind: "same", text: "end", before: 4, after: 4 },
    ]);
    expect(hunks[1]!.lines.map((l) => [l.kind, l.before, l.after])).toEqual([
      ["same", 10, 10],
      ["add", null, 11],
      ["same", 11, 12],
    ]);
  });

  // THE GAP BETWEEN HUNKS IS COUNTED, and the one before the first: a gap
  // closed in silence reads as a file rewritten in the middle.
  test("counts the unchanged lines before each hunk", () => {
    const hunks = parsePatch(PATCH);
    expect(hunks[0]!.skipped).toBe(1);
    expect(hunks[1]!.skipped).toBe(5);
  });

  // A NEW FILE starts at 0 on its older side, which holds nothing.
  test("reads a file added whole as every line added from 1", () => {
    const hunks = parsePatch(
      ["--- /dev/null", "+++ b/new.md", "@@ -0,0 +1,2 @@", "+a", "+b"].join("\n"),
    );
    expect(hunks).toEqual([
      {
        skipped: 0,
        lines: [
          { kind: "add", text: "a", before: null, after: 1 },
          { kind: "add", text: "b", before: null, after: 2 },
        ],
      },
    ]);
  });

  test("names a line by its newer number, else its older", () => {
    expect(lineSide({ kind: "same", text: "", before: 3, after: 4 })).toEqual({
      side: "new",
      n: 4,
    });
    expect(lineSide({ kind: "remove", text: "", before: 3, after: null })).toEqual({
      side: "old",
      n: 3,
    });
  });
});
