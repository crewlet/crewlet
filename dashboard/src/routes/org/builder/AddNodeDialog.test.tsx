/**
 * The Add dialog.
 *
 * What these protect: the name starts free and a taken name offers the next
 * free one with the rule that makes names unique; what is added lands at the
 * end of the parent's list under a key minted for it, as the kind chosen; and
 * a refusal keeps the dialog open instead of adding nothing silently.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { isMintedKey } from "./model/keys.ts";
import { builderReducer, INITIAL_BUILDER } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import type { AddKind } from "./BuilderContext.tsx";
import type { BuilderState } from "./model/reducer.ts";
import { renderInBuilder, type HarnessOptions } from "./viewTestkit.tsx";
import { keyedState } from "./testState.ts";

afterEach(cleanup);

function open(
  state: BuilderState,
  parent: string | null,
  kind?: AddKind,
  options: HarnessOptions = {},
) {
  const onClose = vi.fn();
  const view = renderInBuilder(
    state,
    <AddNodeDialog parent={parent} kind={kind} onClose={onClose} />,
    options,
  );
  return { ...view, onClose };
}

const nameBox = () => screen.getByLabelText("Name") as HTMLInputElement;

test("an agent seat is added at the end of its unit under a minted key, with a name nobody holds", () => {
  const doc = fixtureCompany();
  doc.units![1]!.roles!.push({ name: "New agent seat" });
  const view = open(keyedState(doc), "unit:Sales");
  expect(nameBox().value).toBe("New agent seat 2");
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  const op = view.state().log.ops[0];
  expect(op).toMatchObject({
    type: "addSeat",
    placement: { parent: "unit:Sales", after: "seat:new-agent-seat" },
    data: { name: "New agent seat 2" },
  });
  expect(op?.type === "addSeat" && isMintedKey(op.key)).toBe(true);
  expect(view.onClose).toHaveBeenCalledTimes(1);
});

test("a taken name offers the next free one, with the rule that makes names unique", () => {
  open(keyedState(fixtureCompany()), null);
  expect(
    screen.getByText("Seat names are unique: a lead or a manages entry names exactly one seat."),
  ).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "Dev" } });
  expect(screen.getByText(/A seat named Dev already exists/)).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Use Dev 2" }));
  expect(nameBox().value).toBe("Dev 2");
  expect(screen.queryByText(/already exists/)).toBeNull();
});

test("a unit is added with its type at the company root; its names are checked against units", () => {
  const view = open(keyedState(fixtureCompany()), null, "unit");
  // A lead names a seat, never a unit, so it is no reason a unit's name is unique.
  expect(
    screen.getByText(
      "Unit names are unique: a manages entry or a unit reference names exactly one unit.",
    ),
  ).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "Sales" } });
  expect(screen.getByRole("button", { name: "Use Sales 2" })).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "Legal" } });
  fireEvent.change(screen.getByLabelText("Type"), { target: { value: "department" } });
  fireEvent.click(screen.getByRole("button", { name: "Add unit" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addUnit",
    placement: { parent: "company", after: "unit:Sales" },
    data: { name: "Legal", type: "department" },
  });
});

test("a name somebody typed stays when they choose another kind", () => {
  open(keyedState(fixtureCompany()), "unit:Sales");
  fireEvent.change(nameBox(), { target: { value: "Closer" } });
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("Closer");
});

test("choosing another kind moves an untouched default name along, and a human seat carries its contact", () => {
  const view = open(keyedState(fixtureCompany()), "unit:Engineering");
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("New human seat");
  fireEvent.change(screen.getByLabelText("Contact"), { target: { value: "github_login" } });
  fireEvent.change(screen.getByLabelText("GitHub login"), { target: { value: "pat" } });
  fireEvent.click(screen.getByRole("button", { name: "Add human seat" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addSeat",
    data: { name: "New human seat", kind: "human", contact: { github_login: "pat" } },
  });
});

// A KEY COMES FROM THE BUILDER'S ONE SOURCE, the same that mints every write
// id, so a suite that injects a source knows exactly which key an Add mints.
test("an added node's key is minted from the Builder's key source", () => {
  const view = open(keyedState(fixtureCompany()), null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  // The harness keeps the dialog open, so a second press is a second node.
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(view.state().log.ops.map((op) => op.type === "addSeat" && op.key)).toEqual([
    "new:test1",
    "new:test2",
  ]);
});

test("a unit that has left the draft takes nothing, and says so", () => {
  const view = open(keyedState(fixtureCompany()), "unit:Gone");
  expect(
    screen.getByText("That unit is no longer in the draft, so nothing can be added to it."),
  ).toBeDefined();
  const add = screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement;
  expect(add.disabled).toBe(true);
  fireEvent.click(add);
  expect(view.state().log.ops).toHaveLength(0);
});

test("a refusal keeps the dialog open and adds nothing", () => {
  const loaded = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
  const view = open(loaded, null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(screen.getByText(/has not described this company yet/)).toBeDefined();
  expect(view.state().log.ops).toHaveLength(0);
  expect(view.onClose).not.toHaveBeenCalled();
});

// AND IT IS SPOKEN, NOT ONLY DRAWN. The dialog asks the reducer whether it
// would take the operation before it dispatches, so a refusal never reaches
// the reducer's state and never reaches the builder's own live region, which
// speaks for a recorded operation. This paragraph is the whole report: without
// the role a reader presses the one button, the dialog does not move, and
// nothing tells them why.
test("a refusal is announced", () => {
  const loaded = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
  open(loaded, null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(screen.getByRole("alert").textContent).toMatch(/has not described this company yet/);
});

// A disabled button is not a reason: without the note the operator fills the
// dialog in and nothing on screen says the builder is what is in the way.
test("a read-only builder adds nothing, and says why the button is unavailable", () => {
  open(keyedState(fixtureCompany()), null, "agent", { readOnly: true });
  expect(
    (screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    screen.getByText(
      "The organization cannot be changed right now, so this change cannot be applied.",
    ),
  ).toBeDefined();
});
