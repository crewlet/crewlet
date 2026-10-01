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
 * - the reporting chart is the engine's forest of the SAVED chart with the
 *   cycle group, read-only;
 * - focus lands where the Builder sends it after an add, a delete, a move, an
 *   undo and a redo, opening a collapsed unit on the way and never scrolling;
 * - a live agents push changes a badge and never the layout.
 *
 * jsdom has no layout, so `LayoutObserver` reports sizes (see `viewTestkit`).
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi, type MockInstance } from "vitest";
import type { AgentRow, ChartRead } from "~/protocol/index.ts";
import type { ChartKind } from "./BuilderContext.tsx";
import { CanvasView, type Adding } from "./CanvasView.tsx";
import { OrgNodeLabel } from "@crewlethq/ui";
import { COMPANY_KEY, seatKey, unitKey } from "./model/keys.ts";
import type { BuilderState } from "./model/reducer.ts";
import { NODE_TONES } from "./nodeTone.ts";
import { chartOf, fixtureChart } from "./model/testkit.ts";
import { checkedEdit, checkWith, findingOn, loadedState, record } from "./testState.ts";
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

// WHAT REACT SAYS IS A FAILURE HERE, not a line in the log. Installed before
// the layout observer, because installing it is what draws the design system's
// reference chart: measured from inside React's commit, that chart printed
// eight "flushSync was called from inside a lifecycle method" warnings into
// whichever case of this file ran first, and the case passed.
let consoleError: MockInstance<typeof console.error>;
let restore: () => void;
beforeEach(() => {
  consoleError = vi.spyOn(console, "error");
  restore = LayoutObserver.install();
});
afterEach(() => {
  cleanup();
  restore();
  location.hash = "";
  const said = consoleError.mock.calls.map((call) => call.map(String).join(" "));
  consoleError.mockRestore();
  expect(said).toEqual([]);
});

interface Mounted {
  spies: BuilderSpies;
  probe: HarnessProbe;
  rerender: (props?: { agents?: AgentRow[]; readOnly?: boolean; adding?: Adding | null }) => void;
  container: HTMLElement;
}

function mount(
  initial: BuilderState = checkedEdit(),
  {
    chart = "structure",
    readOnly = false,
    adding = null,
  }: { chart?: ChartKind; readOnly?: boolean; adding?: Adding | null } = {},
): Mounted {
  const spies = builderSpies();
  const probe = harnessProbe();
  const tree = (
    props: { agents?: AgentRow[]; readOnly?: boolean; adding?: Adding | null } = {},
  ) => (
    <BuilderHarness
      initial={initial}
      spies={spies}
      probe={probe}
      readOnly={props.readOnly ?? readOnly}
      agents={props.agents}
    >
      <CanvasView chart={chart} adding={props.adding === undefined ? adding : props.adding} />
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

/**
 * The treeitem whose name starts with `name`.
 *
 * A LOCATOR, so it skips the accessibility filter (`hidden: true`): that
 * filter computes the style of every treeitem's every ancestor, the document
 * changes between two calls so none of it is cached, and the cases that call
 * this once per step spent more on finding nodes than on what they asserted.
 * Whether the tree's items ARE exposed is asserted where it is the claim — by
 * the cases that query the tree by role without it.
 */
const item = (name: string) =>
  screen
    .getAllByRole("treeitem", { hidden: true })
    .find((el) => within(el).queryAllByText(name, { exact: true }).length > 0)!;
const press = (key: string, init: Partial<KeyboardEventInit> = {}) =>
  fireEvent.keyDown(document.activeElement ?? document.body, { key, ...init });
/**
 * A pointer press on a button of the card `node` is drawn in, as a browser
 * makes one: the press moves focus to the button unless the view stops it,
 * which jsdom leaves to the caller.
 *
 * FOUND IN THAT CARD. The pointer's buttons are hidden from assistive
 * technology, so the query has to include hidden elements, and across the
 * whole chart that made every button of every card a candidate whose name
 * jsdom computed — profiled, the costliest line of the cases that press them.
 */
const pointerPress = (node: string, name: string) => {
  const button = within(chartCard(item(node))).getByRole("button", { name, hidden: true });
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
    location.hash = "#/company?lens=builder&view=visualization&chart=reporting";
    mount();
    expect(screen.getByRole("tree", { name: "Structure chart" })).toBeDefined();
    cleanup();
    location.hash = "#/company?lens=builder&view=visualization&chart=structure";
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
      [unitKey("engineering"), "2", "2", "3", "true"],
      // A unit's seats, then its units, each by address: the one order the
      // chart keeps among siblings.
      [seatKey("dev"), "3", "1", "3", null],
      [seatKey("vp-engineering"), "3", "2", "3", null],
      [unitKey("platform"), "3", "3", "3", "true"],
      [seatKey("designer"), "4", "1", "2", null],
      [seatKey("sre"), "4", "2", "2", null],
      [unitKey("sales"), "2", "3", "3", "true"],
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
    pointerPress("Dev", "Actions for Dev");
    expect(screen.getByRole("menu", { name: "Actions for Dev" })).toBeDefined();
    press("Escape");
    // Focus in a subtree hidden from assistive technology is focus nowhere, so
    // the menu hands it back to the node rather than to the button.
    expect(document.activeElement).toBe(item("Dev"));
    expect(probe.selection).toBe(seatKey("dev"));

    pointerPress("Engineering", "Collapse Engineering");
    expect(document.activeElement).toBe(item("Engineering"));
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("false");

    // The lead chip is drawn in the same hidden strip. It says the word
    // itself, as the chart this is drawn from does: see `leadChipLabel`.
    pointerPress("Engineering", "Lead: VP Engineering");
    press("Escape");
    expect(document.activeElement).toBe(item("Engineering"));
  });

  test("a human seat is drawn with the dashed edge, by kind rather than by colour", () => {
    mount(checkedEdit(humanCeo()));
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

  test("a reference that names nothing is marked with the draft's own sentence, and the Datadog fallback wears its mark", () => {
    const chart = fixtureChart();
    chart.units.find((u) => u.key === "sales")!.lead = "ghost";
    const state = checkedEdit(chartOf({ ...chart, manages: { ceo: ["engineering", "nowhere"] } }));
    const { probe } = mount(state);
    // A NODE IS ONE RANK TALL, so a wiring mark is a glyph on its caption
    // rather than a badge with the sentence written out. Both sentences,
    // because both readers need one: the glyph's accessible name and the
    // tooltip a pointer gets say the same thing.
    expect(markNames(item("Sales"))).toEqual([
      [
        "The lead ghost names no seat in this draft.",
        "The lead ghost names no seat in this draft.",
      ],
    ]);
    expect(markSentences(item("CEO"))).toEqual([
      "Manages nowhere, which names no seat and no unit in this draft.",
    ]);
    expect(markSentences(item("SRE"))).toEqual(["Alerts that name no seat wake this seat"]);
    // The control: a unit whose lead names a seat wears no mark.
    expect(markSentences(item("Engineering"))).toEqual([]);
    // READ OFF THE DRAFT, so it holds while the check of a later edit is out.
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
    expect(markSentences(item("Sales"))).toEqual(["The lead ghost names no seat in this draft."]);
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
    expect(card.textContent).toBe("DevAgent seat@dev");
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

  /*
   * A human seat keeps the small figure inside its dashed ring, which is the
   * boundary that says what it is.
   *
   * THE FIGURE'S OWN SIZE, not only the ring's class. This case read the class
   * alone and stayed green while the figure grew to the zone's own step and
   * measured 20px inside the 24px ring, touching it on every side: the ring
   * stops reading as a ring, which is the one thing it is there to do. Every
   * other mark IS the zone and answers `1em`, which is how an agent seat wears
   * the large one.
   */
  test("a human seat's mark stays the small step inside its ring", () => {
    mount(checkedEdit(humanCeo()));
    const zone = item("CEO").querySelector("[aria-hidden='true']")!;
    expect(zone.className).toContain(markStep("ring"));
    expect(zone.className).not.toContain(markStep("large"));
    expect(zone.querySelector("svg")?.getAttribute("width")).toBe("14px");
    // The agent seat beside it takes whatever its zone is set to.
    expect(item("Dev").querySelector("svg")?.getAttribute("width")).toBe("1em");
  });

  /*
   * A NODE SAYS WHAT IT IS, NOT HOW IT IS DOING. The chart drew the engine's
   * problem count and the seat's live state in the label's trailing slot on
   * every node, which is a column of "idle" down a company where nothing is
   * running and an empty box beside every node with no problem. Neither is a
   * fact about the ORGANIZATION, which is what this chart draws.
   *
   * WHERE THEY ARE STILL READ: the toolbar counts the whole draft's problems,
   * and the table draws a live state per row. The per-node COUNT has no other
   * home on this build, which is stated here so that bringing it back is a
   * decision somebody makes rather than a regression nobody noticed.
   */
  test("a node carries no live state and no problem count", () => {
    const state = checkWith(checkedEdit(), [
      findingOn(seatKey("account-executive"), ["name"], "The name is longer than the chart takes."),
    ]);
    mount(state);
    // The node the problem is on says nothing about it, and neither does any
    // other: no count, and no slot kept for one.
    expect(within(item("Account Executive")).queryByLabelText(/problem/)).toBeNull();
    for (const name of ["Account Executive", "Dev", "Engineering", "Acme"]) {
      expect(nodeTrailing(item(name))).toBeNull();
    }
    // And no seat says how it is doing.
    expect(within(item("Dev")).queryByText("offline")).toBeNull();
  });

  test("a unit's other findings are not read as a lead that names no seat", () => {
    const state = checkWith(checkedEdit(), [
      findingOn(unitKey("sales"), ["name"], "another unit is also called Sales", "warning"),
    ]);
    mount(state);
    expect(within(item("Sales")).queryByText("Lead names no seat")).toBeNull();
    expect(markSentences(item("Sales"))).toEqual([]);
  });
});

describe("keys", () => {
  test("arrows walk the visible order, Right and Left open, close and climb, Home and End jump", () => {
    const { probe } = mount();
    item("Acme").focus();
    press("ArrowDown");
    expect(focused()).toBe(seatKey("ceo"));
    press("ArrowDown");
    expect(focused()).toBe(unitKey("engineering"));
    press("ArrowLeft");
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryAllByText("Dev")).toHaveLength(0);
    press("ArrowDown");
    expect(focused()).toBe(unitKey("sales"));
    press("ArrowUp");
    press("ArrowRight");
    expect(item("Engineering").getAttribute("aria-expanded")).toBe("true");
    press("ArrowRight");
    expect(focused()).toBe(seatKey("dev"));
    press("ArrowLeft");
    expect(focused()).toBe(unitKey("engineering"));
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
    expect(focused()).toBe(unitKey("platform"));
    press("0");
    expect(focused()).toBe(unitKey("platform"));
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
    /*
     * NEITHER OF THE TWO THE CARD DRAWS BESIDE IT. Edit and Delete are buttons
     * on this card's own right edge and the keys that do them are Enter and
     * Delete, so a menu offering them again is one action with two entries
     * (`nodeActions.cardMenu`). There is no move among the siblings: the
     * chart keeps no order among them to move a node through.
     */
    expect(within(menu).getAllByRole("menuitem").map(label)).toEqual([
      "Open seat",
      "Edit reports",
      "Change to human seat",
      "Move to",
    ]);
    press("Escape");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(dev);

    press("F10", { shiftKey: true });
    fireEvent.click(screen.getByRole("menuitem", { name: "Move to" }));
    expect(spies.openMove).toHaveBeenCalledWith(seatKey("dev"));
    expect(document.activeElement).toBe(dev);

    // Edit is the card's own control and Enter, not a menu entry; Edit reports
    // opens the same form at the seat's reports and is only in the menu.
    press("Enter");
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
 * THE CHART KEEPS NO ORDER AMONG SIBLINGS: a node sits where its address
 * sorts, so Alt with an arrow has no place to move it to and records nothing,
 * and the arrows alone are still the tree's own.
 */
describe("a node among its siblings", () => {
  test("Alt and an arrow record nothing, and an arrow alone still walks the chart", () => {
    const { probe } = mount();
    item("VP Engineering").focus();
    press("ArrowUp", { altKey: true });
    expect(probe.state.log.ops).toHaveLength(0);
    press("ArrowUp");
    expect(focused()).toBe(seatKey("dev"));
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
      // THE EXPANDER FIRST, on the node's leading edge, which is where every
      // hierarchy a reader has used puts a disclosure. It shared the branch
      // strip with the Add, where the add pill splitting open into its three
      // kinds covered it, and that is the one gesture a reader makes next to
      // it.
      "Collapse Engineering",
      "Edit Engineering",
      "Delete Engineering",
      "Actions for Engineering",
      // The lead along the bottom edge, which stays drawn: it is a fact about
      // the organization rather than a tool for changing it. The X inside it
      // is the tool, and it is quiet with the rest of them.
      "Lead: VP Engineering",
      "Clear the lead of Engineering",
      // And the branch below is the Add, alone.
      "Add to Engineering",
    ]);
    pointerPress("Engineering", "Edit Engineering");
    expect(spies.openEditor).toHaveBeenCalledWith(unitKey("engineering"));
    pointerPress("Engineering", "Delete Engineering");
    expect(spies.openDelete).toHaveBeenCalledWith(unitKey("engineering"));
  });

  /* The company cannot be deleted, so it is not offered and does not refuse. */
  test("the company has no Delete, and a read-only draft has none at all", () => {
    mount();
    expect(controls("Acme")).toEqual([
      "Collapse Acme",
      "Edit Acme",
      "Actions for Acme",
      "Add to Acme",
    ]);
    cleanup();
    mount(undefined, { readOnly: true });
    expect(controls("Engineering")).toEqual([
      "Collapse Engineering",
      "Edit Engineering",
      "Actions for Engineering",
    ]);
    // The lead is still READ on a read-only draft; it is simply not a control.
    expect(within(chartCard(item("Engineering"))).getByText("Lead: VP Engineering")).toBeDefined();
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
   * nowhere, and what it offers is everything the card does not already reach:
   * the three kinds, whose pill on the branch is pointer-only, and the moves.
   * Edit and Delete are not in it, because the card draws both and the keys
   * Enter and Delete do both.
   */
  test("the ContextMenu key opens what the card does not already reach", () => {
    mount();
    item("Engineering").focus();
    press("ContextMenu");
    expect(screen.getAllByRole("menuitem").map((m) => label(m))).toEqual([
      "Add unit",
      "Add agent seat",
      "Add human seat",
      "Move to",
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
    mount(checkedEdit(humanCeo()));
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
    expect(spies.openAdd).toHaveBeenLastCalledWith(unitKey("platform"), "human");
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
    expect(spies.openAdd).toHaveBeenLastCalledWith(unitKey("platform"), "human");
  });

  test("a seat this draft created has no screen to open, and read-only disables every change", () => {
    const added = record(checkedEdit(), {
      type: "addSeat",
      key: "new:s1",
      placement: { parent: unitKey("sales") },
      data: { handle: "closer", name: "Closer" },
    });
    mount(added, { readOnly: true });
    item("Closer").focus();
    press("ContextMenu");
    const names = screen
      .getAllByRole("menuitem")
      .map((m) => [label(m), m.getAttribute("aria-disabled") === "true"]);
    /*
     * READ-ONLY DISABLES, IT DOES NOT HIDE. Edit and Delete are the card's own
     * two controls rather than menu entries, and they carry the refusal there;
     * a seat that exists only in the draft has no screen, so Open seat is
     * absent rather than disabled.
     */
    expect(names).toEqual([
      ["Edit reports", false],
      ["Change to human seat", true],
      ["Move to", true],
    ]);
  });

  test("the lead is chosen in place: No lead says what it inherits, and a member becomes the lead", () => {
    const { probe } = mount();
    // Platform declares no lead and inherits Engineering's, read off the draft
    // by the engine's own cascade.
    const chip = screen.getByRole("button", {
      name: /^Lead: VP Engineering \(inherited\)$/,
      hidden: true,
    });
    fireEvent.click(chip);
    const answers = screen.getAllByRole("menuitemradio");
    expect(answers.map((a) => [a.textContent, a.getAttribute("aria-checked")])).toEqual([
      ["No lead (inherits VP Engineering)", "true"],
      ["Designer", "false"],
      ["SRE", "false"],
    ]);
    fireEvent.click(screen.getByRole("menuitemradio", { name: "SRE" }));
    const platform = probe.state.draft.units.find((u) => u.data.key === "engineering")!
      .children[0]!;
    // Written as the chart writes a lead: the seat's handle.
    expect(platform.data.lead).toBe("sre");
    expect(within(item("Platform")).getByText("Lead: SRE.")).toBeDefined();
    // Engineering's own lead is unchanged, and so is what the check said of it.
    expect(within(item("Engineering")).getByText("Lead: VP Engineering.")).toBeDefined();
    // Now declared, the choice says so, and No lead offers the parent's lead.
    fireEvent.click(screen.getByRole("button", { name: "Lead: SRE", hidden: true }));
    expect(
      screen
        .getAllByRole("menuitemradio")
        .map((a) => [a.textContent, a.getAttribute("aria-checked")]),
    ).toEqual([
      ["No lead (inherits VP Engineering)", "false"],
      ["Designer", "false"],
      ["SRE", "true"],
    ]);
    press("Escape");

    // Engineering cleared: at the top of the company it inherits nobody, and
    // says so at once — the cascade is read off the draft, never waited for.
    act(() =>
      probe.dispatch({
        type: "record",
        intent: { type: "setLead", target: unitKey("engineering") },
      }),
    );
    expect(within(item("Engineering")).getByText("No lead.")).toBeDefined();
    expect(within(item("Platform")).getByText("Lead: SRE.")).toBeDefined();
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
        intent: { type: "setLead", target: unitKey("engineering"), lead: undefined },
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
    mount(checkedEdit(), { readOnly: true });
    expect(screen.queryByRole("button", { name: /^Clear the lead/, hidden: true })).toBeNull();
  });

  test("a lead declared outside the unit is still the checked answer, and another seat is chosen in the editor", () => {
    const chart = fixtureChart();
    chart.units.find((u) => u.key === "sales")!.lead = "ceo";
    const { spies } = mount(checkedEdit(chart));
    const sales = within(chartCard(item("Sales")));
    fireEvent.click(sales.getByRole("button", { name: "Lead: CEO", hidden: true }));
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
    expect(spies.openEditor).toHaveBeenCalledWith(unitKey("sales"), "leadership");
  });

  test("choosing the answer already chosen records nothing and is refused nowhere", () => {
    const { probe, spies } = mount();
    // Engineering declares VP Engineering; Sales declares no lead.
    fireEvent.click(screen.getByRole("button", { name: "Lead: VP Engineering", hidden: true }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: "VP Engineering" }));
    expect(screen.queryByRole("menu")).toBeNull();
    const sales = within(chartCard(item("Sales")));
    // An empty pill still says the word: "Lead", as that chart writes it.
    fireEvent.click(sales.getByRole("button", { name: "Lead", hidden: true }));
    fireEvent.click(screen.getByRole("menuitemradio", { name: "No lead" }));
    expect(spies.dispatched).toEqual([]);
    expect(probe.state.refusal).toBeNull();
  });
});

describe("the reporting chart", () => {
  const loop: ChartRead = chartOf({
    seats: [
      { handle: "chief", name: "Chief" },
      { handle: "ops", name: "Ops" },
      { handle: "a", name: "A" },
      { handle: "b", name: "B" },
    ],
    manages: { chief: ["ops"], a: ["b"], b: ["a"] },
  });
  /** The engine's derivation of the saved chart, as the org push carries it. */
  const derivedLoop = {
    seats: {
      ops: { manager: "chief" },
      a: { manager: "b" },
      b: { manager: "a" },
    },
  };

  /*
   * THE EXPANDER IS BESIDE THE NODE ON BOTH CHARTS, and nothing hangs on the
   * branch here at all: the reporting chart adds nothing, so the strip under a
   * card would be an empty band on every node of it. Both charts of one
   * organization draw one shape, which is the rule this whole module exists
   * for, and the cycle GROUP is a node a reader collapses like any other.
   */
  test("a node's expander is drawn beside it, and the branch carries nothing", () => {
    const { container } = mount(checkedEdit(loop, derivedLoop), { chart: "reporting" });
    for (const name of ["Chief", "Reporting cycle", "A"]) {
      const collapse = within(chartCard(item(name))).getByRole("button", {
        name: `Collapse ${name === "Reporting cycle" ? "the reporting cycles" : name}`,
        hidden: true,
      });
      // Beside the treeitem, never inside it: a tree's items hold nothing
      // focusable, and the whole chart is held to that a few cases above.
      expect(collapse.closest("[role='treeitem']")).toBeNull();
    }
    // A LEAF HAS NONE. Ops is the bottom of its branch, so its card is drawn
    // with no disclosure rather than with an empty one.
    expect(
      within(chartCard(item("Ops"))).queryByRole("button", {
        name: /^(Collapse|Expand) /,
        hidden: true,
      }),
    ).toBeNull();
    // And it still collapses, from the control that is there.
    pointerPress("Chief", "Collapse Chief");
    expect(item("Chief").getAttribute("aria-expanded")).toBe("false");
    // Nothing hangs on a branch of this chart: it adds nothing, so a strip
    // there would be an empty band under every node of it.
    expect(
      within(container).queryAllByRole("button", { name: /^Add /, hidden: true }),
    ).toHaveLength(0);
  });

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

  test("says its lines are the saved company's once the draft has moved past it", () => {
    const { probe, container } = mount(checkedEdit(loop, derivedLoop), { chart: "reporting" });
    const note = () => screen.queryByText(/These are the saved company's reporting lines/);
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
    expect(shown.textContent).toMatch(/appear here once it is saved/);
    // The chart itself is still the saved company's forest.
    expect(nameOf(item("Ops"))).toBe("Ops");
  });

  test("says there is nobody to draw when the checked draft holds no seat", () => {
    mount(checkedEdit(chartOf({}), {}, { name: "Fresh" }), { chart: "reporting" });
    expect(screen.getByText("No seats to report on")).toBeDefined();
    expect(screen.queryByRole("tree")).toBeNull();
  });

  test("says it is waiting for the engine before the org push has described the company", () => {
    mount(loadedState(loop, { name: "Loop" }), { chart: "reporting" });
    expect(
      screen.getByText("Reporting lines appear once the engine describes the company"),
    ).toBeDefined();
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
   * has a lead in the fixture, Platform draws the one it inherits from it, and
   * Sales has none.
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
    expect(topOf("Dev")).toBe(topOf("VP Engineering"));
    expect(topOf("Account Executive")).toBe(topOf("Dev"));
    // And Platform, whose inherited lead makes it the taller card of that
    // rank, is centred on the same line as the seats beside it.
    expect(topOf("Dev") - topOf("Platform")).toBe(LEAD_STRIP / 2);
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
          placement: { parent: unitKey("sales") },
          data: { handle: "closer", name: "Closer" },
        },
      }),
    );
    LayoutObserver.settle();
    expect(follow()).toBe("new:s1");

    act(() =>
      probe.dispatch({ type: "record", intent: { type: "remove", target: seatKey("dev") } }),
    );
    LayoutObserver.settle();
    // The previous sibling, else the parent: Dev is the first of its unit's
    // seats by address.
    expect(follow()).toBe(unitKey("engineering"));

    act(() =>
      probe.dispatch({
        type: "record",
        intent: {
          type: "move",
          target: seatKey("sre"),
          to: { parent: unitKey("sales") },
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
    expect(follow()).toBe(unitKey("engineering"));
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
          placement: { parent: COMPANY_KEY },
          data: { handle: "advisor", name: "Advisor" },
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
  /*
   * A PUSH NEVER MOVES THE CHART. Nothing a push carries is drawn on a node
   * any more, so this is now the stronger claim: an agents push changes
   * nothing at all here, and every card, branch and the view are identical
   * across it.
   */
  test("an agents push never moves the chart", () => {
    const { rerender, container } = mount();
    const layout = () => ({
      boxes: chartCards(container).map((b) => [b, b.style.transform]),
      world: canvasWorld(container).style.transform,
      links: chartLinks(container).innerHTML,
      says: chartCards(container).map((b) => b.textContent),
    });
    const before = layout();
    rerender({
      agents: [{ id: "a1", agent_id: "id-a1", role: "Dev", handle: "dev", state: "working" }],
    });
    expect(layout()).toEqual(before);
  });

  /*
   * WHICH SEAT HAS A LIVE STATE is no longer a question about this chart: a
   * node says what it is and not how it is doing, and the state is drawn in
   * the TABLE. The rule it followed (only a saved agent seat has one, found by
   * the handle it runs with rather than by the name the draft gives it) is
   * `LiveState`'s own and still holds where it is drawn, but no suite covers
   * it any more: the table's do not, and the case that did was this one.
   * Reported to the owners of `nodeMarks.tsx` and `TableView.tsx`.
   */
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
  // THE CHART'S OWN REGION, asked of the chart's control group: the page also
  // holds the application's live region, which the editor speaks into.
  const hint = () => within(group()).getByRole("status");

  test("what the page hands in is drawn in the canvas's own control group", () => {
    const spies = builderSpies();
    const probe = harnessProbe();
    render(
      <BuilderHarness initial={checkedEdit()} spies={spies} probe={probe}>
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
    expect([...group().children]).toEqual([bar(), hint()]);
    expect(hint().textContent).toBe("");
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
    expect(hint().textContent).toBe("");

    const element = container.querySelector("div")!;
    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      value: element,
    });
    act(() => {
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    // The key twice, which is `Kbd`: the glyph a reader sees and the word a
    // screen reader is given for it — said in the chart's own control group,
    // which is where `hint` looks.
    expect(hint().textContent).toBe("PressEscEscto leave fullscreen");

    Object.defineProperty(document, "fullscreenElement", { configurable: true, value: null });
    act(() => {
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    expect(hint().textContent).toBe("");
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
      <BuilderHarness initial={checkedEdit()} spies={spies} probe={probe}>
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
    show(unitKey("engineering"));
    expect(canvas().getAttribute("data-dimmed")).toBe("true");
    show(null);
    expect(canvas().getAttribute("data-dimmed")).toBe("false");
  });

  test("the view goes to the node it is about, and comes back when it closes", () => {
    const { container, show } = mountAbout(null);
    const view = () => canvasWorld(container).style.transform;
    const before = view();
    show(unitKey("engineering"));
    const onIt = view();
    expect(onIt).not.toBe(before);
    show(null);
    expect(view()).toBe(before);
  });
});

// ---------------------------------------------------------------------------
// Adding a node in the chart
// ---------------------------------------------------------------------------

/*
 * WHAT THESE PROTECT. Picking a kind from the Add draws the engine's own add
 * form in the ghost of the node about to exist, rather than a dialog over a
 * blurred chart:
 *
 * - it hangs off the parent it will hang off, in the rank it will land in;
 * - the tree it is drawn in is untouched by it: the counts a screen reader is
 *   told, the keys and the selection are what they were;
 * - focus goes into the form and comes back to the node it was added to;
 * - Escape cancels, and the chart is given back;
 * - every refusal, every help and the address suggestion the dialog could
 *   show are still shown, and the add it records is the same operation.
 */

/** An add of `kind` under `parent`, and what the chart was told about closing it. */
function adding(parent: string | null, kind: "unit" | "agent" | "human" = "agent") {
  const closed: number[] = [];
  const request: Adding = {
    parent,
    kind,
    opening: 1,
    onClose: () => closed.push(1),
  };
  return { request, closed };
}

/** The ghost, found the way a reader meets it: a named region holding a form. */
const ghost = (name: string) => screen.getByRole("group", { name });

describe("adding a node in the chart", () => {
  test("the form is drawn in the chart, under the node it is added to", () => {
    const view = mount();
    const branches = () => chartLinks(view.container).querySelectorAll("path").length;
    const before = branches();
    view.rerender({ adding: adding(unitKey("engineering")).request });
    const form = ghost("Add to Engineering");
    // No dialog: the chart itself is where the question is asked, so the
    // picture behind it is neither covered nor pushed back.
    expect(screen.queryByRole("dialog")).toBeNull();
    // A card of the chart, placed by the same layout as every other one, and
    // below the parent it hangs from.
    expect(translate(chartCard(form)).y).toBeGreaterThan(
      translate(chartCard(item("Engineering"))).y,
    );
    // AND THE BRANCH INTO IT IS DRAWN, which is what makes this a chart saying
    // where the node goes rather than a form that happens to be drawn nearby.
    expect(branches()).toBe(before + 1);
  });

  test("the tree is what it was: the ghost is no node of the draft", () => {
    const shape = () =>
      screen
        .getAllByRole("treeitem")
        .map((el) =>
          [
            el.getAttribute("data-tree-id"),
            el.getAttribute("aria-level"),
            el.getAttribute("aria-posinset"),
            el.getAttribute("aria-setsize"),
          ].join("/"),
        );
    const view = mount();
    const before = shape();
    view.rerender({ adding: adding(unitKey("engineering")).request });
    expect(ghost("Add to Engineering")).toBeDefined();
    expect(shape()).toEqual(before);
  });

  test("no key of the chart lands on the ghost", () => {
    mount(undefined, { adding: adding(unitKey("engineering")).request });
    item("Engineering").focus();
    press("ArrowDown");
    const reached: (string | null | undefined)[] = [];
    for (let step = 0; step < 12; step += 1) {
      reached.push(focused());
      press("ArrowDown");
    }
    expect(reached.every((id) => id !== undefined && id !== null)).toBe(true);
    // Every stop is a node of the draft. The ghost has no `data-tree-id` at
    // all, so a stop on it would read as nothing here.
    expect(
      reached.every(
        (id) => id!.startsWith("seat:") || id!.startsWith("unit:") || id === COMPANY_KEY,
      ),
    ).toBe(true);
  });

  test("focus goes into the form and back to the node it was added to", () => {
    const view = mount();
    item("Engineering").focus();
    view.rerender({ adding: adding(unitKey("engineering")).request });
    /*
     * ON THE KIND, which is the question the add is asking. Everything else in
     * the form follows from the answer: the name is pre-filled with one free
     * FOR THAT KIND and changes when the kind does, the unit type exists only
     * for a unit and the contact identity only for a human seat. A reader who
     * wants what was offered presses the primary control without touching the
     * name at all.
     *
     * AND THE DIALOG OPENS ON THE SAME CONTROL. One set of fields opening on
     * two different ones, because a ghost is hidden on the tick its form
     * mounts and a dialog is not, is worse than either answer: the note at the
     * top of `AddNodeDialog.tsx` is where that is decided.
     */
    expect(document.activeElement).toBe(
      within(ghost("Add to Engineering")).getByRole("radio", { name: "Agent seat" }),
    );
    view.rerender({ adding: null });
    expect(focused()).toBe(unitKey("engineering"));
  });

  test("Escape in the form closes the add", () => {
    const { request, closed } = adding(unitKey("engineering"));
    mount(undefined, { adding: request });
    fireEvent.keyDown(within(ghost("Add to Engineering")).getByLabelText("Name"), {
      key: "Escape",
    });
    expect(closed).toHaveLength(1);
  });

  test("the chart eases onto the ghost and gives the view back", () => {
    const view = mount();
    const world = () => canvasWorld(view.container).style.transform;
    const before = world();
    view.rerender({ adding: adding(unitKey("engineering")).request });
    expect(world()).not.toBe(before);
    view.rerender({ adding: null });
    expect(world()).toBe(before);
  });

  /*
   * THE SAME ADD, RECORDED THE SAME WAY. The ghost is a shell around the form
   * the dialog also draws, so what it records is the reducer's own operation
   * with the key the lens minted: a second, quieter add drawn in the chart is
   * exactly what this arrangement must not become.
   */
  test("the form records the add and closes", () => {
    const { request, closed } = adding(unitKey("engineering"), "unit");
    const view = mount(undefined, { adding: request });
    const form = ghost("Add to Engineering");
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "Tooling" } });
    fireEvent.click(within(form).getByRole("button", { name: "Add unit" }));
    expect(closed).toHaveLength(1);
    const recorded = view.spies.dispatched.at(-1);
    expect(recorded).toMatchObject({
      type: "record",
      intent: { type: "addUnit", placement: { parent: unitKey("engineering") } },
    });
  });

  /*
   * EVERY REFUSAL THE DIALOG COULD SHOW IS STILL SHOWN. The address is the
   * one a reader meets most: it follows the name to one nobody holds, and a
   * taken one is refused in the ghost as in the dialog.
   */
  test("the address follows the name to a free one, and a taken one is refused, in the ghost", () => {
    mount(undefined, { adding: adding(unitKey("engineering")).request });
    const form = ghost("Add to Engineering");
    const handle = () => within(form).getByLabelText(/^Handle/) as HTMLInputElement;
    fireEvent.change(within(form).getByLabelText("Name"), { target: { value: "Dev" } });
    expect(handle().value).toBe("dev-2");
    fireEvent.change(handle(), { target: { value: "dev" } });
    expect(
      within(form).getByText("dev is taken: something in this company holds it, or held it."),
    ).toBeDefined();
  });

  test("a read-only draft says so in the ghost and records nothing", () => {
    const { request } = adding(unitKey("engineering"));
    const view = mount(undefined, { adding: request, readOnly: true });
    const form = ghost("Add to Engineering");
    expect(within(form).getByRole("button", { name: "Add agent seat" })).toHaveProperty(
      "disabled",
      true,
    );
    expect(view.spies.dispatched).toHaveLength(0);
  });

  /*
   * A GHOST ON A BRANCH THAT IS NOT THERE IS NOT DRAWABLE, so the chart draws
   * none. The Builder is what falls back to the dialog in that case, where the
   * refusal saying so is drawn; here the guard is only that the chart does not
   * hang a form off nothing.
   */
  test("no ghost where the parent has left the draft", () => {
    const view = mount();
    const cards = chartCards(view.container).length;
    view.rerender({ adding: adding(unitKey("gone")).request });
    /*
     * NOT ONE MORE CARD ON THE CHART, which is the only honest way to ask
     * this. A ghost whose parent is no card of the chart hangs off nothing, so
     * the layout never places it and it is drawn at `visibility: hidden`: every
     * query by role then answers "absent" whether the chart drew it or not,
     * and a guard written that way passed with the form drawn off the company
     * instead of the missing unit. What the chart may not do is draw a card
     * for it at all.
     */
    expect(chartCards(view.container)).toHaveLength(cards);
    expect(screen.getAllByRole("treeitem").length).toBeGreaterThan(0);
  });

  test("the company's own add is named after the company", () => {
    mount(undefined, { adding: adding(null).request });
    expect(ghost("Add to Acme")).toBeDefined();
  });
});

/** Where the layout put a card, read off the transform the chart writes. */
function translate(card: HTMLElement): { x: number; y: number } {
  const match = /translate\((-?[\d.]+)px,\s*(-?[\d.]+)px\)/.exec(card.style.transform);
  if (!match) throw new Error(`this card is not placed: ${card.style.transform}`);
  return { x: Number(match[1]), y: Number(match[2]) };
}

/** The fixture chart with the CEO a human seat reached on Slack. */
function humanCeo(): ChartRead {
  const chart = fixtureChart();
  chart.seats = chart.seats.map((seat) =>
    seat.handle === "ceo"
      ? { ...seat, kind: "human", runtime: { contact: { slack_user_id: "U0CEO" } } }
      : seat,
  );
  return chart;
}
