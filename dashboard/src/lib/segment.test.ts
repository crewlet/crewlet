/**
 * One rule of what a URL path segment may be, for a request and a link alike.
 *
 * The invariants: a value is one segment however it is spelled, a value of
 * dots alone (which the URL standard resolves as a step along the path,
 * escaped or not) is never one, and a request and a link agree on which is
 * which because the request's rule is the link's and a throw.
 */

import { expect, test } from "vitest";
import { fragmentSegment, linkSegment, pathSegment, pathSegments } from "./segment.ts";

const NOT_ONE = ["", ".", "..", "..."];

test("a value is one segment once a URL resolves it, whatever it holds", () => {
  for (const value of ["a/b", "a?b#c", "a..b", ".hidden", "x.", "50% off"]) {
    const path = new URL(`/base/${linkSegment(value)}/end`, "http://engine.example").pathname;
    const parts = path.split("/");
    expect(parts, value).toHaveLength(4);
    expect(decodeURIComponent(parts[2]!), value).toBe(value);
  }
});

test("dots alone are no segment: a link is drawn as words, and a request is refused", () => {
  for (const value of NOT_ONE) {
    expect(linkSegment(value), value).toBeNull();
    expect(() => pathSegment(value), value).toThrow(RangeError);
  }
  // THE SAME RULE: whatever a link takes, a request takes, encoded the same.
  for (const value of ["a/b", "x.", "~me"]) expect(pathSegment(value)).toBe(linkSegment(value));
});

test("the dashboard's own route takes dots as a name, since a fragment is never resolved", () => {
  expect(fragmentSegment("..")).toBe("..");
  expect(fragmentSegment("a/b")).toBe("a%2Fb");
});

test("a path is escaped segment by segment, its slashes kept as separators", () => {
  expect(pathSegments("a b/c#d.md")).toBe("a%20b/c%23d.md");
  for (const path of ["a//b", "a/../b", "a/./b", "b/"])
    expect(() => pathSegments(path), path).toThrow(RangeError);
});
