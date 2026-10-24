/**
 * The address that opens Principles with parts in edit: what it names is read
 * in reading order and once each, what it does not name opens nothing, and a
 * link from elsewhere lands on Mission when it names no part.
 */

import { expect, test } from "vitest";
import { editValue, partsIn, principlesEditHref } from "./address.ts";

test("reads the parts a value names, in reading order, once each", () => {
  expect(partsIn("policies,mission,policies")).toEqual(["mission", "policies"]);
});

// AN OLD BUILDER ADDRESS (`edit=charter`) or a typo opens nothing rather than
// guessing.
test("opens nothing for a value naming no part", () => {
  expect(partsIn("charter")).toEqual([]);
  expect(partsIn(null)).toEqual([]);
  expect(partsIn("")).toEqual([]);
});

test("writes the open parts in reading order", () => {
  expect(editValue(["policies", "mission"])).toBe("mission,policies");
  expect(editValue([])).toBe("");
});

test("a link names the part it opens, Mission when it names none", () => {
  expect(principlesEditHref("vision")).toBe("#/company/principles?edit=vision");
  expect(principlesEditHref()).toBe("#/company/principles?edit=mission");
});
