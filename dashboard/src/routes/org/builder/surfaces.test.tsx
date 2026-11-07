/**
 * Structure's builder is assembled from the real views and dialogs.
 *
 * Every other Builder suite stands a view in with a fake, so nothing there
 * notices a builder whose visualization or table was never bound: it rendered "not
 * part of this build" for both until this binding existed. These tests mount
 * the builder with the surfaces the screen hands it and hold it to drawing the
 * canvas's tree, the table's rows and the node editor.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { countingKeys } from "./model/testkit.ts";
import { builderSurfaces } from "./surfaces.ts";
import { company, Engine, json, mountBuilder, openTab } from "./testkit.tsx";
import { LayoutObserver } from "./viewTestkit.tsx";
import { focusables } from "@crewlethq/ui";
import { drawnPart, menuEntryLabel, orgNodeParts, orgTableParts } from "~/testing.tsx";
import { withDerived } from "~/test/org.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { AgentRow, OrgProjection } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { seatPath } from "~/lib/seats.ts";
import { LiveStructure } from "../structureLive.tsx";

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
  await screen.findByText("Configuration OK");
  fireEvent.click(screen.getByRole("switch", { name: "Reporting view" }));
  expect(await screen.findByRole("tree", { name: "Reporting chart" })).toBeDefined();
  expect(location.hash).toContain("chart=reporting");
});

test("the table view draws a row per node, and a row's Edit opens the node editor", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  await openTheEditorFromTheTable();
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
});

/**
 * The table row whose NAME cell says `name`.
 *
 * The name cell rather than the row, because a unit's lead is shown by its
 * seat's name in another cell: "Dev" matches Engineering's row too, on the
 * column that says who leads it.
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

// A dialog the builder opens is a component the model never sees, so the one
// thing a suite can hold is that the screen hands in the dialog each action
// names rather than another one.
test("each structural action opens its own dialog", () => {
  expect(builderSurfaces.add).toBe(AddNodeDialog);
  expect(builderSurfaces.move).toBe(MoveDialog);
  expect(builderSurfaces.remove).toBe(DeleteDialog);
});

/** Presses a node's card on the chart, which selects it. */
function pressCard(container: HTMLElement, key: string): void {
  fireEvent.click(
    container.querySelector<HTMLElement>(`[role="treeitem"][data-tree-id="${key}"]`)!,
  );
}

test("the toolbar's Delete opens the delete dialog for the selected seat", async () => {
  // THE TOOLBAR MIRRORS THE SELECTION ON THE CHART, where a card holds no
  // control a keyboard reaches; a table row carries its own.
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org",
  });
  await screen.findByText("Configuration OK");
  pressCard(view.container, "seat:ceo");
  fireEvent.click(await screen.findByRole("button", { name: "Actions for CEO, selected" }));
  const menu = await screen.findByRole("menu", { name: "Actions for CEO, selected" });
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
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org",
  });
  await screen.findByText("Configuration OK");
  // A press on the card selects it.
  fireEvent.click(
    view.container.querySelector<HTMLElement>('[role="treeitem"][data-tree-id="seat:ceo"]')!,
  );
  fireEvent.click(await screen.findByRole("button", { name: "Actions for CEO, selected" }));
  const toolbar = entries(await screen.findByRole("menu", { name: "Actions for CEO, selected" }));
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
    hash: "#/company/org",
  });
  await screen.findByText("Configuration OK");
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

  await openTab("table");
  fireEvent.click(within(await tableRow("CEO")).getByRole("button", { name: "Actions for CEO" }));
  const onRow = entries(await screen.findByRole("menu", { name: "Actions for CEO" }));
  expect(onCard).toEqual(onRow);
});

/*
 * ADDED FROM THE TABLE, which is the view that still asks in a DIALOG: the
 * structure chart draws the same form in the chart instead (`Builder.tsx`
 * decides which, and `CanvasView.test.tsx` holds the chart's half). Bound
 * here, so the builder really does hand a table-view add to `surfaces.add`.
 * Its actions are then the chart's toolbar's, which mirrors the selection.
 */
test("the toolbar offers no screen for a seat that exists only in the draft", async () => {
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    keys: countingKeys("builder"),
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to Acme" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Analyst" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add agent seat" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  // Minted from the builder's one key source, which a suite injects.
  await waitFor(() =>
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"new:builder1"'),
  );
  // Its card, pressed on the chart, selects it.
  await openTab("chart");
  await screen.findByRole("tree", { name: "Structure chart" });
  pressCard(view.container, "new:builder1");
  fireEvent.click(await screen.findByRole("button", { name: "Actions for Analyst, selected" }));
  const menu = await screen.findByRole("menu", { name: "Actions for Analyst, selected" });
  const labels = entries(menu).map((e) => e.label);
  expect(labels).toContain("Edit reports");
  expect(labels).not.toContain("Open seat");
});

// The builder hands the part an action names on to the editor it opens: Edit
// reports is about whom the seat manages, so that is where the form starts.
test("Edit reports opens the seat's editor at Manages", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  fireEvent.click(within(await tableRow("CEO")).getByRole("button", { name: "Actions for CEO" }));
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
// take the builder and the editor with it, typed changes and all.
test("Back off the builder over a changed editor asks first, and keeping the changes keeps the page", async () => {
  // Reached from a seat's page, which Back goes back to: a page beside the
  // builder is as much a departure as any other screen. (This harness draws
  // the builder under any address; the app draws the seat's page there.)
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/seats/ceo",
  });
  act(() => {
    location.hash = "#/company/org/table";
  });
  await screen.findByText("Configuration OK");
  const onTable = location.hash;
  const editor = await typeIntoTheEditor();

  act(() => history.back());
  const asked = await screen.findByRole("alertdialog", { name: "Discard your changes?" });
  expect(asked.textContent).toContain("Your edits to CEO aren’t in the draft yet.");
  await waitFor(() => expect(location.hash).toBe(onTable));
  fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// A MOVE WITHIN THE BUILDER LOSES NOTHING. The Builder keeps the editor open,
// form and all, when Back only turns the Table tab back into the Org chart, so
// asking first would be a question about nothing, worded as a departure.
test("Back within the builder asks nothing, and the editor keeps what was typed", async () => {
  mountBuilder({ engine: new Engine(company()), surfaces: builderSurfaces });
  await screen.findByText("Configuration OK");
  const onCanvas = location.hash;
  await openTab("table");
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
  mountBuilder({ engine, surfaces: builderSurfaces, hash: "#/company/org/table" });
  await waitFor(() => expect(engine.checks()).toHaveLength(1));
  await openTheEditorFromTheTable();
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();

  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });

  engine.script = () => null;
  act(() => answer());
  await screen.findByText("Configuration OK");
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  // The same editor, never one mounted again on the answer: a remount played
  // the drawer's entrance a second time and threw away where focus was.
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
});

// ONE FORM PER OPENING: the builder mounts a new editor for every one, so a node
// opened after another starts from its own data, never from the last form.
test("every opening of the editor builds its own node's form", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  await typeIntoTheEditor();
  const dev = await tableRow("Dev");
  fireEvent.click(within(dev).getByRole("button", { name: "Edit Dev" }));
  const next = await screen.findByRole("dialog", { name: "Edit Dev" });
  expect((within(next).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("");
});

// A COLLEAGUE'S SAVE IS NO REASON TO LOSE A FORM. A builder with no work in its
// draft stands on the newer revision, and did it by reading the document
// again, which keys a seat declaring no handle by its path until the next
// check: the open editor lost its node for that moment and came back empty.
test("a colleague's save leaves an open editor and its typed form, which then applies", async () => {
  const engine = new Engine(company());
  const { store, checkChange } = mountBuilder({
    engine,
    org: { name: "Acme", roles: [], units: [] },
    surfaces: builderSurfaces,
    hash: "#/company/org/table?seat=ceo&editor=open",
  });
  await screen.findByText("Configuration OK");
  // Arrived on a link asking for the CEO's editor, so that is what opened.
  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });
  fireEvent.change(within(editor).getByLabelText(/^Goal/), { target: { value: "Grow" } });
  const next = company();
  next.roles![1]!.goal = "Design things";
  engine.document = next;
  engine.revision = "r2";
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [], units: [] })));

  // Stood on the newer revision, and checked there.
  await waitFor(() =>
    expect(engine.checks().some((c) => c.headers["If-Match"] === '"r2"')).toBe(true),
  );
  await screen.findByText("Configuration OK");
  expect(screen.queryByText("This node is no longer in the draft")).toBeNull();
  expect(screen.getByRole("dialog", { name: "Edit CEO" })).toBe(editor);
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
  // The selection named in the URL is still the CEO's.
  expect(location.hash).toContain("seat=ceo");

  fireEvent.click(within(editor).getByRole("button", { name: "Apply" }));
  await checkChange();
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
 * module each for the words (`nodeMarks`) and the actions (`nodeActions`).
 * Written out twice they drifted in exactly the places two people would not
 * think to compare: a unit's own type came from two functions, and the mark
 * beside it from one list on the chart and another on the row it named. The
 * card writes the type as its caption; the row writes it in the table's
 * lower-case caption, with the unit's size after it.
 */
test("a unit says the same type and wears the same mark on the chart and in the table", async () => {
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org",
  });
  await screen.findByText("Configuration OK");
  const node = orgNodeParts();
  const table = orgTableParts();
  const card = view.container.querySelector<HTMLElement>('[data-tree-id="unit:engineering"]')!;
  const onChart = {
    // The caption under the name, which is where both surfaces write the type.
    caption: drawnPart(card, node.caption)?.textContent,
    mark: drawnPart(card, node.icon)?.querySelector("path")?.getAttribute("d"),
  };

  await openTab("table");
  const row = await waitFor(() => {
    const found = view.container.querySelector<HTMLElement>(
      '[role="row"][data-tree-id="unit:engineering"]',
    );
    if (!found) throw new Error("no row for the unit");
    return found;
  });
  const onRow = {
    caption: drawnPart(row, table.caption)?.textContent,
    mark: drawnPart(row, table.icon)?.querySelector("path")?.getAttribute("d"),
  };

  expect(onChart.caption).toBe("Department · 1 seat");
  expect(onRow.caption).toBe(onChart.caption!.toLowerCase());
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
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to Acme" });
  expect(document.activeElement).toBe(within(dialog).getByRole("radio", { name: "Agent seat" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  await openTab("chart");
  await screen.findByRole("tree", { name: "Structure chart" });
  act(() => LayoutObserver.settle());
  // The Add on the company's own branch, which is where a pointer asks.
  fireEvent.click(await screen.findByRole("button", { name: "Add to Acme", hidden: true }));
  fireEvent.click(
    await screen.findByRole("button", { name: "Add agent seat to Acme", hidden: true }),
  );
  act(() => LayoutObserver.settle());
  const form = screen.getByRole("dialog", { name: "Add to Acme" });
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
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });
  await screen.findByText("Configuration OK");
  // A unit of the draft alone, so an undo can take it away again.
  fireEvent.click(screen.getByRole("button", { name: "Add to the organization" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add unit" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to Acme" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Tooling" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add unit" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  await openTab("chart");
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
  // Opened from a ghost IN the chart, with no modal over it.
  expect(screen.getByRole("group", { name: "Add to Tooling" })).toBeDefined();
  const form = screen.getByRole("dialog", { name: "Add to Tooling" });
  expect(form.getAttribute("aria-modal")).toBe("false");
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

/**
 * The org snapshot of [company] as every signed-in reader holds it, with the
 * engine's derivation, and one difference that matters: Designer is a root
 * seat the engine moved into Engineering by its `unit:` reference. The
 * projection writes it at the root, where it was declared, and only the
 * derivation's membership says where it sits.
 */
const READ_ORG: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo", kind: "agent" },
    { name: "Designer", handle: "designer", kind: "agent" },
  ],
  units: [
    {
      id: "engineering",
      name: "Engineering",
      type: "department",
      lead: "dev",
      roles: [{ name: "Dev", handle: "dev", kind: "agent" }],
    },
  ],
  derived: {
    seats: [
      {
        handle: "ceo",
        name: "CEO",
        kind: "agent",
        manager: "",
        managers: [],
        reports: ["dev"],
        auto_reports: [],
        onboarding_chain: [],
      },
      {
        handle: "designer",
        name: "Designer",
        kind: "agent",
        manager: "dev",
        managers: ["dev"],
        reports: [],
        auto_reports: ["designer"],
        onboarding_chain: [],
      },
      {
        handle: "dev",
        name: "Dev",
        kind: "agent",
        manager: "ceo",
        managers: ["ceo"],
        reports: ["designer"],
        auto_reports: ["designer"],
        onboarding_chain: [],
      },
    ],
    units: [
      {
        id: "engineering",
        name: "Engineering",
        type: "department",
        lead: "dev",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["dev", "designer"],
      },
    ],
  },
} as OrgProjection;

/** Somebody signed in who holds no Admin and leads no unit. */
const READER = { login: "reader", owner: "reader", grants: [] };

/** The reason a held control says after its name, off the kit's described-by. */
const reasonOf = (control: HTMLElement): string =>
  (control.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ")
    .trim();

/*
 * A READER THE CONFIGURATION IS GUARDED FROM READS THE ORG ALL THE SAME, on
 * both tabs, from the projection every signed-in reader holds: Structure is
 * the one page the org is read on, and a refusal where the chart used to be
 * told such a reader nothing. Every edit is drawn and held with the gate's
 * own reason, nothing is checked (there is no configuration to check), so no
 * check pill is drawn, and MEMBERSHIP IS THE ENGINE'S: a seat the engine moved
 * into a unit by reference is drawn in that unit. Mutation: answer a refused
 * read with the guarded posture and there is no table; draw the projection's
 * roots as written and Designer is a row of the company.
 */
test("a reader refused the configuration reads the projection on both tabs, every edit held", async () => {
  const engine = new Engine(company());
  engine.script = (r) => (r.path === "/config" ? json({ error: "unauthorized" }, 403) : null);
  const { view } = mountBuilder({
    engine,
    org: READ_ORG,
    viewer: () => READER,
    surfaces: builderSurfaces,
    hash: "#/company/org/table",
  });

  // The Table, in the engine's places: Designer under Engineering, after Dev.
  const designer = await tableRow("Designer");
  expect(designer.getAttribute("aria-level")).toBe("3");
  expect((await tableRow("Engineering")).getAttribute("aria-level")).toBe("2");
  const order = [...view.container.querySelectorAll<HTMLElement>('[role="row"][data-tree-id]')].map(
    (row) => row.getAttribute("data-tree-id"),
  );
  expect(order).toEqual(["company", "seat:ceo", "unit:engineering", "seat:dev", "seat:designer"]);

  // Every edit drawn and held, saying why in the gate's own words.
  const reason = CONFIG_WRITE_REASONS.no_grant;
  await waitFor(() =>
    expect(reasonOf(within(designer).getByRole("button", { name: "Edit Designer" }))).toBe(reason),
  );
  expect(
    within(designer).getByRole("button", { name: "Edit Designer" }).getAttribute("aria-disabled"),
  ).toBe("true");
  fireEvent.click(within(designer).getByRole("button", { name: "Edit Designer" }));
  const add = screen.getByRole("button", { name: "Add to the organization" });
  expect(add.getAttribute("aria-disabled")).toBe("true");
  expect(reasonOf(add)).toBe(reason);
  expect(screen.queryByRole("dialog")).toBeNull();

  // Nothing checked, and no pill saying how a check went.
  expect(engine.checks()).toHaveLength(0);
  expect(screen.queryByText("Configuration OK")).toBeNull();
  expect(screen.queryByText("Checking")).toBeNull();

  // The Org chart draws the same places, its edits held the same way.
  await openTab("chart");
  await screen.findByRole("tree", { name: "Structure chart" });
  const card = view.container.querySelector<HTMLElement>(
    '[role="treeitem"][data-tree-id="seat:designer"]',
  )!;
  expect(card.getAttribute("aria-level")).toBe("3");
  // By its label: a chart nothing has measured draws its cards unseen, which
  // leaves their controls with no accessible name to find them by.
  const edit = view.container.querySelector<HTMLElement>('button[aria-label="Edit Designer"]')!;
  expect(edit.getAttribute("aria-disabled")).toBe("true");
  expect(reasonOf(edit)).toBe(reason);
  expect(screen.queryByText("Configuration OK")).toBeNull();
});

/*
 * THE RUNNING COMPANY IS ON BOTH TABS where Structure provides its live
 * reading (`structureLive.tsx`): a seat's card says its state before its kind
 * and handle, and on the Table a seat's Status is its state, Reports to is the
 * seat itself (a link to its page), and a box before each name picks agents
 * for the selection bar. A builder with no running company draws none of it,
 * as every other case here shows. Mutation: drop the card's live caption and
 * it reads "agent · @dev" alone; draw Reports to as words and there is no
 * link.
 */
test("with the running company, a card says its seat's state and the Table links who a team reports to", async () => {
  const org = withDerived({
    name: "Acme",
    roles: [
      { name: "CEO", handle: "ceo" },
      { name: "Designer", handle: "designer" },
    ],
    units: [
      {
        id: "engineering",
        name: "Engineering",
        type: "department",
        lead: "dev",
        roles: [{ name: "Dev", handle: "dev" }],
      },
    ],
  });
  const working = { id: "dev", agent_id: "dev", role: "Dev", handle: "dev", activity: "working" };
  const { view } = mountBuilder({
    engine: new Engine(company()),
    org,
    agents: [working as AgentRow],
    surfaces: builderSurfaces,
    wrap: (tree) => <LiveStructure>{tree}</LiveStructure>,
  });
  await screen.findByText("Configuration OK");
  const card = view.container.querySelector<HTMLElement>(
    '[role="treeitem"][data-tree-id="seat:dev"]',
  )!;
  // THE STATE IS A DOT ON THE MARK, its word read with the node and in its title; the
  // caption says the kind and the handle alone, so no state changes the card's width.
  expect(card.querySelector(".crewlet-org-label__caption")?.textContent).toBe("agent · @dev");
  expect(card.querySelector(".crewlet-org-label__status .crewlet-status-dot")).not.toBeNull();
  expect(card.textContent).toContain("Working");
  expect(card.title).toContain("Working");

  await openTab("table");
  const dev = await tableRow("Dev");
  const status = screen
    .getAllByRole("columnheader")
    .findIndex((head) => head.textContent?.trim() === "Status");
  expect(within(dev).getAllByRole("gridcell")[status]!.textContent).toContain("Working");
  const lead = within(await tableRow("Engineering")).getByRole("link", { name: "Dev" });
  expect(lead.getAttribute("href")).toBe(href(seatPath({ handle: "dev", name: "Dev" })));
  expect(screen.getByRole("checkbox", { name: "Select Dev" })).toBeDefined();
});
