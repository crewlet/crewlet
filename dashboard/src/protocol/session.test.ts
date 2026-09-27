// @vitest-environment node

/**
 * A tab's read floor only rises, and only a read that takes one is sent it.
 */

import { describe, expect, test } from "vitest";
import { SESSION_QUERIES } from "../contract/domains.ts";
import { domainOf, isLater, parsePosition, SessionFloors } from "./session.ts";

describe("a position", () => {
  test("reads the engine's own spelling", () => {
    expect(parsePosition("CREWLET_TRACKER_LOG@2:4711")).toEqual({
      stream: "CREWLET_TRACKER_LOG",
      generation: 2,
      seq: 4711,
    });
  });

  test.each(["", "CREWLET_TRACKER_LOG", "@1:2", "L@1", "L@-1:2", "L@1:2x", "L @1:2"])(
    "refuses %j",
    (raw) => {
      expect(parsePosition(raw)).toBeNull();
    },
  );

  // GENERATION FIRST: a re-anchored log restarts its sequence below the old
  // one, and a floor compared by sequence alone would never move again.
  test("orders by generation before sequence", () => {
    const at = (raw: string) => parsePosition(raw)!;
    expect(isLater(at("L@2:1"), at("L@1:900"))).toBe(true);
    expect(isLater(at("L@1:900"), at("L@2:1"))).toBe(false);
    expect(isLater(at("L@1:5"), at("L@1:4"))).toBe(true);
    expect(isLater(at("L@1:4"), at("L@1:4"))).toBe(false);
  });
});

describe("the floor", () => {
  test("only rises: a slow answer to an earlier write does not lower it", () => {
    const floors = new SessionFloors();
    expect(floors.written("tracker", "L@1:10", [])).toBe(true);
    expect(floors.written("tracker", "L@1:9", [])).toBe(false);
    expect(floors.floor("tracker")).toBe("L@1:10");
    expect(floors.written("tracker", "L@1:11", [])).toBe(true);
    expect(floors.floor("tracker")).toBe("L@1:11");
  });

  test("is per domain", () => {
    const floors = new SessionFloors();
    floors.written("tracker", "T@1:10", []);
    expect(floors.floor("pages")).toBeNull();
  });

  test("a write that appended nothing moves no floor, and is still heard", () => {
    const floors = new SessionFloors();
    const heard: unknown[] = [];
    floors.onWritten((domain) => heard.push(domain));
    expect(floors.written("tracker", null, [])).toBe(false);
    expect(floors.floor("tracker")).toBeNull();
    expect(heard).toEqual(["tracker"]);
  });

  test("is named by every question of its domain that takes one, and by nothing else", () => {
    const floors = new SessionFloors();
    expect(floors.freshness("work_items")).toBeNull();
    floors.written("tracker", "T@1:10", []);
    for (const kind of SESSION_QUERIES.tracker) {
      expect(floors.freshness(kind)).toEqual({ read_level: "session", min_position: "T@1:10" });
    }
    expect(floors.freshness("page")).toBeNull();
    expect(floors.freshness("work_search")).toBeNull();
    expect(floors.freshness("viewer")).toBeNull();
  });

  test("a write moves its domain's questions and the ones it names beside them", () => {
    expect(SessionFloors.moves("work_items", "tracker", [])).toBe(true);
    expect(SessionFloors.moves("pages", "tracker", [])).toBe(false);
    expect(SessionFloors.moves("work_search", "tracker", ["work_search"])).toBe(true);
    expect(SessionFloors.moves("work_search", null, ["work_search"])).toBe(true);
  });

  test("every listed question belongs to exactly one domain", () => {
    const seen = new Map<string, string>();
    for (const [domain, kinds] of Object.entries(SESSION_QUERIES)) {
      for (const kind of kinds) {
        expect(seen.get(kind), kind).toBeUndefined();
        seen.set(kind, domain);
        expect(domainOf(kind)).toBe(domain);
      }
    }
  });
});
