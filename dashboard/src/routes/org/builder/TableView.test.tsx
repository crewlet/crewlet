/**
 * The builder's table, which is the organization as an indented table of rows.
 *
 * What these protect:
 * - the rows are the TREE, in the chart's own order, and where a node sits is
 *   its level rather than a path written out on every line;
 * - a row says what the engine knows about it: its kind, the handle it runs
 *   under, who leads or manages it, and what the last dry run placed on it;
 * - a row acts through the same one list of a node's actions the chart's cards
 *   offer, with Edit, Delete and the three kinds a unit can take also drawn as
 *   the controls the console puts on a row, and pressing a row selects it;
 * - a row moves among the siblings it is drawn beside, by Alt with an arrow
 *   and from its own menu, and only where that move is a place to write;
 * - read-only refuses every change and still offers every reading;
 * - the table opens and closes its own hierarchy, and registers itself as the
 *   view the Builder focuses a node in.
 *
 * The table itself is the design system's: its rows, cells, keys, wires, name
 * group, add pill and row controls are covered by that package's suite, and
 * nothing here asserts them again.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { menuEntryLabel } from "~/testing.tsx";
import { fixtureCompany } from "./model/testkit.ts";
import { checkedEdit } from "./testState.ts";
import { TableView } from "./TableView.tsx";
import { builderSpies, BuilderHarness, harnessProbe } from "./viewTestkit.tsx";
import { render } from "@testing-library/react";

afterEach(() => {
  cleanup();
  location.hash = "";
});

beforeEach(() => {
  location.hash = "#/org?lens=builder&view=table";
});

function mount(options: { readOnly?: boolean } = {}) {
  const spies = builderSpies();
  const probe = harnessProbe();
  const rendered = render(
    <BuilderHarness
      initial={checkedEdit(fixtureCompany())}
      spies={spies}
      probe={probe}
      readOnly={options.readOnly}
    >
      <TableView />
    </BuilderHarness>,
  );
  return {
    ...rendered,
    spies,
    state: () => probe.state,
    selected: () => probe.selection,
    view: () => probe.view,
  };
}

/**
 * Every body row's name, in the order the table draws them.
 *
 * The name's own element rather than the whole cell: the cell also carries the
 * word for what kind of thing the row is.
 */
function names(): string[] {
  return screen
    .getAllByRole("row")
    .flatMap((row) => [...row.querySelectorAll<HTMLElement>(".btable-label")])
    .map((name) => name.textContent?.trim() ?? "");
}

/** The row whose NAME cell says `name`, not whichever row mentions it. */
function row(name: string): HTMLElement {
  const found = screen
    .getAllByRole("row")
    .find((candidate) =>
      [...candidate.querySelectorAll<HTMLElement>(".btable-name")].some(
        (cell) => within(cell).queryAllByText(name, { exact: true }).length > 0,
      ),
    );
  if (!found) throw new Error(`no row named ${name}: ${names().join(", ")}`);
  return found;
}

/** Opens a row's own actions menu and returns each entry with whether it is refused. */
function actions(name: string): [string, boolean][] {
  fireEvent.click(within(row(name)).getByRole("button", { name: "Row actions" }));
  const menu = screen.getByRole("menu", { name: "Row actions" });
  return within(menu)
    .getAllByRole("menuitem")
    .map((item) => [menuEntryLabel(item), item.getAttribute("aria-disabled") === "true"]);
}

/** Every cell of a row, as words. A treegrid's cells are `gridcell`s. */
const cells = (name: string) =>
  within(row(name))
    .getAllByRole("gridcell")
    .map((cell) => cell.textContent?.replace(/\s+/g, " ").trim() ?? "");

describe("the rows", () => {
  /*
   * THE CHART'S OWN ORDER, which is the document's: the company, then its root
   * seats and units as the company writes them, each unit's own rows directly
   * under it. This is the order the visualization draws, so a reader arriving
   * from it finds the organization already assembled.
   */
  test("every seat and unit is a row, in the order the chart draws them", () => {
    mount();
    expect(names()).toEqual([
      "Acme",
      "CEO",
      "Engineering",
      "VP Engineering",
      "Dev",
      "Platform",
      // A unit's OWN seats first, then the root seats a `unit:` reference
      // placed in it, which is the order the chart stacks them in.
      "SRE",
      "Designer",
      "Sales",
      "Account Executive",
    ]);
  });

  /*
   * WHERE A NODE SITS IS ITS LEVEL. The table drew a path column and a flat
   * list once ("Engineering / Platform" on every row), which is a sentence a
   * reader has to reassemble where an indent is the answer already drawn.
   */
  test("the hierarchy is the row's own level", () => {
    mount();
    expect(row("Acme").getAttribute("aria-level")).toBe("1");
    expect(row("Engineering").getAttribute("aria-level")).toBe("2");
    expect(row("Platform").getAttribute("aria-level")).toBe("3");
    expect(row("SRE").getAttribute("aria-level")).toBe("4");
    expect(names().some((name) => name.includes("/"))).toBe(false);
  });

  test("a row says its kind, its handle and who leads or manages it", () => {
    mount();
    expect(cells("Dev")).toEqual(expect.arrayContaining(["Agent seat", "@dev"]));
    expect(cells("Engineering")).toEqual(
      expect.arrayContaining(["department", "VP Engineering", "Not a seat"]),
    );
  });
});

describe("opening and closing the hierarchy", () => {
  /* Its own, because the lens toolbar's pair belongs to the visualization. */
  test("Collapse all closes a unit's rows and Expand all brings them back", async () => {
    mount();
    fireEvent.click(screen.getByRole("button", { name: "Collapse all" }));
    await waitFor(() => expect(names()).not.toContain("SRE"));
    fireEvent.click(screen.getByRole("button", { name: "Expand all" }));
    await waitFor(() => expect(names()).toContain("SRE"));
  });

  /* WHICH node to focus after an operation is the Builder's decision, and
     performing it belongs to whichever view is mounted. Without this an add
     made from the table leaves focus nowhere. */
  test("the table registers itself as the view the Builder focuses a node in", () => {
    const { view } = mount();
    act(() => view()!.focusNode("seat:sre"));
    expect(document.activeElement).toBe(row("SRE"));
  });
});

describe("acting on a row", () => {
  test("a row offers the same actions its card on the chart offers", () => {
    mount();
    expect(actions("Dev").map(([label]) => label)).toEqual([
      "Edit",
      "Open seat",
      "Edit reports",
      "Change to human seat",
      "Move to",
      "Delete",
      "Move up",
      "Move down",
    ]);
  });

  /* The two the console's table draws on a row itself, so the reader reaches
     the two they reach for without opening a menu. */
  test("Edit and Delete are also the row's own controls", () => {
    const { spies } = mount();
    fireEvent.click(within(row("Dev")).getByRole("button", { name: "Edit Dev" }));
    expect(spies.openEditor).toHaveBeenLastCalledWith("seat:dev");
    fireEvent.click(within(row("Dev")).getByRole("button", { name: "Delete Dev" }));
    expect(spies.openDelete).toHaveBeenLastCalledWith("seat:dev");
  });

  /* And the company, which can be edited and never deleted. */
  test("the company row offers no Delete control", () => {
    mount();
    expect(within(row("Acme")).queryByRole("button", { name: /^Delete/ })).toBeNull();
  });

  test("the add pill offers every kind under a unit, and under the company too", async () => {
    const { spies } = mount();
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add human seat" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith("unit:Engineering", "human");

    fireEvent.click(within(row("Acme")).getByRole("button", { name: "Add to Acme" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add unit" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });

  /* A seat takes no children, so it is offered no plus rather than a plus that
     would refuse. */
  test("a seat's row has no add pill", () => {
    mount();
    expect(within(row("Dev")).queryByRole("button", { name: /^Add to/ })).toBeNull();
  });

  test("pressing a row selects its node, which is what the toolbar acts on", () => {
    const { selected } = mount();
    fireEvent.click(row("SRE"));
    expect(selected()).toBe("seat:sre");
  });

  /* The same two keys the canvas gives a card, while the row itself holds
     focus, so what an operator learned there holds here. */
  test("Enter edits the row and Delete removes it", () => {
    const { spies } = mount();
    fireEvent.keyDown(row("Dev"), { key: "Enter" });
    expect(spies.openEditor).toHaveBeenLastCalledWith("seat:dev");
    fireEvent.keyDown(row("Dev"), { key: "Delete" });
    expect(spies.openDelete).toHaveBeenLastCalledWith("seat:dev");
  });

  /* Read-only is a draft nobody may change, so every change refuses rather
     than disappearing: a menu whose entries come and go is a menu nobody
     learns, and an operator has to be able to see that Delete exists. */
  test("read-only refuses every change and still offers every reading", async () => {
    const { spies } = mount({ readOnly: true });
    expect(actions("Dev")).toEqual([
      ["Edit", false],
      ["Open seat", false],
      ["Edit reports", false],
      ["Change to human seat", true],
      ["Move to", true],
      ["Delete", true],
    ]);
    fireEvent.keyDown(screen.getByRole("menu", { name: "Row actions" }), { key: "Escape" });
    expect(
      within(row("Dev")).getByRole("button", { name: "Delete Dev" }).getAttribute("aria-disabled"),
    ).toBe("true");
    // The plus still opens, and says the kinds are unavailable rather than
    // offering nothing at all.
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    const add = await screen.findByRole("button", { name: "Add unit" });
    expect(add.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(add);
    expect(spies.openAdd).not.toHaveBeenCalled();
  });
});

describe("moving a row among its siblings", () => {
  /*
   * A SEAT'S PLACE AMONG ITS SIBLINGS DECIDES ITS PRIMARY MANAGER, since the
   * engine's is the first seat that lists it, so the move writes a real change
   * to the document rather than reordering a view.
   */
  test("moving a seat up writes it before the sibling it passes", () => {
    const { state } = mount();
    fireEvent.click(within(row("Dev")).getByRole("button", { name: "Row actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Move up" }));
    expect(names().slice(3, 5)).toEqual(["Dev", "VP Engineering"]);
    expect(state().draft.units[0]!.roles.map((seat) => seat.data.name)).toEqual([
      "Dev",
      "VP Engineering",
    ]);
  });

  /* Alt with an arrow, which is the move without opening anything: the same
     operation the menu records. */
  test("Alt and an arrow move the row the keyboard is on", () => {
    const { state } = mount();
    fireEvent.keyDown(row("Dev"), { key: "ArrowUp", altKey: true });
    expect(state().draft.units[0]!.roles.map((seat) => seat.data.name)).toEqual([
      "Dev",
      "VP Engineering",
    ]);
  });

  /* A root seat the engine placed in a unit by its `unit:` reference lives in
     the COMPANY's list, so its place among the rows it is drawn beside is not
     a place to write. It is offered no move rather than a move that writes
     somewhere else. */
  test("a seat a unit reference placed is offered no move at all", () => {
    mount();
    const labels = actions("Designer").map(([label]) => label);
    expect(labels).not.toContain("Move up");
    expect(labels).not.toContain("Move down");
    expect(labels).toContain("Move to");
  });

  test("the company is never moved", () => {
    mount();
    const labels = actions("Acme").map(([label]) => label);
    expect(labels).not.toContain("Move up");
  });
});
