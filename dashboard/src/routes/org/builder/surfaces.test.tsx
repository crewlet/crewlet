/**
 * The Org screen's Builder lens is assembled from the real views and dialogs.
 *
 * Every other Builder suite stands a view in with a fake, so nothing there
 * notices a lens whose canvas or outline was never bound: it rendered "not
 * part of this build" for both until this binding existed. These tests mount
 * the lens with the surfaces the screen hands it and hold it to drawing the
 * canvas's tree, the outline's treegrid and the node editor.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
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
