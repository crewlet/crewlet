/**
 * The router's rules, which are invisible in a URL.
 *
 * All three of these shipped wrong once, and none of them shows up in an
 * address bar: from a redirected route Back could not escape at all, Back from
 * a screen's fourth lens left the screen entirely, and Back to a list somebody
 * had scrolled halfway down landed at the top.
 */

// @vitest-environment node

import { describe, expect, test } from "vitest";
import { buildHash, parseHash } from "./router.tsx";
import { ALL_NAV, activeNavKey, titleFor } from "./nav.ts";
import { MOVED_PATHS } from "./router.tsx";

describe("parsing", () => {
  test("a bare hash is the overview", () => {
    expect(parseHash("#/").path).toEqual([]);
    expect(parseHash("").path).toEqual([]);
  });

  test("path segments are decoded", () => {
    const route = parseHash("#/seats/product%20manager?tab=model");
    expect(route.path).toEqual(["seats", "product manager"]);
    expect(route.query.get("tab")).toBe("model");
  });

  test("a hand-edited URL with a stray %% lands on a screen rather than throwing", () => {
    // Routing must not be the thing that fails: a bad escape should reach a
    // screen that says so, not a blank page.
    expect(() => parseHash("#/seats/100%")).not.toThrow();
    expect(parseHash("#/seats/100%").path).toEqual(["seats", "100%"]);
  });

  test("building and parsing round-trip", () => {
    const hash = buildHash(["seats", "a b"], { tab: "cost" });
    expect(parseHash(hash).path).toEqual(["seats", "a b"]);
    expect(parseHash(hash).query.get("tab")).toBe("cost");
  });

  test("an empty query value is omitted rather than written as a bare key", () => {
    // A filter cleared to "" means "no filter", and `?actor=` in a URL is a
    // filter for the empty actor.
    expect(buildHash(["activity"], { actor: "" })).toBe("#/activity");
  });
});

describe("navigation identity", () => {
  test("a detail screen keeps the reader's place in the sidebar", () => {
    // Otherwise opening one event loses the highlight on the screen you came
    // from, which reads as having navigated somewhere unrelated.
    expect(activeNavKey(["seats", "pm"])).toBe("people");
    expect(activeNavKey(["traces", "abc"])).toBe("model");
    expect(activeNavKey(["turns", "abc"])).toBe("model");
    expect(activeNavKey(["events", "abc"])).toBe("activity");
    expect(activeNavKey([])).toBe("overview");
  });

  test("every nav entry resolves to a titled screen", () => {
    for (const item of ALL_NAV) {
      expect(titleFor(item.path), item.key).toBe(item.label);
    }
  });

  test("no two nav entries claim the same first path segment", () => {
    // Route dispatch is a switch on that segment, so a duplicate would make
    // one of the two unreachable — silently.
    const heads = ALL_NAV.map((i) => i.path[0] ?? "");
    expect(new Set(heads).size).toBe(heads.length);
  });

  test("every nav entry says what it answers", () => {
    // The hint is what the command palette shows. An entry with none is an
    // entry a reader has to click to understand.
    for (const item of ALL_NAV) {
      expect(item.hint.length, item.key).toBeGreaterThan(10);
    }
  });
});

describe("moved routes", () => {
  // A REDIRECT WHOSE OLD PATH IS NOW A LIVE ROUTE takes every reader of the
  // new screen somewhere else — silently, with the address bar agreeing with
  // them, for ever. That is strictly worse than the dead link the redirect
  // was added to avoid, because a dead link is visible.
  //
  // It happened: `#/work` redirected to `#/runs` from when "work" meant a
  // coding run, and the work board later took the name. The routing smoke
  // test did not catch it — it asserts a screen rendered, and the wrong
  // screen renders perfectly well.
  test("no redirect claims a path a live screen now owns", () => {
    const live = new Set(ALL_NAV.map((item) => item.path[0]).filter(Boolean));
    for (const from of MOVED_PATHS) {
      expect(live.has(from), `#/${from} is both a redirect and a live screen`).toBe(false);
    }
  });
});
