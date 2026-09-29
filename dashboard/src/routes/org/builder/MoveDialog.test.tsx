/**
 * The Move dialog.
 *
 * What these protect: a move places the node under its destination under the
 * recorded operation, with the leads the operator chose to clear; a unit is
 * never offered a destination inside itself; who manages a seat today is said
 * only while the draft is still the chart the engine derived that from; a
 * seat that leads a unit is told it stays lead; and the schedules a move
 * strands and the seats working now are named before the move, not after.
 */

import { cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import type { AgentRow, ChartRead } from "~/protocol/index.ts";
import { locate } from "./model/draft.ts";
import type { BuilderState } from "./model/reducer.ts";
import { chartOf, fixtureChart, type DerivedOverrides } from "./model/testkit.ts";
import { MoveDialog } from "./MoveDialog.tsx";
import { renderInBuilder, type HarnessOptions } from "./viewTestkit.tsx";
import { checkedEdit, record } from "./testState.ts";
import { pick } from "~/testing.tsx";

afterEach(cleanup);

function open(state: BuilderState, key: string, options: HarnessOptions = {}) {
  const onClose = vi.fn();
  const view = renderInBuilder(state, <MoveDialog nodeKey={key} onClose={onClose} />, options);
  return { ...view, onClose };
}

const destination = () => screen.getByLabelText("Move to");
const choose = (label: string) => pick(destination(), label);
/** What the destination list offers, read from the list itself. */
const destinations = () => {
  fireEvent.click(destination());
  const labels = screen.getAllByRole("option").map((option) => option.textContent);
  fireEvent.keyDown(destination(), { key: "Escape" });
  return labels;
};
const moveButton = () => screen.getByRole("button", { name: "Move" }) as HTMLButtonElement;

/** The fixture chart, derived with Dev managed by the VP. */
function withManager(chart: ChartRead = fixtureChart(), overrides: DerivedOverrides = {}) {
  return checkedEdit(chart, {
    ...overrides,
    seats: { dev: { manager: "vp-engineering" }, ...overrides.seats },
  });
}

describe("a seat", () => {
  test("previews what changes and moves under the destination", () => {
    const view = open(withManager(), "seat:dev");
    expect(moveButton().disabled).toBe(true);
    choose("Sales");
    expect(screen.getByText("Today Dev reports to VP Engineering.")).toBeDefined();
    expect(screen.getByText("Dev loses the tool credentials of tracker.")).toBeDefined();
    expect(
      screen.getByText("1 agent seat onboards again, because the units above it change: Dev."),
    ).toBeDefined();
    fireEvent.click(moveButton());
    expect(view.state().log.ops[0]).toMatchObject({
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales" },
      clearLeads: [],
    });
    expect(view.onClose).toHaveBeenCalledTimes(1);
  });

  // WHO MANAGES IT TODAY IS THE ENGINE'S ANSWER ABOUT THE SAVED CHART, and a
  // draft that already changed the chart may have changed that answer too.
  test("who manages the seat today is said only while the draft is the saved chart", () => {
    const edited = record(withManager(), {
      type: "updateSeat",
      target: "seat:ceo",
      set: [{ path: ["goal"], value: "Lead well" }],
    });
    open(edited, "seat:dev");
    choose("Sales");
    expect(screen.queryByText(/^Today Dev reports to/)).toBeNull();
    // What the draft itself says is still said.
    expect(screen.getByText("Dev loses the tool credentials of tracker.")).toBeDefined();
  });

  // What is known before the move is who manages the seat today; when that is
  // only as the lead of the unit it leaves, the move ends it, and says so.
  test("a manager who is only the lead of the unit the seat leaves is said to end with the move", () => {
    open(
      withManager(fixtureChart(), {
        seats: { "vp-engineering": { auto_reports: ["dev"], reports: ["dev"] } },
      }),
      "seat:dev",
    );
    choose("Sales");
    expect(
      screen.getByText(
        "Today Dev reports to VP Engineering as the lead of Engineering, which ends with the move.",
      ),
    ).toBeDefined();
  });

  // The credentials a move gives or takes are named by their SERVER, which is
  // what a unit's mcp_env keys are; a value, masked or not, is never on screen.
  test("the tool credentials a move changes are named by server, never by value", () => {
    const chart = fixtureChart();
    chart.units.find((u) => u.key === "engineering")!.runtime!.mcp_env = {
      tracker: { TOKEN: "__redacted__" },
    };
    const view = open(withManager(chart), "seat:dev");
    choose("Sales");
    expect(screen.getByText("Dev loses the tool credentials of tracker.")).toBeDefined();
    expect(view.container.ownerDocument.body.innerHTML).not.toContain("__redacted__");
  });

  test("at the top level no unit lead manages the seat, and the preview says so", () => {
    open(withManager(), "seat:dev");
    choose("The company (top level)");
    expect(screen.getByText("At the top level, no unit lead manages it.")).toBeDefined();
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
    expect(view.state().log.ops[0]).toMatchObject({ clearLeads: [{ unit: "unit:engineering" }] });
    const engineering = locate(view.state().draft, "unit:engineering");
    expect(engineering?.kind === "unit" && engineering.node.data.lead).toBeUndefined();
  });

  // Where a node sits is its parent and nothing else: the chart keeps no order
  // among siblings, so the unit it is in is not a move at all.
  test("its own unit is where it already is", () => {
    open(withManager(), "seat:vp-engineering");
    choose("Engineering");
    expect(screen.getByText("VP Engineering is already there.")).toBeDefined();
    expect(moveButton().disabled).toBe(true);
    // The control: another unit is a move.
    choose("Sales");
    expect(screen.queryByText("VP Engineering is already there.")).toBeNull();
    expect(moveButton().disabled).toBe(false);
  });

  test("the schedules a move strands and the seat's work in flight are said before the move", () => {
    const chart = chartOf({
      units: [
        {
          key: "ops",
          name: "Ops",
          runtime: { schedules: [{ name: "sweep", cron: "0 * * * *", task: "Sweep" }] },
        },
        { key: "other", name: "Other" },
      ],
      seats: [{ handle: "runner", name: "Runner", unit: "ops" }],
    });
    const agents: AgentRow[] = [
      { id: "1", agent_id: "id-1", role: "Runner", handle: "runner", state: "working" },
    ];
    open(checkedEdit(chart), "seat:runner", { agents });
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
  test("a unit that moves with a seat working inside it names that seat", () => {
    const agents: AgentRow[] = [
      { id: "1", agent_id: "id-1", role: "SRE", handle: "sre", state: "working" },
    ];
    open(checkedEdit(), "unit:platform", { agents });
    expect(
      screen.getByText(
        "SRE is working now. Its current turn continues on the previous configuration until the engine applies this change.",
      ),
    ).toBeDefined();
  });

  test("is never offered itself or a unit inside it, and names the lead and channel its subtree inherits", () => {
    const chart = fixtureChart();
    chart.units.find((u) => u.key === "engineering")!.channel = "eng";
    const state = checkedEdit(chart);
    open(state, "unit:engineering");
    const labels = destinations();
    expect(labels).not.toContain("Engineering");
    expect(labels).not.toContain("Engineering / Platform");
    expect(labels).toContain("Sales");
    cleanup();

    const view = open(state, "unit:platform");
    // The control: a unit elsewhere is offered its own parent's siblings.
    expect(destinations()).toContain("Engineering");
    choose("The company (top level)");
    expect(
      screen.getByText("Platform would inherit no lead instead of VP Engineering."),
    ).toBeDefined();
    expect(screen.getByText("Platform would inherit no channel instead of eng.")).toBeDefined();
    fireEvent.click(moveButton());
    expect(view.state().log.ops[0]).toMatchObject({
      type: "move",
      target: "unit:platform",
      to: { parent: "company" },
    });
  });
});

// A disabled button is not a reason: without the note the operator picks a
// destination and nothing on screen says the builder is what is in the way.
test("a read-only builder moves nothing, and says why the button is unavailable", () => {
  open(checkedEdit(), "seat:dev", { readOnly: true });
  expect(moveButton().disabled).toBe(true);
  expect(
    screen.getByText(
      "The organization cannot be changed right now, so this change cannot be applied.",
    ),
  ).toBeDefined();
});
