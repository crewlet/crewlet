/**
 * The builder's table, which is the organization as an indented table of rows.
 *
 * What these protect:
 * - the rows are the TREE, in the chart's own order, and where a node sits is
 *   its level rather than a path written out on every line;
 * - a row says what the engine knows about it: what it IS, once, under its
 *   name and in the chart's own wording; the handle it runs under; who leads
 *   or manages it; and what the last dry run placed on it, with a cell that
 *   cannot hold a value saying so once in the design system's own mark;
 * - a row acts through the same one list of a node's actions the chart's cards
 *   offer, and EACH ACTION HAS ONE OWNER: the add pill, the pencil and the
 *   trash the console draws on a row are the row's, the menu carries what has
 *   no button of its own, a row whose menu would be empty draws none, and
 *   pressing a row selects it;
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

import { drawnClasses, drawnPart, insidePart, menuEntryLabel, orgTableParts } from "~/testing.tsx";
import { Tag } from "@crewlethq/ui";
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
  location.hash = "#/company?lens=builder&view=table";
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

/*
 * What the design system calls the parts of a row, asked of the design system
 * rather than spelled here: a suite that names a package class is a test of
 * the package, and it goes green again the moment the package renames it.
 */
const PART = orgTableParts();

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

/** The row's own actions menu, named after the node it acts on, or nothing. */
const menuTrigger = (name: string) =>
  within(row(name)).queryByRole("button", { name: `Actions for ${name}` });

/** Opens a row's own actions menu and returns each entry with whether it is refused. */
function actions(name: string): [string, boolean][] {
  fireEvent.click(menuTrigger(name)!);
  const menu = screen.getByRole("menu", { name: `Actions for ${name}` });
  return within(menu)
    .getAllByRole("menuitem")
    .map((item) => [menuEntryLabel(item), item.getAttribute("aria-disabled") === "true"]);
}

/** Every control the row draws, by its accessible name. */
const strip = (name: string): string[] =>
  within(row(name))
    .getAllByRole("button")
    .map((control) => control.getAttribute("aria-label") ?? control.textContent ?? "");

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
    expect(cells("Dev")[0]).toMatch(/^DevAgent seat/);
    expect(cells("Dev")).toEqual(expect.arrayContaining(["@dev"]));
    expect(cells("Engineering")).toEqual(
      expect.arrayContaining(["EngineeringDepartment", "VP Engineering"]),
    );
  });

  /*
   * ONCE. What a row IS was written in the name cell's caption AND in a column
   * of its own, so every row said "Agent seat" twice about 350px apart and the
   * column cost 145px of a 1269px table to do it. The caption is the one to
   * keep: it is what the console's table draws and what the chart's own cards
   * draw.
   */
  test("a row writes what it is once, under its name, and no column repeats it", () => {
    mount();
    const headers = screen
      .getAllByRole("columnheader")
      .map((head) => head.textContent?.trim() ?? "");
    expect(headers).toEqual(["Name", "Handle", "Lead or reports to", "Problems", "Actions"]);
    const said = (name: string, word: string) =>
      (cells(name).join(" ").match(new RegExp(word, "g")) ?? []).length;
    expect(said("Dev", "Agent seat")).toBe(1);
    expect(said("Acme", "Company")).toBe(1);
    expect(said("Engineering", "Department")).toBe(1);
  });

  /*
   * AND IT WRITES IT THE WAY THE CHART WRITES IT. Two functions computed one
   * string and disagreed: one draft's unit read "Team" on the card and "team"
   * on the row. The type is the founder's own word, so only its first letter
   * is touched.
   */
  test("a unit's own type is capitalised, as the chart capitalises it", () => {
    mount();
    expect(within(row("Engineering")).getByText("Department")).toBeDefined();
    expect(within(row("Engineering")).queryByText("department")).toBeNull();
  });

  /*
   * A CELL THAT CANNOT HOLD A VALUE SAYS SO ONCE, in the design system's own
   * mark. The company's row read "Not a seat" in one column and "Not a seat or
   * unit" in the next: two phrasings of one idea, written out as sentences, on
   * the first row a reader meets.
   */
  test("a cell that cannot hold a value draws the dash and speaks one reason", () => {
    mount();
    const company = row("Acme");
    const spoken = within(company).getAllByText("Not applicable");
    expect(spoken).toHaveLength(2);
    expect(cells("Acme")[1]).toBe("–Not applicable");
    expect(within(company).queryByText(/Not a seat/)).toBeNull();
  });

  /*
   * A ROW IS A RANK TALL, so a wiring mark is a glyph and never a tag: one tag
   * measured on a live row took the caption from 13.2px to 20 and the row with
   * it, which made a seat's height depend on its wiring. The sentence is the
   * glyph's own name and its tooltip, so nothing is carried by the drawing
   * alone.
   */
  test("a wiring mark on a row is a glyph carrying its sentence, not a tag", () => {
    mount();
    const caption = drawnPart(row("Designer"), PART.caption)!;
    const mark = caption.querySelector(".bnode-mark")!;
    expect(mark.getAttribute("title")).toBe("Declared at the root with a unit reference");
    expect(within(mark as HTMLElement).getByRole("img")).toBeDefined();
    // A tag is the row's own height, so one on this line is a row whose
    // height depends on its wiring.
    const tag = drawnClasses(Tag, { children: "x" })[0]!;
    expect(drawnPart(caption, tag)).toBeNull();
  });
});

describe("opening and closing the hierarchy", () => {
  /*
   * THE PAIR IS THE LENS TOOLBAR'S, ON BOTH VIEWS, so this table draws none of
   * its own (`controls={false}`): the design system's pair over the table and
   * the toolbar's pair above it were one action with two implementations, each
   * hidden on the view where the other was drawn, so the control moved 800px
   * when a reader changed view. What is asserted here is that the table draws
   * neither and still opens and closes through the handle it registers, which
   * is what the toolbar's buttons call.
   */
  test("the table draws no controls of its own and closes through its handle", async () => {
    const { view } = mount();
    expect(screen.queryByRole("button", { name: "Collapse all" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Expand all" })).toBeNull();
    act(() => view()!.collapseAll());
    await waitFor(() => expect(names()).not.toContain("SRE"));
    act(() => view()!.expandAll());
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
  /*
   * THE MENU CARRIES WHAT THE ROW DOES NOT DRAW, and the row draws the three
   * the console draws. Both offered all eight before: the strip's Edit and
   * Delete opened and closed the menu beside it, and the menu began with the
   * three adds the pill already offers.
   */
  test("a row's menu is the actions it does not draw as a control of its own", () => {
    mount();
    expect(actions("Dev").map(([label]) => label)).toEqual([
      "Open seat",
      "Edit reports",
      "Change to human seat",
      "Move to",
      "Move up",
      "Move down",
    ]);
  });

  /*
   * NO ACTION HAS TWO OWNERS ON ONE ROW. Asserted over every row rather than
   * over one, because the duplication was per node kind: the seat's row
   * repeated Edit and Delete and the company's repeated Edit and all three
   * adds.
   */
  test("no label is both a control the row draws and an entry of its own menu", () => {
    mount();
    for (const name of names()) {
      if (menuTrigger(name) === null) continue;
      const drawn = strip(name).map((label) => label.replace(` ${name}`, "").replace(/ to .*/, ""));
      for (const [entry] of actions(name)) {
        expect(drawn, `${name}: ${entry}`).not.toContain(entry);
      }
      fireEvent.keyDown(screen.getByRole("menu", { name: `Actions for ${name}` }), {
        key: "Escape",
      });
    }
  });

  /*
   * AND A ROW WHOSE MENU WOULD BE EMPTY DRAWS NONE. Everything the company can
   * take is a button on its own row, so its menu opened onto a list that
   * repeated them and nothing else.
   */
  test("the company's row draws the controls and no menu at all", () => {
    mount();
    expect(strip("Acme")).toEqual(["Add to Acme", "Edit Acme"]);
    expect(menuTrigger("Acme")).toBeNull();
    expect(within(row("Acme")).queryByRole("button", { name: /^Delete/ })).toBeNull();
  });

  /*
   * NAMED AFTER THE ROW IT ACTS ON, as the chart names the same control. Every
   * one said "Row actions", so a reader meeting them in a column met several
   * identically named controls and none of them said which row it would act
   * on.
   */
  test("every row's menu is named after that row", () => {
    mount();
    const named = names()
      .map((name) => menuTrigger(name)?.getAttribute("aria-label"))
      .filter((label): label is string => label !== undefined && label !== null);
    expect(named.length).toBeGreaterThan(1);
    expect(new Set(named).size).toBe(named.length);
    for (const label of named) expect(label).toMatch(/^Actions for /);
  });

  /* The two the console's table draws on a row itself, so the reader reaches
     the two they reach for without opening a menu. */
  test("Edit and Delete are the row's own controls", () => {
    const { spies } = mount();
    fireEvent.click(within(row("Dev")).getByRole("button", { name: "Edit Dev" }));
    expect(spies.openEditor).toHaveBeenLastCalledWith("seat:dev");
    fireEvent.click(within(row("Dev")).getByRole("button", { name: "Delete Dev" }));
    expect(spies.openDelete).toHaveBeenLastCalledWith("seat:dev");
  });

  test("the add pill offers every kind under a unit, and under the company too", async () => {
    const { spies } = mount();
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add human seat to Engineering" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith("unit:Engineering", "human");

    fireEvent.click(within(row("Acme")).getByRole("button", { name: "Add to Acme" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add unit to Acme" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });

  /*
   * ONE LIST, ONE SET OF MARKS. The pill drew a folder and a robot from an
   * array of its own while the chart drew a tree and the Crewlet mark for the
   * same two actions, so "Add agent seat" wore one drawing on the row and
   * another everywhere else. Read from what is rendered: the pill's agent
   * section carries the mark every agent seat on this table already wears.
   */
  test("the pill's marks are the chart's own, from the one add list", async () => {
    mount();
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    const agent = await screen.findByRole("button", { name: "Add agent seat to Engineering" });
    const drawing = (element: Element) => element.querySelector("svg")?.innerHTML ?? "";
    expect(drawing(agent)).not.toBe("");
    expect(drawing(agent)).toBe(drawing(drawnPart(row("Dev"), PART.icon)!));
  });

  /*
   * AND THE PLUS IS DRAWN AT REST. Inside the strip it took the strip's own
   * `opacity: 0`, so the one control an operator reaches for by its colour
   * could not be seen until the pointer was already on the row.
   */
  test("the add is a sibling of the quiet strip, never inside it", () => {
    mount();
    const add = within(row("Engineering")).getByRole("button", { name: "Add to Engineering" });
    expect(insidePart(add, PART.actions)).toBe(false);
    expect(
      insidePart(
        within(row("Engineering")).getByRole("button", { name: "Edit Engineering" }),
        PART.actions,
      ),
    ).toBe(true);
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
    /*
     * READ-ONLY DISABLES, IT DOES NOT HIDE, and that holds for the two moves
     * as well: a seat with siblings to pass is offered both, refused. They
     * used to vanish while Move to beside them stayed and said it was
     * unavailable, which told an operator this seat could not be reordered at
     * all.
     */
    expect(actions("Dev")).toEqual([
      ["Open seat", false],
      ["Edit reports", false],
      ["Change to human seat", true],
      ["Move to", true],
      ["Move up", true],
      ["Move down", true],
    ]);
    fireEvent.keyDown(screen.getByRole("menu", { name: "Actions for Dev" }), { key: "Escape" });
    expect(
      within(row("Dev")).getByRole("button", { name: "Delete Dev" }).getAttribute("aria-disabled"),
    ).toBe("true");
    // The plus still opens, and says the kinds are unavailable rather than
    // offering nothing at all.
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    const add = await screen.findByRole("button", { name: "Add unit to Engineering" });
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
    fireEvent.click(menuTrigger("Dev")!);
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

  /* It has no menu at all, which is the same answer: the company is not a row
     that moves among siblings, and nothing on it offers to. */
  test("the company is never moved", () => {
    mount();
    expect(menuTrigger("Acme")).toBeNull();
  });
});
