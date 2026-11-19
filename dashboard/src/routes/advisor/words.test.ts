/**
 * The Advisor's words for a fact's number and a report's week.
 */

import { expect, test } from "vitest";
import { factValue, reportSubject, weekSpan, weekWords } from "./words.ts";

test("a count is the number alone, and a unit this build does not know keeps its word", () => {
  expect(factValue(0, "count")).toBe("0");
  expect(factValue(1200, "count")).toBe("1,200");
  expect(factValue(3, "turns")).toBe("3 turns");
  expect(factValue(2.25, "hours")).toBe("2.3 h");
  expect(factValue(4, "widgets")).toBe("4 widgets");
  expect(factValue(null, "count")).toBe("none");
});

// AN ISO WEEK NAMES ITS DAYS: Monday to Sunday, whatever zone reads them.
test("a report's week is its Monday to its Sunday", () => {
  expect(weekWords("2026-W46")).toBe("Week 46 of 2026");
  expect(weekSpan("2026-W46")).toBe("Monday, Nov 9 to Sunday, Nov 15, 2026");
  // WEEK 1 can start in the year before: 2026-W01 starts on Monday, Dec 29, 2025.
  expect(weekSpan("2026-W01")).toBe("Monday, Dec 29 to Sunday, Jan 4, 2026");
  // A YEAR WITH 53 WEEKS ends its last one in the next year.
  expect(weekSpan("2026-W53")).toBe("Monday, Dec 28 to Sunday, Jan 3, 2027");
  expect(weekSpan("not a week")).toBeNull();
});

test("a subject that is the only one of its kind reads in words", () => {
  expect(reportSubject("company")).toBe("The company");
  expect(reportSubject("atlas")).toBe("The atlas");
  expect(reportSubject("Agent CEO")).toBe("Agent CEO");
});
