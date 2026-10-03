/**
 * What the arrangement controls offer, and what each one writes: the shape
 * row, Group by and Sort at the bar's end, and the Display menu that holds
 * the rest.
 *
 * Every case here is an arrangement that, drawn wrong, is a control whose
 * effect the reader cannot see: a grouping a shape drops on the way to the
 * wire, a second axis the engine refuses because it equals the first, a column
 * set written against one shape and read against the other. None of them
 * throws and none is visible in a diff — the screen simply draws something
 * nobody arranged.
 *
 * TWO HALVES. The controls themselves are props in and callbacks out, so most
 * of this renders them directly and reads the calls. The last section mounts
 * the list they sit in, because two of its claims are about the ADDRESS rather
 * than about a callback: the column key is per shape, and switching the
 * drawing must not throw the order away.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { DisplayMenu, type DisplayMenuProps } from "./DisplayMenu.tsx";
import { ArrangeControls, ShapeTabs, defaultOrderLabel } from "./WorkBar.tsx";
import { ItemsView } from "../ItemsView.tsx";
import { columnChoices } from "../shapes/Grid.tsx";
import { CARD_FACTS } from "~/components/work.tsx";
import { Router } from "~/app/router.tsx";
import { pick } from "~/testing.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { groupAxisOptions, secondAxisOptions, SORTS, type Shape } from "~/lib/work.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  // THE AGENTS PUSH, which the list reads for the turn running on each card:
  // no seat is working in these cases, so the push is empty.
  return {
    ...actual,
    useClient: vi.fn(),
    useConnection: vi.fn(),
    useOrg: vi.fn(),
    useAgents: () => [],
  };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** The menu's callbacks, so a case can say which one a control wrote through. */
function wrote() {
  return {
    onShape: vi.fn(),
    onGroupBy2: vi.fn(),
    onCols: vi.fn(),
    onShowLane: vi.fn(),
    onCardHidden: vi.fn(),
  };
}

/** The menu, rendered and opened. */
function openDisplay(over: Partial<DisplayMenuProps> = {}) {
  const calls = wrote();
  render(
    <DisplayMenu
      shape="list"
      workspace
      viewShape="list"
      groupBy=""
      groupBy2=""
      cols=""
      hidden={[]}
      cardHidden={new Set()}
      {...calls}
      {...over}
    />,
  );
  const trigger = screen.getByRole("button", { name: "Display" });
  fireEvent.click(trigger);
  return { ...calls, trigger };
}

/** Group by and Sort, as the bar draws them. */
function arrange(
  over: Partial<{ shape: Shape; workspace: boolean; groupBy: string; sort: string }> = {},
) {
  const calls = { onGroupBy: vi.fn(), onSort: vi.fn() };
  render(
    <ArrangeControls
      shape={over.shape ?? "list"}
      workspace={over.workspace ?? true}
      groupBy={over.groupBy ?? ""}
      sort={over.sort ?? ""}
      {...calls}
    />,
  );
  return calls;
}

/**
 * The options one picker is offering, as a reader reads them.
 *
 * IT LEAVES THE LIST CLOSED, because the trigger toggles: a case that read two
 * pickers, or read one and then chose from it, was shutting the list it had
 * just opened and finding no options at all.
 */
function optionsOf(name: string): string[] {
  const control = screen.getByRole("combobox", { name });
  fireEvent.click(control);
  const labels = screen.getAllByRole("option").map((el) => el.textContent ?? "");
  fireEvent.click(control);
  return labels;
}

/** The column checkboxes, in the order the menu draws them. */
const columnBoxes = (): string[] =>
  [...document.querySelectorAll(".grid-cols-choices:not([data-card-facts]) .grid-cols-choice")].map(
    (el) => el.textContent ?? "",
  );

// ---------------------------------------------------------------------------
// The shapes, and the button
// ---------------------------------------------------------------------------

// THE FIVE SHAPES ARE THE FIRST ROW'S OWN BUTTONS, in the approved board's
// order, and not the view strip: a shape is a way of drawing any query, and
// mixed in with the queries somebody saved the two read as the same kind of
// thing. They were inside the Display menu for a while, which put the board —
// the shape a team lives in — two presses away.
test("each of the five shapes writes its own value, and the one that is on says so", () => {
  const shapes: { label: string; value: Shape }[] = [
    { label: "List", value: "list" },
    { label: "Board", value: "board" },
    { label: "Timeline", value: "timeline" },
    { label: "Calendar", value: "calendar" },
    { label: "Table", value: "table" },
  ];
  const onShape = vi.fn();
  render(<ShapeTabs shape="board" onShape={onShape} />);
  const row = screen.getByRole("group", { name: "Draw as" });
  expect(
    within(row)
      .getAllByRole("button")
      .map((el) => el.textContent),
  ).toEqual(shapes.map((s) => s.label));
  for (const { label, value } of shapes) {
    fireEvent.click(within(row).getByRole("button", { name: label }));
    expect(onShape).toHaveBeenLastCalledWith(value);
  }
  expect(within(row).getByRole("button", { name: "Board" }).getAttribute("aria-pressed")).toBe(
    "true",
  );
  expect(within(row).getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe(
    "false",
  );
});

// THE MENU IS "DISPLAY" AND HOLDS WHAT A READER SETS ONCE. What it used to say
// on its button — the shape and the axis — is on screen without it now, in the
// shape row and the Group by picker, so the button stopped restating it.
test("the menu holds the second axis, the columns and the lanes, and nothing the bar holds", () => {
  openDisplay({ shape: "list", groupBy: "status" });
  expect(screen.getByRole("combobox", { name: "Then by" })).toBeTruthy();
  expect(screen.getByText("Columns")).toBeTruthy();
  expect(screen.queryByRole("combobox", { name: "Group by" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Sort" })).toBeNull();
  expect(document.querySelector(".work-display-shape")).toBeNull();
});

// WHAT A SAVED VIEW WAS SAVED AS, and the way back to it. A reader who has
// overridden the shape has no other way to tell that they have: the strip
// shows which view is running, not which drawing it was saved with.
test("the way back to a view's own shape appears only where one is overridden", () => {
  const { onShape } = openDisplay({ shape: "list", viewShape: "board" });
  const back = screen.getByRole("button", { name: /Back to this view/ });
  fireEvent.click(back);
  expect(onShape).toHaveBeenCalledWith("board");
  cleanup();
  openDisplay({ shape: "board", viewShape: "board" });
  expect(screen.queryByRole("button", { name: /Back to this view/ })).toBeNull();
  cleanup();
  // AND NOWHERE WITHOUT A SAVED VIEW. A plain board is a shape the reader
  // chose over the builtin list, not a saved view drawn another way — offered
  // there, the way back pointed at a view nobody had open.
  openDisplay({ shape: "board", viewShape: undefined });
  expect(screen.queryByRole("button", { name: /Back to this view/ })).toBeNull();
});

// ---------------------------------------------------------------------------
// The grouping
// ---------------------------------------------------------------------------

// THE PICKER OFFERS THE GRAMMAR'S OWN AXES and nothing else — the list is a
// copy of the engine's `groupKeys`, held against it in both directions by
// `internal/tracker/client_gate_test.go`, so a value invented here would take
// the whole board down with a refusal.
test("the group axis options are the ones the grammar takes, per shape and scope", () => {
  arrange({ shape: "list", workspace: true });
  expect(optionsOf("Group by")).toEqual(groupAxisOptions("list", true).map((o) => o.label));
  // EVERY OTHER SHAPE CAN BE UNGROUPED, so it leads with that row.
  expect(optionsOf("Group by")[0]).toBe("No grouping");
  cleanup();

  // A BOARD IS ALWAYS GROUPED — it is what a board IS — so there is no "No
  // grouping" row and Status is the axis's own entry rather than a second row
  // reading the same word.
  arrange({ shape: "board", workspace: true });
  const board = optionsOf("Group by");
  expect(board).toEqual(groupAxisOptions("board", true).map((o) => o.label));
  expect(board).not.toContain("No grouping");
  expect(board.filter((label) => label === "Status")).toHaveLength(1);
  cleanup();

  // AND PROJECT ONLY AT WORKSPACE SCOPE: inside a project every row is in one
  // project, and the grammar refuses the question.
  arrange({ shape: "list", workspace: false });
  expect(optionsOf("Group by")).not.toContain("Project");
});

// A BOARD SHOWS THE AXIS THE QUERY WAS SENT ON. Its default is status whether
// or not anybody said so, so a picker built from the URL key alone sat on
// nothing over a board whose columns were statuses.
test("a board with no chosen axis shows the one it is grouped by anyway", () => {
  arrange({ shape: "board", groupBy: "" });
  expect(screen.getByRole("combobox", { name: "Group by" }).textContent).toContain("Status");
});

// THE SECOND AXIS IS NEVER THE FIRST, which the engine refuses because every
// row would then be alone in its own band.
test("the second axis offers everything but the first", () => {
  openDisplay({ shape: "list", groupBy: "status", workspace: true });
  const offered = optionsOf("Then by");
  expect(offered).toEqual(secondAxisOptions("status", true).map((o) => o.label));
  expect(offered).not.toContain("Status");
  expect(offered[0]).toBe("No second grouping");
});

// WHAT A SHAPE DOES NOT DRAW, IT DOES NOT OFFER. A board's second axis is a
// swimlane GRID rather than a band in a band; a calendar's axis IS the date,
// so it has no grouping at all; and a band inside a band is what the two grid
// shapes draw. A control writing a key its shape drops is a control whose
// effect the reader cannot see.
test("Then by is drawn on a grid shape with a first axis, and nowhere else", () => {
  openDisplay({ shape: "list", groupBy: "status" });
  expect(screen.getByRole("combobox", { name: "Then by" })).toBeTruthy();
  cleanup();
  openDisplay({ shape: "list", groupBy: "" });
  expect(screen.queryByRole("combobox", { name: "Then by" })).toBeNull();
  cleanup();
  openDisplay({ shape: "board", groupBy: "status" });
  expect(screen.queryByRole("combobox", { name: "Then by" })).toBeNull();
  cleanup();
  openDisplay({ shape: "timeline", groupBy: "status" });
  expect(screen.queryByRole("combobox", { name: "Then by" })).toBeNull();
});

test("the calendar offers neither a grouping nor an order, because it spends both", () => {
  arrange({ shape: "calendar" });
  expect(screen.queryByRole("combobox", { name: "Group by" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Sort" })).toBeNull();
  cleanup();
  // AND ITS DISPLAY MENU SAYS THERE IS NOTHING LEFT TO ARRANGE rather than
  // opening on an empty panel.
  openDisplay({ shape: "calendar" });
  expect(screen.queryByText("Columns")).toBeNull();
  expect(screen.getByText(/Nothing more to arrange on this shape/)).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The order
// ---------------------------------------------------------------------------

// THE ORDERINGS ARE THE GRAMMAR'S, `-` AND ALL: `-updated` is the key the
// engine takes, and a picker that wrote "updated desc" or "recent" would be
// refused at the read rather than sorted differently.
test("Sort offers the grammar's own keys and writes them as written", () => {
  const { onSort } = arrange();
  expect(optionsOf("Sort")).toEqual([
    defaultOrderLabel("list", true),
    ...SORTS.map((s) => s.label),
  ]);
  pick(screen.getByRole("combobox", { name: "Sort" }), "Recently updated");
  expect(onSort).toHaveBeenCalledWith("-updated");
  expect(SORTS.find((s) => s.label === "Recently updated")?.value).toBe("-updated");
  // AND THE ONE MEASURE OF COST IS TOKENS, never money.
  expect(SORTS.find((s) => s.label === "Most tokens")?.value).toBe("-spend_tokens");
});

// THE DEFAULT SAYS WHICH ORDER IT IS, because it is a different one per
// container and shape: "Default" alone named none of them, and a lead could not
// tell whether a drag would stick.
test("the default order is named for what it is", () => {
  expect(defaultOrderLabel("board", false)).toBe("Manual (default)");
  expect(defaultOrderLabel("list", true)).toBe("Recently updated (default)");
  expect(defaultOrderLabel("timeline", false)).toBe("Start date (default)");
});

// AND THE DEFAULT IS A VALUE THE SCREEN RESOLVES, not a deletion this control
// performs: `""` out of the callback means off, and turning a view's own order
// off is the screen's to write because only it knows what the view carries.
test("choosing the default order hands back the empty string", () => {
  const { onSort } = arrange({ sort: "-updated" });
  pick(screen.getByRole("combobox", { name: "Sort" }), defaultOrderLabel("list", true));
  expect(onSort).toHaveBeenCalledWith("");
});

// ---------------------------------------------------------------------------
// The lanes put away
// ---------------------------------------------------------------------------

// A HIDDEN LANE IS NAMED, AND ONE PRESS BRINGS IT BACK. It is a column of work
// nobody can tell is missing otherwise — on an address somebody else sent.
test("the lanes put away are named, and each comes back on its own or all at once", () => {
  const { onShowLane } = openDisplay({
    shape: "board",
    hidden: [
      { key: "done", label: "Done" },
      { key: "", label: "No value" },
    ],
  });
  fireEvent.click(screen.getByRole("button", { name: "Show the Done lane" }));
  expect(onShowLane).toHaveBeenLastCalledWith("done");
  fireEvent.click(screen.getByRole("button", { name: "Show every lane" }));
  expect(onShowLane).toHaveBeenLastCalledWith();
  cleanup();
  // ONLY A BOARD HAS LANES, so no other shape offers them back.
  openDisplay({ shape: "list", hidden: [{ key: "done", label: "Done" }] });
  expect(screen.queryByText("Hidden lanes")).toBeNull();
});

// ---------------------------------------------------------------------------
// The card facts
// ---------------------------------------------------------------------------

// A BOARD'S ARRANGEMENT IS WHAT ITS CARDS SHOW, as a grid's is its columns —
// so the board's Display menu is never the empty panel it was, which said
// "nothing more to arrange" over the one shape with the most on each item.
// Every descriptive fact is offered, ticked while it is drawn, and a tick
// hands back the set with that one fact moved.
test("the board offers what its cards show, and a tick moves exactly that fact", () => {
  const { onCardHidden } = openDisplay({ shape: "board", cardHidden: new Set(["labels"]) });
  expect(screen.queryByText(/Nothing more to arrange/)).toBeNull();
  const boxes = [...document.querySelectorAll("[data-card-facts] .grid-cols-choice")];
  expect(boxes.map((b) => b.textContent)).toEqual(CARD_FACTS.map((f) => f.label));
  expect((screen.getByLabelText("Labels") as HTMLInputElement).checked).toBe(false);
  expect((screen.getByLabelText("Due date") as HTMLInputElement).checked).toBe(true);
  fireEvent.click(screen.getByLabelText("Due date"));
  expect([...(onCardHidden.mock.lastCall![0] as Set<string>)].sort()).toEqual(["due", "labels"]);
  fireEvent.click(screen.getByLabelText("Labels"));
  expect([...(onCardHidden.mock.lastCall![0] as Set<string>)]).toEqual([]);
  cleanup();
  // ONLY A BOARD HAS CARDS.
  openDisplay({ shape: "list" });
  expect(screen.queryByText("Card shows")).toBeNull();
});

// ---------------------------------------------------------------------------
// The columns
// ---------------------------------------------------------------------------

// COLUMNS ARE OFFERED ON BOTH GRID SHAPES, and they are the ACTIVE set's. The
// menu offered them on the table alone under a rule that described the
// implementation rather than the product — the only thing making it true was
// that the list had no columns to give.
test("both grid shapes offer their own column set, and no other shape offers one", () => {
  openDisplay({ shape: "list", workspace: true });
  expect(columnBoxes()).toEqual(columnChoices("list", true).map((c) => c.label));
  cleanup();

  openDisplay({ shape: "table", workspace: true });
  expect(columnBoxes()).toEqual(columnChoices("table", true).map((c) => c.label));
  // THE TWO SETS ARE NOT THE SAME SET IN THE SAME ORDER, which is the whole
  // reason the key that holds them is per shape.
  expect(columnChoices("table", true)).not.toEqual(columnChoices("list", true));
  cleanup();

  for (const shape of ["board", "calendar", "timeline"] as Shape[]) {
    openDisplay({ shape });
    expect(columnBoxes(), `${shape} offers columns`).toEqual([]);
    expect(screen.queryByText("Columns")).toBeNull();
    cleanup();
  }
  // AND INSIDE A PROJECT THERE IS NO PROJECT COLUMN to choose, because the
  // grid never draws one there.
  openDisplay({ shape: "table", workspace: false });
  expect(columnBoxes()).not.toContain("Project");
});

// AN EMPTY `cols=` IS THE DEFAULT SET rather than an empty grid — the grid
// reads it that way, so the ticks show the default until somebody moves one.
test("with nothing chosen the ticks are the set's own default", () => {
  openDisplay({ shape: "list", workspace: true });
  const ticked = [...document.querySelectorAll<HTMLInputElement>(".grid-cols-choice input")]
    .map((box, at) => (box.checked ? columnBoxes()[at] : null))
    .filter(Boolean);
  expect(ticked).toEqual(
    columnChoices("list", true)
      .filter((c) => !c.optional)
      .map((c) => c.label),
  );
});

// THE DECLARATION ORDER, NEVER THE CLICK ORDER. `cols=` is read as the order
// to DRAW the columns in, so a value built from a Set's insertion order would
// rearrange the grid every time somebody ticked a box.
test("ticking a column writes the set in the order the grid declares it", () => {
  const choices = columnChoices("list", true);
  const optional = choices.find((c) => c.optional);
  if (!optional) throw new Error("the list set has no optional column to tick");
  const defaults = choices.filter((c) => !c.optional).map((c) => c.key);

  const { onCols } = openDisplay({ shape: "list", workspace: true });
  fireEvent.click(screen.getByLabelText(optional.label));
  const written = String(onCols.mock.calls.at(-1)?.[0]).split(",");
  expect(written).toEqual(
    choices.filter((c) => defaults.includes(c.key) || c.key === optional.key).map((c) => c.key),
  );
});

// AND UNTICKING TAKES ONE OUT OF THE SET RATHER THAN OUT OF THE GRID: what is
// written is still every other column, in the same order.
test("unticking a column writes the rest of the set", () => {
  const choices = columnChoices("table", true);
  const drawn = choices.filter((c) => !c.optional);
  const dropped = drawn[1];
  if (!dropped) throw new Error("the table set draws fewer than two columns by default");

  const { onCols } = openDisplay({ shape: "table", workspace: true });
  fireEvent.click(screen.getByLabelText(dropped.label));
  expect(String(onCols.mock.calls.at(-1)?.[0]).split(",")).toEqual(
    drawn.filter((c) => c.key !== dropped.key).map((c) => c.key),
  );
});

// ---------------------------------------------------------------------------
// On the screen: the keys the menu actually writes
// ---------------------------------------------------------------------------

/** One socket answering each question with a fixture. */
function serving(answers: Partial<Record<QueryName, unknown>>) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({ name: "Acme", roles: [] } as never);
  return query;
}

const task: WorkSummary = {
  id: "1",
  key: "ENG-1",
  project: "ENG",
  title: "Ship the thing",
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
};

const mountList = () =>
  render(
    <Router>
      <ViewerProvider>
        <ItemsView />
      </ViewerProvider>
    </Router>,
  );

/** The Display menu of the mounted list, opened. */
async function openOnScreen() {
  fireEvent.click(await screen.findByRole("button", { name: "Display" }));
}

// THE COLUMN KEY IS THE SHAPE'S OWN. `cols=` carries an ORDER as well as a
// selection and the two sets declare different ones, so a value written
// against one shape and read against the other draws a row nobody arranged —
// and no validation can catch it, because every name in it is legal in both.
test("the Columns control writes the active shape's own key and never the other", async () => {
  serving({ work_items: { items: [task], groups: [], total_hint: 1, complete: true } });
  mountList();
  await openOnScreen();
  await waitFor(() => expect(screen.getByText("Columns")).toBeTruthy());
  const optional = columnChoices("list", true).find((c) => c.optional);
  if (!optional) throw new Error("the list set has no optional column to tick");
  fireEvent.click(screen.getByLabelText(optional.label));
  await waitFor(() => expect(location.hash).toContain("cols.list="));
  expect(location.hash).not.toContain("cols.table=");
  cleanup();

  location.hash = "#/work?shape=table";
  serving({ work_items: { items: [task], groups: [], total_hint: 1, complete: true } });
  mountList();
  await openOnScreen();
  await waitFor(() => expect(screen.getByText("Columns")).toBeTruthy());
  const tableOptional = columnChoices("table", true).find((c) => c.optional);
  if (!tableOptional) throw new Error("the table set has no optional column to tick");
  fireEvent.click(screen.getByLabelText(tableOptional.label));
  await waitFor(() => expect(location.hash).toContain("cols.table="));
  expect(location.hash).not.toContain("cols.list=");
});

// A DRAWING IS NOT A QUERY, so changing one keeps the other: `shape=` and
// `view=` are two keys precisely so that looking at a saved board as a list
// does not throw the saved filters away — and the order the reader chose is
// part of what survives.
test("switching the shape keeps the order and the view the reader is on", async () => {
  location.hash = "#/work?view=arranged&sort=-updated&group_by=assignee";
  serving({
    work_views: {
      views: [
        {
          id: "v-1",
          key: "arranged",
          name: "Arranged",
          type: "list",
          container: { kind: "workspace", id: "" },
          builtin: false,
          params: {},
        },
      ],
      complete: true,
    },
    work_items: { items: [task], groups: [], total_hint: 1, complete: true },
  });
  mountList();
  const shapes = await screen.findByRole("group", { name: "Draw as" });
  fireEvent.click(within(shapes).getByRole("button", { name: "Board" }));
  await waitFor(() => expect(location.hash).toContain("shape=board"));
  expect(location.hash).toContain("sort=-updated");
  expect(location.hash).toContain("view=arranged");
});

// AND A SAVED VIEW IS A TAB RATHER THAN A DRAWING, so moving between views
// leaves the arrangement the reader chose exactly where it was.
test("moving to another saved view keeps the arrangement", async () => {
  location.hash = "#/work?sort=-updated&shape=table";
  serving({
    work_views: {
      views: ["one", "two"].map((key) => ({
        id: `v-${key}`,
        key,
        name: key === "one" ? "One" : "Two",
        type: "list",
        container: { kind: "workspace", id: "" },
        builtin: false,
        pinned: true,
        params: {},
      })),
      complete: true,
    },
    work_items: { items: [task], groups: [], total_hint: 1, complete: true },
  });
  mountList();
  fireEvent.click(await screen.findByRole("button", { name: "Two" }));
  await waitFor(() => expect(location.hash).toContain("view=two"));
  expect(location.hash).toContain("sort=-updated");
  expect(location.hash).toContain("shape=table");
});
