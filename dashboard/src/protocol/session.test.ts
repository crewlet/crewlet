// @vitest-environment node

/**
 * A tab's read floor only rises, and only a read that takes one is sent it.
 */

import { describe, expect, test } from "vitest";
import { SESSION_QUERIES } from "../contract/domains.ts";
import {
  domainsOf,
  EVERY_QUESTION,
  isLater,
  parsePosition,
  SessionFloors,
  spellPosition,
} from "./session.ts";

describe("a position", () => {
  test("reads the engine's own spelling", () => {
    expect(parsePosition("CREWLET_TRACKER_LOG@2:4711")).toEqual({
      stream: "CREWLET_TRACKER_LOG",
      generation: 2,
      seq: 4711,
    });
  });

  // AND SPELLS ONE BACK AS IT READS ONE, so a floor taken from an answer's
  // position is one the engine accepts.
  test("spells a position the way it reads one", () => {
    const at = { stream: "CREWLET_CODE_LOG", generation: 3, seq: 90 };
    expect(spellPosition(at)).toBe("CREWLET_CODE_LOG@3:90");
    expect(parsePosition(spellPosition(at))).toEqual(at);
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
      expect(floors.freshness(kind)).toEqual({ read_level: "session", min_position: ["T@1:10"] });
    }
    expect(floors.freshness("page")).toBeNull();
    expect(floors.freshness("work_search")).toBeNull();
    expect(floors.freshness("viewer")).toBeNull();
  });

  // ONE FLOOR PER LOG A QUESTION READS. The company feed is the tracker's work
  // and the knowledge base's pages; named only the tracker's floor, the pages
  // half of a feed drawn after a page save would come from before it.
  test("a question read from two logs names the floor of each the tab wrote, in its listed order", () => {
    const floors = new SessionFloors();
    expect(domainsOf("company_feed")).toEqual(["tracker", "pages"]);
    expect(floors.freshness("company_feed")).toBeNull();
    floors.written("pages", "P@1:5", []);
    expect(floors.freshness("company_feed")).toEqual({
      read_level: "session",
      min_position: ["P@1:5"],
    });
    floors.written("tracker", "T@1:10", []);
    expect(floors.freshness("company_feed")).toEqual({
      read_level: "session",
      min_position: ["T@1:10", "P@1:5"],
    });
    // A question of one of those logs still names its own floor alone.
    expect(floors.freshness("work_items")).toEqual({
      read_level: "session",
      min_position: ["T@1:10"],
    });
    expect(floors.freshness("pages")).toEqual({ read_level: "session", min_position: ["P@1:5"] });
  });

  test("a write moves its domain's questions and the ones it names beside them", () => {
    expect(SessionFloors.moves("work_items", "tracker", [])).toBe(true);
    expect(SessionFloors.moves("pages", "tracker", [])).toBe(false);
    expect(SessionFloors.moves("work_search", "tracker", ["work_search"])).toBe(true);
    expect(SessionFloors.moves("work_search", null, ["work_search"])).toBe(true);
    // A question read from two logs is moved by a write to either.
    expect(SessionFloors.moves("company_feed", "tracker", [])).toBe(true);
    expect(SessionFloors.moves("company_feed", "pages", [])).toBe(true);
    expect(SessionFloors.moves("company_feed", null, [])).toBe(false);
  });

  // A WRITE WHOSE REACH NOBODY CAN NAME moves every question, the ones no
  // domain's floor covers (the brand) included.
  test("a write naming every question moves every question", () => {
    expect(SessionFloors.moves("brand", null, [EVERY_QUESTION])).toBe(true);
    expect(SessionFloors.moves("work_items", "pages", [EVERY_QUESTION])).toBe(true);
    expect(SessionFloors.moves("brand", "tracker", [])).toBe(false);
  });

  test("every listed question is listed once under each domain whose log it reads", () => {
    for (const [domain, kinds] of Object.entries(SESSION_QUERIES)) {
      expect(new Set(kinds).size, domain).toBe(kinds.length);
      for (const kind of kinds) {
        expect(domainsOf(kind), kind).toContain(domain);
      }
    }
    for (const kind of new Set(Object.values(SESSION_QUERIES).flat())) {
      const listed = Object.entries(SESSION_QUERIES)
        .filter(([, kinds]) => (kinds as readonly string[]).includes(kind))
        .map(([domain]) => domain);
      expect(domainsOf(kind), kind).toEqual(listed);
    }
    expect(domainsOf("viewer")).toEqual([]);
  });
});
