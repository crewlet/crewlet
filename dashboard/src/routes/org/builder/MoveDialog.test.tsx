/**
 * The Move dialog.
 *
 * What these protect: a move lands at the end of the destination under the
 * recorded operation, with the leads the operator chose to clear; a unit is
 * never offered a destination inside itself; the preview says what the last
 * check reported will change; a seat that leads a unit is told it stays lead;
 * and the schedules a move strands and the seats working now are named before
 * the move, not after.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentRow, CompanyDocument } from "~/protocol/index.ts";
import { locate } from "./model/draft.ts";
import type { BuilderState } from "./model/reducer.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { MoveDialog } from "./MoveDialog.tsx";
import { renderInBuilder, type HarnessOptions } from "./testBuilder.tsx";
import { keyedState } from "./testState.ts";

afterEach(cleanup);

function open(state: BuilderState, key: string, options: HarnessOptions = {}) {
  const onClose = vi.fn();
  const view = renderInBuilder(state, <MoveDialog nodeKey={key} onClose={onClose} />, options);
  return { ...view, onClose };
}

const destination = () => screen.getByLabelText("Move to") as HTMLSelectElement;
const choose = (label: string) => {
  const option = [...destination().options].find((o) => o.textContent === label);
  if (!option) throw new Error(`no destination ${label}`);
  fireEvent.change(destination(), { target: { value: option.value } });
};
const moveButton = () => screen.getByRole("button", { name: "Move" }) as HTMLButtonElement;

function withManager(doc: CompanyDocument = fixtureCompany()) {
  return keyedState(doc, { seats: { "units[0].roles[1]": { manager: "vp-engineering" } } });
}

describe("a seat", () => {
  test("previews what changes and moves to the end of the destination", () => {
    const view = open(withManager(), "seat:dev");
    expect(moveButton().disabled).toBe(true);
    choose("Sales");
    expect(screen.getByText("Dev reports to VP Engineering now.")).toBeDefined();
    expect(screen.getByText("Dev loses the tool credentials of tracker.")).toBeDefined();
    expect(
      screen.getByText("1 agent seat onboards again, because the units above it change: Dev."),
    ).toBeDefined();
    fireEvent.click(moveButton());
    expect(view.state().log.ops[0]).toMatchObject({
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:Sales", after: "seat:account-executive" },
      clearLeads: [],
    });
    expect(view.onClose).toHaveBeenCalledTimes(1);
  });

  // The credentials a move gives or takes are named by their SERVER, which is
  // what a unit's mcp_env keys are; a value, masked or not, is never on screen.
  test("the tool credentials a move changes are named by server, never by value", () => {
    const doc = fixtureCompany();
    doc.units![0]!.mcp_env = { tracker: { TOKEN: "__redacted__" } };
    const view = open(withManager(doc), "seat:dev");
    choose("Sales");
    expect(screen.getByText("Dev loses the tool credentials of tracker.")).toBeDefined();
    expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
  });

  test("moving into a unit with a lead names the lead that manages its members", () => {
    open(withManager(), "seat:sre");
    choose("Engineering");
    expect(
      screen.getByText(
        "In Engineering, VP Engineering leads the unit and manages its direct members unless another member manages SRE.",
      ),
    ).toBeDefined();
  });

  test("a seat that leads a unit stays its lead unless the operator clears it with the move", () => {
    const view = open(withManager(), "seat:vp-engineering");
    expect(screen.getByRole("heading", { name: "Stays lead of Engineering" })).toBeDefined();
    choose("Sales");
    fireEvent.click(screen.getByRole("checkbox", { name: "Clear lead" }));
    fireEvent.click(moveButton());
    expect(view.state().log.ops[0]).toMatchObject({ clearLeads: [{ unit: "unit:Engineering" }] });
    const engineering = locate(view.state().draft, "unit:Engineering");
    expect(engineering?.kind === "unit" && engineering.node.data.lead).toBeUndefined();
  });

  test("its own unit is where it already is; a seat placed by a unit reference says the move replaces it", () => {
    // Not the last seat of its unit either, where a move to the end would
    // quietly be a reorder.
    open(withManager(), "seat:vp-engineering");
    choose("Engineering");
    expect(screen.getByText("VP Engineering is already there.")).toBeDefined();
    expect(moveButton().disabled).toBe(true);
    cleanup();
    open(withManager(), "seat:designer");
    expect(
      screen.getByText(
        "Designer is placed in Platform by its unit reference. Moving it writes it into the destination and removes the reference.",
      ),
    ).toBeDefined();
  });

  test("the schedules a move strands and the seat's work in flight are said before the move", () => {
    const doc: CompanyDocument = {
      name: "X",
      units: [
        {
          name: "Ops",
          schedules: [{ name: "sweep", cron: "0 * * * *", task: "Sweep" }],
          roles: [{ name: "Runner" }],
        },
        { name: "Other" },
      ],
    };
    const agents: AgentRow[] = [{ id: "1", role: "Runner", handle: "runner", state: "working" }];
    open(keyedState(doc), "seat:runner", { agents });
    choose("Other");
    expect(screen.getByText(/Schedule sweep on Ops would have no runner/)).toBeDefined();
    expect(
      screen.getByText(
        "Runner is working now. Its current turn continues on the previous configuration until the engine applies this change.",
      ),
    ).toBeDefined();
  });
});

describe("a unit", () => {
  test("is never offered itself or a unit inside it, and names the lead and channel its subtree inherits", () => {
    const doc = fixtureCompany();
    doc.units![0]!.channel = "eng";
    const state = keyedState(doc, {
      units: {
        "units[0].children[0]": {
          lead: "vp-engineering",
          lead_inherited: true,
          channel: "eng",
          channel_inherited: true,
        },
      },
    });
    open(state, "unit:Engineering");
    const labels = [...destination().options].map((o) => o.textContent);
    expect(labels).not.toContain("Engineering");
    expect(labels).not.toContain("Engineering / Platform");
    expect(labels).toContain("Sales");
    cleanup();

    const view = open(state, "unit:Platform");
    choose("The company (top level)");
    expect(
      screen.getByText("Platform would inherit no lead instead of VP Engineering."),
    ).toBeDefined();
    expect(screen.getByText("Platform would inherit no channel instead of eng.")).toBeDefined();
    fireEvent.click(moveButton());
    expect(view.state().log.ops[0]).toMatchObject({
      type: "move",
      target: "unit:Platform",
      to: { parent: "company", after: "unit:Sales" },
    });
  });
});
