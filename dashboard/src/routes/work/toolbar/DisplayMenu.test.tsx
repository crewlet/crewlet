/**
 * What the Display menu offers, and what each control writes.
 *
 * Every case here is an arrangement that, drawn wrong, is a control whose
 * effect the reader cannot see: a grouping a shape drops on the way to the
 * wire, a second axis the engine refuses because it equals the first, a column
 * set written against one shape and read against the other. None of them
 * throws and none is visible in a diff — the screen simply draws something
 * nobody arranged.
 *
 * TWO HALVES. The menu itself is props in and callbacks out, so most of this
 * renders it directly and reads the calls. The last section mounts the list it
 * sits in, because two of its claims are about the ADDRESS rather than about a
 * callback: the column key is per shape, and switching the drawing must not
 * throw the order away.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { DisplayMenu, type DisplayMenuProps } from "./DisplayMenu.tsx";
import { ItemsView } from "../ItemsView.tsx";
import { columnChoices } from "../shapes/Grid.tsx";
import { Router } from "~/app/router.tsx";
import { pick } from "~/testing.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { groupAxisOptions, secondAxisOptions, SORTS, type Shape } from "~/lib/work.ts";
import type { QueryName, WorkSummary } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

/** The five callbacks, so a case can say which one a control wrote through. */
function wrote() {
  return {
    onShape: vi.fn(),
    onGroupBy: vi.fn(),
    onGroupBy2: vi.fn(),
    onSort: vi.fn(),
    onCols: vi.fn(),
  };
}

/** The menu, rendered and opened. */
function openDisplay(over: Partial<DisplayMenuProps> = {}) {
  const calls = wrote();
  render(
    <DisplayMenu
      shape="list"
      axis=""
      workspace
      viewShape="list"
      groupBy=""
      groupBy2=""
      sort=""
      cols=""
      {...calls}
      {...over}
    />,
  );
  const trigger = screen.getByRole("button", { name: /^(List|Board|Table|Calendar|Timeline)/ });
  fireEvent.click(trigger);
  return { ...calls, trigger };
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
  [...document.querySelectorAll(".work-display-col")].map((el) => el.textContent ?? "");

// ---------------------------------------------------------------------------
// The button, and the shapes
// ---------------------------------------------------------------------------

// THE BUTTON SAYS WHAT IS ON, so the arrangement is readable without opening
// anything — which is what the strip of shape tabs used to do and what a bare
// "Display" would have taken away.
test("the button names the shape, and the axis where there is one", () => {
  const { trigger } = openDisplay({ shape: "board", axis: "status" });
  expect(trigger.textContent).toBe("Board · Status");
  cleanup();
  // AND `due:bucket` IS CALLED "Due" — the one axis that is not a stored
  // value, named the way the picker names it rather than by its wire key.
  expect(openDisplay({ axis: "due:bucket" }).trigger.textContent).toBe("List · Due");
  cleanup();
  expect(openDisplay({ axis: "" }).trigger.textContent).toBe("List");
  cleanup();
  // AN AXIS THIS BUILD DOES NOT OFFER names nothing rather than printing a key
  // the reader has no control for.
  expect(openDisplay({ axis: "invented" }).trigger.textContent).toBe("List");
});

// THE FIVE SHAPES ARE HERE AND NOT IN THE VIEW STRIP: a shape is a way of
// drawing any query, and mixed in with the queries somebody saved the two read
// as the same kind of thing.
test("each of the five shapes writes its own value", () => {
  const shapes: { label: string; value: Shape }[] = [
    { label: "List", value: "list" },
    { label: "Board", value: "board" },
    { label: "Table", value: "table" },
    { label: "Calendar", value: "calendar" },
    { label: "Timeline", value: "timeline" },
  ];
  const { onShape } = openDisplay({ shape: "board" });
  expect(columnShapeLabels()).toEqual(shapes.map((s) => s.label));
  for (const { label, value } of shapes) {
    fireEvent.click(shapeButton(label));
    expect(onShape).toHaveBeenLastCalledWith(value);
  }
  // AND THE ONE THAT IS ON SAYS SO, which is what a reader looks for before
  // pressing anything.
  expect(shapeButton("Board").getAttribute("aria-pressed")).toBe("true");
  expect(shapeButton("List").getAttribute("aria-pressed")).toBe("false");
});

const shapeButtons = (): HTMLElement[] => [
  ...document.querySelectorAll<HTMLElement>(".work-display-shape"),
];
const columnShapeLabels = (): string[] => shapeButtons().map((el) => el.textContent ?? "");
function shapeButton(label: string): HTMLElement {
  const found = shapeButtons().find((el) => el.textContent === label);
  if (!found) throw new Error(`no ${label} shape: ${columnShapeLabels().join(", ")}`);
  return found;
}

// WHAT THE VIEW ITSELF WAS SAVED AS, and the way back to it. A reader who has
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
});

// ---------------------------------------------------------------------------
// The grouping
// ---------------------------------------------------------------------------

// THE PICKER OFFERS THE GRAMMAR'S OWN AXES and nothing else — the list is a
// copy of the engine's `groupKeys`, held against it in both directions by
// `internal/tracker/client_gate_test.go`, so a value invented here would take
// the whole board down with a refusal.
test("the group axis options are the ones the grammar takes, per shape and scope", () => {
  openDisplay({ shape: "list", workspace: true });
  expect(optionsOf("Group by")).toEqual(groupAxisOptions("list", true).map((o) => o.label));
  // EVERY OTHER SHAPE CAN BE UNGROUPED, so it leads with that row.
  expect(optionsOf("Group by")[0]).toBe("No grouping");
  cleanup();

  // A BOARD IS ALWAYS GROUPED — it is what a board IS — so there is no "No
  // grouping" row and Status is the axis's own entry rather than a second row
  // reading the same word.
  openDisplay({ shape: "board", workspace: true });
  const board = optionsOf("Group by");
  expect(board).toEqual(groupAxisOptions("board", true).map((o) => o.label));
  expect(board).not.toContain("No grouping");
  expect(board.filter((label) => label === "Status")).toHaveLength(1);
  cleanup();

  // AND PROJECT ONLY AT WORKSPACE SCOPE: inside a project every row is in one
  // project, and the grammar refuses the question.
  openDisplay({ shape: "list", workspace: false });
  expect(optionsOf("Group by")).not.toContain("Project");
});

// A BOARD SHOWS THE AXIS THE QUERY WAS SENT ON. Its default is status whether
// or not anybody said so, so a picker built from the URL key alone sat on
// nothing over a board whose columns were statuses.
test("a board with no chosen axis shows the one it is grouped by anyway", () => {
  openDisplay({ shape: "board", groupBy: "" });
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
  openDisplay({ shape: "calendar" });
  expect(screen.queryByRole("combobox", { name: "Group by" })).toBeNull();
  expect(screen.queryByRole("combobox", { name: "Order by" })).toBeNull();
  expect(screen.queryByText("Columns")).toBeNull();
});

// ---------------------------------------------------------------------------
// The order
// ---------------------------------------------------------------------------

// THE ORDERINGS ARE THE GRAMMAR'S, `-` AND ALL: `-updated` is the key the
// engine takes, and a picker that wrote "updated desc" or "recent" would be
// refused at the read rather than sorted differently.
test("Order by offers the grammar's own keys and writes them as written", () => {
  const { onSort } = openDisplay();
  expect(optionsOf("Order by")).toEqual(["Default order", ...SORTS.map((s) => s.label)]);
  pick(screen.getByRole("combobox", { name: "Order by" }), "Recently updated");
  expect(onSort).toHaveBeenCalledWith("-updated");
  expect(SORTS.find((s) => s.label === "Recently updated")?.value).toBe("-updated");
});

// AND "DEFAULT ORDER" IS A VALUE THE SCREEN RESOLVES, not a deletion this
// control performs: `""` out of the callback means off, and turning a view's
// own order off is the screen's to write because only it knows what the view
// carries.
test("choosing the default order hands back the empty string", () => {
  const { onSort } = openDisplay({ sort: "-updated" });
  pick(screen.getByRole("combobox", { name: "Order by" }), "Default order");
  expect(onSort).toHaveBeenCalledWith("");
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
  const ticked = [...document.querySelectorAll<HTMLInputElement>(".work-display-col input")]
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
      <ItemsView />
    </Router>,
  );

/** The Display menu of the mounted list, opened. */
async function openOnScreen() {
  const trigger = await screen.findByRole("button", {
    name: /^(List|Board|Table|Calendar|Timeline)/,
  });
  fireEvent.click(trigger);
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
  await openOnScreen();
  fireEvent.click(shapeButton("Board"));
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
        params: {},
      })),
      complete: true,
    },
    work_items: { items: [task], groups: [], total_hint: 1, complete: true },
  });
  mountList();
  fireEvent.click(await screen.findByRole("tab", { name: "Two" }));
  await waitFor(() => expect(location.hash).toContain("view=two"));
  expect(location.hash).toContain("sort=-updated");
  expect(location.hash).toContain("shape=table");
});
