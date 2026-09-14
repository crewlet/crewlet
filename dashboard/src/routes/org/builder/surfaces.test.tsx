/**
 * The Org screen's Builder lens is assembled from the real views and dialogs.
 *
 * Every other Builder suite stands a view in with a fake, so nothing there
 * notices a lens whose canvas or outline was never bound: it rendered "not
 * part of this build" for both until this binding existed. These tests mount
 * the lens with the surfaces the screen hands it and hold it to drawing the
 * canvas's tree, the outline's treegrid and the node editor.
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

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
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

test("the outline view draws the treegrid, and a row's Enter opens the node editor", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=outline",
  });
  const grid = await screen.findByRole("treegrid", { name: "Organization outline" });
  await screen.findByText("No problems");
  const row = within(grid)
    .getAllByRole("row")
    .find((r) => r.getAttribute("data-row-id") === "seat:ceo")!;
  fireEvent.keyDown(row, { key: "Enter" });
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
});

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
    hash: "#/org?lens=builder&view=outline&seat=ceo",
  });
  await screen.findByText("No problems");
  fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
  const menu = await screen.findByRole("menu", { name: "Actions for CEO" });
  fireEvent.click(within(menu).getByRole("menuitem", { name: /^Delete/ }));
  await waitFor(() => expect(screen.getByRole("dialog", { name: "Delete CEO" })).toBeDefined());
});

/** A menu's entries as a person meets them: the label and the icon drawn beside it. */
const entries = (menu: HTMLElement) =>
  [...menu.querySelectorAll<HTMLElement>(".menu-item")].map((item) => ({
    label: item.querySelector(".menu-item-label")!.textContent,
    icon: item.querySelector("svg path")?.getAttribute("d") ?? null,
    disabled: item.getAttribute("aria-disabled") === "true",
  }));

// THE TOOLBAR IS WHERE A KEYBOARD REACHES A CARD'S ACTIONS, since a tree item
// may hold no tab stop of its own. It once built its own list, which put the
// entries in another order, left out Edit reports and offered Open seat for a
// seat that existed only in the draft.
test("the toolbar offers the selected seat's own card menu: its entries, order and icons", async () => {
  const { view } = mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    hash: "#/org?lens=builder&view=canvas&seat=ceo",
  });
  await screen.findByText("No problems");
  fireEvent.click(await screen.findByRole("button", { name: "CEO" }));
  const toolbar = entries(await screen.findByRole("menu", { name: "Actions for CEO" }));
  fireEvent.keyDown(screen.getByRole("menu", { name: "Actions for CEO" }), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());

  const card = view.container.querySelector<HTMLElement>(
    '[role="treeitem"][data-tree-id="seat:ceo"]',
  )!;
  card.focus();
  fireEvent.keyDown(card, { key: "ContextMenu" });
  const own = view.container.querySelector<HTMLElement>(".canvas-overlay [role='menu']")!;
  expect(own.getAttribute("aria-label")).toBe("Actions for CEO");
  expect(toolbar).toEqual(entries(own));
  expect(toolbar.map((e) => e.label)).toContain("Edit reports");
});

test("the toolbar offers no screen for a seat that exists only in the draft", async () => {
  mountBuilder({
    engine: new Engine(company()),
    surfaces: builderSurfaces,
    keys: countingKeys("lens"),
  });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Add agent seat" }));
  const dialog = await screen.findByRole("dialog", { name: "Add to the company" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Analyst" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Add agent seat" }));
  // The new seat is selected by the focus the Builder moves to it; select it
  // through the outline, whose rows take the selection with focus.
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  // Minted from the lens's one key source, which a suite injects.
  await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain('"new:lens1"'));
  fireEvent.click(screen.getByRole("tab", { name: "Outline" }));
  const grid = await screen.findByRole("treegrid", { name: "Organization outline" });
  const row = within(grid)
    .getAllByRole("row")
    .find((r) => r.textContent?.includes("Analyst"))!;
  fireEvent.click(row);
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
    hash: "#/org?lens=builder&view=outline&seat=ceo",
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

/** Opens the CEO's editor from its outline row and types a goal into it. */
async function typeIntoTheEditor(): Promise<HTMLElement> {
  const grid = await screen.findByRole("treegrid", { name: "Organization outline" });
  const row = within(grid)
    .getAllByRole("row")
    .find((r) => r.getAttribute("data-row-id") === "seat:ceo")!;
  fireEvent.keyDown(row, { key: "Enter" });
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
    location.hash = "#/org?lens=builder&view=outline";
  });
  await screen.findByText("No problems");
  const onOutline = location.hash;
  const editor = await typeIntoTheEditor();

  act(() => history.back());
  const asked = await screen.findByRole("dialog", { name: "Discard your changes?" });
  expect(asked.textContent).toContain("you are leaving the builder");
  await waitFor(() => expect(location.hash).toBe(onOutline));
  fireEvent.click(within(asked).getByRole("button", { name: "Keep editing" }));
  expect((within(editor).getByLabelText(/^Goal/) as HTMLTextAreaElement).value).toBe("Grow");
});

// A MOVE WITHIN THE LENS LOSES NOTHING. The Builder keeps the editor open,
// form and all, when Back only turns the outline back into the canvas, so
// asking first would be a question about nothing, worded as a departure.
test("Back within the lens asks nothing, and the editor keeps what was typed", async () => {
  mountBuilder({ engine: new Engine(company()), surfaces: builderSurfaces });
  await screen.findByText("No problems");
  const onCanvas = location.hash;
  fireEvent.click(screen.getByRole("tab", { name: "Outline" }));
  const editor = await typeIntoTheEditor();

  act(() => history.back());
  await waitFor(() => expect(location.hash).toBe(onCanvas));
  await screen.findByRole("tree", { name: "Structure chart" });
  expect(screen.queryByRole("dialog", { name: "Discard your changes?" })).toBeNull();
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
  mountBuilder({ engine, surfaces: builderSurfaces, hash: "#/org?lens=builder&view=outline" });
  const grid = await screen.findByRole("treegrid", { name: "Organization outline" });
  await waitFor(() => expect(engine.checks()).toHaveLength(1));
  const row = within(grid)
    .getAllByRole("row")
    .find((r) => r.getAttribute("data-row-id") === "seat@roles[0]")!;
  fireEvent.keyDown(row, { key: "Enter" });
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
    hash: "#/org?lens=builder&view=outline",
  });
  await screen.findByText("No problems");
  await typeIntoTheEditor();
  const dev = within(screen.getByRole("treegrid", { name: "Organization outline" }))
    .getAllByRole("row")
    .find((r) => r.getAttribute("data-row-id") === "seat:dev")!;
  fireEvent.keyDown(dev, { key: "Enter" });
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
    hash: "#/org?lens=builder&view=outline&seat=ceo",
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
