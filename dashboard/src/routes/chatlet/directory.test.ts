/**
 * What the screen offers about a room's membership: the three refusals the
 * engine makes, mirrored so the screen does not offer a button whose only
 * outcome is a refusal. A private room admits no join, a unit's room no
 * leave, and a direct conversation neither.
 */

import { describe, expect, test } from "vitest";

import { canJoin, canLeave, membershipNote } from "./directory.ts";

describe("what the screen may offer about membership", () => {
  test("a public room somebody is not in can be joined", () => {
    expect(canJoin({ kind: "public", member: false })).toBe(true);
  });

  test("a unit's room is joined through the org chart, so no join is offered", () => {
    // Readable by every seat, and its membership is the chart's alone: the
    // engine refuses the join, and a member the chart did not put there would
    // be taken out at the next apply. The note says where the door is.
    expect(canJoin({ kind: "unit", member: false })).toBe(false);
    expect(membershipNote({ kind: "unit", member: false, unit: "platform" })).toMatch(
      /platform team’s own room.*org chart/i,
    );
    expect(membershipNote({ kind: "unit", member: false })).toMatch(/a team’s own room/i);
  });

  test("a private room is joined by being added, so no join is offered", () => {
    // Its membership is the only way in, and the engine refuses the gesture.
    expect(canJoin({ kind: "private", member: false })).toBe(false);
    expect(membershipNote({ kind: "private", member: false })).toMatch(
      /someone already in it has to add you/i,
    );
  });

  test("a direct conversation offers neither, because its id IS its people", () => {
    // Adding somebody does not widen this room — it names a different one,
    // which is a conversation to open rather than an edit to make.
    for (const kind of ["dm", "group"]) {
      expect(canJoin({ kind, member: false })).toBe(false);
      expect(canLeave({ kind, member: true })).toBe(false);
      expect(membershipNote({ kind, member: true })).toMatch(/different conversation/i);
    }
  });

  test("a unit's room cannot be left, because the org chart would put you back", () => {
    expect(canLeave({ kind: "unit", member: true })).toBe(false);
    expect(membershipNote({ kind: "unit", member: true })).toMatch(/org chart/i);
    // And an ordinary room can.
    expect(canLeave({ kind: "public", member: true })).toBe(true);
    expect(canLeave({ kind: "private", member: true })).toBe(true);
  });

  test("a room this build cannot classify offers nothing at all", () => {
    // An open enum read two-valued falls out as "not private", which is how a
    // newer peer's room becomes writable by anyone. The engine refuses every
    // write to one; the screen offers no gesture.
    expect(canJoin({ kind: "broadcast", member: false })).toBe(false);
    expect(canLeave({ kind: "broadcast", member: true })).toBe(false);
    expect(membershipNote({ kind: "broadcast", member: false })).toMatch(
      /newer version of the engine/i,
    );
  });

  test("being in a room is not an invitation to join it again", () => {
    expect(canJoin({ kind: "public", member: true })).toBe(false);
    expect(canLeave({ kind: "public", member: false })).toBe(false);
  });
});
