/**
 * The builder canvas: its tree, its keys, its menus and its layout.
 *
 * What these protect:
 * - the chart is an ARIA tree whose items say their level, position and
 *   expansion, and hold nothing focusable: the pointer's buttons sit beside
 *   them, hidden and out of the tab order;
 * - the arrows, Home, End and type-ahead move one roving tab stop, Enter
 *   edits, Delete and Backspace delete (never the company, never read-only),
 *   and the ContextMenu key or Shift+F10 opens the node's menu in the canvas
 *   overlay, returning focus to the node;
 * - the Add, More and lead menus ask the Builder for exactly the action
 *   named, and the lead is chosen in place;
 * - the reporting chart is the engine's forest with the cycle group, read-only;
 * - focus lands where the Builder sends it after an add, a delete, a move, an
 *   undo and a redo, opening a collapsed unit on the way and never scrolling;
 * - a live agents push changes a badge and never the layout.
 *
 * jsdom has no layout, so `LayoutObserver` reports sizes (see `viewTestkit`).
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { AgentRow, CompanyDocument } from "~/protocol/index.ts";
import type { ChartKind } from "./BuilderContext.tsx";
import { CanvasView } from "./CanvasView.tsx";
import { OrgNodeLabel, Tag } from "@crewlethq/ui";
import { isDrawnAs } from "~/testing.tsx";
import { COMPANY_KEY, seatKey, unitKey } from "./model/keys.ts";
import type { BuilderState } from "./model/reducer.ts";
import { NODE_TONES } from "./nodeTone.ts";
import { fixtureCompany, fixtureDerived } from "./model/testkit.ts";
import { answered, checkedEdit, PLACED, record } from "./testState.ts";
import {
  BuilderHarness,
  LayoutObserver,
  builderSpies,
  canvasWorld,
  CARD_WIDTH,
  ROW_HEIGHT,
  chartCard,
  chartCards,
  chartLinks,
  harnessProbe,
  isOutlinedCard,
  nodeName,
  nodeTrailing,
  type BuilderSpies,
  type HarnessProbe,
} from "./viewTestkit.tsx";
import { focusables } from "@crewlethq/ui";
import { menuEntryLabel } from "~/testing.tsx";

let restore: () => void;
beforeEach(() => {
  restore = LayoutObserver.install();
});
afterEach(() => {
  cleanup();
  restore();
  location.hash = "";
});

interface Mounted {
  spies: BuilderSpies;
  probe: HarnessProbe;
  rerender: (props?: { agents?: AgentRow[]; readOnly?: boolean }) => void;
  container: HTMLElement;
}

function mount(
  initial: BuilderState = checkedEdit(fixtureCompany()),
  { chart = "structure", readOnly = false }: { chart?: ChartKind; readOnly?: boolean } = {},
): Mounted {
  const spies = builderSpies();
  const probe = harnessProbe();
  const tree = (props: { agents?: AgentRow[]; readOnly?: boolean } = {}) => (
    <BuilderHarness
      initial={initial}
      spies={spies}
      probe={probe}
      readOnly={props.readOnly ?? readOnly}
      agents={props.agents}
    >
      <CanvasView chart={chart} />
    </BuilderHarness>
  );
  const utils = render(tree());
  LayoutObserver.settle();
  return {
    spies,
    probe,
    container: utils.container,
    rerender: (props) => {
      utils.rerender(tree(props));
      LayoutObserver.settle();
    },
  };
}

/** The treeitem whose name starts with `name`. */
const item = (name: string) =>
  screen
    .getAllByRole("treeitem")
    .find((el) => within(el).queryAllByText(name, { exact: true }).length > 0)!;
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
const focused = () => document.activeElement?.getAttribute("data-tree-id");
/** A menu item's label, without the shortcut hint beside it. */
const label = menuEntryLabel;
/**
 * What the design system calls the large icon step, and the dashed ring,
 * without this suite spelling either.
 *
 * ASKED BY DRAWING ONE EACH WAY AND TAKING THE DIFFERENCE, which is how the
 * chart harness finds the mark a card wears for somebody outside the system. A
 * class the package draws changes on a bump and is invisible to a reader;
 * spelt here, this suite would match nothing and report it as a defect in the
 * screen rather than in itself.
 */
const markStep = (which: "large" | "ring"): string => {
  const zone = (props: { iconSize?: "md" | "lg"; iconRing?: "none" | "dashed" }) => {
    const { container, unmount } = render(<OrgNodeLabel name="x" icon={<i />} {...props} />);
    const classes = [...container.firstElementChild!.classList];
    unmount();
    return classes;
  };
  const plain = zone({});
  const marked = which === "large" ? zone({ iconSize: "lg" }) : zone({ iconRing: "dashed" });
  const extra = marked.filter((name) => !plain.includes(name))[0];
  if (!extra) throw new Error(`the node label draws no ${which} step this suite can find`);
  return extra;
};

/** A treeitem's node name. */
const nameOf = (el: HTMLElement) => nodeName(el);
/**
 * The marks drawn on a node's caption, as the two sentences each carries: the
 * glyph's accessible name and the tooltip a pointer gets, which must agree.
 */
const markNames = (el: HTMLElement) =>
  [...el.querySelectorAll<HTMLElement>(".bnode-mark")].map((mark) => [
    mark.getAttribute("title"),
    mark.querySelector("[role='img']")?.getAttribute("aria-label"),
  ]);
/** The same, as one sentence each, for a claim about what a node says. */
const markSentences = (el: HTMLElement) => markNames(el).map(([tooltip]) => tooltip);

describe("which chart", () => {
  test("the chart the lens hands in is the one drawn, whatever the URL says", () => {
    // The Builder owns the `chart` param; a second reading here could disagree.
    location.hash = "#/org?lens=builder&view=visualization&chart=reporting";
    mount();
    expect(screen.getByRole("tree", { name: "Structure chart" })).toBeDefined();
    cleanup();
    location.hash = "#/org?lens=builder&view=visualization&chart=structure";
    mount(undefined, { chart: "reporting" });
    expect(screen.getByRole("tree", { name: "Reporting chart" })).toBeDefined();
  });
});

describe("the tree", () => {
  test("every node is a treeitem saying its level, position and expansion", () => {
    mount();
    expect(screen.getByRole("tree", { name: "Structure chart" })).toBeDefined();
    const shape = screen
      .getAllByRole("treeitem")
      .map((el) => [
        el.getAttribute("data-tree-id"),
        el.getAttribute("aria-level"),
        el.getAttribute("aria-posinset"),
        el.getAttribute("aria-setsize"),
        el.getAttribute("aria-expanded"),
      ]);
    expect(shape).toEqual([
      [COMPANY_KEY, "1", "1", "1", "true"],
      [seatKey("ceo"), "2", "1", "3", null],
      [unitKey("Engineering"), "2", "2", "3", "true"],
      [seatKey("vp-engineering"), "3", "1", "3", null],
      [seatKey("dev"), "3", "2", "3", null],
      [unitKey("Platform"), "3", "3", "3", "true"],
      [seatKey("sre"), "4", "1", "2", null],
      [seatKey("designer"), "4", "2", "2", null],
      [unitKey("Sales"), "2", "3", "3", "true"],
      [seatKey("account-executive"), "3", "1", "1", null],
    ]);
  });

  test("a treeitem holds nothing focusable, and the pointer's buttons are hidden beside it", () => {
    mount();
    for (const el of screen.getAllByRole("treeitem")) {
      expect(el.querySelectorAll("button, a, input, select, textarea, [tabindex]")).toHaveLength(0);
    }
    const tree = screen.getByRole("tree");
    // One tab stop in the whole tree: the roving one.
    expect(focusables(tree).filter((el) => el.tabIndex === 0)).toEqual([item("Acme")]);
    for (const button of tree.querySelectorAll("button")) {
      expect(button.tabIndex).toBe(-1);
      expect(button.closest("[aria-hidden='true']")).not.toBeNull();
      expect(button.closest("[role='treeitem']")).toBeNull();
    }
  });

  test("a press on a card's hidden buttons focuses the node, never the button", () => {
    const { probe } = mount();
    pointerPress("Actions for Dev");
    expect(screen.getByRole("menu", { name: "Actions for Dev" })).toBeDefined();
    press("Escape");
    // Focus in a subtree hidden from assistive technology is focus nowhere, so
    // the menu hands it back to the node rather than to the button.
    expect(document.activeElement).toBe(item("Dev"));
    expect(probe.selection).toBe(seatKey("dev"));

    pointerPress("Collapse Engineering");
    expect(document.activeElement).toBe(item("Engineering"));
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("false");

    // The lead chip is drawn in the same hidden strip.
    pointerPress("VP Engineering");
    press("Escape");
    expect(document.activeElement).toBe(item("Engineering"));
  });

  test("a human seat is drawn with the dashed edge, by kind rather than by colour", () => {
    const doc = fixtureCompany();
    doc.roles![0] = { name: "CEO", kind: "human", contact: { slack: "U0CEO" } };
    mount(checkedEdit(doc));
    // The mark is the CARD's boundary, which the design system draws, so the
    // claim is "drawn as the chart draws somebody outside the system" rather
    // than the name of a class that belongs to the package.
    expect(isOutlinedCard(chartCard(item("CEO")))).toBe(true);
    // EVERY SEAT IS A NODE OF ITS OWN, so an agent seat inside a unit is a
    // card that is not outlined rather than a row that is not marked.
    expect(isOutlinedCard(chartCard(item("Dev")))).toBe(false);
    expect(within(item("CEO")).getByText("Human seat")).toBeDefined();
    // And the hue is an agent seat's alone: a human seat wears the boundary.
    expect(chartCard(item("CEO")).getAttribute("data-tone")).toBeNull();
    expect(chartCard(item("Dev")).getAttribute("data-tone")).not.toBeNull();
  });

  test("a root seat placed by reference is a row of its unit, and a dangling reference is marked at the root", () => {
    const doc = fixtureCompany();
    doc.roles!.push({ name: "Scout", unit: "Ghost" });
    const state = answered(checkedEdit(doc), {
      status: "clean",
      warnings: [
        {
          kind: "dangling_reference",
          ref: "unit",
          path: "roles[2].unit",
          segments: ["roles", 2, "unit"],
          seat: "scout",
          unit: "",
          from: "Scout",
          to: "Ghost",
          message: "Seat Scout names unit Ghost, which is no unit.",
        },
      ],
      derived: fixtureDerived(doc, PLACED),
    });
    mount(state);
    // A NODE IS ONE RANK TALL, so a wiring mark is a glyph on its caption
    // rather than a badge with the sentence written out. It is still named,
    // and the name is the engine's own sentence where the engine gave one.
    // Both sentences, because both readers need one: the glyph's accessible
    // name and the tooltip a pointer gets say the same thing.
    expect(markNames(item("Designer"))).toEqual([
      ["Declared at the root with a unit reference", "Declared at the root with a unit reference"],
    ]);
    expect(item("Designer").getAttribute("aria-level")).toBe("4");
    expect(item("Scout").getAttribute("aria-level")).toBe("2");
    expect(markSentences(item("Scout"))).toEqual([
      "Seat Scout names unit Ghost, which is no unit.",
    ]);
    expect(markSentences(item("SRE"))).toEqual(["Alerts that name no seat wake this seat"]);
  });

  /*
   * A SEAT SAYS ITS KIND ONCE. The caption under the name is where a chart
   * says what a node is, and the sentence read after it carried the kind
   * again: measured on the running build, one card announced itself as "SRE
   * Lead, Agent seat, idle, Agent seat, @sre-lead". What the drawing does not
   * say is the HANDLE, so that is what the hidden sentence carries; the whole
   * sentence is still the tooltip, which is not read after the caption.
   */
  test("a seat's kind is drawn once and said once", () => {
    mount();
    const card = item("Dev");
    expect(card.textContent).toBe("DevAgent seatoffline@dev");
    expect(card.getAttribute("title")).toBe("Agent seat, @dev");
  });

  /*
   * AN AGENT SEAT WEARS THE LARGE MARK, because the chart this is drawn from
   * sizes a mark by what it stands for: a container at half its icon zone and
   * the thing the chart is ABOUT at three quarters of it. Drawn at one step
   * for all three, an agent seat was told from a unit by its hue and its
   * caption alone.
   */
  test("an agent seat's mark is the large step and a container's is not", () => {
    mount();
    // What the package calls the large step is asked OF the package, by
    // drawing one node each way and taking the difference: a class the design
    // system draws is not the engine's to spell.
    const large = markStep("large");
    const mark = (name: string) => item(name).querySelector("[aria-hidden='true']")!.className;
    expect(mark("Dev")).toContain(large);
    expect(mark("Engineering")).not.toContain(large);
    expect(mark("Acme")).not.toContain(large);
  });

  /* A human seat keeps the small figure inside its dashed ring, which is the
     boundary that says what it is. */
  test("a human seat's mark stays the small step inside its ring", () => {
    const doc = fixtureCompany();
    doc.roles![0] = { name: "CEO", kind: "human", contact: { slack: "U0CEO" } };
    mount(checkedEdit(doc));
    const zone = item("CEO").querySelector("[aria-hidden='true']")!;
    expect(zone.className).toContain(markStep("ring"));
    expect(zone.className).not.toContain(markStep("large"));
  });

  test("the problems the last check placed are counted on the node in the critical tone", () => {
    const doc = fixtureCompany();
    const state = answered(checkedEdit(doc), {
      status: "problems",
      problems: [
        {
          path: "units[1].roles[0].name",
          segments: ["units", 1, "roles", 0, "name"],
          kind: "missing",
          message: "units[1].roles[0].name is required",
        },
        {
          path: "units[1].type",
          segments: ["units", 1, "type"],
          kind: "unknown_value",
          message: "units[1].type is not a unit type",
        },
      ],
      derived: fixtureDerived(doc, PLACED),
      code: "validation_error",
      hint: "",
    });
    const { probe } = mount(state);
    /*
     * THE COUNT, NOT THE SENTENCE. A node is one rank tall and as wide as its
     * own name, and the slot a push arrives in is a fixed width: "1 problem"
     * written out was clipped inside it. What is drawn is the number, and the
     * sentence is the badge's own name and its tooltip, so both readers are
     * told the same thing.
     */
    const badge = within(item("Account Executive")).getByLabelText("1 problem");
    expect(badge.textContent).toBe("1");
    // The claim is the VARIANT the builder passes, so the element is compared
    // against the one uilet draws for it rather than against a class name.
    expect(
      isDrawnAs(
        badge.closest(".bnode-count")!.firstElementChild!,
        Tag,
        { variant: "danger", children: 1 },
        { children: 1 },
      ),
    ).toBe(true);
    expect(within(item("Sales")).getByLabelText("1 problem")).toBeDefined();
    expect(within(item("Engineering")).queryByLabelText(/problem/)).toBeNull();
    // IN THE NODE'S TRAILING SLOT, which stays when a check is out and the
    // count with it: the node is the same size either way, so it keeps its
    // place and so does every node beside it.
    const slot = badge.closest(".bnode-count")!;
    expect(slot.parentElement).toBe(nodeTrailing(item("Account Executive")));
    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "updateSeat",
          target: seatKey("dev"),
          set: [{ path: ["goal"], value: "x" }],
        },
      }),
    );
    expect(within(item("Account Executive")).queryByLabelText(/problem/)).toBeNull();
    expect(nodeTrailing(item("Account Executive"))!.querySelector(".bnode-count")).not.toBeNull();
  });

  test("a unit whose lead names no seat is marked with the engine's warning", () => {
    const doc = fixtureCompany();
    doc.units![1]!.lead = "Ghost";
    const state = answered(checkedEdit(doc), {
      status: "clean",
      warnings: [
        {
          kind: "dangling_reference",
          ref: "lead",
          path: "units[1].lead",
          segments: ["units", 1, "lead"],
          seat: "",
          unit: "Sales",
          from: "Sales",
          to: "Ghost",
          message: "Unit Sales names lead Ghost, which is no seat.",
        },
      ],
      derived: fixtureDerived(doc, PLACED),
    });
    const { probe } = mount(state);
    expect(markSentences(item("Sales"))).toEqual([
      "Unit Sales names lead Ghost, which is no seat.",
    ]);
    expect(markSentences(item("Engineering"))).toEqual([]);
    // Still there while the check of a later edit is out.
    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "updateSeat",
          target: seatKey("dev"),
          set: [{ path: ["goal"], value: "x" }],
        },
      }),
    );
    expect(markSentences(item("Sales"))).toEqual([
      "Unit Sales names lead Ghost, which is no seat.",
    ]);
  });

  test("a unit's other warnings are not read as a lead that names no seat", () => {
    const doc = fixtureCompany();
    const state = answered(checkedEdit(doc), {
      status: "clean",
      warnings: [
        {
          kind: "admission",
          ref: "",
          path: "units[1].name",
          segments: ["units", 1, "name"],
          seat: "",
          unit: "Sales",
          from: "",
          to: "",
          message: "units[1].name: another unit is also called Sales",
        },
      ],
      derived: fixtureDerived(doc, PLACED),
    });
    mount(state);
    expect(within(item("Sales")).queryByText("Lead names no seat")).toBeNull();
  });
});

describe("keys", () => {
  test("arrows walk the visible order, Right and Left open, close and climb, Home and End jump", () => {
    const { probe } = mount();
    item("Acme").focus();
    press("ArrowDown");
    expect(focused()).toBe(seatKey("ceo"));
    press("ArrowDown");
    expect(focused()).toBe(unitKey("Engineering"));
    press("ArrowLeft");
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryAllByText("Dev")).toHaveLength(0);
    press("ArrowDown");
    expect(focused()).toBe(unitKey("Sales"));
    press("ArrowUp");
    press("ArrowRight");
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("true");
    press("ArrowRight");
    expect(focused()).toBe(seatKey("vp-engineering"));
    press("ArrowLeft");
    expect(focused()).toBe(unitKey("Engineering"));
    press("End");
    expect(focused()).toBe(seatKey("account-executive"));
    press("Home");
    expect(focused()).toBe(COMPANY_KEY);
    // Selection follows focus, for the toolbar and the URL.
    expect(probe.selection).toBe(COMPANY_KEY);
    // The roving stop moved with it.
    expect(item("Acme").tabIndex).toBe(0);
    expect(item("CEO").tabIndex).toBe(-1);
  });

  test("typing finds a node by name, and a zoom key never starts a word", () => {
    mount();
    item("Acme").focus();
    press("p");
    expect(focused()).toBe(unitKey("Platform"));
    press("0");
    expect(focused()).toBe(unitKey("Platform"));
  });

  test("Enter edits, Delete and Backspace delete, and the company is never deleted", () => {
    const { spies } = mount();
    item("Dev").focus();
    press("Enter");
    expect(spies.openEditor).toHaveBeenLastCalledWith(seatKey("dev"));
    press("Delete");
    press("Backspace");
    expect(spies.openDelete.mock.calls).toEqual([[seatKey("dev")], [seatKey("dev")]]);

    item("Acme").focus();
    press("Delete");
    expect(spies.openDelete).toHaveBeenCalledTimes(2);
    press("Enter");
    expect(spies.openEditor).toHaveBeenLastCalledWith(COMPANY_KEY);

    // A chord is the Builder's (Undo), never the node's, and Shift turns
    // neither Enter nor Delete into the node's action.
    item("Dev").focus();
    press("Backspace", { metaKey: true });
    press("Delete", { shiftKey: true });
    expect(spies.openDelete).toHaveBeenCalledTimes(2);
    press("Enter", { shiftKey: true });
    expect(spies.openEditor).toHaveBeenCalledTimes(2);
  });

  test("read-only, a node can still be opened but never deleted", () => {
    const { spies } = mount(undefined, { readOnly: true });
    item("Dev").focus();
    press("Delete");
    expect(spies.openDelete).not.toHaveBeenCalled();
    press("Enter");
    expect(spies.openEditor).toHaveBeenCalledWith(seatKey("dev"));
    // AND THE BRANCH CARRIES NOTHING. The Add under a card is the one control
    // whose whole purpose is a change, so on a draft nobody may change it is
    // absent rather than present and refusing: there is no reading of it to
    // keep, unlike Delete in a menu, where the entry says the action exists.
    expect(screen.queryByRole("button", { name: /^Add to /, hidden: true })).toBeNull();
  });

  test("the ContextMenu key and Shift+F10 open the node's menu in the overlay, and Escape returns", () => {
    const { container, spies } = mount();
    const dev = item("Dev");
    dev.focus();
    press("ContextMenu");
    const menu = screen.getByRole("menu", { name: "Actions for Dev" });
    // Over the chart rather than in it, so the zoom neither scales nor clips it.
    expect(canvasWorld(container).contains(menu)).toBe(false);
    expect(within(menu).getAllByRole("menuitem").map(label)).toEqual([
      "Edit",
      "Open seat",
      "Edit reports",
      "Change to human seat",
      "Move to",
      "Delete",
    ]);
    press("Escape");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(dev);

    press("F10", { shiftKey: true });
    fireEvent.click(screen.getByRole("menuitem", { name: "Move to" }));
    expect(spies.openMove).toHaveBeenCalledWith(seatKey("dev"));
    expect(document.activeElement).toBe(dev);

    // Edit opens the whole form; Edit reports opens it at the seat's reports.
    press("ContextMenu");
    fireEvent.click(screen.getAllByRole("menuitem").find((m) => label(m) === "Edit")!);
    expect(spies.openEditor).toHaveBeenLastCalledWith(seatKey("dev"));
    press("ContextMenu");
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit reports" }));
    expect(spies.openEditor).toHaveBeenLastCalledWith(seatKey("dev"), "reports");
  });
});

/*
 * WHAT A POINTER GETS ON A NODE, and where each of them sits.
 *
 * A node is one rank tall, so its right edge splits into two cells and no
 * more: Edit and Delete, which are also what Enter and Delete do. What is
 * about a node's CHILDREN rather than about the node (expand, add) hangs on
 * the branch below it, where the children come off. Everything else is the
 * node's own menu, which the ContextMenu key opens and the toolbar mirrors,
 * and this is the suite that says a control moved rather than went away.
 */
/*
 * ALT WITH AN ARROW MOVES A NODE among the siblings it is drawn beside, which
 * the outline already bound and the chart did not. This is the view where a
 * reader reaches for it first: a chart draws siblings left to right in exactly
 * the order the move changes, and passing one can change which seat manages
 * this one.
 */
describe("moving a node among its siblings", () => {
  const order = (probe: HarnessProbe) =>
    probe.state.draft.units[0]!.roles.map((seat) => seat.data.name);

  test("Alt and an arrow move the node the keyboard is on", () => {
    const { probe } = mount();
    expect(order(probe)).toEqual(["VP Engineering", "Dev"]);
    item("Dev").focus();
    press("ArrowUp", { altKey: true });
    expect(order(probe)).toEqual(["Dev", "VP Engineering"]);
    press("ArrowDown", { altKey: true });
    expect(order(probe)).toEqual(["VP Engineering", "Dev"]);
  });

  /* The arrows without Alt are the tree's own, and still walk the chart. */
  test("an arrow alone still moves the reader rather than the node", () => {
    const { probe } = mount();
    item("Dev").focus();
    press("ArrowUp");
    expect(order(probe)).toEqual(["VP Engineering", "Dev"]);
    expect(focused()).not.toBe(seatKey("dev"));
  });

  /* A draft nobody may write is drawn and records nothing. */
  test("a read-only draft is moved by nothing", () => {
    const { probe } = mount(checkedEdit(fixtureCompany()), { readOnly: true });
    item("Dev").focus();
    press("ArrowUp", { altKey: true });
    expect(order(probe)).toEqual(["VP Engineering", "Dev"]);
  });
});

describe("what a node offers a pointer", () => {
  /**
   * Every control the chart draws for a node, by its accessible name: the
   * label an icon-only control carries, else the words on it.
   */
  const controls = (name: string) => {
    const card = chartCard(item(name));
    return [...card.querySelectorAll("button")].map(
      (b) => b.getAttribute("aria-label") ?? b.textContent,
    );
  };

  test("the node's own edge carries Edit and Delete, and nothing else", () => {
    const { spies } = mount();
    expect(controls("Engineering")).toEqual([
      "Edit Engineering",
      "Delete Engineering",
      "Actions for Engineering",
      // The lead along the bottom edge, which stays drawn: it is a fact about
      // the organization rather than a tool for changing it. The X inside it
      // is the tool, and it is quiet with the rest of them.
      "VP Engineering",
      "Clear the lead of Engineering",
      "Collapse Engineering",
      "Add to Engineering",
    ]);
    pointerPress("Edit Engineering");
    expect(spies.openEditor).toHaveBeenCalledWith(unitKey("Engineering"));
    pointerPress("Delete Engineering");
    expect(spies.openDelete).toHaveBeenCalledWith(unitKey("Engineering"));
  });

  /* The company cannot be deleted, so it is not offered and does not refuse. */
  test("the company has no Delete, and a read-only draft has none at all", () => {
    mount();
    expect(controls("Acme")).toEqual([
      "Edit Acme",
      "Actions for Acme",
      "Collapse Acme",
      "Add to Acme",
    ]);
    cleanup();
    mount(undefined, { readOnly: true });
    expect(controls("Engineering")).toEqual([
      "Edit Engineering",
      "Actions for Engineering",
      "Collapse Engineering",
    ]);
    // The lead is still READ on a read-only draft; it is simply not a control.
    expect(within(chartCard(item("Engineering"))).getByText("VP Engineering")).toBeDefined();
  });

  /*
   * A SEAT TAKES NO CHILD, so its branch carries nothing: the band collapses
   * rather than drawing an empty strip under every leaf of the chart.
   */
  test("a seat has the two actions and no branch controls", () => {
    mount();
    expect(controls("Dev")).toEqual(["Edit Dev", "Delete Dev", "Actions for Dev"]);
  });

  /*
   * THE MENU IS STILL THERE FOR THE KEY THAT OPENS IT. Its trigger is drawn
   * nowhere, and what it offers is the one list every surface offers.
   */
  test("the ContextMenu key still opens the node's whole menu", () => {
    mount();
    item("Engineering").focus();
    press("ContextMenu");
    expect(screen.getAllByRole("menuitem").map((m) => label(m))).toEqual([
      "Add unit",
      "Add agent seat",
      "Add human seat",
      "Edit",
      "Move to",
      "Delete",
    ]);
  });
});

/*
 * AN AGENT SEAT CARRIES A HUE, and it is the one thing on this chart drawn by
 * what a node IS rather than by what the engine said about it. Derived from
 * the seat's KEY, because a company document has no colour in it and a hue
 * hashed from the name would repaint the chart on every keystroke in the
 * editor.
 */
describe("a seat's hue", () => {
  const toneOf = (name: string) => chartCard(item(name)).getAttribute("data-tone");

  test("an agent seat has one, and nothing else on the chart does", () => {
    const doc = fixtureCompany();
    doc.roles![0] = { name: "CEO", kind: "human", contact: { slack_user_id: "U0CEO" } };
    mount(checkedEdit(doc));
    expect(toneOf("Dev")).not.toBeNull();
    // A human seat wears the dashed boundary instead, which reads to somebody
    // who cannot separate the hues at all.
    expect(toneOf("CEO")).toBeNull();
    expect(toneOf("Engineering")).toBeNull();
    expect(toneOf("Acme")).toBeNull();
  });

  test("it is one of the six the design system measures", () => {
    mount();
    for (const seat of ["Dev", "VP Engineering", "SRE", "Designer"]) {
      expect(NODE_TONES, seat).toContain(toneOf(seat));
    }
  });

  /*
   * A RENAME DOES NOT REPAINT IT. The key outlives the name, so a seat keeps
   * its hue while it is renamed, moved or given a handle; the colour is a mark
   * an operator recognises it by, and one that changed as they typed would be
   * no mark at all.
   */
  test("a rename leaves the hue where it was", () => {
    const { probe } = mount();
    const before = toneOf("Dev");
    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "renameSeat", target: seatKey("dev"), name: "Staff Engineer" },
      }),
    );
    expect(toneOf("Staff Engineer")).toBe(before);
  });
});

describe("menus", () => {
  /*
   * THE ADD ON A BRANCH SPLITS rather than opening a menu. What can go under a
   * node is three things, so the mark grows into the three where it stands: in
   * the chart rather than over it, which is the opposite of what a menu does
   * and is the point, because the choice a reader presses is within a few
   * pixels of the mark they pointed at. Each section names the node, so the
   * nine on this chart are nine controls a reader can tell apart.
   */
  test("the Add on a branch splits into the kinds, each asking the right parent", () => {
    const { spies, container } = mount();
    const press = (name: string) =>
      fireEvent.click(screen.getByRole("button", { name, hidden: true }));
    press("Add to Platform");
    expect(
      canvasWorld(container).contains(
        screen.getByRole("button", { name: "Add human seat to Platform", hidden: true }),
      ),
    ).toBe(true);
    press("Add human seat to Platform");
    expect(spies.openAdd).toHaveBeenLastCalledWith(unitKey("Platform"), "human");
    press("Add to Acme");
    press("Add unit to Acme");
    expect(spies.openAdd).toHaveBeenLastCalledWith(null, "unit");
  });

  /*
   * AND THE KEYBOARD NEVER NEEDS IT. The branch strip is pointer-only and out
   * of the tab order, so the same three kinds have to be somewhere a key
   * reaches: they are the first entries of the node's own menu, which is one
   * list with the pill's sections (`nodeActions.addSections`).
   */
  test("the same three kinds are in the node's own menu, for the keyboard", () => {
    const { spies } = mount();
    item("Platform").focus();
    press("ContextMenu");
    expect(screen.getAllByRole("menuitem").map(label).slice(0, 3)).toEqual([
      "Add unit",
      "Add agent seat",
      "Add human seat",
    ]);
    fireEvent.click(screen.getByRole("menuitem", { name: "Add human seat" }));
    expect(spies.openAdd).toHaveBeenLastCalledWith(unitKey("Platform"), "human");
  });

  test("a seat this draft created has no screen to open, and read-only disables every change", () => {
    const added = record(checkedEdit(fixtureCompany()), {
      type: "addSeat",
      key: "new:s1",
      placement: { parent: unitKey("Sales"), after: null },
      data: { name: "Closer" },
    });
    mount(added, { readOnly: true });
    item("Closer").focus();
    press("ContextMenu");
    const names = screen
      .getAllByRole("menuitem")
      .map((m) => [label(m), m.getAttribute("aria-disabled") === "true"]);
    expect(names).toEqual([
      ["Edit", false],
      ["Edit reports", false],
      ["Change to human seat", true],
      ["Move to", true],
      ["Delete", true],
    ]);
  });

  test("the lead is chosen in place: No lead says what it inherits, and a member becomes the lead", () => {
    const { probe } = mount(
      checkedEdit(fixtureCompany(), {
        units: { "units[0].children[0]": { lead: "vp-engineering", lead_inherited: true } },
        seats: { "roles[1]": { placed_by_ref: true, unit_path: "units[0].children[0]" } },
      }),
    );
    const chip = screen.getByRole("button", { name: /VP Engineering \(inherited\)/, hidden: true });
    fireEvent.click(chip);
    const answers = screen.getAllByRole("menuitemradio");
    expect(answers.map((a) => [a.textContent, a.getAttribute("aria-checked")])).toEqual([
      ["No lead (inherits VP Engineering)", "true"],
      ["SRE", "false"],
      ["Designer", "false"],
    ]);
    fireEvent.click(screen.getByRole("menuitemradio", { name: "SRE" }));
    const platform = probe.state.draft.units[0]!.children[0]!;
    expect(platform.data.lead).toBe("SRE");
    expect(within(item("Platform")).getByText("Lead: SRE.")).toBeDefined();
    // Engineering's own lead is unchanged, and so is what the check said of it.
    expect(within(item("Engineering")).getByText("Lead: VP Engineering.")).toBeDefined();
    // Now declared, the choice says so, and No lead offers the parent's lead.
    fireEvent.click(screen.getByRole("button", { name: "SRE", hidden: true }));
    expect(
      screen
        .getAllByRole("menuitemradio")
        .map((a) => [a.textContent, a.getAttribute("aria-checked")]),
    ).toEqual([
      ["No lead (inherits VP Engineering)", "false"],
      ["SRE", "true"],
      ["Designer", "false"],
    ]);
    press("Escape");

    // Engineering cleared: it inherits what no check of this draft has
    // reported yet, and says so rather than that a check is running.
    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "setLead", target: unitKey("Engineering") },
      }),
    );
    expect(within(item("Engineering")).getByText("Lead after the check.")).toBeDefined();
    expect(
      screen.getByRole("button", { name: "Lead after the check", hidden: true }),
    ).toBeDefined();
  });

  /*
   * ONE PRESS TAKES A LEAD AWAY. Through the menu it is a press, a list and a
   * choice, for the answer a reader is most likely to want after setting the
   * wrong one; the chart this is drawn from puts a small X inside the pill for
   * exactly that.
   */
  test("the X inside the pill clears a declared lead in one press", () => {
    const { spies } = mount();
    const engineering = within(chartCard(item("Engineering")));
    fireEvent.click(
      engineering.getByRole("button", { name: "Clear the lead of Engineering", hidden: true }),
    );
    expect(spies.dispatched).toEqual([
      {
        type: "record",
        intent: { type: "setLead", target: unitKey("Engineering"), lead: undefined },
      },
    ]);
  });

  /*
   * AND ONLY WHERE A PRESS WOULD CHANGE THE DRAFT. A lead the engine derived
   * from an ancestor is not written on this unit, so there is nothing here to
   * take away; a unit with none has nothing either. Drawn everywhere it would
   * be a control that refuses on two thirds of the units in a chart.
   */
  test("a unit with no declared lead of its own is offered no X", () => {
    mount();
    // Sales declares none, and inherits nothing the chart can name yet.
    expect(
      within(chartCard(item("Sales"))).queryByRole("button", {
        name: /^Clear the lead/,
        hidden: true,
      }),
    ).toBeNull();
  });

  test("a read-only draft has no X at all", () => {
    mount(checkedEdit(fixtureCompany()), { readOnly: true });
    expect(screen.queryByRole("button", { name: /^Clear the lead/, hidden: true })).toBeNull();
  });

  test("a lead declared outside the unit is still the checked answer, and another seat is chosen in the editor", () => {
    const doc = fixtureCompany();
    doc.units![1]!.lead = "CEO";
    const { spies } = mount(checkedEdit(doc));
    const sales = within(chartCard(item("Sales")));
    fireEvent.click(sales.getByRole("button", { name: "CEO", hidden: true }));
    expect(
      screen
        .getAllByRole("menuitemradio")
        .map((a) => [a.textContent, a.getAttribute("aria-checked")]),
    ).toEqual([
      ["No lead", "false"],
      ["Account Executive", "false"],
      ["CEO", "true"],
    ]);
    fireEvent.click(screen.getByRole("menuitem", { name: /Choose another seat/ }));
    // At the unit's Leadership, where a lead outside the unit is chosen.
    expect(spies.openEditor).toHaveBeenCalledWith(unitKey("Sales"), "leadership");
  });

  test("choosing the answer already chosen records nothing and is refused nowhere", () => {
    const { probe, spies } = mount();
    // Engineering declares VP Engineering; Sales declares no lead.
    fireEvent.click(screen.getByRole("button", { name: "VP Engineering", hidden: true }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: "VP Engineering" }));
    expect(screen.queryByRole("menu")).toBeNull();
    const sales = within(chartCard(item("Sales")));
    fireEvent.click(sales.getByRole("button", { name: "No lead", hidden: true }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: "No lead" }));
    expect(spies.dispatched).toEqual([]);
    expect(probe.state.refusal).toBeNull();
  });
});

describe("the reporting chart", () => {
  const loop: CompanyDocument = {
    name: "Loop",
    roles: [
      { name: "Chief", manages: ["Ops"] },
      { name: "Ops" },
      { name: "A", manages: ["B"] },
      { name: "B", manages: ["A"] },
    ],
  };
  const derivedLoop = {
    seats: {
      "roles[1]": { manager: "chief" },
      "roles[2]": { manager: "b" },
      "roles[3]": { manager: "a" },
    },
  };

  test("draws the forest with no-manager tops and the cycle group, and changes nothing itself", () => {
    const { spies, probe } = mount(checkedEdit(loop, derivedLoop), { chart: "reporting" });
    expect(screen.getByRole("tree", { name: "Reporting chart" })).toBeDefined();
    const shape = screen
      .getAllByRole("treeitem")
      .map((el) => [el.getAttribute("aria-level"), nameOf(el)]);
    expect(shape).toEqual([
      ["1", "Chief"],
      ["2", "Ops"],
      ["1", "Reporting cycle"],
      ["2", "A"],
      ["3", "B"],
    ]);
    // A TOP OF THE FOREST IS DRAWN BY BEING ONE, so what it says about having
    // no manager is said to a reader who cannot see where it sits. A seat in a
    // cycle is NOT drawn by where it sits, since every seat in a loop has one
    // above it, so that one is a glyph on the caption as well.
    expect(within(item("Chief")).getByText("No manager.")).toBeDefined();
    expect(markSentences(item("B"))).toEqual(["In a reporting cycle of 2 seats"]);
    expect(within(item("B")).getByText("In a reporting cycle of 2 seats.")).toBeDefined();

    item("B").focus();
    press("Delete");
    expect(spies.openDelete).not.toHaveBeenCalled();
    press("Enter");
    // Enter is the menu's first entry here, Edit reports, so the editor opens
    // at the seat's reports.
    expect(spies.openEditor).toHaveBeenCalledWith(seatKey("b"), "reports");
    press("ContextMenu");
    expect(screen.getAllByRole("menuitem").map((m) => m.textContent)).toEqual([
      expect.stringMatching(/^Edit reports/),
      "Open seat",
    ]);
    press("Escape");

    // The group heading is no node of the draft, so reaching it selects
    // nothing: the selection is what the toolbar acts on and the URL names.
    item("Chief").focus();
    press("ArrowDown");
    expect(probe.selection).toBe(seatKey("ops"));
    press("ArrowDown");
    expect(nameOf(document.activeElement as HTMLElement)).toBe("Reporting cycle");
    expect(probe.selection).toBe(seatKey("ops"));
  });

  test("says its lines are the last check's once the draft has moved past it", () => {
    const { probe, container } = mount(checkedEdit(loop, derivedLoop), { chart: "reporting" });
    const note = () => screen.queryByText(/These reporting lines are from the last check/);
    expect(note()).toBeNull();
    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "setManages", target: seatKey("chief"), manages: [] },
      }),
    );
    // Drawn OVER the canvas rather than in it, so it neither resizes the
    // viewport the layout is measured against nor moves when the chart pans.
    const shown = note()!;
    expect(canvasWorld(container).contains(shown)).toBe(false);
    expect(container.contains(shown)).toBe(true);
    expect(shown.textContent).not.toMatch(/current check/);
    // The chart itself is still the last check's forest.
    expect(nameOf(item("Ops"))).toBe("Ops");
  });

  test("says there is nobody to draw when the checked draft holds no seat", () => {
    mount(checkedEdit({ name: "Fresh" }, {}), { chart: "reporting" });
    expect(screen.getByText("No seats to report on")).toBeDefined();
    expect(screen.queryByRole("tree")).toBeNull();
  });

  test("says it is waiting for the engine before any check has described the draft", () => {
    const loaded = checkedEdit(loop, derivedLoop);
    const unchecked: BuilderState = { ...loaded, check: { ...loaded.check, derived: null } };
    mount(unchecked, { chart: "reporting" });
    expect(screen.getByText("Reporting lines appear after the check")).toBeDefined();
    expect(screen.queryByRole("tree")).toBeNull();
  });
});

/*
 * THE SHAPE OF THE CHART, which is the whole of why this builder draws the
 * console's org chart node rather than a panel. A rank of an organization is a
 * row a reader scans across: two units of one company sit on one line whether
 * or not one of them carries a lead along its bottom edge, and a seat beside a
 * unit does too. The LAYOUT is the design system's, and what this case asserts
 * is that the builder asked for that layout, which it does by the appearance it
 * draws in: a chart of panels would hang each card under its own parent, and a
 * unit with a lead would drop its whole subtree half a card below its
 * neighbour's.
 */
describe("the shape of the chart", () => {
  const boxOf = (name: string) => chartCard(item(name));
  const topOf = (name: string) =>
    Number(/translate\((-?[\d.]+)px, (-?[\d.]+)px\)/.exec(boxOf(name).style.transform)![2]);

  /*
   * A UNIT THAT CARRIES A LEAD IS TALLER than one that does not, and in a
   * browser that is what makes this question real. Every card is one row to the
   * default sizer, so this case says which cards are tall itself: Engineering
   * has a lead in the fixture and Sales has none.
   */
  const LEAD_STRIP = ROW_HEIGHT / 2;
  const withLeadStrip = () => {
    const plain = LayoutObserver.sizer;
    LayoutObserver.sizer = (el) => {
      const size = plain(el);
      // A card is what the default sizer measured at the card width; the one
      // that says who leads it is half a row taller than the rest.
      if (!size || size.width !== CARD_WIDTH) return size;
      const led = el.textContent?.includes("Lead: VP Engineering") === true;
      return led ? { ...size, height: size.height + LEAD_STRIP } : size;
    };
  };

  test("every node of a depth is drawn on one line, whatever its card holds", () => {
    withLeadStrip();
    mount();
    // Engineering is half a row taller than Sales and they are still one rank:
    // the taller card sets the rank's height and the shorter is centred in it,
    // so their two tops differ by exactly half the difference.
    expect(topOf("Sales") - topOf("Engineering")).toBe(LEAD_STRIP / 2);
    // The rank below starts under the TALLER of the two rather than under each
    // card's own bottom, so the seats of both units are on one line.
    expect(topOf("VP Engineering")).toBe(topOf("Platform"));
    expect(topOf("Account Executive")).toBe(topOf("Platform"));
    expect(topOf("Platform")).toBeGreaterThan(topOf("Engineering"));
  });
});

describe("focus", () => {
  const boxOf = (name: string) => chartCard(item(name));

  test("focusing a node inside a collapsed unit opens it and never scrolls", () => {
    const { probe } = mount();
    item("Engineering").focus();
    press("ArrowLeft");
    expect(screen.queryAllByText("Dev")).toHaveLength(0);
    const focus = vi.spyOn(HTMLElement.prototype, "focus");
    act(() => probe.view!.focusNode(seatKey("dev")));
    LayoutObserver.settle();
    expect(focused()).toBe(seatKey("dev"));
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    focus.mockRestore();
  });

  test("lands where the Builder sends it after an add, a delete, a move, an undo and a redo", () => {
    const { probe } = mount();
    const follow = () => {
      act(() => probe.view!.focusNode(probe.state.last!.focus));
      LayoutObserver.settle();
      return focused();
    };

    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "addSeat",
          key: "new:s1",
          placement: { parent: unitKey("Sales"), after: seatKey("account-executive") },
          data: { name: "Closer" },
        },
      }),
    );
    LayoutObserver.settle();
    expect(follow()).toBe("new:s1");

    act(() =>
      probe.dispatch({ type: "record", intent: { type: "remove", target: seatKey("dev") } }),
    );
    LayoutObserver.settle();
    // The previous sibling, else the parent.
    expect(follow()).toBe(seatKey("vp-engineering"));

    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "move",
          target: seatKey("sre"),
          to: { parent: unitKey("Sales"), after: null },
        },
      }),
    );
    LayoutObserver.settle();
    expect(follow()).toBe(seatKey("sre"));
    expect(item("SRE").getAttribute("aria-level")).toBe("3");

    act(() => probe.dispatch({ type: "undo" }));
    LayoutObserver.settle();
    expect(follow()).toBe(seatKey("sre"));
    expect(item("SRE").getAttribute("aria-level")).toBe("4");

    act(() => probe.dispatch({ type: "undo" }));
    LayoutObserver.settle();
    expect(follow()).toBe(seatKey("dev"));

    act(() => probe.dispatch({ type: "redo" }));
    LayoutObserver.settle();
    expect(follow()).toBe(seatKey("vp-engineering"));
    expect(boxOf("VP Engineering")).toBeDefined();
  });

  test("a relayout keeps the node the operator acted on where it was on screen", () => {
    const { probe, container } = mount();
    const translate = (el: HTMLElement) => {
      const [, x, y] = /translate\((-?[\d.]+)px, (-?[\d.]+)px\)/.exec(el.style.transform)!;
      return { x: Number(x), y: Number(y) };
    };
    const place = (name: string) => {
      const world = translate(canvasWorld(container));
      const box = translate(boxOf(name));
      return { world: box, screen: { x: world.x + box.x, y: world.y + box.y } };
    };
    const before = place("Acme");

    // A seat added at the top of the company widens the row beneath it, so
    // the company card moves in the world. The new seat had no place before
    // the relayout, so the card that held it, the company, is kept still.
    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "addSeat",
          key: "new:s9",
          placement: { parent: COMPANY_KEY, after: null },
          data: { name: "Advisor" },
        },
      }),
    );
    LayoutObserver.settle();
    const after = place("Acme");
    expect(after.world).not.toEqual(before.world);
    expect(after.screen).toEqual(before.screen);
  });

  test("collapse all keeps the company open, and expand all opens everything", () => {
    const { probe } = mount();
    act(() => probe.view!.collapseAll());
    expect(item("Acme").getAttribute("aria-expanded")).toBe("true");
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryAllByText("Dev")).toHaveLength(0);
    act(() => probe.view!.expandAll());
    expect(item("Platform").getAttribute("aria-expanded")).toBe("true");
    expect(item("SRE")).toBeDefined();
  });
});

describe("live state", () => {
  test("an agents push changes a badge and never the layout", () => {
    const { rerender, container } = mount();
    const layout = () => ({
      boxes: chartCards(container).map((b) => [b, b.style.transform]),
      world: canvasWorld(container).style.transform,
      links: chartLinks(container).innerHTML,
    });
    const before = layout();
    expect(within(item("Dev")).getByText("offline")).toBeDefined();

    rerender({
      agents: [{ id: "a1", role: "Dev", handle: "dev", state: "working" }],
    });
    expect(within(item("Dev")).getByText("working")).toBeDefined();
    // The same card elements, at the same places, under the same view.
    expect(layout()).toEqual(before);
  });

  test("a live state is shown only for a saved agent seat, found by its saved handle", () => {
    const state = record(checkedEdit(fixtureCompany()), {
      type: "renameSeat",
      target: seatKey("dev"),
      name: "Builder",
    });
    mount(state);
    // Renamed in the draft, still found under the handle it runs with.
    expect(within(item("Builder")).getByText("offline")).toBeDefined();
    const added = record(state, {
      type: "addSeat",
      key: "new:s2",
      placement: { parent: unitKey("Sales"), after: null },
      data: { name: "Closer" },
    });
    cleanup();
    mount(added);
    expect(within(item("Closer")).queryByText("offline")).toBeNull();

    // A saved seat that becomes human in this draft will not run, and says
    // nothing live; nor does a seat the saved company already holds as human.
    const doc = fixtureCompany();
    doc.roles![0] = { name: "CEO", kind: "human", contact: { slack: "U0CEO" } };
    const human = record(checkedEdit(doc), {
      type: "changeKind",
      target: seatKey("dev"),
      kind: "human",
      contact: { slack: "U0DEV" },
    });
    cleanup();
    mount(human);
    expect(within(item("Dev")).queryByText("offline")).toBeNull();
    expect(within(item("CEO")).queryByText("offline")).toBeNull();
    expect(within(item("SRE")).getByText("offline")).toBeDefined();
  });
});

// ---------------------------------------------------------------------------
// The chart's own chrome
// ---------------------------------------------------------------------------

/*
 * WHAT ACTS ON THE CANVAS IS DRAWN BESIDE IT. The chart this is drawn from
 * keeps its zoom bar, its key hint and its chart switch in one column in the
 * canvas's own corner; measured on this build, the switch and the fullscreen
 * toggle had ended up in the page's toolbar 800px from the bar they belong to.
 * The page still OWNS them, because one acts on the builder's whole container
 * and the other writes the lens's own section param; this view says where they
 * are drawn.
 */
describe("the chart's chrome", () => {
  /*
   * FOUND BY WHAT A READER MEETS rather than by what the package calls it: the
   * bar is whatever holds the canvas's own zoom controls, and the group is the
   * column that bar sits in.
   */
  const bar = () => screen.getByRole("button", { name: "Zoom out" }).parentElement!;
  const group = () => bar().parentElement!;

  test("what the page hands in is drawn in the canvas's own control group", () => {
    const spies = builderSpies();
    const probe = harnessProbe();
    render(
      <BuilderHarness initial={checkedEdit(fixtureCompany())} spies={spies} probe={probe}>
        <CanvasView
          chart="structure"
          chrome={{
            controls: <button type="button">Fullscreen</button>,
            switcher: <div data-testid="chart-switch">Structure</div>,
          }}
        />
      </BuilderHarness>,
    );
    LayoutObserver.settle();
    // In the BAR, after the four controls the canvas draws itself.
    expect([...bar().querySelectorAll("button")].map((b) => b.getAttribute("aria-label"))).toEqual([
      "Zoom out",
      "Zoom is 100 percent. Set a zoom level",
      "Zoom in",
      "Fit to view",
      null,
    ]);
    expect(bar().textContent).toContain("Fullscreen");
    // And the switch is its own bar under it, in the same group.
    expect(group().querySelector("[data-testid='chart-switch']")).not.toBeNull();
  });

  test("a chart the page hands nothing draws the bar and an empty region", () => {
    mount();
    // The bar, and the region the way out of fullscreen is said in, which is
    // empty until there is something to say and drawn as nothing while it is.
    expect([...group().children]).toEqual([bar(), screen.getByRole("status")]);
    expect(screen.getByRole("status").textContent).toBe("");
  });

  /*
   * A READER WHO CANNOT LEAVE A SCREEN IS STUCK ON IT. Fullscreen takes the
   * browser's own chrome with it, so the only way back is a key, and the
   * builder said nothing about which: searched on the running build, no
   * element anywhere matched "press esc".
   */
  test("the way out of fullscreen is drawn and announced while the page is in it", () => {
    const { container } = mount();
    // The region is there and says nothing, which is what makes what it says
    // next an announcement rather than a surface appearing.
    expect(screen.getByRole("status").textContent).toBe("");

    const element = container.querySelector("div")!;
    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      value: element,
    });
    act(() => {
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    const note = screen.getByRole("status");
    // The key twice, which is `Kbd`: the glyph a reader sees and the word a
    // screen reader is given for it.
    expect(note.textContent).toBe("PressEscEscto leave fullscreen");
    expect(group().contains(note)).toBe(true);

    Object.defineProperty(document, "fullscreenElement", { configurable: true, value: null });
    act(() => {
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    expect(screen.getByRole("status").textContent).toBe("");
  });
});

/*
 * THE CHART SAYS WHERE. A dialog that asks for the name of a child says
 * nothing about where the child will go, and the chart is the only thing that
 * can say it: it goes to the node the surface is about, pushes itself back
 * behind it, and gives the reader their own view back when it closes. It is
 * the gesture the chart this is drawn from makes with the viewBox it saves.
 */
describe("a surface opened about one node", () => {
  const mountAbout = (about: string | null) => {
    const spies = builderSpies();
    const probe = harnessProbe();
    const tree = (node: string | null) => (
      <BuilderHarness initial={checkedEdit(fixtureCompany())} spies={spies} probe={probe}>
        <CanvasView chart="structure" about={node} />
      </BuilderHarness>
    );
    const utils = render(tree(about));
    LayoutObserver.settle();
    return {
      container: utils.container,
      show: (node: string | null) => {
        utils.rerender(tree(node));
        LayoutObserver.settle();
      },
    };
  };
  /* The canvas is the box its own viewport is drawn in, found by what it IS:
     a focusable group announced as a canvas. */
  const canvas = () =>
    screen.getByRole("group", { name: "Structure chart" }).closest("[data-dimmed]")!;

  test("the chart is pushed back while it is open and drawn plainly when it is not", () => {
    const { show } = mountAbout(null);
    expect(canvas().getAttribute("data-dimmed")).toBe("false");
    show(unitKey("Engineering"));
    expect(canvas().getAttribute("data-dimmed")).toBe("true");
    show(null);
    expect(canvas().getAttribute("data-dimmed")).toBe("false");
  });

  test("the view goes to the node it is about, and comes back when it closes", () => {
    const { container, show } = mountAbout(null);
    const view = () => canvasWorld(container).style.transform;
    const before = view();
    show(unitKey("Engineering"));
    const onIt = view();
    expect(onIt).not.toBe(before);
    show(null);
    expect(view()).toBe(before);
  });
});
