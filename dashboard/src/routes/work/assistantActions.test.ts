// @vitest-environment node

/**
 * The work list's page action writes the keys `URL_HOMES` calls a chip or an
 * arrangement and no other, words them as the chips do, and never widens a
 * host's lock: `#/me` stays one person's work whatever the assistant asks.
 */

import { describe, expect, test } from "vitest";
import { parseUrlPatch } from "~/app/assistant/urlAction.ts";
import { URL_HOMES } from "~/lib/work.ts";
import { lockGate, workFilterKeys } from "./assistantActions.ts";

const keys = workFilterKeys({ seatName: (h) => (h === "swe" ? "Agent SWE" : h) });

describe("the keys the assistant may write", () => {
  test("are exactly the chips and the arrangement, held both ways against URL_HOMES", () => {
    const offered = Object.entries(URL_HOMES)
      .filter(([, home]) => home === "chip" || home === "arrangement")
      .map(([key]) => key)
      .sort();
    expect(Object.keys(keys).sort()).toEqual(offered);
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

  test("is no gate on a list nobody hosts", () => {
    expect(lockGate(undefined)({ assignee: "pm" })).toBeNull();
  });
});
