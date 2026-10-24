/**
 * A reader's Home as a board: widgets on a twelve-column grid, some of them
 * inside groups, each with a width and a height of its own.
 *
 * TWO KINDS OF WIDGET. Company widgets draw what the engine knows, and are
 * offered by the dashboard's sections (`registry.tsx`): the board stores only
 * a widget's key and the reader's choices for it, so a section can change how
 * its widget draws without a stored board changing. Some are one per board
 * (`once`), because two "Needs your decision" cards are one decision drawn
 * twice. Layout widgets are the reader's own words and structure (a group, a
 * heading, a divider, a note, a list of links), as many as they like.
 *
 * PER READER, PER BROWSER, for `lib/starred.ts`'s reasons, and read from
 * storage as untrusted input: every field is checked, every string capped,
 * every link held to the two shapes a link may take (`linkHref`).
 */

import { NODE_HUES, type NodeHue } from "@crewlethq/ui";
import { readerStore } from "~/lib/readerStore.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { widgetDef } from "./registry.tsx";

export type Tone = NodeHue | "none";
export const TONES: readonly Tone[] = ["none", ...NODE_HUES];

/** One row of the grid, in pixels: the step a height snaps to. */
export const ROW = 16;
export const COLUMNS = 12;

interface Base {
  id: string;
  /** Columns of its grid (the page's, or its group's) it spans. */
  w: number;
  /** Rows tall, or null for its content's height. */
  h: number | null;
}

/** A widget a section offers, by its key, with the reader's choices for this copy. */
export interface ContribWidget extends Base {
  kind: "widget";
  widget: string;
  params: Record<string, string>;
}
export interface NoteWidget extends Base {
  kind: "note";
  text: string;
  tone: Tone;
  align: "start" | "center";
  size: "md" | "lg";
}
export interface HeadingWidget extends Base {
  kind: "heading";
  text: string;
  size: "md" | "lg";
  align: "start" | "center";
}
export interface DividerWidget extends Base {
  kind: "divider";
  label: string;
  align: "start" | "center";
}
export interface LinkRow {
  label: string;
  href: string;
}
export interface LinksWidget extends Base {
  kind: "links";
  title: string;
  links: LinkRow[];
}
export type Leaf = ContribWidget | NoteWidget | HeadingWidget | DividerWidget | LinksWidget;

export interface GroupWidget extends Base {
  kind: "group";
  title: string;
  tone: Tone;
  collapsed: boolean;
  children: Leaf[];
}

export type Widget = Leaf | GroupWidget;
export type WidgetKind = Widget["kind"];

export interface Board {
  widgets: Widget[];
}

// ---- limits ---------------------------------------------------------------

export const LIMITS = {
  widgets: 60,
  title: 60,
  heading: 120,
  note: 2000,
  links: 12,
  linkLabel: 60,
  linkHref: 500,
  widgetKey: 80,
  params: 8,
  paramValue: 200,
} as const;

/**
 * The narrowest each layout widget reads at, and whether its height may be
 * set. A contributed widget says its own (`WidgetDef.size`); `widget` here is
 * for one whose section no longer offers it.
 */
export const SIZING: Record<WidgetKind, { minW: number; minH: number; vertical: boolean }> = {
  widget: { minW: 3, minH: 6, vertical: true },
  note: { minW: 2, minH: 3, vertical: true },
  heading: { minW: 2, minH: 2, vertical: false },
  divider: { minW: 2, minH: 2, vertical: false },
  links: { minW: 2, minH: 4, vertical: true },
  group: { minW: 4, minH: 4, vertical: false },
};

export function sizingOf(w: Widget) {
  return w.kind === "widget" ? (widgetDef(w.widget)?.size ?? SIZING.widget) : SIZING[w.kind];
}

/** Whether a widget may be on a board once only. */
export const isOnce = (w: Widget) => w.kind === "widget" && widgetDef(w.widget)?.once === true;

/** A contributed widget at its default size, with no choices made. */
export function contrib(key: string, w?: number, h: number | null = null): ContribWidget {
  return {
    id: newId("w"),
    kind: "widget",
    widget: key,
    params: {},
    w: w ?? widgetDef(key)?.size.w ?? 4,
    h,
  };
}

/**
 * The only links a widget keeps: a place in this dashboard (`#/…`) or an
 * http(s) address. Anything else, `javascript:` included, is refused with a
 * sentence rather than stored.
 */
export function linkHref(raw: string): { href: string } | { error: string } {
  const value = raw.trim();
  if (value === "") return { error: "Enter an address." };
  if (value.length > LIMITS.linkHref)
    return { error: `Keep the address under ${LIMITS.linkHref} characters.` };
  if (value.startsWith("#/")) return { href: value };
  try {
    const url = new URL(value);
    if (url.protocol === "https:" || url.protocol === "http:") return { href: url.toString() };
  } catch {
    // Not an absolute address.
  }
  return {
    error: "Use a dashboard address starting with #/ or a web address starting with https://.",
  };
}

export const isExternal = (href: string) => !href.startsWith("#/");

// ---- ids --------------------------------------------------------------------

let counter = 0;
export function newId(kind: string): string {
  counter += 1;
  return `${kind}-${Date.now().toString(36)}${counter.toString(36)}`;
}

// ---- validation ---------------------------------------------------------------

const str = (v: unknown, max: number, fallback = ""): string =>
  typeof v === "string" ? v.slice(0, max) : fallback;
const int = (v: unknown, lo: number, hi: number, fallback: number): number =>
  typeof v === "number" && Number.isFinite(v)
    ? Math.max(lo, Math.min(hi, Math.round(v)))
    : fallback;
const oneOf = <T extends string>(v: unknown, allowed: readonly T[], fallback: T): T =>
  allowed.includes(v as T) ? (v as T) : fallback;
const ID = /^[a-z][a-z0-9-]{0,40}$/;
const WIDGET_KEY = /^[a-z]+(\.[a-z0-9-]+)+$/;
const PARAM_KEY = /^[a-z][a-z0-9-]{0,30}$/;

/** The reader's choices for a widget, as plain strings: anything else is dropped. */
function validParams(raw: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (typeof raw !== "object" || raw === null) return out;
  for (const [k, v] of Object.entries(raw).slice(0, LIMITS.params)) {
    if (PARAM_KEY.test(k) && typeof v === "string") out[k] = v.slice(0, LIMITS.paramValue);
  }
  return out;
}

function validLeaf(
  raw: Record<string, unknown>,
  seen: Set<string>,
  once: Set<string>,
): Leaf | null {
  const id =
    typeof raw.id === "string" && ID.test(raw.id) && !seen.has(raw.id) ? raw.id : newId("w");
  seen.add(id);
  const kind = raw.kind;
  const key =
    kind === "widget" && typeof raw.widget === "string" && raw.widget.length <= LIMITS.widgetKey
      ? raw.widget
      : "";
  const def = key ? widgetDef(key) : undefined;
  const sizing =
    kind === "widget"
      ? (def?.size ?? SIZING.widget)
      : (SIZING[(kind as WidgetKind) ?? "note"] ?? SIZING.note);
  const w = int(raw.w, sizing.minW, COLUMNS, Math.max(sizing.minW, 4));
  const h =
    sizing.vertical && raw.h !== null && raw.h !== undefined
      ? int(raw.h, sizing.minH, 80, sizing.minH)
      : null;
  switch (kind) {
    case "widget": {
      // A widget whose section no longer offers it is KEPT, drawn as such, so
      // a section missing from one build does not cost the reader its place.
      if (!WIDGET_KEY.test(key)) return null;
      if (def?.once) {
        if (once.has(key)) return null;
        once.add(key);
      }
      return { id, kind, widget: key, params: validParams(raw.params), w, h };
    }
    case "note":
      return {
        id,
        kind,
        text: str(raw.text, LIMITS.note),
        tone: oneOf(raw.tone, TONES, "amber"),
        align: oneOf(raw.align, ["start", "center"] as const, "start"),
        size: oneOf(raw.size, ["md", "lg"] as const, "md"),
        w,
        h,
      };
    case "heading":
      return {
        id,
        kind,
        text: str(raw.text, LIMITS.heading),
        size: oneOf(raw.size, ["md", "lg"] as const, "md"),
        align: oneOf(raw.align, ["start", "center"] as const, "start"),
        w,
        h: null,
      };
    case "divider":
      return {
        id,
        kind,
        label: str(raw.label, LIMITS.title),
        align: oneOf(raw.align, ["start", "center"] as const, "start"),
        w,
        h: null,
      };
    case "links": {
      const links = (Array.isArray(raw.links) ? raw.links : [])
        .slice(0, LIMITS.links)
        .flatMap((l: unknown) => {
          const row = (typeof l === "object" && l !== null ? l : {}) as Record<string, unknown>;
          const checked = linkHref(str(row.href, LIMITS.linkHref));
          return "href" in checked
            ? [{ label: str(row.label, LIMITS.linkLabel), href: checked.href }]
            : [];
        });
      return { id, kind, title: str(raw.title, LIMITS.title, "Links"), links, w, h };
    }
    default:
      return null;
  }
}

/** A board read from storage, made safe and whole. */
export function validBoard(raw: unknown, fallback: Board): Board {
  const r = (typeof raw === "object" && raw !== null ? raw : {}) as { widgets?: unknown };
  if (!Array.isArray(r.widgets)) return fallback;
  const seen = new Set<string>();
  const once = new Set<string>();
  const widgets: Widget[] = [];
  // THE LIMIT IS THE WHOLE BOARD'S, groups' widgets included: sixty groups of
  // sixty would otherwise be 3,660 widgets drawn from one stored value.
  let room: number = LIMITS.widgets;
  for (const item of r.widgets.slice(0, LIMITS.widgets)) {
    if (room <= 0) break;
    if (typeof item !== "object" || item === null) continue;
    const row = item as unknown as Record<string, unknown>;
    if (row.kind === "group") {
      const id =
        typeof row.id === "string" && ID.test(row.id) && !seen.has(row.id)
          ? row.id
          : newId("group");
      seen.add(id);
      room -= 1;
      const children = (Array.isArray(row.children) ? row.children : [])
        .slice(0, room)
        .flatMap((c: unknown) =>
          typeof c === "object" && c !== null
            ? [validLeaf(c as Record<string, unknown>, seen, once)].filter(
                (x): x is Leaf => x !== null,
              )
            : [],
        );
      widgets.push({
        id,
        kind: "group",
        title: str(row.title, LIMITS.title),
        tone: oneOf(row.tone, TONES, "none"),
        collapsed: row.collapsed === true,
        w: int(row.w, SIZING.group.minW, COLUMNS, COLUMNS),
        h: null,
        children,
      });
      room -= children.length;
    } else {
      const leaf = validLeaf(row, seen, once);
      if (leaf) {
        widgets.push(leaf);
        room -= 1;
      }
    }
  }
  return { widgets };
}

// ---- reading and editing ------------------------------------------------------

export function* everyWidget(board: Board): Generator<Widget> {
  for (const w of board.widgets) {
    yield w;
    if (w.kind === "group") yield* w.children;
  }
}

export function findWidget(board: Board, id: string): Widget | undefined {
  for (const w of everyWidget(board)) if (w.id === id) return w;
  return undefined;
}

/** Where a widget sits: the group holding it, or null on the page. */
export function parentOf(board: Board, id: string): GroupWidget | null {
  for (const w of board.widgets)
    if (w.kind === "group" && w.children.some((c) => c.id === id)) return w;
  return null;
}

/** The keys of the contributed widgets already on the board. */
export function widgetsOn(board: Board): Set<string> {
  const out = new Set<string>();
  for (const w of everyWidget(board)) if (w.kind === "widget") out.add(w.widget);
  return out;
}

/** `board` with one widget replaced by `edit(widget)`. */
export function updateWidget(board: Board, id: string, edit: (w: Widget) => Widget): Board {
  return {
    widgets: board.widgets.map((w) => {
      if (w.id === id) return edit(w);
      if (w.kind === "group" && w.children.some((c) => c.id === id)) {
        return { ...w, children: w.children.map((c) => (c.id === id ? (edit(c) as Leaf) : c)) };
      }
      return w;
    }),
  };
}

export function removeWidget(board: Board, id: string): Board {
  return {
    widgets: board.widgets
      .filter((w) => w.id !== id)
      .map((w) =>
        w.kind === "group" ? { ...w, children: w.children.filter((c) => c.id !== id) } : w,
      ),
  };
}

/** A group taken apart: its widgets take its place on the page, in order. */
export function ungroup(board: Board, id: string): Board {
  return {
    widgets: board.widgets.flatMap((w) => (w.id === id && w.kind === "group" ? w.children : [w])),
  };
}

/** Add a widget at the end of the page; the reader drags it into a group from there. */
export function addWidget(board: Board, widget: Widget): Board {
  return { widgets: [...board.widgets, widget] };
}

/** A copy beside the original, with fresh ids. A once-only widget is never copied. */
export function duplicate(board: Board, id: string): { board: Board; id: string } | null {
  const w = findWidget(board, id);
  if (!w || isOnce(w)) return null;
  const copy: Widget =
    w.kind === "group"
      ? {
          ...w,
          id: newId("group"),
          title: `${w.title || "Group"} copy`,
          children: w.children.filter((c) => !isOnce(c)).map((c) => ({ ...c, id: newId(c.kind) })),
        }
      : { ...w, id: newId(w.kind) };
  const place = (list: Widget[]): Widget[] => list.flatMap((x) => (x.id === id ? [x, copy] : [x]));
  return {
    id: copy.id,
    board: {
      widgets: place(board.widgets).map((x) =>
        x.kind === "group" ? { ...x, children: place(x.children) as Leaf[] } : x,
      ),
    },
  };
}

/** The board's order as lists, by container: the page, then each group. */
/**
 * The widths a list of blocks is DRAWN at once some of them are left out,
 * without changing what is saved.
 *
 * The board's grid places each block in order, spanning its own columns, and
 * starts a new row when the next block does not fit (`ArrangeGrid`, columns
 * only, so the rows are exactly these). A block the company's services hide
 * draws nothing, and its row closed up short: on a company running Jira,
 * Tokens by team stood alone in a third of the row with two thirds of the
 * board empty beside it. So each row is packed as saved, and the columns its
 * hidden blocks held are shared out over the ones left in that row, in
 * proportion to their own widths: a lone survivor takes the row. PURE, so it
 * is held without a layout engine.
 */
export function drawnWidths(
  blocks: readonly { id: string; w: number }[],
  shown: (id: string) => boolean,
  columns: number = COLUMNS,
): Map<string, number> {
  const drawn = new Map<string, number>();
  let row: { id: string; w: number }[] = [];
  let used = 0;
  const close = () => {
    const kept = row.filter((b) => shown(b.id));
    const keptWidth = kept.reduce((sum, b) => sum + b.w, 0);
    if (kept.length > 0 && keptWidth < used) {
      // SHARED IN PROPORTION, rounded down, and the columns rounding left
      // over handed out one each from the row's start, so the row's total is
      // exactly what it was saved at.
      const shares = kept.map((b) => Math.floor((b.w * used) / keptWidth));
      let spare = used - shares.reduce((sum, w) => sum + w, 0);
      for (let i = 0; spare > 0; i = (i + 1) % shares.length, spare--) shares[i]! += 1;
      kept.forEach((b, i) => drawn.set(b.id, shares[i]!));
    } else {
      for (const b of kept) drawn.set(b.id, b.w);
    }
    row = [];
    used = 0;
  };
  for (const block of blocks) {
    const w = Math.min(Math.max(1, block.w), columns);
    if (used + w > columns) close();
    row.push({ id: block.id, w });
    used += w;
  }
  close();
  return drawn;
}

export const PAGE = "page";
export const groupList = (id: string) => `group:${id}`;

export function listsOf(board: Board): Record<string, string[]> {
  const lists: Record<string, string[]> = { [PAGE]: board.widgets.map((w) => w.id) };
  for (const w of board.widgets)
    if (w.kind === "group") lists[groupList(w.id)] = w.children.map((c) => c.id);
  return lists;
}

/** The page columns a container spans: the page's twelve, or a group's width. */
function spanOf(board: Board, container: string | null): number {
  if (container === null) return COLUMNS;
  const g = board.widgets.find((w) => w.id === container);
  return g?.kind === "group" ? g.w : COLUMNS;
}

/**
 * The board rebuilt in the order `lists` gives.
 *
 * A WIDGET THAT CHANGES CONTAINER KEEPS ITS SIZE ON SCREEN. Its width counts
 * columns of the grid it sits on, and a group's grid is narrower than the
 * page's, so a third of the page dropped into a third-wide group becomes the
 * whole group rather than a ninth of the page.
 */
export function fromLists(board: Board, lists: Record<string, string[]>): Board {
  const byId = new Map<string, Widget>();
  const wasIn = new Map<string, string | null>();
  for (const w of board.widgets) {
    byId.set(w.id, w);
    wasIn.set(w.id, null);
    if (w.kind === "group")
      for (const c of w.children) {
        byId.set(c.id, c);
        wasIn.set(c.id, w.id);
      }
  }
  const resized = (w: Widget, nowIn: string | null): Widget => {
    const before = wasIn.get(w.id) ?? null;
    if (before === nowIn) return w;
    const across = (w.w * spanOf(board, before)) / spanOf(board, nowIn);
    return { ...w, w: Math.max(sizingOf(w).minW, Math.min(COLUMNS, Math.round(across))) };
  };
  const page = (lists[PAGE] ?? []).flatMap((id) => {
    const w = byId.get(id);
    return w ? [resized(w, null)] : [];
  });
  return {
    widgets: page.map((w) =>
      w.kind === "group"
        ? {
            ...w,
            children: (lists[groupList(w.id)] ?? []).flatMap((id) => {
              const c = byId.get(id);
              return c && c.kind !== "group" ? [resized(c, w.id) as Leaf] : [];
            }),
          }
        : w,
    ),
  };
}

export const sameBoard = (a: Board, b: Board) => JSON.stringify(a) === JSON.stringify(b);

/** The reader's board, kept in this browser per reader as their stars are. */
export function boardStore(fallback: Board) {
  return readerStore<Board>(STORAGE_KEYS.homeBoard, fallback, (raw) => validBoard(raw, fallback));
}
