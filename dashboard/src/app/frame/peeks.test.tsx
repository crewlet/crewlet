/**
 * Every kind the frame can ADDRESS can also be SHOWN — or is on the list of
 * the ones that deliberately cannot.
 *
 * This is the gap the registry was built to close, and the one it can silently
 * reopen. `objects.ts` knew nineteen kinds and `DetailRail` took its body from
 * whichever screen rendered it, so exactly one kind — `item`, on the tracker —
 * could ever appear in the rail; every other list could compose a `peek=`
 * token that opened nothing. Nothing failed: the URL parsed, the rail mounted,
 * and it closed again.
 *
 * So the invariant is stated here rather than remembered: a kind added to
 * `KINDS` with no body, and no reason written down, fails the build.
 */

import { describe, expect, test } from "vitest";

import { KINDS, type ObjectKind } from "./objects.ts";
import { PEEKS, peekable } from "./peeks.tsx";

/**
 * The kinds that are addressed and deliberately have no rail.
 *
 * ONE ENTRY, and it carries its reason. A `notice` is a row in somebody's
 * inbox naming a change to something else, so the place to read one is the
 * Inbox — where it is also marked. A rail over it would be the same rows in a
 * narrower column, and marking from two places is two things to keep right.
 */
const NO_PEEK: Partial<Record<ObjectKind, string>> = {
  notice: "read in place in the Inbox, which is also where it is marked",
};

describe("the peek registry", () => {
  test("covers every addressable kind but the ones excused by name", () => {
    const missing = (Object.keys(KINDS) as ObjectKind[]).filter((k) => !PEEKS[k] && !NO_PEEK[k]);
    expect(missing).toEqual([]);
  });

  test("excuses nothing it also registers", () => {
    // Both lists would be satisfied by an entry in each, and then the excuse
    // is a comment about code that does the opposite.
    const both = (Object.keys(NO_PEEK) as ObjectKind[]).filter((k) => PEEKS[k]);
    expect(both).toEqual([]);
  });

  test("registers nothing `objects.ts` cannot address", () => {
    // A body under a key `parseRef` never produces is unreachable: the rail is
    // opened from a `peek=` token and nothing else.
    const unknown = (Object.keys(PEEKS) as ObjectKind[]).filter((k) => !KINDS[k]);
    expect(unknown).toEqual([]);
  });

  test("`peekable` answers for a ref rather than for a kind", () => {
    // What the shell asks before it widens the grid: a null ref is not a
    // peekable one, and an excused kind is not either.
    expect(peekable(null)).toBe(false);
    expect(peekable({ kind: "item", id: "ENG-1" })).toBe(true);
    expect(peekable({ kind: "notice", id: "n1" })).toBe(false);
  });
});
