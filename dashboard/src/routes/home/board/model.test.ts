/**
 * A reader's board: read from storage as untrusted input, and edited by pure
 * functions the editor composes.
 *
 * STORAGE IS ANYBODY'S. A board is read back from `localStorage`, which any
 * script on the origin, an older build or a hand in the console may have
 * written. So every field is checked, every string capped, every link held to
 * the two shapes a link may take, and the whole board to one number of
 * widgets, before anything draws it.
 */

import { describe, expect, test } from "vitest";
import {
  COLUMNS,
  LIMITS,
  PAGE,
  SIZING,
  addWidget,
  contrib,
  drawnWidths,
  duplicate,
  everyWidget,
  fromLists,
  groupList,
  linkHref,
  listsOf,
  removeWidget,
  ungroup,
  validBoard,
  widgetsOn,
  type Board,
  type GroupWidget,
  type Leaf,
  type Widget,
} from "./model.ts";
import { widgetDef } from "./registry.tsx";

const EMPTY: Board = { widgets: [] };
const FALLBACK: Board = { widgets: [contrib("home.kpis")] };

const note = (id: string, w = 4): Leaf => ({
  id,
  kind: "note",
  text: "A note",
  tone: "amber",
  align: "start",
  size: "md",
  w,
  h: null,
});

const group = (id: string, w: number, children: Leaf[]): GroupWidget => ({
  id,
  kind: "group",
  title: "Delivery",
  tone: "blue",
  collapsed: false,
  w,
  h: null,
  children,
});

const total = (board: Board) => [...everyWidget(board)].length;

describe("a board read from storage", () => {
  test("anything but a list of widgets is the default board", () => {
    for (const raw of [null, undefined, 42, "board", [], { widgets: "many" }, { widgets: null }]) {
      expect(validBoard(raw, FALLBACK)).toBe(FALLBACK);
    }
  });

  test("an entry that is not a widget, or of a kind no build draws, is dropped", () => {
    const board = validBoard(
      { widgets: [null, 7, "note", { kind: "iframe", src: "https://example.com" }, note("n-1")] },
      FALLBACK,
    );
    expect(board.widgets.map((w) => w.id)).toEqual(["n-1"]);
  });

  test("every string is capped at its limit", () => {
    const long = "x".repeat(10_000);
    const board = validBoard(
      {
        widgets: [
          { ...note("n-1"), text: long },
          { id: "h-1", kind: "heading", text: long, size: "md", align: "start", w: 12 },
          { id: "d-1", kind: "divider", label: long, align: "start", w: 12 },
          { ...group("g-1", 12, []), title: long },
          {
            id: "l-1",
            kind: "links",
            title: long,
            links: [{ label: long, href: "#/inbox" }],
            w: 4,
          },
        ],
      },
      FALLBACK,
    );
    const [n, h, d, g, l] = board.widgets as [Widget, Widget, Widget, Widget, Widget];
    expect(n.kind === "note" && n.text.length).toBe(LIMITS.note);
    expect(h.kind === "heading" && h.text.length).toBe(LIMITS.heading);
    expect(d.kind === "divider" && d.label.length).toBe(LIMITS.title);
    expect(g.kind === "group" && g.title.length).toBe(LIMITS.title);
    expect(l.kind === "links" && l.title.length).toBe(LIMITS.title);
    expect(l.kind === "links" && l.links[0]!.label.length).toBe(LIMITS.linkLabel);
  });

  test("a field of the wrong type takes its default rather than its value", () => {
    const board = validBoard(
      {
        widgets: [
          { id: "n-1", kind: "note", text: 12, tone: "neon", align: "middle", size: "xl", w: "4" },
        ],
      },
      FALLBACK,
    );
    expect(board.widgets[0]).toMatchObject({
      kind: "note",
      text: "",
      tone: "amber",
      align: "start",
      size: "md",
      w: 4,
    });
  });

  test("the whole board holds at most the widget limit, groups' widgets included", () => {
    const children = Array.from({ length: LIMITS.widgets }, (_, i) => note(`c-${i}`));
    const groups = Array.from({ length: LIMITS.widgets }, (_, i) => group(`g-${i}`, 12, children));
    const board = validBoard({ widgets: groups }, FALLBACK);
    expect(total(board)).toBeLessThanOrEqual(LIMITS.widgets);
    const flat = validBoard(
      { widgets: Array.from({ length: LIMITS.widgets * 3 }, (_, i) => note(`n-${i}`)) },
      FALLBACK,
    );
    expect(flat.widgets).toHaveLength(LIMITS.widgets);
  });

  test("an id that is taken or malformed is replaced, so every widget has its own", () => {
    const board = validBoard(
      { widgets: [note("n-1"), note("n-1"), note("<script>"), note("")] },
      FALLBACK,
    );
    const ids = board.widgets.map((w) => w.id);
    expect(ids[0]).toBe("n-1");
    expect(new Set(ids).size).toBe(4);
    for (const id of ids) expect(id).toMatch(/^[a-z][a-z0-9-]{0,40}$/);
  });

  test("a link is kept only as a dashboard address or a web address", () => {
    const board = validBoard(
      {
        widgets: [
          {
            id: "l-1",
            kind: "links",
            title: "Links",
            w: 4,
            links: [
              { label: "Inbox", href: "#/inbox" },
              { label: "Docs", href: "https://docs.example.com/start" },
              { label: "Run", href: "javascript:alert(1)" },
              { label: "Data", href: "data:text/html,<b>x</b>" },
              { label: "Relative", href: "../settings" },
              { label: "Mail", href: "mailto:someone@example.com" },
            ],
          },
        ],
      },
      FALLBACK,
    );
    const links = board.widgets[0]!.kind === "links" ? board.widgets[0]!.links : [];
    expect(links.map((l) => l.href)).toEqual(["#/inbox", "https://docs.example.com/start"]);
  });

  test("linkHref says why it refuses an address", () => {
    expect(linkHref("#/worklet")).toEqual({ href: "#/worklet" });
    expect(linkHref("http://example.com")).toEqual({ href: "http://example.com/" });
    expect(linkHref("")).toEqual({ error: "Enter an address." });
    expect("error" in linkHref("javascript:void(0)")).toBe(true);
    expect("error" in linkHref("/settings")).toBe(true);
    expect("error" in linkHref(`#/${"a".repeat(LIMITS.linkHref)}`)).toBe(true);
  });

  test("a widget that may be on a board once is kept once, in or out of a group", () => {
    expect(widgetDef("home.decisions")?.once).toBe(true);
    const decisions = (id: string) => ({ ...contrib("home.decisions"), id });
    const board = validBoard(
      { widgets: [decisions("a-1"), group("g-1", 12, [decisions("a-2")]), decisions("a-3")] },
      FALLBACK,
    );
    expect([...widgetsOn(board)]).toEqual(["home.decisions"]);
    expect([...everyWidget(board)].filter((w) => w.kind === "widget")).toHaveLength(1);
  });

  test("a widget that may be placed often keeps every copy", () => {
    const figure = (id: string, f: string) => ({
      ...contrib("home.figure"),
      id,
      params: { figure: f },
    });
    const board = validBoard(
      { widgets: [figure("f-1", "agents"), figure("f-2", "tokens")] },
      EMPTY,
    );
    expect(board.widgets).toHaveLength(2);
  });

  test("a widget's choices are plain strings under plain keys, capped in number and length", () => {
    const params: Record<string, unknown> = {
      figure: "agents",
      "Bad Key": "x",
      nested: { a: 1 },
      count: 3,
      long: "y".repeat(10_000),
    };
    for (let i = 0; i < 20; i++) params[`k${i}`] = "v";
    const board = validBoard(
      { widgets: [{ id: "f-1", kind: "widget", widget: "home.figure", params, w: 3 }] },
      EMPTY,
    );
    const kept = board.widgets[0]!.kind === "widget" ? board.widgets[0]!.params : {};
    expect(Object.keys(kept).length).toBeLessThanOrEqual(LIMITS.params);
    expect(kept.figure).toBe("agents");
    expect(kept["Bad Key"]).toBeUndefined();
    expect(kept.nested).toBeUndefined();
    expect(kept.count).toBeUndefined();
    expect(kept.long?.length).toBe(LIMITS.paramValue);
    for (const value of Object.values(kept)) expect(typeof value).toBe("string");
  });

  test("a widget no section offers any more is kept, at the generic size, so its place is not lost", () => {
    const board = validBoard(
      { widgets: [{ id: "w-1", kind: "widget", widget: "gone.widget", params: {}, w: 1, h: 1 }] },
      EMPTY,
    );
    expect(board.widgets[0]).toMatchObject({
      kind: "widget",
      widget: "gone.widget",
      w: SIZING.widget.minW,
      h: SIZING.widget.minH,
    });
  });

  test("a widget key that is not a key is dropped", () => {
    const board = validBoard(
      {
        widgets: [
          { id: "w-1", kind: "widget", widget: "NOT A KEY", w: 4 },
          { id: "w-2", kind: "widget", widget: 42, w: 4 },
          { id: "w-3", kind: "widget", widget: `a.${"b".repeat(LIMITS.widgetKey)}`, w: 4 },
        ],
      },
      EMPTY,
    );
    expect(board.widgets).toEqual([]);
  });

  test("sizes are whole columns and rows, held to each widget's minimums and the grid", () => {
    const decisions = widgetDef("home.decisions")!.size;
    const board = validBoard(
      {
        widgets: [
          { ...contrib("home.decisions"), w: 1, h: 1 },
          { ...note("n-1"), w: 99, h: 999 },
          { ...note("n-2"), w: 5.6, h: 7.4 },
          { id: "h-1", kind: "heading", text: "Plan", size: "md", align: "start", w: 0, h: 40 },
          { ...group("g-1", 1, []) },
        ],
      },
      EMPTY,
    );
    const [d, n1, n2, h, g] = board.widgets as [Widget, Widget, Widget, Widget, Widget];
    expect(d.w).toBe(decisions.minW);
    expect(d.h).toBe(decisions.minH);
    expect(n1.w).toBe(COLUMNS);
    expect(n1.h).toBe(80);
    expect(n2.w).toBe(6);
    expect(n2.h).toBe(7);
    // A heading's height is its words': none is stored.
    expect(h.w).toBe(SIZING.heading.minW);
    expect(h.h).toBeNull();
    expect(g.w).toBe(SIZING.group.minW);
  });
});

describe("editing a board", () => {
  const board: Board = {
    widgets: [
      note("n-1", 6),
      group("g-1", 6, [note("n-2", 12), contrib("home.live")]),
      note("n-3"),
    ],
  };

  test("a widget added lands at the end of the page", () => {
    const next = addWidget(board, note("n-9"));
    expect(next.widgets.map((w) => w.id)).toEqual(["n-1", "g-1", "n-3", "n-9"]);
    expect(board.widgets).toHaveLength(3);
  });

  test("a widget removed goes from the page or from its group", () => {
    expect(removeWidget(board, "n-1").widgets.map((w) => w.id)).toEqual(["g-1", "n-3"]);
    const g = removeWidget(board, "n-2").widgets[1] as GroupWidget;
    expect(g.children.map((c) => c.kind)).toEqual(["widget"]);
  });

  test("a group taken apart puts its widgets in its place, in order", () => {
    const next = ungroup(board, "g-1");
    expect(next.widgets.map((w) => w.id)).toEqual(["n-1", "n-2", next.widgets[2]!.id, "n-3"]);
    expect(next.widgets.some((w) => w.kind === "group")).toBe(false);
  });

  test("widgetsOn names the contributed widgets, in groups too", () => {
    expect([...widgetsOn(board)]).toEqual(["home.live"]);
  });

  test("contrib places a widget at its own default size with no choices made", () => {
    const w = contrib("home.decisions");
    expect(w).toMatchObject({ kind: "widget", widget: "home.decisions", params: {}, h: null });
    expect(w.w).toBe(widgetDef("home.decisions")!.size.w);
    expect(contrib("home.decisions", 12, 20)).toMatchObject({ w: 12, h: 20 });
  });

  test("a copy lands beside its original with fresh ids", () => {
    const made = duplicate(board, "n-1")!;
    expect(made.board.widgets.map((w) => w.id)).toEqual(["n-1", made.id, "g-1", "n-3"]);
    expect(made.id).not.toBe("n-1");
  });

  test("a once-only widget is never copied, and a copied group leaves it behind", () => {
    const live = (board.widgets[1] as GroupWidget).children[1]!;
    expect(widgetDef("home.live")?.once).toBe(true);
    expect(duplicate(board, live.id)).toBeNull();
    const made = duplicate(board, "g-1")!;
    const copy = made.board.widgets.find((w) => w.id === made.id) as GroupWidget;
    expect(copy.title).toBe("Delivery copy");
    expect(copy.children.map((c) => c.kind)).toEqual(["note"]);
    expect(copy.children[0]!.id).not.toBe("n-2");
  });

  test("the board's order round-trips through its lists", () => {
    expect(fromLists(board, listsOf(board))).toEqual(board);
  });

  test("a widget moved into a group keeps its size on screen", () => {
    // Half the page dropped into a half-wide group is the whole group.
    const lists = listsOf(board);
    lists[PAGE] = ["g-1", "n-3"];
    lists[groupList("g-1")] = ["n-1", ...lists[groupList("g-1")]!];
    const g = fromLists(board, lists).widgets[0] as GroupWidget;
    expect(g.children[0]).toMatchObject({ id: "n-1", w: COLUMNS });
  });

  test("a widget moved out of a group keeps its size on screen", () => {
    // The whole of a half-wide group is half the page.
    const lists = listsOf(board);
    lists[groupList("g-1")] = lists[groupList("g-1")]!.filter((id) => id !== "n-2");
    lists[PAGE] = [...lists[PAGE]!, "n-2"];
    const next = fromLists(board, lists);
    expect(next.widgets.find((w) => w.id === "n-2")).toMatchObject({ w: 6 });
  });

  test("a widget never shrinks below its minimum by changing container", () => {
    const narrow: Board = { widgets: [note("n-1", 2), group("g-1", 12, [])] };
    const lists = listsOf(narrow);
    lists[PAGE] = ["g-1"];
    lists[groupList("g-1")] = ["n-1"];
    const moved = fromLists(narrow, lists).widgets[0] as GroupWidget;
    expect(moved.children[0]!.w).toBe(2);
    const wide: Board = { widgets: [group("g-1", 4, [note("n-1", SIZING.note.minW)])] };
    const out = listsOf(wide);
    out[groupList("g-1")] = [];
    out[PAGE] = ["g-1", "n-1"];
    expect(fromLists(wide, out).widgets[1]!.w).toBe(SIZING.note.minW);
  });
});

// A ROW CLOSES UP OVER WHAT THE COMPANY'S SERVICES HIDE: the columns a hidden
// block held go to the blocks left in its row, in proportion, so the row is
// drawn as wide as it was saved; the rows around it are untouched.
describe("the widths a board is drawn at", () => {
  const row = (...ws: [string, number][]) => ws.map(([id, w]) => ({ id, w }));
  test("a lone survivor takes its row, and other rows keep their widths", () => {
    const drawn = drawnWidths(
      row(["a", 6], ["b", 6], ["done", 4], ["proj", 4], ["tok", 4]),
      (id) => id !== "done" && id !== "proj",
    );
    expect(Object.fromEntries(drawn)).toEqual({ a: 6, b: 6, tok: 12 });
  });
  test("the hidden columns are shared in proportion, and the row's total holds", () => {
    const drawn = drawnWidths(row(["x", 3], ["gone", 3], ["y", 6]), (id) => id !== "gone");
    expect(drawn.get("x")! + drawn.get("y")!).toBe(12);
    expect(drawn.get("y")!).toBeGreaterThan(drawn.get("x")!);
  });
  test("a short row stays as wide as it was saved", () => {
    const drawn = drawnWidths(row(["x", 4], ["gone", 4]), (id) => id !== "gone");
    expect(drawn.get("x")).toBe(8);
  });
  test("nothing hidden is nothing changed", () => {
    expect(Object.fromEntries(drawnWidths(row(["a", 5], ["b", 9]), () => true))).toEqual({
      a: 5,
      b: 9,
    });
  });
});
