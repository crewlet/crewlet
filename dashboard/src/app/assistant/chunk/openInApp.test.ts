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
    expect(openInApp("#/worklet/ENG-156")).toEqual({ path: ["worklet", "ENG-156"], query: {} });
    expect(openInApp("#/worklet/tasks?blocked=true")).toEqual({
      path: ["worklet", "tasks"],
      query: { blocked: "true" },
    });
  });

  test("anything that is not a hash route of this dashboard is refused", () => {
    for (const refused of [
      "javascript:alert(1)",
      "JAVASCRIPT:alert(1)",
      "https://evil.example/#/worklet",
      "//evil.example/#/worklet",
      "#//evil.example",
      "/worklet/ENG-1",
      "worklet/ENG-1",
      "#/worklet/ENG-1\\..",
      "#/worklet/ENG-1\nhttp://x",
      "data:text/html,hi",
      "",
      `#/worklet/${"x".repeat(3000)}`,
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
