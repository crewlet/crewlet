/**
 * The builder's table, and the four things it took over from the outline it
 * replaced.
 *
 * What these protect:
 * - every seat and unit is a row, and nothing has to be opened to reach one;
 * - the hierarchy is the "In" column, and the table OPENS in the chart's own
 *   order rather than in the alphabet's, so the rows read as the tree;
 * - a row acts through the same one list of a node's actions the chart's cards
 *   offer, and pressing a row selects that node for the toolbar;
 * - a row still moves among the siblings it is drawn beside, and that move is
 *   offered only while the order it means something in is the order on screen.
 *
 * The table itself is the design system's: its toolbar, sortable headers, row
 * shapes, settings frame and pager are covered by that package's suite, and
 * nothing here asserts them again.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { menuEntryLabel, narrow } from "~/testing.tsx";
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
  return { ...rendered, spies, state: () => probe.state, selected: () => probe.selection };
}

/**
 * Every body row's name, in the order the table draws them.
 *
 * The name's own element rather than the whole cell: an agent seat's avatar
 * draws its initials, so the cell's text reads "CCEO".
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

const cells = (name: string) =>
  within(row(name))
    .getAllByRole("cell")
    .map((cell) => cell.textContent?.replace(/\s+/g, " ").trim() ?? "");

describe("the rows", () => {
  /*
   * THE CHART'S OWN ORDER, which is the document's: the company, then its root
   * seats and units as the company writes them, each unit's own rows directly
   * under it. Sorted by name this would open on "Account Executive", and a
   * reader arriving from the visualization would have to rebuild the tree in
   * their head to find anything.
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
   * NOTHING IS OPENED TO REACH A ROW. The outline hid a unit's seats until its
   * row was expanded, so reaching SRE meant opening Engineering and then
   * Platform. Here every node is on screen from the first render.
   */
  test("a seat three levels down needs nothing opened to reach it", () => {
    mount();
    expect(row("SRE")).toBeDefined();
    expect(screen.queryByRole("button", { name: /^Expand/ })).toBeNull();
  });

  /** The hierarchy, as a column: where the node is, not how far it is indented. */
  test("the In column is the path, and the company's own name is not a step in it", () => {
    mount();
    expect(cells("SRE")).toContain("Engineering / Platform");
    expect(cells("Engineering")).toContain("The company");
    expect(cells("Acme")).toContain("The company");
  });

  test("a row says its kind, its handle and who leads or manages it", () => {
    mount();
    expect(cells("Dev")).toEqual(expect.arrayContaining(["Agent seat", "@dev"]));
    expect(cells("Engineering")).toEqual(
      expect.arrayContaining(["department", "VP Engineering", "Not a seat"]),
    );
  });
});

describe("narrowing", () => {
  test("the search box narrows by name, by handle and by unit", async () => {
    mount();
    const search = screen.getByRole("searchbox", { name: /Search the organization/ });
    fireEvent.change(search, { target: { value: "platform" } });
    // The unit itself, and the seats whose path runs through it.
    await waitFor(() => expect(names()).toEqual(["Platform", "SRE", "Designer"]));

    // A handle is written with its `@` where a reader sees it, so it is typed
    // with one too.
    fireEvent.change(search, { target: { value: "@sre" } });
    await waitFor(() => expect(names()).toEqual(["SRE"]));
  });

  test("the Kind axis narrows to units, and says so in the URL", async () => {
    mount();
    narrow("Kind", "Units");
    await waitFor(() => expect(names()).toEqual(["Engineering", "Platform", "Sales"]));
    // A narrowed table is a link somebody sends, and the parameter is named
    // after the view because the lens shares its URL with the chart.
    expect(location.hash).toContain("table.kind=unit");
  });

  test("nothing matching says which of the two empty tables this is", async () => {
    mount();
    fireEvent.change(screen.getByRole("searchbox", { name: /Search the organization/ }), {
      target: { value: "nobody" },
    });
    expect(await screen.findByText("No seat or unit matches these filters")).toBeDefined();
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

  test("a unit's row can add every kind under it, and the company's can too", () => {
    const { spies } = mount();
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Row actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Add human seat" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith("unit:Engineering", "human");

    fireEvent.click(within(row("Acme")).getByRole("button", { name: "Row actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Add unit" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });

  test("pressing a row selects its node, which is what the toolbar acts on", () => {
    const { selected } = mount();
    fireEvent.click(row("SRE"));
    expect(selected()).toBe("seat:sre");
  });

  /* Read-only is a draft nobody may change, so every change refuses rather
     than disappearing: a menu whose entries come and go is a menu nobody
     learns, and an operator has to be able to see that Delete exists. */
  test("read-only refuses every change and still offers every reading", () => {
    mount({ readOnly: true });
    expect(actions("Dev")).toEqual([
      ["Edit", false],
      ["Open seat", false],
      ["Edit reports", false],
      ["Change to human seat", true],
      ["Move to", true],
      ["Delete", true],
    ]);
  });
});

describe("moving a row among its siblings", () => {
  /*
   * THE MOVE IS ONLY MEANINGFUL IN THE TABLE'S OWN ORDER, because "up" means
   * "past the row above". A seat's place among its siblings decides its
   * primary manager, so a move that passed a row the reader cannot see would
   * change who reports to whom for no visible reason.
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

  test("sorted by anything else the move is refused, and says what would allow it", () => {
    mount();
    fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }).querySelector("button")!);
    const menu = actions("Dev");
    // The entries stay and say why: a menu whose entries come and go is a menu
    // nobody learns.
    expect(menu.filter(([label]) => label.startsWith("Move up"))).toEqual([
      ["Move upSort the table by In to move a row among the rows it is drawn beside.", true],
    ]);
    expect(menu.filter(([label]) => label.startsWith("Move down"))).toHaveLength(1);
    // "Move to" is a different action (it changes the parent) and is not one
    // of the two the order decides.
    expect(menu.every(([label, refused]) => !/^Move (up|down)/.test(label) || refused)).toBe(true);
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
