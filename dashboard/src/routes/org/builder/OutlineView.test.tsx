/**
 * The builder outline: a treegrid whose rows and cells a keyboard walks.
 *
 * What these protect:
 * - the grid says each row's level, position and expansion, an inline add
 *   row closes each unit's rows and the company's (counted among its
 *   siblings), and read-only removes the add rows;
 * - Up and Down walk rows, Right opens a row or steps into its cells, Left
 *   steps back out, and a cell holding a control focuses the control, which
 *   is then the grid's one tab stop;
 * - Enter edits, Delete and Backspace delete (never the company), the
 *   ContextMenu key opens the row's actions, and the add row's buttons ask
 *   for that kind under that parent;
 * - Alt+Up and Alt+Down reorder a row among its siblings, refuse at either
 *   end and for a seat placed by reference, and announce a primary manager
 *   change once the check of the reordered draft reports one.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import type { AgentRow, CompanyDocument } from "~/protocol/index.ts";
import { OutlineView } from "./OutlineView.tsx";
import { toDocument } from "./model/document.ts";
import { COMPANY_KEY, seatKey, unitKey } from "./model/keys.ts";
import type { BuilderState } from "./model/reducer.ts";
import { fixtureCompany, fixtureDerived, type DerivedOverrides } from "./model/testkit.ts";
import { checkedEdit, PLACED } from "./testState.ts";
import {
  BuilderHarness,
  builderSpies,
  harnessProbe,
  type BuilderSpies,
  type HarnessProbe,
} from "./viewTestkit.tsx";

afterEach(cleanup);

function mount(
  initial: BuilderState = checkedEdit(fixtureCompany()),
  { readOnly = false }: { readOnly?: boolean } = {},
): { spies: BuilderSpies; probe: HarnessProbe } {
  const spies = builderSpies();
  const probe = harnessProbe();
  render(
    <BuilderHarness initial={initial} spies={spies} probe={probe} readOnly={readOnly}>
      <OutlineView />
    </BuilderHarness>,
  );
  return { spies, probe };
}

const rowOf = (id: string) =>
  screen.getAllByRole("row").find((r) => r.getAttribute("data-row-id") === id)!;
const press = (key: string, init: Partial<KeyboardEventInit> = {}) =>
  fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });
/**
 * A pointer press on a button, as a browser makes one: the press moves focus
 * to the button unless the view stops it, which jsdom leaves to the caller.
 */
const pointerPress = (name: string) => {
  const button = screen.getByRole("button", { name, hidden: true });
  if (fireEvent.mouseDown(button)) button.focus();
  fireEvent.click(button);
};
const focusedRow = () =>
  (document.activeElement as HTMLElement | null)
    ?.closest("[data-row-id]")
    ?.getAttribute("data-row-id");
const focusedColumn = () =>
  (document.activeElement as HTMLElement | null)
    ?.closest("[role='gridcell']")
    ?.getAttribute("aria-colindex") ?? null;

describe("the grid", () => {
  test("rows say their level, position and expansion, with an add row closing each unit", () => {
    mount();
    expect(screen.getByRole("treegrid", { name: "Organization outline" })).toBeDefined();
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Name",
      "Kind or type",
      "Handle",
      "Lead or reports to",
      "Problems",
      "Actions",
    ]);
    const body = screen.getAllByRole("row").filter((r) => r.hasAttribute("data-row-id"));
    expect(
      body.map((r) => [
        r.getAttribute("data-row-id"),
        r.getAttribute("aria-level"),
        r.getAttribute("aria-posinset"),
        r.getAttribute("aria-setsize"),
        r.getAttribute("aria-expanded"),
      ]),
    ).toEqual([
      [COMPANY_KEY, "1", "1", "1", "true"],
      [seatKey("ceo"), "2", "1", "4", null],
      [unitKey("Engineering"), "2", "2", "4", "true"],
      [seatKey("vp-engineering"), "3", "1", "4", null],
      [seatKey("dev"), "3", "2", "4", null],
      [unitKey("Platform"), "3", "3", "4", "true"],
      [seatKey("sre"), "4", "1", "3", null],
      [seatKey("designer"), "4", "2", "3", null],
      [`add:${unitKey("Platform")}`, "4", "3", "3", null],
      [`add:${unitKey("Engineering")}`, "3", "4", "4", null],
      [unitKey("Sales"), "2", "3", "4", "true"],
      [seatKey("account-executive"), "3", "1", "2", null],
      [`add:${unitKey("Sales")}`, "3", "2", "2", null],
      [`add:${COMPANY_KEY}`, "2", "4", "4", null],
    ]);
    // The cells of a node's row, in their columns.
    const dev = rowOf(seatKey("dev"));
    expect(
      within(dev)
        .getAllByRole("gridcell")
        .map((c) => c.getAttribute("aria-colindex")),
    ).toEqual(["1", "2", "3", "4", "5", "6"]);
    expect(within(dev).getByText("@dev")).toBeDefined();
    expect(within(rowOf(unitKey("Engineering"))).getByText("VP Engineering")).toBeDefined();
    expect(within(rowOf(seatKey("designer"))).getByText("Placed by unit reference")).toBeDefined();
  });

  test("read-only, there are no add rows and no lead choice to open", () => {
    mount(undefined, { readOnly: true });
    expect(
      screen.getAllByRole("row").some((r) => r.getAttribute("data-row-id")?.startsWith("add:")),
    ).toBe(false);
    expect(rowOf(seatKey("ceo")).getAttribute("aria-setsize")).toBe("3");
    expect(
      within(rowOf(unitKey("Engineering"))).queryByRole("button", { name: "VP Engineering" }),
    ).toBeNull();
  });

  test("the lead or reports-to column says what the engine derived, and waits after an edit", () => {
    const { probe } = mount(
      checkedEdit(fixtureCompany(), {
        seats: { ...PLACED.seats, "units[0].roles[1]": { manager: "vp-engineering" } },
      }),
    );
    const leadOrManager = (id: string) => within(rowOf(id)).getAllByRole("gridcell")[3]!;
    expect(leadOrManager(seatKey("dev")).textContent).toBe("VP Engineering");
    expect(leadOrManager(seatKey("sre")).textContent).toBe("No manager");
    expect(leadOrManager(unitKey("Engineering")).textContent).toBe("VP Engineering");

    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "setLead", target: unitKey("Engineering") },
      }),
    );
    // Nothing the engine has not derived for this draft is named, and nothing
    // claims a check is running: that is the toolbar's to say.
    expect(leadOrManager(seatKey("dev")).textContent).toBe("Manager after the check");
    expect(leadOrManager(unitKey("Engineering")).textContent).toBe("Lead after the check");
  });

  test("a human seat is marked by its dashed avatar and its kind, not a colour", () => {
    const doc = fixtureCompany();
    doc.roles![0] = { name: "CEO", kind: "human", contact: { slack: "U0CEO" } };
    mount(checkedEdit(doc));
    const ceo = rowOf(seatKey("ceo"));
    expect(ceo.querySelector(".avatar.human")).not.toBeNull();
    expect(within(ceo).getByText("Human seat")).toBeDefined();
  });
});

describe("keys", () => {
  test("rows and cells: Down and Up keep the column, Right steps in, Left steps out and climbs", () => {
    const { probe } = mount();
    rowOf(COMPANY_KEY).focus();
    press("ArrowDown");
    expect(focusedRow()).toBe(seatKey("ceo"));
    expect(probe.selection).toBe(seatKey("ceo"));
    press("ArrowRight");
    expect([focusedRow(), focusedColumn()]).toEqual([seatKey("ceo"), "1"]);
    press("ArrowRight");
    press("ArrowRight");
    expect(focusedColumn()).toBe("3");
    // One tab stop in the grid: the active cell.
    const tree = screen.getByRole("treegrid");
    expect([...tree.querySelectorAll<HTMLElement>("[tabindex='0']")]).toEqual([
      document.activeElement,
    ]);
    press("ArrowDown");
    expect([focusedRow(), focusedColumn()]).toEqual([unitKey("Engineering"), "3"]);
    press("ArrowRight");
    // The lead column holds the lead choice: the control takes focus.
    expect(document.activeElement?.tagName).toBe("BUTTON");
    expect(document.activeElement?.textContent).toBe("VP Engineering");
    // ArrowDown on that button moves the grid rather than opening the menu.
    press("ArrowDown");
    expect(screen.queryByRole("menu")).toBeNull();
    expect([focusedRow(), focusedColumn()]).toEqual([seatKey("vp-engineering"), "4"]);
    press("Home");
    expect(focusedColumn()).toBe("1");
    press("ArrowLeft");
    expect([focusedRow(), focusedColumn()]).toEqual([seatKey("vp-engineering"), null]);
    press("ArrowLeft");
    expect(focusedRow()).toBe(unitKey("Engineering"));
    press("ArrowLeft");
    expect(rowOf(unitKey("Engineering")).getAttribute("aria-expanded")).toBe("false");
    press("ArrowRight");
    expect(rowOf(unitKey("Engineering")).getAttribute("aria-expanded")).toBe("true");
    press("End");
    expect(focusedRow()).toBe(`add:${COMPANY_KEY}`);
    // An add row names no node, so reaching it leaves the selection where it
    // was: the selection is what the toolbar acts on and the URL names.
    expect(probe.selection).toBe(unitKey("Engineering"));
  });

  test("Enter edits, Delete and Backspace delete but never the company, ContextMenu opens the actions", () => {
    const { spies } = mount();
    rowOf(seatKey("dev")).focus();
    press("Enter");
    expect(spies.openEditor).toHaveBeenCalledWith(seatKey("dev"));
    press("Backspace");
    press("Delete");
    expect(spies.openDelete.mock.calls).toEqual([[seatKey("dev")], [seatKey("dev")]]);
    rowOf(COMPANY_KEY).focus();
    press("Delete");
    expect(spies.openDelete).toHaveBeenCalledTimes(2);

    rowOf(unitKey("Sales")).focus();
    press("ContextMenu");
    const menu = screen.getByRole("menu", { name: "Actions for Sales" });
    fireEvent.click(within(menu).getByRole("menuitem", { name: /Move to/ }));
    expect(spies.openMove).toHaveBeenCalledWith(unitKey("Sales"));
    expect(document.activeElement).toBe(rowOf(unitKey("Sales")));
  });

  test("a press keeps the one tab stop where focus actually went", () => {
    const { probe } = mount();
    const tabStops = () => [
      ...screen.getByRole("treegrid").querySelectorAll<HTMLElement>("[tabindex='0']"),
    ];
    // The chevron is pointer-only and hidden from assistive technology, so it
    // takes no focus: the row does.
    pointerPress("Collapse Engineering");
    expect(document.activeElement).toBe(rowOf(unitKey("Engineering")));
    expect(rowOf(unitKey("Engineering")).getAttribute("aria-expanded")).toBe("false");
    expect(probe.selection).toBe(unitKey("Engineering"));
    expect(tabStops()).toEqual([document.activeElement]);

    // A menu's trigger keeps the focus the press gave it and hands it back on
    // Escape, so the tab stop is that cell.
    pointerPress("Actions for Sales");
    expect(screen.getByRole("menu", { name: "Actions for Sales" })).toBeDefined();
    fireEvent.keyDown(document.activeElement!, { key: "Escape" });
    expect(document.activeElement?.getAttribute("aria-label")).toBe("Actions for Sales");
    expect(tabStops()).toEqual([document.activeElement]);
  });

  test("an open menu keeps its own keys: the grid's navigation never reaches into it", () => {
    mount();
    const lead = within(rowOf(unitKey("Engineering"))).getByRole("button", {
      name: "VP Engineering",
    });
    fireEvent.click(lead);
    const menu = screen.getByRole("menu", { name: "Lead of Engineering" });
    const answers = within(menu).getAllByRole("menuitemradio");
    expect(document.activeElement).toBe(answers[0]);
    press("ArrowDown");
    expect(document.activeElement).toBe(answers[1]);
    press("End");
    expect(within(menu).getByRole("menuitem", { name: /Choose another seat/ })).toBe(
      document.activeElement,
    );
    // The grid moved nowhere, and the menu is still open.
    expect(screen.getByRole("menu", { name: "Lead of Engineering" })).toBe(menu);
  });

  test("a row's menus open over the grid, outside the box that scrolls, and follow its scroll", () => {
    mount();
    const wrap = document.querySelector<HTMLElement>(".boutline-wrap")!;
    // A box that scrolls sideways clips on both axes, so a menu drawn inside
    // it under a trigger in the last rows would be cut off.
    const actions = within(rowOf(unitKey("Sales"))).getByRole("button", {
      name: "Actions for Sales",
    });
    fireEvent.click(actions);
    const menu = screen.getByRole("menu", { name: "Actions for Sales" });
    expect(wrap.contains(menu)).toBe(false);
    expect(menu.closest(".popup-layer")).not.toBeNull();
    // Scrolled sideways until its trigger has left the frame, the menu closes
    // rather than float beside a row nobody can see.
    actions.getBoundingClientRect = () =>
      DOMRect.fromRect({ x: 5000, y: 0, width: 24, height: 24 });
    fireEvent.scroll(wrap);
    expect(screen.queryByRole("menu")).toBeNull();

    fireEvent.click(
      within(rowOf(unitKey("Engineering"))).getByRole("button", { name: "VP Engineering" }),
    );
    const lead = screen.getByRole("menu", { name: "Lead of Engineering" });
    expect(wrap.contains(lead)).toBe(false);
  });

  test("an add row's buttons ask for their kind under their parent", () => {
    const { spies } = mount();
    const add = rowOf(`add:${unitKey("Platform")}`);
    expect(add.getAttribute("aria-label")).toBe("Add to Platform");
    add.focus();
    press("Enter");
    expect(document.activeElement?.textContent).toBe("Add agent seat");
    press("ArrowRight");
    fireEvent.click(document.activeElement!);
    expect(spies.openAdd).toHaveBeenLastCalledWith(unitKey("Platform"), "human");
    fireEvent.click(within(rowOf(`add:${COMPANY_KEY}`)).getByRole("button", { name: "Add unit" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });
});

describe("reorder", () => {
  /** Two seats that both manage a third: whichever comes first is its primary manager. */
  const doc: CompanyDocument = {
    name: "Pair",
    units: [
      {
        name: "Ops",
        roles: [
          { name: "Alpha", manages: ["Cora"] },
          { name: "Beta", manages: ["Cora"] },
          { name: "Cora" },
        ],
      },
    ],
  };
  const primary = (manager: string): DerivedOverrides => ({
    seats: { "units[0].roles[2]": { manager, managers: ["alpha", "beta"] } },
  });

  test("Alt+Up moves a row before its sibling and announces the primary manager change the check reports", () => {
    const { probe, spies } = mount(checkedEdit(doc, primary("alpha")));
    rowOf(seatKey("beta")).focus();
    press("ArrowUp", { altKey: true });
    const ops = probe.state.draft.units[0]!.roles.map((r) => r.data.name);
    expect(ops).toEqual(["Beta", "Alpha", "Cora"]);
    expect(focusedRow()).toBe(seatKey("beta"));
    expect(spies.announce).not.toHaveBeenCalled();

    // The engine's answer for the reordered draft: Beta is listed first now.
    const reordered = toDocument(probe.state.draft).document;
    act(() =>
      probe.dispatch({
        type: "checked",
        settled: {
          generation: probe.state.generation,
          sent: toDocument(probe.state.draft),
          baseRevision: probe.state.base.revision,
          outcome: {
            status: "clean",
            warnings: [],
            derived: fixtureDerived(reordered, {
              seats: { "units[0].roles[2]": { manager: "beta", managers: ["beta", "alpha"] } },
            }),
          },
        },
      }),
    );
    expect(spies.announce).toHaveBeenCalledWith(
      "The new order changes a primary manager. Cora now reports to Beta instead of Alpha.",
    );
  });

  test("Alt+Down moves a row past the sibling below it and follows it", () => {
    const { probe } = mount(checkedEdit(doc, primary("alpha")));
    rowOf(seatKey("alpha")).focus();
    press("ArrowDown", { altKey: true });
    expect(probe.state.draft.units[0]!.roles.map((r) => r.data.name)).toEqual([
      "Beta",
      "Alpha",
      "Cora",
    ]);
    expect(focusedRow()).toBe(seatKey("alpha"));
  });

  test("a reorder that changes no reporting line announces nothing, and the ends and placed seats refuse", () => {
    const { probe, spies } = mount(checkedEdit(doc, primary("alpha")));
    rowOf(seatKey("cora")).focus();
    press("ArrowUp", { altKey: true });
    const reordered = toDocument(probe.state.draft).document;
    act(() =>
      probe.dispatch({
        type: "checked",
        settled: {
          generation: probe.state.generation,
          sent: toDocument(probe.state.draft),
          baseRevision: probe.state.base.revision,
          outcome: {
            status: "clean",
            warnings: [],
            // Cora is second now, and Alpha, still listed first, still manages it.
            derived: fixtureDerived(reordered, {
              seats: { "units[0].roles[1]": { manager: "alpha", managers: ["alpha", "beta"] } },
            }),
          },
        },
      }),
    );
    expect(spies.announce).not.toHaveBeenCalled();

    rowOf(seatKey("alpha")).focus();
    press("ArrowUp", { altKey: true });
    expect(spies.announce).toHaveBeenLastCalledWith("Alpha is already the first seat in Ops.");
    rowOf(seatKey("beta")).focus();
    press("ArrowDown", { altKey: true });
    expect(spies.announce).toHaveBeenLastCalledWith("Beta is already the last seat in Ops.");

    cleanup();
    const placed = mount();
    rowOf(seatKey("designer")).focus();
    press("ArrowDown", { altKey: true });
    expect(placed.spies.announce).toHaveBeenLastCalledWith(
      "Designer is placed in this unit by its unit reference. Move it into the unit to reorder it.",
    );
  });

  test("a change recorded before the check answers leaves the reorder's own effect untold", () => {
    const { probe, spies } = mount(checkedEdit(doc, primary("alpha")));
    rowOf(seatKey("beta")).focus();
    press("ArrowUp", { altKey: true });
    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "renameSeat", target: seatKey("cora"), name: "Corinne" },
      }),
    );
    const next = toDocument(probe.state.draft).document;
    act(() =>
      probe.dispatch({
        type: "checked",
        settled: {
          generation: probe.state.generation,
          sent: toDocument(probe.state.draft),
          baseRevision: probe.state.base.revision,
          outcome: {
            status: "clean",
            warnings: [],
            derived: fixtureDerived(next, {
              seats: {
                "units[0].roles[2]": {
                  handle: "cora",
                  manager: "beta",
                  managers: ["beta", "alpha"],
                },
              },
            }),
          },
        },
      }),
    );
    // That answer describes the rename too; the Review lists both.
    expect(spies.announce).not.toHaveBeenCalled();
  });

  test("a root seat moves past the root seat drawn beside it, never one drawn inside a unit", () => {
    // Designer is a root seat the engine placed in Platform: the company's
    // list holds it between CEO and Advisor, the outline draws it in Platform.
    const withAdvisor = fixtureCompany();
    withAdvisor.roles!.push({ name: "Advisor" });
    const rootNames = (p: HarnessProbe) => p.state.draft.roles.map((r) => r.data.name);

    const down = mount(checkedEdit(withAdvisor));
    rowOf(seatKey("ceo")).focus();
    press("ArrowDown", { altKey: true });
    expect(rootNames(down.probe)).toEqual(["Designer", "Advisor", "CEO"]);
    cleanup();

    const up = mount(checkedEdit(withAdvisor));
    rowOf(seatKey("advisor")).focus();
    press("ArrowUp", { altKey: true });
    expect(rootNames(up.probe)).toEqual(["Advisor", "CEO", "Designer"]);
    cleanup();

    // Up past a row with a sibling before it lands straight before that row.
    const withBoard = fixtureCompany();
    withBoard.roles!.push({ name: "Advisor" }, { name: "Board" });
    const board = mount(checkedEdit(withBoard));
    rowOf(seatKey("board")).focus();
    press("ArrowUp", { altKey: true });
    expect(rootNames(board.probe)).toEqual(["CEO", "Designer", "Board", "Advisor"]);
    cleanup();

    // With nothing drawn below it at the root, CEO is already last there.
    const alone = mount();
    rowOf(seatKey("ceo")).focus();
    press("ArrowDown", { altKey: true });
    expect(alone.probe.state.log.ops).toEqual([]);
    expect(alone.spies.announce).toHaveBeenLastCalledWith(
      "CEO is already the last seat in the company.",
    );
  });

  test("a chord with Ctrl or Command is not a reorder", () => {
    const { probe } = mount(checkedEdit(doc, primary("alpha")));
    rowOf(seatKey("beta")).focus();
    press("ArrowUp", { altKey: true, ctrlKey: true });
    press("ArrowUp", { altKey: true, metaKey: true });
    expect(probe.state.log.ops).toEqual([]);
  });

  test("read-only, Alt+Down reorders nothing", () => {
    const { probe } = mount(checkedEdit(doc, primary("alpha")), { readOnly: true });
    rowOf(seatKey("alpha")).focus();
    press("ArrowDown", { altKey: true });
    expect(probe.state.log.ops).toEqual([]);
  });
});

describe("focus", () => {
  test("the Builder's focus opens a collapsed unit and lands on the row; expand and collapse all", () => {
    const { probe } = mount();
    act(() => probe.view!.collapseAll());
    expect(screen.queryAllByText("Dev")).toHaveLength(0);
    act(() => probe.view!.focusNode(seatKey("dev")));
    expect(focusedRow()).toBe(seatKey("dev"));
    act(() => probe.view!.expandAll());
    expect(rowOf(unitKey("Platform")).getAttribute("aria-expanded")).toBe("true");
  });

  test("a focus request lands now even on the active row, and never on a later render", () => {
    const spies = builderSpies();
    const probe = harnessProbe();
    const view = (agents: AgentRow[] = []) => (
      <>
        <button type="button">Undo</button>
        <BuilderHarness
          initial={checkedEdit(fixtureCompany())}
          spies={spies}
          probe={probe}
          agents={agents}
        >
          <OutlineView />
        </BuilderHarness>
      </>
    );
    const { rerender } = render(view());
    const toolbar = screen.getByRole("button", { name: "Undo" });
    rowOf(COMPANY_KEY).focus();
    press("ArrowDown");
    expect(focusedRow()).toBe(seatKey("ceo"));

    // An undo from the toolbar asks for the row that is already active.
    toolbar.focus();
    act(() => probe.view!.focusNode(seatKey("ceo")));
    expect(document.activeElement).toBe(rowOf(seatKey("ceo")));

    // Once taken, the request is spent: a live push later leaves focus alone.
    toolbar.focus();
    rerender(view([{ id: "a1", role: "Dev", handle: "dev", state: "working" }]));
    expect(document.activeElement).toBe(toolbar);
  });
});
