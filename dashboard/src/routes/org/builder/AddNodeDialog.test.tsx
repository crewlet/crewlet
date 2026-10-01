/**
 * The Add dialog.
 *
 * What these protect: a new node asks for a name, which is prose two nodes
 * may share, and an ADDRESS, suggested from the name until the operator types
 * one and never one the chart holds, held or keeps as somebody's identity;
 * what is added is placed under its parent under a key minted for it, as the
 * kind chosen; and a refusal keeps the dialog open instead of adding nothing
 * silently.
 */

import { cleanup, fireEvent, screen } from "~/test/inCase.ts";
import { afterEach, expect, test, vi } from "vitest";
import { AddNodeDialog } from "./AddNodeDialog.tsx";
import { isMintedKey } from "./model/keys.ts";
import { chartOf, fixtureChart, strippedChart } from "./model/testkit.ts";
import type { AddKind } from "./BuilderContext.tsx";
import type { BuilderState } from "./model/reducer.ts";
import { renderInBuilder, type HarnessOptions } from "./viewTestkit.tsx";
import { checkedEdit, record } from "./testState.ts";
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
const handleBox = () => screen.getByLabelText(/^Handle/) as HTMLInputElement;

test("an agent seat is added under its unit with a minted key, a free name and a handle from it", () => {
  const chart = fixtureChart();
  chart.seats.push({ handle: "new-agent-seat", name: "New agent seat", unit: "sales" });
  const view = open(checkedEdit(chart), "unit:sales");
  expect(nameBox().value).toBe("New agent seat 2");
  expect(handleBox().value).toBe("new-agent-seat-2");
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  const op = view.state().log.ops[0];
  expect(op).toMatchObject({
    type: "addSeat",
    placement: { parent: "unit:sales" },
    data: { handle: "new-agent-seat-2", name: "New agent seat 2" },
  });
  expect(op?.type === "addSeat" && isMintedKey(op.key)).toBe(true);
  expect(view.onClose).toHaveBeenCalledTimes(1);
});

// A NAME IS PROSE, and two seats may share one: every reference names a seat
// by its handle. The handle follows the name until the operator types one.
test("a shared name is allowed, and the handle it suggests is free", () => {
  const view = open(checkedEdit(), null);
  fireEvent.change(nameBox(), { target: { value: "Dev" } });
  expect(screen.queryByText(/already exists/)).toBeNull();
  expect(handleBox().value).toBe("dev-2");
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(view.state().log.ops[0]).toMatchObject({ data: { handle: "dev-2", name: "Dev" } });
});

test("a handle the chart holds is refused, naming why, and nothing is added", () => {
  const view = open(checkedEdit(), null);
  fireEvent.change(handleBox(), { target: { value: "dev" } });
  expect(
    screen.getByText("dev is taken: something in this company holds it, or held it."),
  ).toBeDefined();
  const add = screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement;
  expect(add.disabled).toBe(true);
  // The handle is the operator's now: the name no longer moves it.
  fireEvent.change(nameBox(), { target: { value: "Developer" } });
  expect(handleBox().value).toBe("dev");
  fireEvent.change(handleBox(), { target: { value: "developer" } });
  expect(add.disabled).toBe(false);
  fireEvent.click(add);
  expect(view.state().log.ops[0]).toMatchObject({ data: { handle: "developer" } });
});

// THE ADDRESS A SEAT WAS CREATED UNDER IS ITS IDENTITY for as long as the
// chart holds it, whatever it is called now: a second seat on it would share
// the first one's mailbox and memory. It is taken, and never suggested.
test("the handle a renamed seat was created under is taken, and never suggested", () => {
  const chart = chartOf({
    seats: [
      { handle: "developer", name: "Developer", origin_handle: "dev", former_handles: ["dev"] },
    ],
  });
  open(checkedEdit(chart), null);
  fireEvent.change(nameBox(), { target: { value: "Dev" } });
  expect(handleBox().value).toBe("dev-2");
  fireEvent.change(handleBox(), { target: { value: "dev" } });
  expect(
    screen.getByText("dev is taken: something in this company holds it, or held it."),
  ).toBeDefined();
  // The control: an address nothing was created under is free.
  fireEvent.change(handleBox(), { target: { value: "dev-ops" } });
  expect(screen.queryByText(/is taken/)).toBeNull();
});

test("a unit is added with its key and its type at the company root", () => {
  const view = open(checkedEdit(), null, "unit");
  expect(screen.getByLabelText(/^Key/)).toBeDefined();
  fireEvent.change(nameBox(), { target: { value: "Legal" } });
  expect((screen.getByLabelText(/^Key/) as HTMLInputElement).value).toBe("legal");
  pick(screen.getByLabelText("Type"), "Department");
  fireEvent.click(screen.getByRole("button", { name: "Add unit" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addUnit",
    placement: { parent: "company" },
    data: { key: "legal", name: "Legal", type: "department" },
  });
});

test("a name somebody typed stays when they choose another kind", () => {
  open(checkedEdit(), "unit:sales");
  fireEvent.change(nameBox(), { target: { value: "Closer" } });
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("Closer");
});

test("choosing another kind moves an untouched default name along, and a human seat carries its contact", () => {
  const view = open(checkedEdit(), "unit:engineering");
  fireEvent.click(screen.getByRole("radio", { name: "Human seat" }));
  expect(nameBox().value).toBe("New human seat");
  pick(screen.getByLabelText("Contact"), "GitHub login");
  fireEvent.change(screen.getByLabelText(/^GitHub login/), { target: { value: "pat" } });
  fireEvent.click(screen.getByRole("button", { name: "Add human seat" }));
  expect(view.state().log.ops[0]).toMatchObject({
    type: "addSeat",
    data: {
      handle: "new-human-seat",
      name: "New human seat",
      kind: "human",
      runtime: { contact: { github_login: "pat" } },
    },
  });
});

// THE CONTACT IS IN THE RUNTIME HALF, which a reader the chart did not show it
// cannot write: the field is not offered rather than offered and refused.
test("a reader who was not shown the runtime half is offered no contact field", () => {
  const view = open(checkedEdit(fixtureChart()), "unit:engineering", "human");
  expect(screen.getByLabelText("Contact")).toBeDefined();
  cleanup();
  open(checkedEdit(strippedChart(fixtureChart())), "unit:engineering", "human");
  expect(screen.queryByLabelText("Contact")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Add human seat" }));
  expect(view.onClose).not.toHaveBeenCalled();
});

// A KEY COMES FROM THE BUILDER'S ONE SOURCE, the same that mints every write
// id, so a suite that injects a source knows exactly which key an Add mints.
test("an added node's key is minted from the Builder's key source", () => {
  const view = open(checkedEdit(), null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  // The harness keeps the dialog open, so a second press, under a handle the
  // first did not take, is a second node.
  fireEvent.change(handleBox(), { target: { value: "second-seat" } });
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(view.state().log.ops.map((op) => op.type === "addSeat" && op.key)).toEqual([
    "new:test1",
    "new:test2",
  ]);
});

test("a unit that has left the draft takes nothing, and says so", () => {
  const view = open(checkedEdit(), "unit:gone");
  expect(
    screen.getByText("That unit is no longer in the draft, so nothing can be added to it."),
  ).toBeDefined();
  const add = screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement;
  expect(add.disabled).toBe(true);
  fireEvent.click(add);
  expect(view.state().log.ops).toHaveLength(0);
});

/** A draft already holding a node under the key the harness mints first. */
const holdingFirstKey = () =>
  record(checkedEdit(), {
    type: "addSeat",
    key: "new:test1",
    placement: { parent: "company" },
    data: { handle: "qa", name: "QA" },
  });

// AND IT IS SPOKEN, NOT ONLY DRAWN. The dialog asks the reducer whether it
// would take the operation before it dispatches, so a refusal never reaches
// the reducer's state and never reaches the builder's own live region, which
// speaks for a recorded operation. This paragraph is the whole report: without
// the role a reader presses the one button, the dialog does not move, and
// nothing tells them why.
test("a refusal keeps the dialog open, adds nothing, and is announced", () => {
  const view = open(holdingFirstKey(), null);
  fireEvent.click(screen.getByRole("button", { name: "Add agent seat" }));
  expect(screen.getByRole("alert").textContent).toMatch(/That key already names a node\./);
  expect(view.state().log.ops).toHaveLength(1);
  expect(view.onClose).not.toHaveBeenCalled();
});

// A disabled button is not a reason: without the note the operator fills the
// dialog in and nothing on screen says the builder is what is in the way.
test("a read-only builder adds nothing, and says why the button is unavailable", () => {
  open(checkedEdit(), null, "agent", { readOnly: true });
  expect(
    (screen.getByRole("button", { name: "Add agent seat" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    screen.getByText(
      "The organization cannot be changed right now, so this change cannot be applied.",
    ),
  ).toBeDefined();
});
