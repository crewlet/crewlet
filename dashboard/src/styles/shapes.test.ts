// @vitest-environment node
import { expect, test } from "vitest";
import { sheetRules } from "../test/sheets.ts";

/**
 * THE WORK SHAPES HOLD AT A PHONE'S WIDTH. These are layouts a screenshot shows
 * and a DOM test cannot: jsdom lays nothing out, and each one broke by a rule
 * that read correctly until a column ran out of room.
 */

const rules = sheetRules();
const rule = (selector: string, within?: RegExp) => {
  const found = rules.find((r) => r.selector === selector && (!within || within.test(r.body)));
  expect(found, selector).toBeDefined();
  return found!.body;
};

test("the timeline's names stand in their own pinned track, clear of the strip", () => {
  expect(rule(".tl-names")).toMatch(/position:\s*sticky/);
  expect(rule(".tl-names")).toMatch(/left:\s*0/);
  expect(rule(".tl-scroll", /grid-template-columns/)).toMatch(
    /grid-template-columns:\s*var\(--tl-names-w\)\s+minmax\(max-content,\s*1fr\)/,
  );
  // The strip no longer starts by a margin only the wide column's width explains.
  expect(rule(".tl-inner")).not.toMatch(/margin-left/);
});

test("a timeline row's title alone gives way, so the holder's face stays in the column", () => {
  expect(rule(".tl-name > *")).toMatch(/flex:\s*none/);
  expect(rule(".tl-name > .tl-name-title")).toMatch(/flex:\s*1 1 0/);
});

test("a narrow band's names column leaves the strip room", () => {
  const narrow = rules.find(
    (r) => r.selector === ".tl-scroll" && /--tl-names-w:\s*136px/.test(r.body),
  );
  expect(narrow).toBeDefined();
});

test("a day too narrow for a key draws its count in place of its chips", () => {
  const chips = rules.filter(
    (r) => /\.work-cal-chip/.test(r.selector) && /display:\s*none/.test(r.body),
  );
  expect(chips.map((r) => r.selector)).toEqual([".work-cal-chip, .work-cal-more"]);
  expect(rule(".work-cal-count", /display:\s*none/)).toBeDefined();
  expect(rule(".work-cal-count", /display:\s*flex/)).toBeDefined();
});

test("a chip's key and a day's +N more stay inside their day", () => {
  expect(rule(".work-cal-chip", /overflow/)).toMatch(/overflow:\s*hidden/);
  expect(rule(".work-cal-chip .work-cal-key")).toMatch(/flex:\s*0 1 auto/);
  expect(rule(".work-cal-more")).toMatch(/white-space:\s*nowrap/);
});

test("on a phone only the chips scroll, with a focus ring's room inside their clip", () => {
  const run = rule(".work-bar-run", /overflow-x/);
  expect(run).toMatch(/overflow-x:\s*auto/);
  expect(run).toMatch(/padding:\s*var\(--ring-room\)/);
  expect(run).toMatch(/margin:\s*calc\(-1 \* var\(--ring-room\)\)/);
  // The chips share one line with "+ Filter" and Clear, which never scroll.
  expect(rule(".work-bar .work-chips")).toMatch(/flex-wrap:\s*nowrap/);
  expect(rule(".work-bar .work-chips > :not(.work-bar-run)")).toMatch(/flex:\s*none/);
  // And nothing else in the bar is a scroller.
  const scrollers = rules.filter(
    (r) => /\.work-bar/.test(r.selector) && /overflow-x:\s*auto/.test(r.body),
  );
  expect(scrollers.map((r) => r.selector)).toEqual([".work-bar-run"]);
});

test("a card's focus ring stands inside its edge, and a lane head's has room", () => {
  expect(rule(".work-card:focus-visible")).toMatch(
    /outline-offset:\s*var\(--size-focus-ring-inset-offset\)/,
  );
  const board = rule(".work-board", /overflow-x/);
  expect(board).toMatch(/padding:\s*var\(--spacing-1\)/);
  expect(board).toMatch(/margin-top:\s*calc\(-1 \* var\(--spacing-1\)\)/);
});
