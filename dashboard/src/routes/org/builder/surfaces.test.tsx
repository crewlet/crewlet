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
