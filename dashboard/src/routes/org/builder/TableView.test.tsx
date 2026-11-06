/**
 * The builder's table, which is the organization as an indented table of rows.
 *
 * What these protect:
 * - the rows are the TREE, in the chart's own order, and where a node sits is
 *   its level rather than a path written out on every line;
 * - a row says what the engine knows about it: what it IS, once, under its
 *   name, with a seat's handle and a unit's size; who it reports to; and
 *   what the last dry run placed on it, with a cell that cannot hold a value
 *   saying so once in the design system's own mark;
 * - a row acts through the same one list of a node's actions the chart's cards
 *   offer, and EACH ACTION HAS ONE OWNER: the add pill, the pencil and the
 *   trash the console draws on a row are the row's, the menu carries what has
 *   no button of its own, a row whose menu would be empty draws none, and
 *   pressing a row selects it;
 * - a row moves among the siblings it is drawn beside, by Alt with an arrow
 *   and from its own menu, and only where that move is a place to write;
 * - read-only refuses every change and still offers every reading, and a
 *   reader who may not change the org finds every edit held with its reason;
 * - the table opens and closes its own hierarchy, and registers itself as the
 *   view the Builder focuses a node in.
 *
 * The table itself is the design system's: its rows, cells, keys, wires, name
 * group, add pill and row controls are covered by that package's suite, and
 * nothing here asserts them again.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { drawnClasses, drawnPart, insidePart, menuEntryLabel, orgTableParts } from "~/testing.tsx";
import { Tag } from "@crewlethq/ui";
import { fixtureCompany } from "./model/testkit.ts";
import { checkedEdit, scopedState } from "./testState.ts";
import { useSectionCounts } from "~/app/Shell.tsx";

// THE TAB COUNTS THE SCREEN HANDS THE FRAME, read where they are handed over.
vi.mock("~/app/Shell.tsx", async (importOriginal) => ({
  ...(await importOriginal<typeof import("~/app/Shell.tsx")>()),
  useSectionCounts: vi.fn(),
}));
import { TableView, tableColumns } from "./TableView.tsx";
import { NodeGlyph } from "~/ui/orgNodes.tsx";
import { builderSpies, BuilderHarness, harnessProbe, LayoutObserver } from "./viewTestkit.tsx";
import type { CompanyEdit } from "./BuilderContext.tsx";
import { render } from "@testing-library/react";
import { indexOrg, nameOfIn } from "~/lib/seats.ts";
import { seatLiveOf, type LiveOrg } from "~/lib/seatLive.ts";
import type { AgentRow, OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { StructureLiveProvider } from "../structureLive.tsx";

afterEach(() => {
  cleanup();
  location.hash = "";
});

beforeEach(() => {
  location.hash = "#/company/org/table";
});

function mount(options: { readOnly?: boolean; editsHeld?: string; principles?: CompanyEdit } = {}) {
  const spies = builderSpies();
  const probe = harnessProbe();
  const rendered = render(
    <BuilderHarness
      initial={checkedEdit(fixtureCompany())}
      spies={spies}
      probe={probe}
      readOnly={options.readOnly}
      editsHeld={options.editsHeld ?? null}
      principles={options.principles}
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
/** An element's text with a seat badge's initials left out: the name is printed beside them. */
const said = (el: Element): string =>
  [...el.childNodes]
    .map((node) =>
      node.nodeType === Node.TEXT_NODE
        ? (node.textContent ?? "")
        : node instanceof Element && !node.classList.contains("crewlet-avatar")
          ? said(node)
          : "",
    )
    .join("");

const cells = (name: string) =>
  within(row(name))
    .getAllByRole("gridcell")
    .map((cell) => said(cell).replace(/\s+/g, " ").trim());

/** The reason a held control says after its name, off the kit's described-by. */
const reasonOf = (control: HTMLElement): string =>
  (control.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ")
    .trim();

/** The line under a row's name, as this screen writes it. */
const caption = (name: string) => row(name).querySelector(".btable-caption")?.textContent ?? "";

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

  /*
   * A SEAT'S HANDLE RIDES ITS CAPTION, after its kind, which is where every
   * reference to it is read; there is no Handle column to repeat it. Who a
   * row reports to has a column of its own: a unit's lead, a seat's manager.
   */
  test("a row says its kind and handle under its name, and who it reports to", () => {
    mount();
    expect(cells("Dev")[0]).toBe("Devagent · @dev");
    expect(cells("Acme")[0]).toBe("Acmecompany");
    expect(cells("Engineering")[0]).toBe("Engineeringdepartment · 4 seats");
    const reportsTo = screen
      .getAllByRole("columnheader")
      .findIndex((head) => head.textContent?.trim() === "Reports to");
    expect(cells("Engineering")[reportsTo]).toBe("VP Engineering");
    // A unit that declares none, and a seat the check placed under nobody.
    expect(cells("Platform")[reportsTo]).toBe("No one");
    expect(cells("Dev")[reportsTo]).toBe("No manager");
  });

  /*
   * ONCE. What a row IS was written in the name cell's caption AND in a column
   * of its own, so every row said "Agent seat" twice about 350px apart and the
   * column cost 145px of a 1269px table to do it. The caption is the one to
   * keep: it is what the console's table draws and what the chart's own cards
   * draw. The columns are the live table's: what a seat is doing, who it
   * reports to, its problems, its run-time controls and its edits.
   */
  test("a row writes what it is once, under its name, and no column repeats it", () => {
    mount();
    const headers = screen
      .getAllByRole("columnheader")
      .map((head) => head.textContent?.trim() ?? "");
    expect(headers).toEqual(["Name", "Status", "Reports to", "Problems", "Controls", "Edit"]);
    const said = (name: string, word: string) =>
      (cells(name).join(" ").match(new RegExp(word, "g")) ?? []).length;
    expect(said("Dev", "agent")).toBe(1);
    expect(said("Acme", "company")).toBe(1);
    expect(said("Engineering", "department")).toBe(1);
  });

  /*
   * A UNIT'S CAPTION IS ITS TYPE AND ITS SIZE: the founder's own word for the
   * type, in the caption's lower case as every caption on the table is, and
   * how many seats it holds, nested units' and the seats a `unit:` reference
   * placed in it included. A unit with no type reads "unit".
   */
  test("a unit's caption is its own type and every seat it holds", () => {
    mount();
    // VP Engineering, Dev, SRE (in Platform) and Designer (placed by reference).
    expect(caption("Engineering")).toBe("department · 4 seats");
    expect(caption("Platform")).toBe("team · 2 seats");
    expect(caption("Sales")).toBe("unit · 1 seat");
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
    // The company reports to nobody: the one cell of its row that has no value.
    const spoken = within(company).getAllByText("Not applicable");
    expect(spoken).toHaveLength(1);
    expect(cells("Acme")[2]).toBe("–Not applicable");
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

  /*
   * AN AGENT SEAT IS TONED, IN THE TABLE AS ON THE CHART, and nothing else is:
   * its name, its mark and the wire arriving at it take the purple the console
   * paints an agent seat in, and every other row keeps the neutral ink. One hue
   * for one kind, never a hash of a key: each agent seat used to take one of
   * six hues hashed from its key, a legend nobody could decode. The design
   * system tints a row only when asked (`data-tone` on its name group and on
   * its wire), so what carries one here is exactly what asked.
   */
  test("an agent seat's row is toned purple, and no other row is", () => {
    const { container } = mount();
    const rows = screen.getAllByRole("row").filter((r) => r.hasAttribute("aria-level"));
    const agents = rows.filter((r) =>
      r.querySelector(".btable-caption")?.textContent?.startsWith("agent · "),
    );
    // The table really drew agent seats and other rows, so the checks below
    // are statements about them rather than about an empty table.
    expect(agents.length).toBeGreaterThan(0);
    expect(agents.length).toBeLessThan(rows.length);
    const tinted = [...container.querySelectorAll("[data-tone]")];
    for (const el of tinted) {
      expect(el.getAttribute("data-tone")).toBe("purple");
      expect(agents).toContain(el.closest("[role='row']"));
    }
    for (const r of agents)
      expect(r.querySelector("[data-tone='purple']"), r.textContent ?? "").not.toBeNull();
  });
});

describe("opening and closing the hierarchy", () => {
  /*
   * THE PAIR IS THE BUILDER TOOLBAR'S, ON BOTH VIEWS, so this table draws none of
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
   * THE COMPANY'S ROW CARRIES NO EDIT. Its principles are written on Company ›
   * Principles, a page the toolbar offers, so the row draws its Add and a menu
   * of Rename company alone, and Enter on it opens nothing. Mutation: draw the
   * pencil on every row again and the company's strip carries one.
   */
  test("the company's row draws its Add and a menu of Rename company alone", () => {
    const { spies } = mount();
    expect(strip("Acme")).toEqual(["Add to Acme", "Actions for Acme"]);
    expect(within(row("Acme")).queryByRole("button", { name: /^Delete/ })).toBeNull();
    expect(actions("Acme")).toEqual([["Rename company", false]]);
    fireEvent.click(screen.getByRole("menuitem", { name: /Rename company/ }));
    expect(spies.openCompanyRename).toHaveBeenCalledTimes(1);
    fireEvent.keyDown(row("Acme"), { key: "Enter" });
    expect(spies.openEditor).not.toHaveBeenCalled();
    expect(spies.openPrinciples).not.toHaveBeenCalled();
  });

  test("a reader who may not write the principles is offered no way to them on the row", () => {
    mount({ principles: "read" });
    expect(strip("Acme")).toEqual(["Add to Acme", "Actions for Acme"]);
    expect(actions("Acme")).toEqual([["Rename company", false]]);
  });

  /*
   * A COMPANY STILL BEING CREATED is named the same way: no Principles page
   * exists yet, and its row has no Edit either, only Rename company.
   */
  test("a company still being created is renamed from its menu, with no Edit", () => {
    mount({ principles: null });
    expect(strip("Acme")).toEqual(["Add to Acme", "Actions for Acme"]);
    expect(actions("Acme")).toEqual([["Rename company", false]]);
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
    expect(spies.openAdd).toHaveBeenLastCalledWith("unit:engineering", "human");

    fireEvent.click(within(row("Acme")).getByRole("button", { name: "Add to Acme" }));
    fireEvent.click(await screen.findByRole("button", { name: "Add unit to Acme" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });

  /*
   * ONE LIST, ONE SET OF MARKS. The pill drew a folder and a robot from an
   * array of its own while the chart drew a tree and the Crewlet mark for the
   * same two actions, so "Add agent seat" wore one drawing on the row and
   * another everywhere else. Read from what is rendered: the pill's agent and
   * unit entries carry `NodeGlyph`'s marks — the Crewlet figure and the tree a
   * unit row on this table already wears.
   */
  test("the pill's marks are the chart's own, from the one add list", async () => {
    mount();
    fireEvent.click(within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }));
    const drawing = (element: Element) => element.querySelector("svg")?.innerHTML ?? "";
    const agent = await screen.findByRole("button", { name: "Add agent seat to Engineering" });
    const figure = render(<NodeGlyph kind="agent" />);
    expect(drawing(agent)).not.toBe("");
    expect(drawing(agent)).toBe(drawing(figure.container));
    figure.unmount();
    const unit = screen.getByRole("button", { name: "Add unit to Engineering" });
    expect(drawing(unit)).not.toBe("");
    expect(drawing(unit)).toBe(drawing(drawnPart(row("Engineering"), PART.icon)!));
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

describe("a reader who may not change the org", () => {
  const REASON = "Changing the configuration needs Admin access.";

  /*
   * EVERY EDIT IS DRAWN AND HELD, SAYING WHY in the gate's own words, rather
   * than opening an editor that has nothing for this reader to do: Edit and
   * Delete refuse with the reason, Enter opens nothing, the menu holds what
   * opens an editor and says the reason once, and a team draws no add pill.
   * What only reads (Open seat) stays open. Mutation: leave `editsHeld` off
   * the row's Edit and it opens the editor; leave it off the menu and Edit
   * reports does.
   */
  test("every edit on a row is held, saying why, and every reading stays open", () => {
    const { spies } = mount({ readOnly: true, editsHeld: REASON });
    const edit = within(row("Dev")).getByRole("button", { name: "Edit Dev" });
    expect(edit.getAttribute("aria-disabled")).toBe("true");
    expect(reasonOf(edit)).toBe(REASON);
    fireEvent.click(edit);
    fireEvent.keyDown(row("Dev"), { key: "Enter" });
    expect(spies.openEditor).not.toHaveBeenCalled();
    const remove = within(row("Dev")).getByRole("button", { name: "Delete Dev" });
    expect(reasonOf(remove)).toBe(REASON);
    fireEvent.click(remove);
    expect(spies.openDelete).not.toHaveBeenCalled();

    expect(within(row("Engineering")).queryByRole("button", { name: /^Add to/ })).toBeNull();
    expect(actions("Dev")).toEqual([
      ["Open seat", false],
      ["Edit reports", true],
      ["Move to", true],
      ["Move up", true],
      ["Move down", true],
    ]);
    expect(screen.getByRole("menu", { name: "Actions for Dev" }).textContent).toContain(REASON);
  });

  /* AND A READER WHO MAY is told nothing of the kind: the same rows, open. */
  test("a reader who may change the org meets no held edit", () => {
    mount();
    const edit = within(row("Dev")).getByRole("button", { name: "Edit Dev" });
    expect(edit.getAttribute("aria-disabled")).toBeNull();
    expect(reasonOf(edit)).toBe("");
    expect(
      within(row("Engineering")).getByRole("button", { name: "Add to Engineering" }),
    ).toBeDefined();
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
    const entries = actions("Acme").map(([label]) => label);
    expect(entries).not.toContain("Move up");
    expect(entries).not.toContain("Move down");
  });
});

describe("selecting agents", () => {
  /** The fixture company running, every agent idle but `over`'s, controlled by the reader. */
  const running = (over: Record<string, Partial<AgentRow>> = {}, controllable = true): LiveOrg => {
    // THE PUSH IS THE DOCUMENT'S ORG with each seat's derivation, which the
    // fixture carries beside fields the push leaves out.
    const index = indexOrg(withDerived(fixtureCompany()) as unknown as OrgProjection);
    const nameOf = nameOfIn(index);
    const live = new Map(
      index.seats.map((seat) => [
        seat.handle,
        seatLiveOf(
          seat,
          {
            id: seat.handle,
            handle: seat.handle,
            role: seat.name,
            activity: "idle",
            ...over[seat.handle],
          } as AgentRow,
          { now: Date.now(), nameOf, controllable, ask: null },
        ),
      ]),
    );
    return { index, live, loaded: true, nameOf };
  };

  /*
   * THE SELECTION BAR STANDS BEFORE THE ROWS, in the chips' place. The kit's
   * bar sticks to the top of what scrolls, so drawn after a long table it was
   * out of view the moment it appeared; and it takes the chips' place rather
   * than pushing the rows down under the pointer between two picks.
   * Mutation: draw the bar after the table, or drop `data-selecting`.
   */
  test("the selection bar stands before the rows, in the chips' place", () => {
    const { container } = render(
      <BuilderHarness
        initial={checkedEdit(fixtureCompany())}
        spies={builderSpies()}
        probe={harnessProbe()}
      >
        <StructureLiveProvider value={running()}>
          <TableView />
        </StructureLiveProvider>
      </BuilderHarness>,
    );
    const table = container.querySelector<HTMLElement>(".btable")!;
    expect(table.hasAttribute("data-selecting")).toBe(false);
    fireEvent.click(screen.getByRole("checkbox", { name: "Select every agent in Engineering" }));
    const bar = container.querySelector(".crewlet-selection-bar")!;
    expect(bar).not.toBeNull();
    expect(table.hasAttribute("data-selecting")).toBe(true);
    const grid = screen.getByRole("treegrid", { name: "Organization" });
    expect(bar.compareDocumentPosition(grid) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  /*
   * A SEAT'S PROBLEM IS SAID ON ITS ROW AT EVERY WIDTH. The Problems column is
   * left out below 900 px, and a seat whose last turn failed read "Idle" with
   * nothing else, its problem counted by the chips and shown nowhere. There it
   * joins the seat's status. Mutation: drop the fold, and the row says Idle alone.
   */
  test("a seat's problem is said beside its state where the Problems column is left out", () => {
    const restore = LayoutObserver.install();
    LayoutObserver.sizer = () => ({ width: 700, height: 600 });
    try {
      const live = running({
        dev: { last_turn: { outcome: "failed" }, last_error: { kind: "", detail: "" } } as never,
      });
      const tag = live.live.get("dev")!.problems[0]!.tag;
      render(
        <BuilderHarness
          initial={checkedEdit(fixtureCompany())}
          spies={builderSpies()}
          probe={harnessProbe()}
        >
          <StructureLiveProvider value={live}>
            <TableView />
          </StructureLiveProvider>
        </BuilderHarness>,
      );
      LayoutObserver.settle(false);
      expect(screen.queryByRole("columnheader", { name: /Problems/ })).toBeNull();
      expect(row("Dev").textContent).toContain(tag);
      expect(row("SRE").textContent).not.toContain(tag);
    } finally {
      LayoutObserver.sizer = () => null;
      restore();
    }
  });

  /*
   * THE TABLE TAB COUNTS THE SEATS DRAWN. A lead's page holds the units they
   * lead, and its tab said the whole company's seat count beside its own
   * rows and chips. Mutation: count the running company's seats instead.
   */
  test("the Table tab counts the seats the builder draws, a lead's unit alone", () => {
    const counts = vi.mocked(useSectionCounts);
    counts.mockClear();
    const live = running();
    render(
      <BuilderHarness
        initial={scopedState(fixtureCompany(), "engineering")}
        spies={builderSpies()}
        probe={harnessProbe()}
      >
        <StructureLiveProvider value={live}>
          <TableView />
        </StructureLiveProvider>
      </BuilderHarness>,
    );
    // Engineering holds VP Engineering and Dev, and SRE in Platform under it.
    expect(counts).toHaveBeenLastCalledWith({ table: "3" });
    expect(live.index.seats.length).toBeGreaterThan(3);
  });

  /** The Table over `live`, at a table width of `width` (none: the full width). */
  function drawAt(live: LiveOrg, width?: number) {
    const restore = LayoutObserver.install();
    if (width) LayoutObserver.sizer = () => ({ width, height: 600 });
    render(
      <BuilderHarness
        initial={checkedEdit(fixtureCompany())}
        spies={builderSpies()}
        probe={harnessProbe()}
      >
        <StructureLiveProvider value={live}>
          <TableView />
        </StructureLiveProvider>
      </BuilderHarness>,
    );
    LayoutObserver.settle(false);
    return () => {
      LayoutObserver.sizer = () => null;
      restore();
    };
  }
  const spent = { dev: { stopped_reason: "budget" } as Partial<AgentRow> };

  /*
   * BELOW 900 PX THE NAME KEEPS THE ROOM: the Controls column held a remedy
   * beside three icons at every width and cut names at 768 px and beside the
   * seat panel. There a seat's controls fold to Pause or Resume, its remedy
   * stacks under its name, and the rest is in the row's menu. Mutation: keep
   * the remedy in the Controls cell below 900 px.
   */
  test("below 900 px a remedy stacks under the name and the controls fold to one", () => {
    const done = drawAt(running(spent), 700);
    try {
      const dev = row("Dev");
      const raise = within(dev).getByRole("button", { name: /Raise/ });
      expect(raise.closest(".btable-under")).not.toBeNull();
      expect(dev.querySelector(".btable-controls")?.querySelectorAll("button")).toHaveLength(1);
    } finally {
      done();
    }
  });

  /*
   * BELOW 900 PX STATUS GIVES THE NAME ITS SHARE: at the wide window's share,
   * kept for a company's summary on one line, it cut a deep seat's name at
   * 768 px and beside the seat panel. Mutation: keep 2.2fr below 900 px.
   */
  test("below 900 px Status gives the name the share it keeps at a wide table", () => {
    const share = (narrow: boolean) =>
      Number(
        /([\d.]+)fr\)$/.exec(
          String(
            tableColumns({ phone: false, narrow, anyRemedy: false }).find((c) => c.key === "state")
              ?.width,
          ),
        )?.[1],
      );
    expect(share(false)).toBe(2.2);
    expect(share(true)).toBeLessThan(share(false));
  });

  /*
   * A READER WHO MAY NOT TAKE A REMEDY IS SHOWN IT, held with its reason, as
   * every other control is: the Table left it out for a seat the reader does
   * not control, while the seat panel showed it held. Mutation: draw the
   * remedy only for a seat the reader controls.
   */
  test("a remedy is drawn for a seat the reader does not control", () => {
    const done = drawAt(running(spent, false));
    try {
      expect(within(row("Dev")).getByRole("button", { name: /Raise/ })).toBeDefined();
    } finally {
      done();
    }
  });

  /*
   * A PHONE'S SELECTION BAR KEEPS ONE ROW: Pause and Resume by their words,
   * and a "More" holding Pause and stop. All three wrapped it onto a second
   * row over the list. Mutation: leave `folded` off the Table's bar, or keep
   * the glyphs.
   */
  test("on a phone the selection bar holds Pause and stop in its More", () => {
    const done = drawAt(running({ dev: { activity: "working" } as Partial<AgentRow> }), 400);
    try {
      fireEvent.click(screen.getByRole("button", { name: "Select agents" }));
      fireEvent.click(screen.getByRole("checkbox", { name: "Select every agent in Engineering" }));
      const bar = screen.getByRole("toolbar", { name: "Selected agents" });
      expect(within(bar).queryByRole("button", { name: /^Pause and stop/ })).toBeNull();
      // By their words alone: with their glyphs the two wrapped the "More".
      for (const name of [/^Pause \d/, /^Resume\b/]) {
        expect(within(bar).getByRole("button", { name }).querySelector("svg")).toBeNull();
      }
      fireEvent.click(within(bar).getByRole("button", { name: "More for the selected agents" }));
      // The harness holds every write offline, and the entry says so as its button did.
      const [stop, ...rest] = within(
        screen.getByRole("menu", { name: "More for the selected agents" }),
      ).getAllByRole("menuitem");
      expect(rest).toHaveLength(0);
      expect(stop!.querySelector(".crewlet-menu__label")?.textContent).toBe("Pause and stop 1");
      expect(stop!.getAttribute("aria-disabled")).toBe("true");
      expect(stop!.textContent).toContain("Offline. Reconnect to make changes.");
    } finally {
      done();
    }
  });
});
