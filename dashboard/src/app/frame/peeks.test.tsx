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
 * `KINDS` with no body, and no reason written down, fails the build — in the
 * console, which reads every object the engine has, and in the person shell,
 * whose list of what it does NOT peek at is written down too.
 */

import { describe, expect, test } from "vitest";

import { KINDS, type ObjectKind } from "./objects.ts";
import { PEEKS, peekable } from "./peeks.tsx";
import type { Shell } from "../nav.ts";

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

/**
 * What the PERSON shell does not peek at, beyond [NO_PEEK]: the machine's own
 * objects. A turn, a run, a node or a credential is what the console reads,
 * and a member's `peek=` naming one is a closed rail rather than the console's
 * code fetched into a member's browser to draw it.
 */
const CONSOLE_ONLY: Partial<Record<ObjectKind, string>> = {
  turn: "a turn's trace is the console's",
  run: "a coding run is the console's",
  channel: "an agent-to-agent channel is the console's",
  event: "the event log is the console's",
  schedule: "a seat's schedules are its setup's",
  node: "a node is the engine's",
  tool: "the tool catalogue is the console's",
  integration: "an integration's state is the console's",
  credential: "a credential is the console's",
  revision: "a configuration revision is the console's",
};

const SHELLS: Shell[] = ["person", "admin"];

describe("the peek registry", () => {
  test("covers every addressable kind but the ones excused by name, in each shell", () => {
    const missing = (shell: Shell, excused: Partial<Record<ObjectKind, string>>) =>
      (Object.keys(KINDS) as ObjectKind[]).filter((k) => !PEEKS[shell][k] && !excused[k]);
    expect(missing("admin", NO_PEEK)).toEqual([]);
    expect(missing("person", { ...NO_PEEK, ...CONSOLE_ONLY })).toEqual([]);
  });

  test("excuses nothing it also registers", () => {
    // Both lists would be satisfied by an entry in each, and then the excuse
    // is a comment about code that does the opposite.
    for (const shell of SHELLS) {
      const both = (Object.keys(NO_PEEK) as ObjectKind[]).filter((k) => PEEKS[shell][k]);
      expect(both, shell).toEqual([]);
    }
    const leaked = (Object.keys(CONSOLE_ONLY) as ObjectKind[]).filter((k) => PEEKS.person[k]);
    expect(leaked, "the person shell peeks at a console object").toEqual([]);
  });

  test("registers nothing `objects.ts` cannot address", () => {
    // A body under a key `parseRef` never produces is unreachable: the rail is
    // opened from a `peek=` token and nothing else.
    for (const shell of SHELLS) {
      const unknown = (Object.keys(PEEKS[shell]) as ObjectKind[]).filter((k) => !KINDS[k]);
      expect(unknown, shell).toEqual([]);
    }
  });

  test("`peekable` answers for a ref, in the shell it is drawn in", () => {
    // What the frame asks before it widens the grid: a null ref is not a
    // peekable one, an excused kind is not either, and a console object is
    // peekable in the console alone.
    expect(peekable(null, "person")).toBe(false);
    expect(peekable({ kind: "item", id: "ENG-1" }, "person")).toBe(true);
    expect(peekable({ kind: "item", id: "ENG-1" }, "admin")).toBe(true);
    expect(peekable({ kind: "notice", id: "n1" }, "admin")).toBe(false);
    expect(peekable({ kind: "turn", id: "t1" }, "person")).toBe(false);
    expect(peekable({ kind: "turn", id: "t1" }, "admin")).toBe(true);
    // A SEAT HAS A BODY IN EACH: a colleague in one, a runtime in the other.
    expect(peekable({ kind: "seat", id: "swe" }, "person")).toBe(true);
    expect(PEEKS.person.seat).not.toBe(PEEKS.admin.seat);
  });
});
