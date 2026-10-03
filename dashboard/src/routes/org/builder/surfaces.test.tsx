/**
 * Edit org's builder is assembled from the real views and dialogs.
 *
 * Every other Builder suite stands a view in with a fake, so nothing there
 * notices a builder whose visualization or table was never bound: it rendered "not
 * part of this build" for both until this binding existed. These tests mount
 * the builder with the surfaces the screen hands it and hold it to drawing the
 * canvas's tree, the table's rows and the node editor.
 */

import { act, cleanup, fireEvent, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { ChangeKindDialog } from "./ChangeKindDialog.tsx";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { countingKeys } from "./model/testkit.ts";
import { builderSurfaces } from "./surfaces.ts";
import { company, Engine, mountBuilder, type Settle, pressInToolbar } from "./testkit.tsx";
import { chartCard, LayoutObserver } from "./viewTestkit.tsx";
import { focusables } from "@crewlethq/ui";
import { drawnPart, menuEntryLabel, orgNodeParts, orgTableParts } from "~/testing.tsx";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

/** What one case installed and every case has to take back down. */
const onTeardown: (() => void)[] = [];

afterEach(() => {
  cleanup();
  while (onTeardown.length > 0) onTeardown.pop()!();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  location.hash = "#/";
});

test("the canvas view draws the structure chart and the reporting chart", async () => {
  const engine = new Engine(company());
  // The reporting lines are the engine's derivation, which the org push carries.
  const { settle, checked } = mountBuilder({
    engine,
    org: engine.orgPush(),
    surfaces: builderSurfaces,
  });
  await checked();
  expect(screen.getByRole("tree", { name: "Structure chart" })).toBeDefined();
  fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
  await settle();
  expect(screen.getByRole("tree", { name: "Reporting chart" })).toBeDefined();
});

// THE CONTROL: with no derivation there are no lines to draw, and the chart
// says where they come from rather than drawing a forest nobody derived.
test("the reporting chart waits for the engine's derivation", async () => {
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
  });
  await checked();
  fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
  await settle();
  expect(
    screen.getByText("Reporting lines appear once the engine describes the company"),
  ).toBeDefined();
  expect(screen.queryByRole("tree", { name: "Reporting chart" })).toBeNull();
});

test("the table view draws a row per node, and a row's Edit opens the node editor", async () => {
  const { checked, settle } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  await openTheEditorFromTheTable(settle);
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBeDefined();
});

/**
 * The table row whose NAME cell says `name`.
 *
 * The name cell rather than the row, because a unit's lead is a seat's name in
 * another cell: "Dev" matches Engineering's row too, on the column that says
 * who leads it.
 */
async function tableRow(settle: Settle, name: string): Promise<HTMLElement> {
  await settle();
  const rows = screen.getAllByRole("row");
  const row = rows.find((r) =>
    [...r.querySelectorAll<HTMLElement>(".btable-name")].some(
      (cell) => within(cell).queryAllByText(name, { exact: true }).length > 0,
    ),
  );
  if (!row) throw new Error(`no table row named ${name}`);
  return row as HTMLElement;
}

/**
 * Opens the CEO's editor from its row's own Edit control, which is where the
 * table draws it: Edit is one of the three the console puts on a row, so it is
 * a button rather than an entry of the menu beside it.
 */
async function openTheEditorFromTheTable(settle: Settle): Promise<void> {
  const row = await tableRow(settle, "CEO");
  fireEvent.click(within(row).getByRole("button", { name: "Edit CEO" }));
}

/**
 * The Edit control of the table row named `name`, found while the builder has a
 * check out that the suite is holding — so read from the rows already drawn
 * rather than settled first, which would wait for the held answer.
 */
async function tableRowWhileChecking(name: string): Promise<HTMLElement> {
  const row = screen
    .getAllByRole("row")
    .find((r) =>
      [...r.querySelectorAll<HTMLElement>(".btable-name")].some(
        (cell) => within(cell).queryAllByText(name, { exact: true }).length > 0,
      ),
    );
  if (!row) throw new Error(`no table row named ${name}`);
  return within(row as HTMLElement).getByRole("button", { name: `Edit ${name}` });
}

/** Opens the toolbar's Add menu, picks `entry`, and settles on the dialog it asks in. */
async function askToAdd(settle: Settle, entry: string): Promise<HTMLElement> {
  pressInToolbar("Add");
  fireEvent.click(
    within(screen.getByRole("menu", { name: "Add to the organization" })).getByRole("menuitem", {
      name: entry,
    }),
  );
  await settle();
  return screen.getByRole("dialog", { name: "Add to Acme" });
}

/**
 * Presses one of the pointer-only controls on the card `node` is drawn in.
 *
 * FOUND IN THAT CARD. The controls are hidden from assistive technology, so
 * the query has to include hidden elements, and hidden, every button it can
 * see is a candidate whose accessible name jsdom computes element by element:
 * across the whole document that was the slowest line of these cases, and
 * across the whole chart it was still the slowest of the case that falls back
 * to the dialog. The card is found as `CanvasView.test.tsx` finds it, from the
 * treeitem carrying the node's name.
 */
function pressOnTheChart(node: string, name: string): void {
  const chart = screen.getByRole("tree", { name: "Structure chart" });
  const item = within(chart)
    .getAllByRole("treeitem", { hidden: true })
    .find((el) => within(el).queryAllByText(node, { exact: true }).length > 0);
  if (!item) throw new Error(`the structure chart draws no node named ${node}`);
  fireEvent.click(within(chartCard(item)).getByRole("button", { name, hidden: true }));
}

// A dialog the builder opens is a component the model never sees, so the one
// thing a suite can hold is that the screen hands in the dialog each action
// names rather than another one.
test("each structural action opens its own dialog", () => {
  expect(builderSurfaces.add).toBe(AddNodeDialog);
  expect(builderSurfaces.move).toBe(MoveDialog);
  expect(builderSurfaces.remove).toBe(DeleteDialog);
  expect(builderSurfaces.changeKind).toBe(ChangeKindDialog);
});

test("the toolbar's Delete opens the delete dialog for the selected seat", async () => {
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  // SELECTED BY A PRESS, not by `seat=` in the address, which is a request
  // to open the seat's editor now (`editWiring.test.tsx`).
  fireEvent.click(await tableRow(settle, "CEO"));
  pressInToolbar("CEO");
  const menu = screen.getByRole("menu", { name: "Actions for CEO" });
  fireEvent.click(within(menu).getByRole("menuitem", { name: /^Delete/ }));
  await settle();
  // An ALERT dialog: a removal interrupts to ask something a save makes
  // permanent, so the whole surface is announced rather than its name alone.
  expect(screen.getByRole("alertdialog", { name: "Delete CEO" })).toBeDefined();
});

/** A menu's entries as a person meets them: the label and the icon drawn beside it. */
const entries = (menu: HTMLElement) =>
  within(menu)
    .getAllByRole("menuitem")
    .map((item) => ({
      label: menuEntryLabel(item),
      icon: item.querySelector("svg path")?.getAttribute("d") ?? null,
      disabled: item.getAttribute("aria-disabled") === "true",
    }));

/*
 * THE TOOLBAR IS WHERE A KEYBOARD REACHES A CARD'S ACTIONS, since a tree item
 * may hold no tab stop of its own. It once built its own list, which put the
 * entries in another order, left out Edit reports and offered Open seat for a
 * seat that existed only in the draft.
 *
 * IT CARRIES THE WHOLE LIST, because it draws none of the controls a card and
 * a row draw for themselves: the toolbar has no pencil and no trash of its
 * own, so every action of the node is in the menu it opens.
 */
test("the toolbar offers the selected seat's whole list: its entries, order and icons", async () => {
  const { view, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=visualization",
  });
  await checked();
  // A press on the card selects it.
  fireEvent.click(
    view.container.querySelector<HTMLElement>('[role="treeitem"][data-tree-id="seat:ceo"]')!,
  );
  pressInToolbar("CEO");
  const toolbar = entries(screen.getByRole("menu", { name: "Actions for CEO" }));
  const labels = toolbar.map((e) => e.label);
  expect(labels).toContain("Edit reports");
  // The three a card and a row draw as controls of their own, which only the
  // toolbar has to put in words.
  expect(labels).toContain("Edit");
  expect(labels).toContain("Delete");
});

/*
 * ONE NODE, ONE MENU, WHICHEVER VIEW IS DRAWING IT. A card on the chart and a
 * row in the table draw the same add pill, the same pencil and the same trash,
 * so both carry the same remainder (`nodeActions.surfaceMenu`). Written out
 * separately they were two menus on one node: the card kept an Edit and a
 * Delete it had drawn two inches to the left and dropped the Move up and Move
 * down the row beside it offered.
 */
test("a node's card menu and its row menu are the same list", async () => {
  // NO `seat=` IN THE ADDRESS: a link naming a seat opens its editor now
  // (`editWiring.test.tsx`), and this case is about the menus under it.
  const { view, settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=visualization",
  });
  await checked();
  const card = view.container.querySelector<HTMLElement>(
    '[role="treeitem"][data-tree-id="seat:ceo"]',
  )!;
  card.focus();
  fireEvent.keyDown(card, { key: "ContextMenu" });
  const onCard = entries(screen.getByRole("menu", { name: "Actions for CEO" }));
  fireEvent.keyDown(screen.getByRole("menu", { name: "Actions for CEO" }), { key: "Escape" });
  await settle();
  expect(screen.queryByRole("menu")).toBeNull();
  // Neither of the two the card draws beside it.
  expect(onCard.map((e) => e.label)).not.toContain("Edit");
  expect(onCard.map((e) => e.label)).not.toContain("Delete");

  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  await settle();
  const row = view.container.querySelector<HTMLElement>('[role="row"][data-tree-id="seat:ceo"]')!;
  fireEvent.click(within(row).getByRole("button", { name: "Actions for CEO" }));
  const onRow = entries(screen.getByRole("menu", { name: "Actions for CEO" }));
  expect(onCard).toEqual(onRow);
});

/*
 * ADDED FROM THE TABLE, which is the view that still asks in a DIALOG: the
 * structure chart draws the same form in the chart instead (`Builder.tsx`
 * decides which, and `CanvasView.test.tsx` holds the chart's half). Bound
 * here, so the builder really does hand a table-view add to `surfaces.add`.
 */
test("the toolbar offers no screen for a seat that exists only in the draft", async () => {
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    keys: countingKeys("builder"),
    hash: "#/agents/edit?view=table",
  });
  await checked();
  const dialog = await askToAdd(settle, "Add agent seat");
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Analyst" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add agent seat" }));
  await settle();
  expect(screen.queryByRole("dialog")).toBeNull();
  // Minted from the builder's one key source, which a suite injects.
  expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"new:builder1"');
  // Pressing a row selects the node it holds.
  fireEvent.click(await tableRow(settle, "Analyst"));
  pressInToolbar("Analyst");
  const menu = screen.getByRole("menu", { name: "Actions for Analyst" });
  const labels = entries(menu).map((e) => e.label);
  expect(labels).toContain("Edit reports");
  expect(labels).not.toContain("Open seat");
});

// The builder hands the part an action names on to the editor it opens: Edit
// reports is about whom the seat manages, so that is where the form starts.
test("Edit reports opens the seat's editor at Manages", async () => {
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  fireEvent.click(await tableRow(settle, "CEO"));
  pressInToolbar("CEO");
  const menu = screen.getByRole("menu", { name: "Actions for CEO" });
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Edit reports" }));
  await settle();
  const editor = screen.getByRole("dialog", { name: "Edit CEO" });
  expect(document.activeElement).toBe(within(editor).getByRole("combobox", { name: /^Manages/ }));
});

/** Opens the CEO's editor from its table row and types a goal into it. */
async function typeIntoTheEditor(settle: Settle): Promise<HTMLElement> {
  await openTheEditorFromTheTable(settle);
  await settle();
  const editor = screen.getByRole("dialog", { name: "Edit CEO" });
  fireEvent.change(within(editor).getByLabelText(/^Goal/), { target: { value: "Grow" } });
  return editor;
}

// BACK HAS ALREADY HAPPENED by the time the page hears of it, and it used to
// take the builder and the editor with it, typed changes and all.
test("Back off the builder over a changed editor asks first, and keeping the changes keeps the page", async () => {
  // Reached from the org chart, which Back goes back to: the chart beside the
  // builder is as much a departure as any other screen.
  const { checked, navigate, settle } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents",
  });
  const onTable = "#/agents/edit?view=table";
  await navigate(() => {
    location.hash = onTable;
  }, onTable);
  await checked();
  const editor = await typeIntoTheEditor(settle);

  // Held, so the browser is put back on the table once it has gone.
  await navigate(() => history.back(), onTable);
  const asked = screen.getByRole("alertdialog", { name: "Discard your changes?" });
  expect(asked.textContent).toContain("you are leaving the builder");
  fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// A MOVE WITHIN THE BUILDER LOSES NOTHING. The Builder keeps the editor open,
// form and all, when Back only turns the table back into the visualization, so
// asking first would be a question about nothing, worded as a departure.
test("Back within the builder asks nothing, and the editor keeps what was typed", async () => {
  const { checked, navigate, settle } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
  });
  await checked();
  const onCanvas = location.hash;
  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  const editor = await typeIntoTheEditor(settle);

  await navigate(() => history.back(), onCanvas);
  expect(screen.getByRole("tree", { name: "Structure chart" })).toBeDefined();
  expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// A CHECK'S ANSWER MOVES NOTHING AN EDITOR HOLDS. A node is keyed by the
// identity the chart serves, so the answer that arrives while an editor is
// open finds the same key there, and the editor its node.
test("an editor opened before the first check answers keeps its node", async () => {
  const engine = new Engine(company());
  let answer: () => void = () => {};
  let reads = 0;
  engine.script = (r, e) =>
    r.method === "GET" && r.path === "/chart" && ++reads === 2
      ? new Promise<Response>((resolve) => {
          answer = () => resolve(e.answer(r));
        })
      : null;
  const { checked } = mountBuilder({
    engine,
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  // The check's read is HELD, so the builder is waited for by the request
  // rather than settled: settling would wait for the answer this case holds
  // back.
  await engine.reached(() => engine.chartReads().length === 2);
  fireEvent.click(await tableRowWhileChecking("CEO"));
  const editor = screen.getByRole("dialog", { name: "Edit CEO" });

  engine.script = () => null;
  act(() => answer());
  await checked();
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  // The same editor, never one mounted again on the answer: a remount played
  // the drawer's entrance a second time and threw away where focus was.
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
});

// ONE FORM PER OPENING: the builder mounts a new editor for every one, so a node
// opened after another starts from its own data, never from the last form.
test("every opening of the editor builds its own node's form", async () => {
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  await typeIntoTheEditor(settle);
  const dev = await tableRow(settle, "Dev");
  fireEvent.click(within(dev).getByRole("button", { name: "Edit Dev" }));
  await settle();
  const next = screen.getByRole("dialog", { name: "Edit Dev" });
  expect((within(next).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("");
});

// A COLLEAGUE'S SAVE IS NO REASON TO LOSE A FORM. A builder with no work in
// its draft stands on the newer company through the one rebase every update
// takes, which carries every key an open editor holds across it; a plain load
// would start the builder over and the editor would come back empty.
test("a colleague's save leaves an open editor and its typed form, which then applies", async () => {
  const engine = new Engine(company());
  const { store, checked } = mountBuilder({
    engine,
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table&seat=ceo",
  });
  await checked();
  // Arrived on a link naming the CEO, so the CEO's editor is what opened.
  const editor = screen.getByRole("dialog", { name: "Edit CEO" });
  fireEvent.change(within(editor).getByLabelText(/^Goal/), { target: { value: "Grow" } });
  engine.seats.find((s) => s.handle === "designer")!.goal = "Design things";
  const reads = engine.chartReads().length;
  act(() => store.applyOrg(engine.orgPush()));

  // Stood on the newer chart — the check's read, the update's, and the check
  // that follows it — and clean there.
  await checked();
  expect(engine.chartReads().length).toBeGreaterThanOrEqual(reads + 3);
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
  // The selection named in the URL is still the CEO's.
  expect(location.hash).toContain("seat=ceo");

  fireEvent.click(within(editor).getByRole("button", { name: "Apply" }));
  await checked();
  pressInToolbar("Review and save");
  const review = screen.getByRole("dialog", { name: "Review and save" });
  // The typed form is the draft's only change, and the colleague's is the base.
  expect(within(review).getByText("Edits CEO: goal.")).toBeDefined();
  expect(within(review).queryByText(/Designer/)).toBeNull();
});

/*
 * ONE NODE READS THE SAME WAY ON BOTH VIEWS.
 *
 * The chart and the table are two arrangements of one draft, drawn from one
 * module each for the words (`nodeMarks`) and the actions (`nodeActions`).
 * Written out twice they drifted in exactly the places two
 * people would not think to compare: a unit's own type read "Department" on
 * the card and "department" on the row, and the mark beside it came from one
 * list on the chart and another on the row it named.
 */
test("a unit says the same word and wears the same mark on the chart and in the table", async () => {
  const { view, settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=visualization",
  });
  await checked();
  const node = orgNodeParts();
  const table = orgTableParts();
  const card = view.container.querySelector<HTMLElement>('[data-tree-id="unit:engineering"]')!;
  const onChart = {
    // The caption under the name, which is where both surfaces write the type.
    caption: drawnPart(card, node.caption)?.textContent,
    mark: drawnPart(card, node.icon)?.querySelector("path")?.getAttribute("d"),
  };

  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  await settle();
  const row = view.container.querySelector<HTMLElement>(
    '[role="row"][data-tree-id="unit:engineering"]',
  )!;
  const onRow = {
    caption: drawnPart(row, table.caption)?.textContent,
    mark: drawnPart(row, table.icon)?.querySelector("path")?.getAttribute("d"),
  };

  expect(onChart.caption).toBe("Department");
  expect(onRow.caption).toBe(onChart.caption);
  expect(onRow.mark).toBe(onChart.mark);
});

/*
 * BOTH SHELLS OPEN ON THE SAME CONTROL, and it is the kind: the question the
 * add is asking, and the one everything else in the form follows from. They
 * did not, and the reason they did not is the kind of thing only a case
 * mounting the real builder can see: the name carried `autoFocus`, the dialog is
 * not hidden on the tick it mounts so the field took the focus, and the chart
 * ghost IS hidden until the layout places it, so the same request was refused
 * and the chart's own answer landed instead. One set of fields opening on two
 * different controls is worse than either answer.
 *
 * WHAT EACH HALF ACTUALLY HOLDS, because they are not the same claim and the
 * difference decides what this case can catch.
 *
 * The dialog half asserts WHO HOLDS THE FOCUS, so it fails if anything in the
 * shared fields asks for focus again: restoring `autoFocus` reddens it, which
 * is measured rather than assumed.
 *
 * The chart half asserts DOM ORDER, and cannot assert focus. Two things
 * compete for it on the tick a ghost opens, the chart and the control that
 * opened the composition, and jsdom commits them in an order a browser does
 * not: measured here, the pill that opened it wins and the form does not hold
 * focus at all, however the chart is settled first. `CanvasView.test.tsx`
 * holds the chart's own focus answer, where that ordering is driven
 * deliberately.
 *
 * So restoring `autoFocus` reddens the dialog half ALONE, and that is not a
 * hole: `autoFocus` is inert in the chart for the same reason the defect
 * existed, since a hidden card refuses the request in a browser too. What this
 * case guards across both shells is the thing that can silently disagree,
 * which is WHICH control the shared fields put first.
 */
test("an add puts the kind first in both shells, and opens on it in the dialog", async () => {
  const restore = LayoutObserver.install();
  onTeardown.push(restore);
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  const dialog = await askToAdd(settle, "Add agent seat");
  expect(document.activeElement).toBe(within(dialog).getByRole("radio", { name: "Agent seat" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await settle();
  expect(screen.queryByRole("dialog")).toBeNull();

  fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
  await settle();
  act(() => LayoutObserver.settle());
  // The Add on the company's own branch, which is where a pointer asks.
  pressOnTheChart("Acme", "Add to Acme");
  pressOnTheChart("Acme", "Add agent seat to Acme");
  act(() => LayoutObserver.settle());
  const form = screen.getByRole("group", { name: "Add to Acme" });
  /*
   * THE SAME CONTROL, asserted as the first one a keyboard reaches rather than
   * as the one holding focus. Which of the two wins the tick a ghost opens is
   * settled between the chart and the control that opened it, and jsdom
   * commits them in an order a browser does not: `CanvasView.test.tsx` holds
   * the chart's own answer where that ordering is controlled, and this case
   * holds the thing the two shells must agree about, which is WHICH control it
   * is.
   */
  expect(focusables(form)[0]).toBe(within(form).getByRole("radio", { name: "Agent seat" }));
});

/*
 * AN ADD WHOSE PARENT LEAVES THE DRAFT FALLS BACK TO THE DIALOG.
 *
 * The structure chart draws an add in the ghost of the node about to exist,
 * hanging off its parent's own branch. A unit can leave the draft under an
 * open add, because an undo takes no dialog and so reaches the toolbar while
 * the ghost is drawn, and a ghost on a branch that is not there is not
 * drawable. It falls back to the dialog, which is where the refusal saying so
 * has always been written: a chart that simply stopped drawing the form would
 * take it away with no word about why.
 *
 * Mounted with the real surfaces, because the whole of this is the builder and
 * the chart agreeing about where one add is asked.
 */
test("an add falls back to the dialog when its parent leaves the draft", async () => {
  /*
   * MEASURED, unlike every other case in this file. jsdom has no layout, so a
   * chart nothing measures draws every card at `visibility: hidden` and the
   * whole chart is then outside the accessibility tree: the cases above reach
   * their treeitems with `querySelector` for exactly that reason. This one is
   * about a control a reader points at, so the chart has to be laid out.
   */
  const restore = LayoutObserver.install();
  onTeardown.push(restore);
  const { settle, checked } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/agents/edit?view=table",
  });
  await checked();
  // A unit of the draft alone, so an undo can take it away again.
  const dialog = await askToAdd(settle, "Add unit");
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Tooling" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add unit" }));
  await settle();
  expect(screen.queryByRole("dialog")).toBeNull();

  fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
  await settle();
  act(() => LayoutObserver.settle());
  // The Add on the new unit's own branch, which is where a pointer asks. It
  // is pointer-only, so it is hidden from assistive technology and the query
  // has to say so.
  pressOnTheChart("Tooling", "Add to Tooling");
  pressOnTheChart("Tooling", "Add agent seat to Tooling");
  // The ghost is a card of the chart, so it is drawn once it is measured.
  act(() => LayoutObserver.settle());
  // Drawn IN the chart, with no dialog over it.
  const form = screen.getByRole("group", { name: "Add to Tooling" });
  expect(screen.queryByRole("dialog")).toBeNull();
  // The form a reader types into, with the fields the dialog would have had.
  expect(within(form).getByLabelText("Name")).toBeDefined();

  pressInToolbar("Undo");
  await settle();
  const fallback = screen.getByRole("dialog");
  expect(
    within(fallback).getByText(
      "That unit is no longer in the draft, so nothing can be added to it.",
    ),
  ).toBeDefined();
});
