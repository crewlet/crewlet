/**
 * The Add dialog.
 *
 * What these protect: a name is prose two seats may share, and the identity
 * offered from it (a seat's handle, a unit's key) is free of every one the
 * draft and the saved company hold, a removed node's included; an identity
 * typed over the offer that breaks the grammar or is held is refused before
 * anything is recorded; what is added lands at the end of the parent's list
 * under a key minted for it, as the kind chosen, carrying its identity; and a
 * refusal keeps the dialog open instead of adding nothing silently.
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
import { pick } from "~/testing.tsx";

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
const handleBox = () => screen.getByLabelText("Handle") as HTMLInputElement;
const keyBox = () => screen.getByLabelText("Key") as HTMLInputElement;

test("an agent seat is added at the end of its unit under a minted key, carrying a handle nobody holds", () => {
  const doc = fixtureCompany();
  doc.units![1]!.roles!.push({ name: "New agent seat", handle: "new-agent-seat" });
  const view = open(keyedState(doc), "unit:sales");
  // The name is shared freely; the handle is not.
  expect(nameBox().value).toBe("New agent seat");
  expect(handleBox().value).toBe("new-agent-seat-2");
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  const op = view.state().log.ops[0];
  expect(op).toMatchObject({
    type: "addSeat",
    placement: { parent: "unit:sales", after: "seat:new-agent-seat" },
    data: { name: "New agent seat", handle: "new-agent-seat-2" },
  });
  expect(op?.type === "addSeat" && isMintedKey(op.key)).toBe(true);
  expect(view.onClose).toHaveBeenCalledTimes(1);
});

// A REMOVED SEAT'S HANDLE IS STILL ITS MEMORY AND MAILBOX, so a new seat may
// not take it before a save has made the removal the company's.
test("the handle follows the name until typed, avoids a removed seat's, and a held one is refused", () => {
  const removed = builderReducer(keyedState(fixtureCompany()), {
    type: "record",
    intent: { type: "remove", target: "seat:sre" },
  });
  const view = open(removed, null);
  expect(screen.getByText(/everything that names this seat uses its handle/)).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "SRE" } });
  expect(handleBox().value).toBe("sre-2");
  fireEvent.change(handleBox(), { target: { value: "sre" } });
  expect(screen.getByText("sre already names a seat or a unit.")).toBeDefined();
  const add = screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement;
  expect(add.disabled).toBe(true);
  fireEvent.change(handleBox(), { target: { value: "Site Reliability" } });
  expect(screen.getByText(/lowercase letters, digits and hyphens/)).toBeDefined();
  // A typed handle is kept whatever the name becomes.
  fireEvent.change(handleBox(), { target: { value: "reliability" } });
  fireEvent.change(nameBox(), { target: { value: "Reliability Engineer" } });
  expect(handleBox().value).toBe("reliability");
  fireEvent.click(add);
  expect(view.state().log.ops.at(-1)).toMatchObject({
    type: "addSeat",
    data: { name: "Reliability Engineer", handle: "reliability" },
  });
});

test("a unit is added with its key and type at the company root; a key any node holds is not offered", () => {
  const view = open(keyedState(fixtureCompany()), null, "unit");
  expect(screen.getByText(/everything that names this unit uses its key/)).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "Sales" } });
  expect(keyBox().value).toBe("sales-2");
  // ONE NAMESPACE: a unit keyed like a seat is a unit no manages entry can name.
  fireEvent.change(nameBox(), { target: { value: "Dev" } });
  expect(keyBox().value).toBe("dev-2");
  fireEvent.change(nameBox(), { target: { value: "Legal" } });
  pick(screen.getByLabelText("Type"), "Department");
  fireEvent.click(screen.getByRole("button", { name: "Add unit" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addUnit",
    placement: { parent: "company", after: "unit:sales" },
    data: { name: "Legal", id: "legal", type: "department" },
  });
});

test("a name somebody typed stays when they choose another kind", () => {
  open(keyedState(fixtureCompany()), "unit:sales");
  fireEvent.change(nameBox(), { target: { value: "Closer" } });
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("Closer");
});

test("choosing another kind moves an untouched default name along, and a human seat carries its contact", () => {
  const view = open(keyedState(fixtureCompany()), "unit:engineering");
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("New human seat");
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText("GitHub login"), { target: { value: "pat" } });
  fireEvent.click(screen.getByRole("button", { name: "Add human seat" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addSeat",
    data: {
      name: "New human seat",
      handle: "new-human-seat",
      kind: "human",
      contact: { github_login: "pat" },
    },
  });
});

// A CONTACT IS OPTIONAL: the engine admits a human seat with none, and the
// person is then reached through the dashboard.
test("a human seat is added with no contact when none is typed", () => {
  const view = open(keyedState(fixtureCompany()), "unit:engineering", "human");
  fireEvent.click(screen.getByRole("button", { name: "Add human seat" }));
  const op = view.state().log.ops[0];
  expect(op?.type === "addSeat" && op.data).toEqual({
    name: "New human seat",
    handle: "new-human-seat",
    kind: "human",
  });
});

// A KEY COMES FROM THE BUILDER'S ONE SOURCE, the same that mints every write
// id, so a suite that injects a source knows exactly which key an Add mints.
test("an added node's key is minted from the Builder's key source", () => {
  const view = open(keyedState(fixtureCompany()), null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  // The harness keeps the dialog open, so a second press is a second node,
  // under the next handle, since the first now holds the one offered.
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(view.state().log.ops.map((op) => op.type === "addSeat" && op.key)).toEqual([
    "new:test1",
    "new:test2",
  ]);
});

test("a unit that has left the draft takes nothing, and says so", () => {
  const view = open(keyedState(fixtureCompany()), "unit:gone");
  expect(
    screen.getByText("That unit is no longer in the draft, so nothing can be added to it."),
  ).toBeDefined();
  const add = screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement;
  expect(add.disabled).toBe(true);
  fireEvent.click(add);
  expect(view.state().log.ops).toHaveLength(0);
});

test("a refusal keeps the dialog open and adds nothing", () => {
  const view = open(INITIAL_BUILDER, null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(screen.getByText(/has not loaded yet/)).toBeDefined();
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
  open(INITIAL_BUILDER, null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(screen.getByRole("alert").textContent).toMatch(/has not loaded yet/);
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
