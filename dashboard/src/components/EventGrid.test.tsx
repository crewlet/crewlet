/**
 * The event log as a table: a named head over every column, one band per day,
 * each row a link to its event, a failed row marked, and the source said only
 * where it is not the actor.
 *
 * It was a hand-built ledger with no head row: nothing named its columns and
 * nothing gave way at a narrow width.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeAll, expect, test } from "vitest";

import { EventGrid, eventDayBands } from "./EventGrid.tsx";
import { Router } from "~/app/router.tsx";
import type { EventEnvelope, FeedRow } from "~/protocol/index.ts";

afterEach(cleanup);

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
