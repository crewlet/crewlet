// @vitest-environment node

/**
 * The code host's values as the Code workspace says them: what a push read in
 * brief left out, and where a file's lines begin for the line an address
 * picks.
 */

import { describe, expect, test } from "vitest";
import {
  PICK_CONTEXT,
  changeRef,
  lineToOpenAt,
  omittedOf,
  openingLine,
  parseChangeRef,
  preconditionWord,
} from "./code.ts";

describe("what a push read in brief left out", () => {
  // THE REFUSALS FIRST, then the engine's order: what did not happen is what a
  // reader acts on. Mutation: drop the reordering, and refused is said last.
  test("is said refusals first, then in the engine's order, and summed", () => {
    expect(omittedOf({ deleted: 5, created: 990, refused: 2, moved: 1 })).toEqual({
      total: 998,
      outcomes: [
        { outcome: "refused", count: 2 },
        { outcome: "created", count: 990 },
        { outcome: "moved", count: 1 },
        { outcome: "deleted", count: 5 },
      ],
    });
  });

  // AN OUTCOME A NEWER ENGINE COUNTS BY is said by its key and never dropped
  // from the sum: "and 5 more" over four counted would be a count the row
  // contradicts.
  test("says an outcome this build has no word for by its key, and counts it", () => {
    expect(omittedOf({ created: 1, renamed: 4 })).toEqual({
      total: 5,
      outcomes: [
        { outcome: "created", count: 1 },
        { outcome: "renamed", count: 4 },
      ],
    });
  });

  test("is nothing where the push left nothing out", () => {
    expect(omittedOf(undefined)).toEqual({ total: 0, outcomes: [] });
    expect(omittedOf({})).toEqual({ total: 0, outcomes: [] });
  });
});

describe("a change's reference", () => {
  // THE ENGINE'S GRAMMAR, both halves: a repository's name and a number from
  // 1. Mutation: accept a number 0, or a name no repository can have, and a
  // reference nothing can answer is offered as one.
  test("is a repository's name and a number from 1, and nothing else", () => {
    expect(parseChangeRef("web!42")).toEqual({ repo: "web", number: 42 });
    expect(parseChangeRef("dev.scratch!1")).toEqual({ repo: "dev.scratch", number: 1 });
    expect(changeRef("web", 42)).toBe("web!42");
    for (const text of [
      "web!0",
      "web!042",
      "web!",
      "!42",
      "Web!42",
      "web!4x",
      "web!!4",
      "web 42",
    ]) {
      expect(parseChangeRef(text), text).toBeNull();
    }
  });
});

describe("a change's readiness, as a row says it", () => {
  // A KIND A NEWER ENGINE JUDGES is said by its own word rather than dropped.
  test("names a precondition this build knows, and one it does not by its kind", () => {
    expect(preconditionWord("approval")).toBe("Approved");
    expect(preconditionWord("rebase_needed")).toBe("rebase needed");
  });
});

describe("where a file's lines begin for the line an address picks", () => {
  const at = (start: number) => ({ start, end: start });

  // A LINE ON SCREEN MOVES NOTHING, and neither does no line or a line the
  // file does not have (a stale link opens the file where it is). Mutation:
  // move for a drawn line, and every press would read the file again.
  test("stay where they are for a line drawn, none, or one the file lacks", () => {
    expect(lineToOpenAt(null, 1, at(1), 3000)).toBeNull();
    expect(lineToOpenAt(at(40), 1, { start: 1, end: 1500 }, 3000)).toBeNull();
    expect(lineToOpenAt(at(1500), 1, { start: 1, end: 1500 }, 3000)).toBeNull();
    expect(lineToOpenAt(at(3001), 1, { start: 1, end: 1500 }, 3000)).toBeNull();
  });

  // A LINE OFF SCREEN OPENS PICK_CONTEXT LINES BEFORE IT, and at the line
  // itself where they begin there already and still do not draw it — never
  // where they were asked from, which would read the same lines forever.
  // Mutation: open at the line's page rather than before it, or answer the
  // place asked from, and this fails.
  test("begin PICK_CONTEXT lines before a line off screen, never where they began", () => {
    expect(lineToOpenAt(at(2400), 1, { start: 1, end: 1500 }, 3000)).toBe(2400 - PICK_CONTEXT);
    expect(lineToOpenAt(at(100), 1800, { start: 1800, end: 3000 }, 3000)).toBe(100 - PICK_CONTEXT);
    expect(lineToOpenAt(at(30), 1800, { start: 1800, end: 3000 }, 3000)).toBe(1);
    expect(lineToOpenAt(at(20), 1, { start: 1, end: 5 }, 3000)).toBe(20);
    expect(lineToOpenAt(at(2400), 2350, { start: 2350, end: 2360 }, 3000)).toBe(2400);
  });

  // THE FIRST READ IS ASKED WHERE THE LINE IS, before anything is known of
  // the file but its line count where the caller has it: PICK_CONTEXT lines
  // before a picked line, line 1 for none, near the top, or one past the
  // file's end. Mutation: open every file at line 1, or at the picked line
  // whatever the file's lines, and this fails.
  test("open PICK_CONTEXT lines before the picked line, from line 1 where none fits", () => {
    expect(openingLine(at(2400))).toBe(2400 - PICK_CONTEXT);
    expect(openingLine(at(2400), 3000)).toBe(2400 - PICK_CONTEXT);
    expect(openingLine({ start: 300, end: 320 }, 3000)).toBe(300 - PICK_CONTEXT);
    expect(openingLine(at(30), 3000)).toBe(1);
    expect(openingLine(null, 3000)).toBe(1);
    expect(openingLine(at(3001), 3000)).toBe(1);
    expect(openingLine(at(3000), 3000)).toBe(3000 - PICK_CONTEXT);
  });
});
