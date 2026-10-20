// @vitest-environment node
/** The artifacts' write requests, as the engine reads them. */

import { describe, expect, test } from "vitest";
import { linkWorkItem } from "./api.ts";

describe("linkWorkItem", () => {
  test("names the work item, and no floor for an item filed earlier", () => {
    expect(linkWorkItem("a1", "ENG-12")).toEqual({
      method: "POST",
      path: "/artifacts/a1/work-items",
      body: { work_item: "ENG-12" },
    });
  });

  test("carries the create's position as after, so the engine reads the item from there on", () => {
    expect(linkWorkItem("a1", "ENG-12", "CREWLET_TRACKER_LOG@1:9")).toEqual({
      method: "POST",
      path: "/artifacts/a1/work-items",
      body: { work_item: "ENG-12", after: "CREWLET_TRACKER_LOG@1:9" },
    });
  });

  test("sends no after for an empty position", () => {
    expect(linkWorkItem("a1", "ENG-12", "").body).toEqual({ work_item: "ENG-12" });
  });
});
