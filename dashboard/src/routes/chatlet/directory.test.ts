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

import { act, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";

import { MAX_IN_FLIGHT_QUERIES } from "~/contract/wire.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import {
  canJoin,
  canLeave,
  membershipNote,
  roomsToOffer,
  useRoomDetails,
  MAX_BROWSE_ROOMS,
} from "./directory.ts";

/** A room this tab saw a frame for, at sequence `seq` of the chat log. */
const seen = (id: string, seq: number) => ({
  id,
  name: id,
  kind: "public",
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
});

describe("which rooms are offered", () => {
  test("the rooms the viewer is already in are not among them", () => {
    const offered = roomsToOffer({
      hits: [{ channel_id: "mine" }, { channel_id: "theirs" }],
      seen: [seen("mine", 20), seen("other", 10)],
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
      seen: [seen("busy", 20)],
      mine: new Set(),
    });
    expect(offered.ids).toEqual(["busy"]);
  });

  test("what somebody searched for comes before what happens to be busy", () => {
    // The hits answer the question they typed; the sightings are the company
    // getting on with its day.
    const offered = roomsToOffer({
      hits: [{ channel_id: "asked-for" }],
      seen: [seen("busy", 60)],
      mine: new Set(),
    });
    expect(offered.ids[0]).toBe("asked-for");
  });

  test("every other room the names read holds comes last, once, and never one the viewer is in", () => {
    // A ROOM NOBODY HAS SPOKEN IN is found by name alone: after what was
    // searched for and what is busy, in the names read's own order.
    const offered = roomsToOffer({
      hits: [{ channel_id: "asked-for" }],
      seen: [seen("busy", 60)],
      named: ["mine", "busy", "quiet", "asked-for", "silent"],
      mine: new Set(["mine"]),
    });
    expect(offered.ids).toEqual(["asked-for", "busy", "quiet", "silent"]);
  });

  test("the busiest room is the first of the sightings", () => {
    const offered = roomsToOffer({
      hits: [],
      seen: [seen("quiet", 10), seen("loud", 90)],
      mine: new Set(),
    });
    expect(offered.ids).toEqual(["loud", "quiet"]);
  });

  // THE LOG'S ORDER, NOT THE CLOCK'S TEXT. A broker instant trims its
  // trailing zeros, so as text `…:05Z` sorted after every fraction of its own
  // second; a later GENERATION is later whatever its sequence says, because a
  // log restored or re-anchored restarts its count.
  test("the newest sighting is the one latest on the log, generation first", () => {
    const at = (id: string, generation: number, seq: number) => ({
      ...seen(id, seq),
      position: { stream: "CREWLET_CHAT_LOG", generation, seq },
    });
    const offered = roomsToOffer({
      hits: [],
      seen: [at("old-log", 1, 900), at("new-log", 2, 3), at("origin", 0, 0), at("mid", 1, 950)],
      mine: new Set(),
      limit: 10,
    });
    expect(offered.ids.slice(0, 3)).toEqual(["new-log", "mid", "old-log"]);
  });

  test("a sighting no frame positioned is offered last", () => {
    const offered = roomsToOffer({
      hits: [],
      seen: [{ ...seen("blank", 0), position: null }, seen("said", 1)],
      mine: new Set(),
    });
    expect(offered.ids).toEqual(["said", "blank"]);
  });

  test("one pass is one wave of the socket's queries", () => {
    // EACH ROW IS A READ, and past the socket's own bound the engine stops
    // reading the socket until one finishes — so a pass longer than one wave
    // holds the rail and the transcript behind the browse list's own lookups.
    expect(MAX_BROWSE_ROOMS).toBe(MAX_IN_FLIGHT_QUERIES);
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

describe("reading the rooms a pass offers", () => {
  /** A room's description, as `chat_channel` answers it. */
  const detail = (id: string) => ({
    read_level: "session",
    complete: true,
    channel: { v: 1, id, kind: "public", name: id, created_at: "2026-01-01T00:00:00Z" },
    revision: 1,
    message_seq: 1,
    member: false,
    members: [],
  });

  /** The hook over a socket whose `chat_channel` reads the test answers by
   *  hand, one room at a time. */
  function lookups(initial: string[]) {
    const out = new Map<string, (answer: unknown) => void>();
    const query = vi.fn(
      (_what: string, params?: Record<string, unknown>) =>
        new Promise((resolve) => out.set(String(params?.channel_id), resolve)),
    );
    const store = new Store();
    store.applyHealth(healthFrame({ nodes: 1 }));
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(
        ClientContext.Provider,
        { value: { store, socket: { query } } as never },
        children,
      );
    const view = renderHook(({ ids }: { ids: string[] }) => useRoomDetails(ids), {
      initialProps: { ids: initial },
      wrapper,
    });
    const answer = async (id: string) => {
      await act(async () => {
        out.get(id)?.(detail(id));
        await Promise.resolve();
      });
    };
    return { ...view, query, answer };
  }

  // A READ OUTLIVES THE PASS THAT ASKED IT. A new pass skips every id whose
  // read is still out — asking twice would be two queries for one room — so
  // the read a superseded pass started is the only read that room gets, and
  // its answer has to redraw the list. Gated on the pass, it was filed and
  // drawn by nothing: the dialog said "Reading the rooms" for ever.
  test("rooms whose order moved while they were being read are drawn when they answer", async () => {
    const { result, rerender, answer, query } = lookups(["a", "b"]);
    rerender({ ids: ["b", "a"] });
    await answer("a");
    await answer("b");
    expect(result.current.loading).toBe(false);
    expect([...result.current.rooms.keys()].sort()).toEqual(["a", "b"]);
    // AND NOTHING WAS ASKED TWICE.
    expect(query).toHaveBeenCalledTimes(2);
  });

  test("a room still wanted after the list narrowed is drawn when it answers", async () => {
    const { result, rerender, answer } = lookups(["a", "b"]);
    rerender({ ids: ["b"] });
    await answer("a");
    await answer("b");
    expect(result.current.loading).toBe(false);
    expect([...result.current.rooms.keys()]).toEqual(["b"]);
  });
});
