/**
 * The event log is a LIST SCREEN, and every narrowing on it lives in the URL.
 *
 * That is the whole claim worth a build over: a narrowed log is a link
 * somebody can send, and the answer a reader is looking at is one they can go
 * back to. The list view draws the toolbar, the chips and the footer, and the
 * screen hands it values and takes callbacks; a screen that kept a filter in
 * component state instead would look identical and lose it on every reload.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type FeedRow } from "~/protocol/index.ts";
import { narrow } from "~/testing.tsx";
import { Activity } from "./Activity.tsx";

function event(over: Partial<FeedRow>): FeedRow {
  return {
    id: "e1",
    type: "agent_turn_completed",
    timestamp: "2026-09-14T10:00:00Z",
    source: "engine",
    actor: "planner",
    summary: "planner finished a turn",
    category: "task",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    ...over,
  };
}

const EVENTS: FeedRow[] = [
  event({ id: "e1", actor: "planner", summary: "planner finished a turn", category: "task" }),
  event({
    id: "e2",
    actor: "reviewer",
    summary: "reviewer refused the work",
    category: "decision",
    failed: true,
    timestamp: "2026-09-14T09:00:00Z",
  }),
  event({
    id: "e3",
    actor: "engine",
    summary: "a schedule fired",
    category: "lifecycle",
    timestamp: "2026-09-14T08:00:00Z",
  }),
];

function mount(hash = "#/activity") {
  location.hash = hash;
  const store = new Store();
  store.applySnapshot({ events: EVENTS });
  const socket = new LiveSocket(store);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Activity />
      </Router>
    </ClientContext.Provider>,
  );
}

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

/** The actor cell of every row the table is drawing, in order. */
function actors(): string[] {
  return screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => row.querySelectorAll("td")[1]?.textContent ?? "");
}

test("a filter the link carries is on screen as a chip, and narrows the rows", () => {
  mount("#/activity?category=decision");
  expect(actors()).toEqual(["reviewer"]);
  // The chip is what says a filter is on. A screen that narrowed the rows and
  // drew no chip is a list a reader cannot tell is incomplete.
  expect(screen.getByRole("button", { name: "Remove the Category filter" })).toBeDefined();
});

test("narrowing the log writes the filter into the URL", () => {
  mount();
  expect(actors()).toHaveLength(3);
  narrow("Outcome", "Failures only");
  expect(actors()).toEqual(["reviewer"]);
  expect(location.hash).toContain("failed=1");
});

test("the log pages, and the page a reader turned to is in the link", () => {
  // The list screens draw the settings cog already; what they did not draw is
  // a page control, because nothing paged. A page size the link carries is
  // what the table slices by, and the page it lands on goes back into the URL
  // beside the filters, so page two of a narrowed log is one link.
  mount("#/activity?per=1");
  expect(actors()).toEqual(["planner"]);

  fireEvent.click(screen.getByRole("button", { name: /next page/i }));
  expect(location.hash).toContain("page=2");
  expect(actors()).toEqual(["reviewer"]);
});

test("searching again from deep in the log goes back to the first page", () => {
  // The table puts the page back for a sort or a filter of ITS own, and this
  // screen's filters are not its own: each is a URL parameter applied to the
  // rows before they ever reach the view. Both searches here keep three rows,
  // so nothing about the page COUNT gives it away. Without the screen saying
  // that a filter changed, the reader is answered with page three of a list
  // they have not seen the start of.
  mount("#/activity?per=1&page=3&q=r");
  expect(actors()).toEqual(["engine"]);

  fireEvent.change(screen.getByRole("searchbox", { name: "Search events" }), {
    target: { value: "e" },
  });
  expect(actors()).toEqual(["planner"]);
  expect(location.hash).not.toContain("page=3");
});

test("the footer counts what the filters kept, not what the page holds", () => {
  // Not the row count alone: a reader who has narrowed a log needs to know
  // there is more behind the filter than the one row in front of them. And
  // not "Showing 1 of 3" either, now that the table pages: the first number
  // is what MATCHED, which is a different claim from what is drawn, and the
  // pager beside it is what says which rows those are.
  mount("#/activity?category=task");
  expect(screen.getByText(/match/).textContent).toContain("1");
  expect(screen.getByText(/match/).textContent).toContain("3");
});

test("a failed event is marked as failed rather than only tinted", () => {
  mount("#/activity?actor=reviewer");
  const row = screen.getAllByRole("row")[1]!;
  expect(within(row).getByText("Failed")).toBeDefined();
});

test("a row is a link to the event, so it opens in a tab and copies its address", () => {
  mount("#/activity?actor=planner");
  const row = screen.getAllByRole("row")[1]!;
  expect(within(row).getByRole("link").getAttribute("href")).toBe("#/events/e1");
});
