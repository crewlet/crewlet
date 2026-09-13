/**
 * The Web Storage the suites run against, as the setup file supplies it.
 *
 * Asserted rather than assumed because its absence is a property of the runner:
 * two suites passed every local run and failed in CI with `localStorage is
 * undefined`. A builder draft is kept in sessionStorage, which has the same
 * origin dependence, so both areas are pinned here.
 */

import { afterEach, expect, test } from "vitest";

afterEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

test("both storage areas exist and store", () => {
  localStorage.setItem("k", "local");
  sessionStorage.setItem("k", "session");
  expect(localStorage.getItem("k")).toBe("local");
  expect(sessionStorage.getItem("k")).toBe("session");
});

test("the two areas are separate, as they are in a browser", () => {
  sessionStorage.setItem("draft", "{}");
  expect(localStorage.getItem("draft")).toBeNull();
  sessionStorage.removeItem("draft");
  expect(sessionStorage.length).toBe(0);
});
