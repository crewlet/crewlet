/**
 * The Org screen's Builder lens is assembled from the real views and dialogs.
 *
 * Every other Builder suite stands a view in with a fake, so nothing there
 * notices a lens whose visualization or table was never bound: it rendered "not
 * part of this build" for both until this binding existed. These tests mount
 * the lens with the surfaces the screen hands it and hold it to drawing the
 * canvas's tree, the table's rows and the node editor.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { ChangeKindDialog } from "./ChangeKindDialog.tsx";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { countingKeys } from "./model/testkit.ts";
import { builderSurfaces } from "./surfaces.ts";
import { company, Engine, mountBuilder } from "./testkit.tsx";
import { LayoutObserver } from "./viewTestkit.tsx";
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
  mountBuilder({ engine: new Engine(company()), surfaces: builderSurfaces });
  expect(await screen.findByRole("tree", { name: "Structure chart" })).toBeDefined();
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("tab", { name: "Reporting" }));
  expect(await screen.findByRole("tree", { name: "Reporting chart" })).toBeDefined();
});

test("the table view draws a row per node, and a row's Edit opens the node editor", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table",
  });
  await screen.findByText("No problems");
  await openTheEditorFromTheTable();
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
});

/**
 * The table row whose NAME cell says `name`.
 *
 * The name cell rather than the row, because a unit's lead is a seat's name in
 * another cell: "Dev" matches Engineering's row too, on the column that says
 * who leads it.
 */
async function tableRow(name: string): Promise<HTMLElement> {
  const rows = await screen.findAllByRole("row");
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
async function openTheEditorFromTheTable(): Promise<void> {
  const row = await tableRow("CEO");
  fireEvent.click(within(row).getByRole("button", { name: "Edit CEO" }));
}

// A dialog the lens opens is a component the model never sees, so the one
// thing a suite can hold is that the screen hands in the dialog each action
// names rather than another one.
test("each structural action opens its own dialog", () => {
  expect(builderSurfaces.add).toBe(AddNodeDialog);
  expect(builderSurfaces.move).toBe(MoveDialog);
  expect(builderSurfaces.remove).toBe(DeleteDialog);
  expect(builderSurfaces.changeKind).toBe(ChangeKindDialog);
});

test("the toolbar's Delete opens the delete dialog for the selected seat", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table&seat=ceo",
  });
  await screen.findByText("No problems");
  fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
  const menu = await screen.findByRole("menu", { name: "Actions for CEO" });
  fireEvent.click(within(menu).getByRole("menuitem", { name: /^Delete/ }));
  // An ALERT dialog: a removal interrupts to ask something a save makes
  // permanent, so the whole surface is announced rather than its name alone.
  await waitFor(() =>
    expect(screen.getByRole("alertdialog", { name: "Delete CEO" })).toBeDefined(),
  );
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
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=visualization&seat=ceo",
  });
  await screen.findByText("No problems");
  fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
  const toolbar = entries(await screen.findByRole("menu", { name: "Actions for CEO" }));
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
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=visualization&seat=ceo",
  });
  await screen.findByText("No problems");
  const card = view.container.querySelector<HTMLElement>(
    '[role="treeitem"][data-tree-id="seat:ceo"]',
  )!;
  card.focus();
  fireEvent.keyDown(card, { key: "ContextMenu" });
  const onCard = entries(await screen.findByRole("menu", { name: "Actions for CEO" }));
  fireEvent.keyDown(screen.getByRole("menu", { name: "Actions for CEO" }), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
  // Neither of the two the card draws beside it.
  expect(onCard.map((e) => e.label)).not.toContain("Edit");
  expect(onCard.map((e) => e.label)).not.toContain("Delete");

  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  fireEvent.click(await screen.findByRole("button", { name: "Actions for CEO" }));
  const onRow = entries(await screen.findByRole("menu", { name: "Actions for CEO" }));
  expect(onCard).toEqual(onRow);
});

/*
 * ADDED FROM THE TABLE, which is the view that still asks in a DIALOG: the
 * structure chart draws the same form in the chart instead (`Builder.tsx`
 * decides which, and `CanvasView.test.tsx` holds the chart's half). Bound
 * here, so the lens really does hand a table-view add to `surfaces.add`.
 */
test("the toolbar offers no screen for a seat that exists only in the draft", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    keys: countingKeys("lens"),
    hash: "#/org?lens=builder&view=table",
  });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to the company" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Analyst" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add agent seat" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  // Minted from the lens's one key source, which a suite injects.
  await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"new:lens1"'));
  // Pressing a row selects the node it holds.
  fireEvent.click(await tableRow("Analyst"));
  fireEvent.click(await screen.findByRole("button", { name: "Analyst" }));
  const menu = await screen.findByRole("menu", { name: "Actions for Analyst" });
  const labels = entries(menu).map((e) => e.label);
  expect(labels).toContain("Edit reports");
  expect(labels).not.toContain("Open seat");
});

// The lens hands the part an action names on to the editor it opens: Edit
// reports is about whom the seat manages, so that is where the form starts.
test("Edit reports opens the seat's editor at Manages", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table&seat=ceo",
  });
  await screen.findByText("No problems");
  fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
  const menu = await screen.findByRole("menu", { name: "Actions for CEO" });
  fireEvent.click(within(menu).getByRole("menuitem", { name: "Edit reports" }));
  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });
  await waitFor(() =>
    expect(document.activeElement).toBe(within(editor).getByRole("combobox", { name: /^Manages/ })),
  );
});

/** Opens the CEO's editor from its table row and types a goal into it. */
async function typeIntoTheEditor(): Promise<HTMLElement> {
  await openTheEditorFromTheTable();
  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });
  fireEvent.change(within(editor).getByLabelText(/^Goal/), { target: { value: "Grow" } });
  return editor;
}

// BACK HAS ALREADY HAPPENED by the time the page hears of it, and it used to
// take the lens and the editor with it, typed changes and all.
test("Back off the lens over a changed editor asks first, and keeping the changes keeps the page", async () => {
  // Reached from the screen's Chart lens, which Back goes back to: another
  // lens of the same screen is as much a departure as another screen.
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=chart",
  });
  act(() => {
    location.hash = "#/org?lens=builder&view=table";
  });
  await screen.findByText("No problems");
  const onTable = location.hash;
  const editor = await typeIntoTheEditor();

  act(() => history.back());
  const asked = await screen.findByRole("alertdialog", { name: "Discard your changes?" });
  expect(asked.textContent).toContain("you are leaving the builder");
  await waitFor(() => expect(location.hash).toBe(onTable));
  fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// A MOVE WITHIN THE LENS LOSES NOTHING. The Builder keeps the editor open,
// form and all, when Back only turns the table back into the visualization, so
// asking first would be a question about nothing, worded as a departure.
test("Back within the lens asks nothing, and the editor keeps what was typed", async () => {
  mountBuilder({ engine: new Engine(company()), surfaces: builderSurfaces });
  await screen.findByText("No problems");
  const onCanvas = location.hash;
  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  const editor = await typeIntoTheEditor();

  act(() => history.back());
  await waitFor(() => expect(location.hash).toBe(onCanvas));
  await screen.findByRole("tree", { name: "Structure chart" });
  expect(screen.queryByRole("alertdialog", { name: "Discard your changes?" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// Until the first check answers, a seat that declares no handle is keyed by
// its path, and the answer re-keys it by the handle the engine gives it. An
// editor opened in between used to lose its node at that moment.
test("an editor opened before the engine described the company keeps its node", async () => {
  const engine = new Engine(company());
  let answer: () => void = () => {};
  engine.script = (r, e) =>
    r.query.get("dry_run") === "true"
      ? new Promise<Response>((resolve) => {
          answer = () => resolve(e.answer(r));
        })
      : null;
  mountBuilder({ engine, surfaces: builderSurfaces, hash: "#/org?lens=builder&view=table" });
  await waitFor(() => expect(engine.checks()).toHaveLength(1));
  await openTheEditorFromTheTable();
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();

  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });

  engine.script = () => null;
  act(() => answer());
  await screen.findByText("No problems");
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  // The same editor, never one mounted again on the answer: a remount played
  // the drawer's entrance a second time and threw away where focus was.
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
});

// ONE FORM PER OPENING: the lens mounts a new editor for every one, so a node
// opened after another starts from its own data, never from the last form.
test("every opening of the editor builds its own node's form", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table",
  });
  await screen.findByText("No problems");
  await typeIntoTheEditor();
  const dev = await tableRow("Dev");
  fireEvent.click(within(dev).getByRole("button", { name: "Edit Dev" }));
  const next = await screen.findByRole("dialog", { name: "Edit Dev" });
  expect((within(next).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("");
});

// A COLLEAGUE'S SAVE IS NO REASON TO LOSE A FORM. A lens with no work in its
// draft stands on the newer revision, and did it by reading the document
// again, which keys a seat declaring no handle by its path until the next
// check: the open editor lost its node for that moment and came back empty.
test("a colleague's save leaves an open editor and its typed form, which then applies", async () => {
  const engine = new Engine(company());
  const { store } = mountBuilder({
    engine,
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table&seat=ceo",
  });
  await screen.findByText("No problems");
  const editor = await typeIntoTheEditor();
  const next = company();
  next.roles![1]!.goal = "Design things";
  engine.document = next;
  engine.revision = "r2";
  act(() => store.applyOrg({ name: "Acme", roles: [], units: [] }));

  // Stood on the newer revision, and checked there.
  await waitFor(() =>
    expect(engine.checks().some((c) => c.headers["If-Match"] === '"r2"')).toBe(true),
  );
  await screen.findByText("No problems");
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
  // The selection named in the URL is still the CEO's.
  expect(location.hash).toContain("seat=ceo");

  fireEvent.click(within(editor).getByRole("button", { name: "Apply" }));
  await waitFor(() => {
    const last = engine.checks().at(-1)!;
    expect(last.headers["If-Match"]).toBe('"r2"');
    expect(JSON.stringify(last.body)).toContain("Grow");
  });
});

/*
 * ONE NODE READS THE SAME WAY ON BOTH VIEWS.
 *
 * The chart and the table are two arrangements of one draft, drawn from one
 * module each for the words (`nodeMarks`), the actions (`nodeActions`) and the
 * hue (`nodeTone`). Written out twice they drifted in exactly the places two
 * people would not think to compare: a unit's own type read "Department" on
 * the card and "department" on the row, and the mark beside it came from one
 * list on the chart and another on the row it named.
 */
test("a unit says the same word and wears the same mark on the chart and in the table", async () => {
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=visualization",
  });
  await screen.findByText("No problems");
  const node = orgNodeParts();
  const table = orgTableParts();
  const card = view.container.querySelector<HTMLElement>('[data-tree-id="unit:Engineering"]')!;
  const onChart = {
    // The caption under the name, which is where both surfaces write the type.
    caption: drawnPart(card, node.caption)?.textContent,
    mark: drawnPart(card, node.icon)?.querySelector("path")?.getAttribute("d"),
  };

  fireEvent.click(screen.getByRole("tab", { name: "Table" }));
  const row = await waitFor(() => {
    const found = view.container.querySelector<HTMLElement>(
      '[role="row"][data-tree-id="unit:Engineering"]',
    );
    if (!found) throw new Error("no row for the unit");
    return found;
  });
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
 * mounting the real lens can see: the name carried `autoFocus`, the dialog is
 * not hidden on the tick it mounts so the field took the focus, and the chart
 * ghost IS hidden until the layout places it, so the same request was refused
 * and the chart's own answer landed instead. One set of fields opening on two
 * different controls is worse than either answer.
 */
test("an add opens on the kind, in the dialog and in the chart alike", async () => {
  const restore = LayoutObserver.install();
  onTeardown.push(restore);
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table",
  });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to the company" });
  expect(document.activeElement).toBe(within(dialog).getByRole("radio", { name: "Agent seat" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
  await screen.findByRole("tree", { name: "Structure chart" });
  act(() => LayoutObserver.settle());
  // The Add on the company's own branch, which is where a pointer asks.
  fireEvent.click(await screen.findByRole("button", { name: "Add to Acme", hidden: true }));
  fireEvent.click(
    await screen.findByRole("button", { name: "Add agent seat to Acme", hidden: true }),
  );
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
 * Mounted with the real surfaces, because the whole of this is the lens and
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
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=table",
  });
  await screen.findByText("No problems");
  // A unit of the draft alone, so an undo can take it away again.
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add unit" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to the company" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Tooling" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add unit" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  fireEvent.click(screen.getByRole("tab", { name: "Visualization" }));
  await screen.findByRole("tree", { name: "Structure chart" });
  act(() => LayoutObserver.settle());
  // The Add on the new unit's own branch, which is where a pointer asks. It
  // is pointer-only, so it is hidden from assistive technology and the query
  // has to say so.
  fireEvent.click(screen.getByRole("button", { name: "Add to Tooling", hidden: true }));
  fireEvent.click(
    await screen.findByRole("button", { name: "Add agent seat to Tooling", hidden: true }),
  );
  // The ghost is a card of the chart, so it is drawn once it is measured.
  act(() => LayoutObserver.settle());
  // Drawn IN the chart, with no dialog over it.
  const form = screen.getByRole("group", { name: "Add to Tooling" });
  expect(screen.queryByRole("dialog")).toBeNull();
  // The form a reader types into, with the fields the dialog would have had.
  expect(within(form).getByLabelText("Name")).toBeDefined();

  fireEvent.click(screen.getByRole("button", { name: "Undo" }));
  const fallback = await screen.findByRole("dialog");
  expect(
    within(fallback).getByText(
      "That unit is no longer in the draft, so nothing can be added to it.",
    ),
  ).toBeDefined();
});
