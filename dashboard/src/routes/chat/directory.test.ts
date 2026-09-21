/**
 * The other list: the rooms somebody is NOT in.
 *
 * THE INVARIANT THIS SUITE EXISTS FOR is the exclusion. A browse list that
 * quietly carries the rooms somebody is already in is not wrong in a way
 * anything else would notice — the rows render, the names are right, the Join
 * button works and writes nothing — it just stops being the list it claims to
 * be, and the person reading it cannot tell.
 *
 * Beside it are the three refusals the engine makes about membership, mirrored
 * here so the screen does not offer a button whose only outcome is a refusal:
 * a private room admits no join, a unit's room no leave, and a direct
 * conversation neither.
 */

import { describe, expect, test } from "vitest";

import { canJoin, canLeave, membershipNote, roomsToOffer, MAX_BROWSE_ROOMS } from "./directory.ts";

const seen = (id: string, at: string) => ({ id, name: id, kind: "public", at });

describe("which rooms are offered", () => {
  test("the rooms the viewer is already in are not among them", () => {
    const offered = roomsToOffer({
      hits: [{ channel_id: "mine" }, { channel_id: "theirs" }],
      seen: [seen("mine", "2026-01-02T00:00:00Z"), seen("other", "2026-01-01T00:00:00Z")],
      mine: new Set(["mine"]),
    });
    expect(offered.ids).not.toContain("mine");
    expect(offered.ids).toEqual(["theirs", "other"]);
  });

  test("a room found twice is offered once", () => {
    // A room that matched the search AND has been busy since this tab
    // connected is one room. Listed twice it would also be looked up twice.
    const offered = roomsToOffer({
      hits: [{ channel_id: "busy" }],
      seen: [seen("busy", "2026-01-02T00:00:00Z")],
      mine: new Set(),
    });
    expect(offered.ids).toEqual(["busy"]);
  });

  test("what somebody searched for comes before what happens to be busy", () => {
    // The hits answer the question they typed; the sightings are the company
    // getting on with its day.
    const offered = roomsToOffer({
      hits: [{ channel_id: "asked-for" }],
      seen: [seen("busy", "2026-06-01T00:00:00Z")],
      mine: new Set(),
    });
    expect(offered.ids[0]).toBe("asked-for");
  });

  test("the busiest room is the first of the sightings", () => {
    const offered = roomsToOffer({
      hits: [],
      seen: [seen("quiet", "2026-01-01T00:00:00Z"), seen("loud", "2026-09-01T00:00:00Z")],
      mine: new Set(),
    });
    expect(offered.ids).toEqual(["loud", "quiet"]);
  });

  test("what the pass could not read is counted rather than dropped", () => {
    // A truncated list of rooms and a company with few of them look identical
    // from outside, so the remainder is stated.
    const many = Array.from({ length: MAX_BROWSE_ROOMS + 3 }, (_, n) => ({
      channel_id: `room-${n}`,
    }));
    const offered = roomsToOffer({ hits: many, seen: [], mine: new Set() });
    expect(offered.ids).toHaveLength(MAX_BROWSE_ROOMS);
    expect(offered.more).toBe(3);
  });

  test("a room the viewer is in is not counted as one they could not read", () => {
    const offered = roomsToOffer({
      hits: [{ channel_id: "mine" }],
      seen: [],
      mine: new Set(["mine"]),
    });
    expect(offered.ids).toEqual([]);
    expect(offered.more).toBe(0);
  });
});

describe("what the screen may offer about membership", () => {
  test("a public or unit room somebody is not in can be joined", () => {
    expect(canJoin({ kind: "public", member: false })).toBe(true);
    expect(canJoin({ kind: "unit", member: false })).toBe(true);
  });

  test("a private room is joined by being added, so no join is offered", () => {
    // Its membership is the only way in, and the engine refuses the gesture.
    expect(canJoin({ kind: "private", member: false })).toBe(false);
    expect(membershipNote({ kind: "private", member: false })).toMatch(/only way into it/i);
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
    expect(membershipNote({ kind: "broadcast", member: false })).toMatch(/newer build/i);
  });

  test("being in a room is not an invitation to join it again", () => {
    expect(canJoin({ kind: "public", member: true })).toBe(false);
    expect(canLeave({ kind: "public", member: false })).toBe(false);
  });
});
