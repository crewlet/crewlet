/**
 * The event log as a table: a named head over every column, one band per day,
 * each row a link to its event, a failed row marked, and the source said only
 * where it is not the actor.
 *
 * It was a hand-built ledger with no head row: nothing named its columns and
 * nothing gave way at a narrow width.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeAll, expect, test, vi } from "vitest";

import { EventGrid, eventDayBands } from "./EventGrid.tsx";
import { Router } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtTime } from "~/lib/format.ts";
import type { EventEnvelope, FeedRow } from "~/protocol/index.ts";

// HOW MANY ROWS WERE DRAWN, counted where every row draws: its time cell is
// the one caller of `fmtTime` in the grid.
vi.mock("~/lib/format.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/format.ts")>();
  return { ...real, fmtTime: vi.fn(real.fmtTime) };
});
const drawnRows = vi.mocked(fmtTime);

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

beforeAll(() => {
  Element.prototype.scrollIntoView = () => {};
});

const row = (over: Partial<FeedRow> = {}): FeedRow =>
  ({
    id: "e-1",
    type: "org.started",
    timestamp: "2026-09-28T10:00:00Z",
    source: "harness-0",
    actor: "harness-0",
    summary: "Organization 'Nimbus' started",
    category: "lifecycle",
    ...over,
  }) as FeedRow;

function grid(events: FeedRow[]) {
  return render(
    <Router>
      <EventGrid events={events} label="Events" />
    </Router>,
  );
}

const heads = (c: HTMLElement) =>
  [...c.querySelectorAll('[role="columnheader"]')].map((el) => el.textContent);

// Mutation: draw the rows as the old headless `.feed-row` list, and no head names a column.
test("every column has a named head, and none of them sorts the engine's walk", () => {
  const { container } = grid([row()]);
  expect(heads(container)).toEqual(["Time", "Actor", "What happened", "Category", "Source"]);
  expect(container.querySelectorAll('[role="columnheader"] button')).toHaveLength(0);
});

test("a row links to its event and a failed row is marked", () => {
  const { container } = grid([
    row(),
    row({ id: "e-2", failed: true, summary: "guard stall tripped" }),
  ]);
  const links = [...container.querySelectorAll("a.crewlet-data-grid__row-link")].map((a) =>
    a.getAttribute("href"),
  );
  expect(links).toEqual(["#/live/events/e-1", "#/live/events/e-2"]);
  expect(container.querySelector('[aria-label="failed"]')).toBeTruthy();
});

// Mutation: print `event.source` unconditionally, and `harness-0` is drawn twice.
test("a source that is the actor is not drawn again", () => {
  const { container } = grid([row()]);
  expect(container.textContent?.match(/harness-0/g)).toHaveLength(1);
  cleanup();
  const other = grid([row({ source: "webhook" })]);
  expect(other.container.textContent).toContain("webhook");
});

// A DATE IS A PROPERTY OF THE ROWS UNDER IT: one band per day, in the rows' order.
test("rows are cut into one band per day, in the order given", () => {
  const bands = eventDayBands([
    row({ id: "a", timestamp: "2026-09-28T10:00:00Z" }),
    row({ id: "b", timestamp: "2026-09-28T09:00:00Z" }),
    row({ id: "c", timestamp: "2026-09-27T09:00:00Z" }),
  ]);
  expect(bands.map((b) => b.rows.map((r) => r.id))).toEqual([["a", "b"], ["c"]]);
});

// THE ACTOR IS SAID ONCE, in its column: the engine opens its sentences with
// it, and every row of the log said a long seat name twice, the useful words
// pushed past the cut. Mutation: draw `summary` as it comes, and it is twice.
test("what happened is said without the actor its column already names", () => {
  const { container } = grid([
    row({
      actor: "Agent PM",
      source: "Agent PM",
      summary: "Agent PM Agent PM's executor launched",
    }),
  ]);
  expect(container.textContent).toContain("Its executor launched");
  expect(container.textContent?.match(/Agent PM/g)).toHaveLength(1);
});

// AN ANSWER A DELEGATE GAVE IS MARKED WITH ITS RULE (ADR-0125), off the live
// row's payload (`EventEnvelope`); a person's answer is not. Mutation: drop
// the mark, or draw it off any answer.
test("an answer a delegate gave is marked as decided under delegation", () => {
  const answered = (id: string, payload: Record<string, unknown>): FeedRow =>
    ({
      ...row({ id, type: "executor_answered", summary: "SRE's executor was answered" }),
      payload,
    }) as EventEnvelope;
  const { container } = grid([
    answered("e-1", { delegated_rule: 3 }),
    answered("e-2", { answered_by: "ops-lead" }),
  ]);
  const marks = [...container.querySelectorAll(".crewlet-data-grid__row")].map((r) =>
    r.textContent?.includes("Decided under delegation, rule 3"),
  );
  expect(marks).toEqual([true, false]);
});

/**
 * `n` rows a minute apart, newest first, from two days and an hour before
 * `now`: every one says "2d ago", which no second of a case moves, so a row
 * drawn again is drawn for something other than its own words changing.
 */
function hundred(n: number, now: number): FeedRow[] {
  return Array.from({ length: n }, (_, i) =>
    row({
      id: `e-${i}`,
      summary: `event ${i}`,
      timestamp: new Date(now - 2 * 86_400_000 - 3_600_000 - i * 60_000).toISOString(),
    }),
  );
}

// A PUSH DRAWS ITS OWN ROW. The log is fed one event at a time, and each push
// is a new list holding the same rows and one more: every row of the log was
// drawn again for each, ten thousand elements per event while events streamed.
//
// Mutation: draw a cell inline (no `memo`), or build the columns inside the
// grid, and every row is drawn again.
test("a pushed event draws its own row and no other", () => {
  const now = Date.now();
  const rows = hundred(100, now);
  const { rerender } = render(
    <Router>
      <EventGrid events={rows} label="Events" />
    </Router>,
  );
  expect(drawnRows).toHaveBeenCalledTimes(100);
  drawnRows.mockClear();
  const pushed = row({
    id: "new",
    summary: "just arrived",
    timestamp: new Date(now).toISOString(),
  });
  rerender(
    <Router>
      <EventGrid events={[pushed, ...rows]} label="Events" />
    </Router>,
  );
  expect(drawnRows).toHaveBeenCalledTimes(1);
});

// A PASSING SECOND DRAWS NO ROW. The grid read the shared clock for the
// relative time in every row's title, so a log beside a clock (every screen
// it is on has one) drew every row once a second for as long as it was open.
//
// Mutation: read `useNow` in the grid again, and every row is drawn per tick.
test("a passing second draws no row again", async () => {
  vi.useFakeTimers();
  const rows = hundred(100, Date.now());
  // THE SCREEN AROUND IT READS THE CLOCK, as the event log does, and hands
  // the grid the same rows on every tick.
  function Screen() {
    useNow();
    return <EventGrid events={rows} label="Events" />;
  }
  render(
    <Router>
      <Screen />
    </Router>,
  );
  drawnRows.mockClear();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(5_000);
  });
  expect(drawnRows).not.toHaveBeenCalled();
});

// AND THE TITLE'S "HOW LONG AGO" STILL MOVES, off the clock the row reads
// for itself: a row that is never drawn again must not say "just now" an
// hour later.
test("a row's relative time moves with the clock", async () => {
  vi.useFakeTimers();
  const at = new Date(Date.now()).toISOString();
  const { container } = render(
    <Router>
      <EventGrid events={[row({ timestamp: at })]} label="Events" />
    </Router>,
  );
  const time = () => container.querySelector("time")?.getAttribute("title") ?? "";
  expect(time()).toContain("just now");
  await act(async () => {
    await vi.advanceTimersByTimeAsync(120_000);
  });
  expect(time()).toContain("2m ago");
});
