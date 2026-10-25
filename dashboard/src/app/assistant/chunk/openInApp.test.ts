// @vitest-environment node

/**
 * The one way the assistant moves the page: an address the engine built,
 * checked again before the drawer follows it. A link that is not one of this
 * dashboard's own framed routes is refused, whatever the card says.
 */

import { describe, expect, test } from "vitest";
import { openInApp } from "./openInApp.ts";

describe("an open in app address", () => {
  test("a framed route this build draws is followed, with its query", () => {
    expect(openInApp("#/work/ENG-156")).toEqual({ path: ["work", "ENG-156"], query: {} });
    expect(openInApp("#/work/tasks?blocked=true")).toEqual({
      path: ["work", "tasks"],
      query: { blocked: "true" },
    });
  });

  test("anything that is not a hash route of this dashboard is refused", () => {
    for (const refused of [
      "javascript:alert(1)",
      "JAVASCRIPT:alert(1)",
      "https://evil.example/#/work",
      "//evil.example/#/work",
      "#//evil.example",
      "/work/ENG-1",
      "work/ENG-1",
      "#/work/ENG-1\\..",
      "#/work/ENG-1\nhttp://x",
      "data:text/html,hi",
      "",
      `#/work/${"x".repeat(3000)}`,
    ]) {
      expect(openInApp(refused), refused).toBeNull();
    }
  });

  test("a path the product has no page for is refused rather than drawn as Not Found", () => {
    expect(openInApp("#/nowhere/at/all")).toBeNull();
  });

  test("a sign-in screen is never a place the assistant sends anybody", () => {
    expect(openInApp("#/login")).toBeNull();
  });
});
