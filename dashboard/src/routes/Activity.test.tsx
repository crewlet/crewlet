/**
 * The event log is a LIST SCREEN, and every narrowing on it lives in the URL.
 *
 * That is the whole claim worth a build over: a narrowed log is a link
 * somebody can send, and the answer a reader is looking at is one they can go
 * back to. The list view draws the toolbar, the chips and the rows, and the
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
  // The engine's own answer about its retention, which the head reports. A
  // socket that answers nothing leaves the screen saying it was not told, so
  // stubbing it is what makes the head's claim testable at all.
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "stream" ? { event_history_seconds: 2_592_000 } : null);
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

test("Reset to Default returns the log to the size the screen opens at", () => {
  // A list screen opens at twenty rows, which is about one screenful of the
  // log; the design system's own default is ten. The settings frame resets to
  // whatever it was TOLD the default is, so a screen that does not say hands
  // its reader back a size nobody on this screen ever chose, and writes it
  // into the link as well.
  mount("#/activity?per=5");
  fireEvent.click(screen.getByRole("button", { name: /table settings/i }));
  fireEvent.click(screen.getByRole("button", { name: "Reset to Default" }));
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  // Absent is how the default says its own name: every parameter falls back to
  // the value the screen opens on.
  expect(location.hash).not.toContain("per=");
});

test("the count is in the head, and nothing repeats it under the rows", () => {
  // The line that used to close this list is gone from the design system, and
  // what it said is said where a reader already looks: the screen's own head
  // counts what the filters kept, the chips above the rows say what narrowed
  // them, and the pager announces which rows of how many are in front of them.
  mount("#/activity?category=task");
  const header = screen.getByRole("banner");
  expect(within(header).getByText(/1 event shown/)).toBeDefined();
  // What the removed line said, in the words it said it in.
  expect(screen.queryByText(/match/)).toBeNull();
  // And said once: a second number under the rows was free to disagree with
  // this one, and on every page past the first it did.
  expect(screen.getAllByText(/event shown/)).toHaveLength(1);
});

test("the head says how far back the log goes, not the end of the rows", async () => {
  // It used to ride beside the Load older button under the rows. Left there it
  // would now be visible only to a reader who had already reached the end,
  // which is the one reader it cannot help: it is a fact about the SCREEN, and
  // what it answers is whether loading older rows is worth the press.
  mount();
  const header = screen.getByRole("banner");
  expect(await within(header).findByText(/the store keeps 30 days/)).toBeDefined();
});

test("older history is loaded from the table's own control under the rows", async () => {
  // The strip under the rows was the SCREEN's, drawn beside the count line and
  // outside the frame, and it carried the retention note as well as the
  // button. The table draws the button now, under its last row; what "older"
  // means is still the screen's, because it is the engine's event query,
  // narrowed by the same filters the rows are.
  mount();
  await screen.findByText(/the store keeps 30 days/);
  const rows = screen.getAllByRole("row");
  const last = rows[rows.length - 1]!;
  const more = screen.getByRole("button", { name: /Load 100 older/ });
  expect(last.compareDocumentPosition(more) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  // And the strip is gone rather than halved: the note it carried is in the
  // head, once, not repeated beside the button.
  expect(screen.getAllByText(/the store keeps 30 days/)).toHaveLength(1);
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
