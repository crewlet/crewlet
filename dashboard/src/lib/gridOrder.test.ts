import { expect, test } from "vitest";
import { nextGridSort } from "@crewlethq/ui";
import { oneWayHead } from "./gridOrder.ts";

const ORDERS = ["title", "-updated"];
const FALLBACK = "-updated";

/** What a press on `column` asks for, by the kit's own step from the order in force. */
const press = (column: string, inForce: string) =>
  oneWayHead(
    nextGridSort(column, inForce === FALLBACK ? "" : inForce, FALLBACK),
    ORDERS,
    inForce,
    FALLBACK,
  );

// THE ENGINE READS EACH ORDER ONE WAY. Mutation: return `asked`, and a second
// press on Name asks the engine for `-title`, which it does not read.
test("a head asks only for the order its column has, in the direction the engine reads it", () => {
  expect(press("title", FALLBACK)).toBe("title");
  expect(press("updated", "title")).toBe("");
});

test("a press on the head in force goes back to the default, never to a reverse", () => {
  expect(press("title", "title")).toBe("");
  expect(press("updated", FALLBACK)).toBe("");
});

test("from an order no column draws, a head takes the rows to its own", () => {
  expect(press("title", "created")).toBe("title");
  expect(press("updated", "created")).toBe("");
});

test("a column with no order of its own asks for the default", () => {
  expect(oneWayHead("owner", ORDERS, FALLBACK, FALLBACK)).toBe("");
  expect(oneWayHead("", ORDERS, "title", FALLBACK)).toBe("");
});
