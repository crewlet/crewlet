// @vitest-environment node

/**
 * The work list's page action writes the keys `URL_HOMES` calls a chip or an
 * arrangement and no other, words them as the chips do, and never widens a
 * host's lock: `#/me` stays one person's work whatever the assistant asks.
 */

import { describe, expect, test } from "vitest";
import { parseUrlPatch } from "~/app/assistant/urlAction.ts";
import { URL_HOMES, clearFiltersPatch } from "~/lib/work.ts";
import { lockGate, withoutLocked, workClearKeys, workKeys } from "./assistantActions.ts";

const labels = { seatName: (h: string) => (h === "swe" ? "Agent SWE" : h) };
const chips = workKeys("chip", labels);
const arrangement = workKeys("arrangement", labels);
const keys = { ...chips, ...arrangement };

describe("the keys the assistant may write", () => {
  test("are exactly the chips and the arrangement, each for its own action, held against URL_HOMES", () => {
    const of = (home: string) =>
      Object.entries(URL_HOMES)
        .filter(([, at]) => at === home)
        .map(([key]) => key)
        .sort();
    expect(Object.keys(chips).sort()).toEqual(of("chip"));
    expect(Object.keys(arrangement).sort()).toEqual(of("arrangement"));
    // A FILTER IS NOT AN ARRANGEMENT: each action writes only its own half.
    expect(parseUrlPatch({ group_by: "status" }, chips).ok).toBe(false);
    expect(parseUrlPatch({ status: "open" }, arrangement).ok).toBe(false);
  });

  test("Clear writes the keys the screen's own Clear does, and every one is a key it may write", () => {
    const patch = clearFiltersPatch(["f.severity"]);
    const clear = workClearKeys(labels);
    for (const name of Object.keys(patch)) {
      expect(parseUrlPatch({ [name]: null }, clear).ok, name).toBe(true);
    }
    expect(Object.keys(patch)).toContain("unit");
    expect(Object.keys(patch)).toContain("scope");
  });

  test("leave out the scope switch, the saved view's tab and the calendar's month", () => {
    for (const name of ["scope", "view", "month"]) {
      expect(parseUrlPatch({ [name]: "x" }, keys).ok).toBe(false);
    }
  });

  test("take a company field by its slug, and a switch only as on or cleared", () => {
    expect(parseUrlPatch({ "f.severity": "high" }, keys).ok).toBe(true);
    expect(parseUrlPatch({ blocked: "true" }, keys).ok).toBe(true);
    expect(parseUrlPatch({ blocked: null }, keys).ok).toBe(true);
    expect(parseUrlPatch({ blocked: "false" }, keys).ok).toBe(false);
  });

  test("take only an axis, a sort and a shape the list draws", () => {
    expect(parseUrlPatch({ group_by: "assignee", sort: "due", shape: "board" }, keys).ok).toBe(
      true,
    );
    expect(parseUrlPatch({ group_by: "mood" }, keys).ok).toBe(false);
    expect(parseUrlPatch({ sort: "vibes" }, keys).ok).toBe(false);
    expect(parseUrlPatch({ shape: "pie" }, keys).ok).toBe(false);
  });

  test("are worded as the chips word them", () => {
    expect(keys.assignee!.label).toBe("Assignee");
    expect(keys.assignee!.words!("swe")).toBe("Agent SWE");
    expect(keys.assignee!.words!("none")).toBe("Unassigned");
    expect(keys.tag!.label).toBe("Labels");
    expect(keys.blocked!.words!("true")).toBe("On");
  });
});

describe("a host's lock", () => {
  const me = lockGate({ assignee: "swe" });

  test("refuses a patch that names the locked key, clearing it included", () => {
    expect(me({ assignee: "pm" })).not.toBeNull();
    expect(me({ assignee: null })).not.toBeNull();
    expect(me({ status: "open", assignee: null })).not.toBeNull();
  });

  test("lets every other narrowing through", () => {
    expect(me({ status: "open", blocked: "true" })).toBeNull();
  });

  test("is never part of what Clear writes", () => {
    const patch = withoutLocked(clearFiltersPatch([]), { assignee: "swe" });
    expect(Object.keys(patch)).not.toContain("assignee");
    expect(Object.keys(patch)).toContain("status");
  });

  test("is no gate on a list nobody hosts", () => {
    expect(lockGate(undefined)({ assignee: "pm" })).toBeNull();
  });
});
