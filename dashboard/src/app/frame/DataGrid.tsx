/**
 * The grid every list in the product is drawn with.
 *
 * # The sort lives in the URL
 *
 * `ui/DataTable.tsx` held its sort in component state, which broke the one
 * rule this dashboard has about screens: every screen, section and filter is
 * in the URL. A sorted ops table could not be sent to anybody, did not survive
 * a reload, and came back unsorted from the Back button — on the tables an
 * operator is most likely to be sharing.
 *
 * # Bands are the answer's own groups
 *
 * A grouped answer arrives with `groups[]`, each carrying its own count over
 * the WHOLE set and a bounded slice of rows. So a band's count is the engine's
 * and the rows under it are a page — the band says both, because a column head
 * reading `12` over 12 visible rows out of 300 is the number a person plans
 * against.
 *
 * # A selection is a read gesture
 *
 * There is no bulk action, because there is no write. What a selection is for
 * is copying keys and exporting the rows that are loaded — and the footer says
 * how many that is, so an export never claims more than the page it has.
 */

import {
  memo,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useParam } from "../router.tsx";
import { Button, EmptyState, cx } from "@crewlethq/ui";
import { KeyboardArrowDownGlyph, KeyboardArrowUpGlyph } from "@crewlethq/icons/glyphs";
// A GRID'S EMPTY MARK IS NAME-KEYED, because `empty.icon` is part of the prop
// every screen fills in — so the name→drawing lookup stays in `~/ui/Icon.tsx`.
import { Mark, type MarkName } from "~/ui/glyph.tsx";
import { useKeyChords } from "~/lib/keys.ts";
import { naturalCompare } from "~/lib/format.ts";

export interface GridColumn<T> {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  /** The value sorted on. Omit to make the column unsortable. */
  sortValue?: (row: T) => string | number | null | undefined;
  align?: "left" | "right";
  /**
   * The column's name where `header` cannot carry one — a glyph head, or none
   * at all. Used by the card layout a grid takes on a phone, where every value
   * draws its own name beside it; ignored everywhere `header` is drawn.
   */
  label?: string;
  /** Shrink to content and never wrap. */
  shrink?: boolean;
  width?: string;
  /** Hidden unless named in `cols=`. */
  optional?: boolean;
}

/** One column a reader may turn on or off, as a chooser offers it. */
export interface ColumnChoice {
  key: string;
  label: string;
  /** Off until `cols=` names it. */
  optional?: boolean;
}

/**
 * The choices a chooser offers, DERIVED from the columns a grid draws.
 *
 * A chooser holding its own list of names would be a second declaration of the
 * grid's columns — wrong the first time one is added, and indistinguishable
 * from a correct one. This takes the three facts a chooser needs off the
 * columns themselves, so a column added to a grid appears in its menu with no
 * second edit.
 *
 * THE CELL RENDERERS ARE NEVER CALLED. A name and a flag are properties of the
 * COLUMN, so nothing here touches a row — which is what lets a caller derive
 * the choices from a column list built over a stub context.
 */
export function columnChoicesOf<T>(columns: GridColumn<T>[]): ColumnChoice[] {
  return columns.map((column) => ({
    key: column.key,
    // THE HEAD, OR THE NAME THE COLUMN CARRIES WHERE THE HEAD IS A GLYPH —
    // which is exactly what [GridColumn.label] is for on the card layout a
    // narrow grid takes.
    label:
      typeof column.header === "string" && column.header
        ? column.header
        : (column.label ?? column.key),
    optional: column.optional,
  }));
}

/**
 * The checkbox list a reader chooses columns with, wherever it is drawn.
 *
 * ONE IMPLEMENTATION, because the two rules below are not obvious and a second
 * copy would have to re-derive both. It was the work list's Display menu
 * alone; the projects directory now draws the same control beside its segment,
 * and a screen whose grid has an optional column and no chooser is a column
 * nobody can reach — which is what `#/work/projects` shipped as when Unit
 * became optional.
 *
 * AN EMPTY `cols=` IS THE DEFAULT SET, NOT AN EMPTY GRID. [DataGrid] reads it
 * that way, so the boxes have to show the default until somebody moves one —
 * a chooser that drew an empty value as "nothing ticked" would say the grid is
 * blank while it is drawing every ordinary column.
 *
 * IT WRITES THE DECLARATION ORDER, NEVER THE CLICK ORDER. `cols=` carries an
 * ORDER as well as a selection, so a value built from a Set's insertion order
 * would rearrange the grid every time somebody ticked a box.
 *
 * The caller owns the KEY: the work list's is per shape (`cols.list=` /
 * `cols.table=`, since its two column sets declare two orders) and a screen
 * with one grid takes the bare `cols=` — see [DataGrid]'s own `name` and
 * `colsName`.
 */
export function ColumnChooser({
  choices,
  value,
  onChange,
  label = "Columns",
}: {
  choices: ColumnChoice[];
  /** `cols=` as it stands, empty for the default set. */
  value: string;
  onChange: (cols: string) => void;
  /** The heading above the boxes, where a menu wants another word. */
  label?: string;
}) {
  if (choices.length === 0) return null;
  const chosen = new Set(value ? value.split(",").filter(Boolean) : []);
  return (
    <>
      <div className="grid-cols-label">{label}</div>
      <div className="grid-cols-choices">
        {choices.map((choice) => {
          const on = chosen.size === 0 ? !choice.optional : chosen.has(choice.key);
          return (
            <label key={choice.key} className="grid-cols-choice">
              <input
                type="checkbox"
                checked={on}
                onChange={() => {
                  const next = new Set(
                    chosen.size === 0
                      ? choices.filter((c) => !c.optional).map((c) => c.key)
                      : chosen,
                  );
                  if (on) next.delete(choice.key);
                  else next.add(choice.key);
                  onChange(
                    choices
                      .filter((c) => next.has(c.key))
                      .map((c) => c.key)
                      .join(","),
                  );
                }}
              />
              <span>{choice.label}</span>
            </label>
          );
        })}
      </div>
    </>
  );
}

export interface GridBand<T> {
  key: string;
  label: ReactNode;
  /**
   * The axis value's own mark, drawn before the label.
   *
   * A SIBLING OF THE LABEL RATHER THAN PART OF IT, because the head is a flex
   * row with a gap: folded into `label` the mark sits inside the truncating
   * span, where the gap never reaches it and an ellipsis eventually eats it.
   * An axis with no mark passes nothing rather than a placeholder.
   */
  mark?: ReactNode;
  /** The engine's count over the whole group, where it gave one. */
  total?: number;
  rows: T[];
  /**
   * The bands INSIDE this one, where the answer was grouped twice.
   *
   * ONE LEVEL, and a band carries rows OR sub-bands, never both — which is the
   * shape the answer arrives in: sub-groups REPLACE a group's rows exactly as
   * groups replace the ungrouped ones, because an answer carrying both would be
   * the same rows twice. A grid that knew only `rows` drew a run of EMPTY bands
   * over a twice-grouped answer with the rows nowhere at all, which is what the
   * work table did for as long as its Display menu offered a second axis.
   */
  bands?: GridBand<T>[];
  /** A per-band aggregate line. */
  footer?: ReactNode;
}

/** Every row a band holds, its sub-bands' included, in the order they draw. */
function bandRows<T>(band: GridBand<T>): T[] {
  return band.bands ? band.bands.flatMap(bandRows) : band.rows;
}

/**
 * A three-way comparator that is STABLE on equal operands.
 *
 * Rows arrive from a live push several times a second, and a comparator that
 * answers -1 for equal operands — the `a < b ? 1 : -1` idiom — makes equal rows
 * swap places on every render: a list that shuffles itself while somebody is
 * reading it.
 */
function compare(a: string | number, b: string | number): number {
  if (typeof a === "number" && typeof b === "number") {
    // NaN sorts last rather than poisoning the comparator's transitivity.
    if (Number.isNaN(a)) return Number.isNaN(b) ? 0 : 1;
    if (Number.isNaN(b)) return -1;
    return a < b ? -1 : a > b ? 1 : 0;
  }
  // ONE COLLATOR for every comparison, rather than one built per comparison.
  return naturalCompare(String(a), String(b));
}

/**
 * The most of a grid one `shrink` column may take.
 *
 * A cap has to leave room for the flexible columns that are the point of the
 * list — a title, a subject, a name — and a grid's widest shrink column is
 * routinely a status pill, a handle or a timestamp, all of which sit far under
 * this. What it bites on is the value that had no business being a column of
 * its own width: the schedules grid's Wakes at 502px of an 822px grid, the
 * work table's Assignee at 220px of 644.
 *
 * 20% rather than a pixel count because it has to hold at every width — a
 * fraction of the grid is the same promise on a phone and on a wide screen,
 * where a pixel cap is a desktop's proportions pinned onto a laptop. Two
 * shrink columns at the cap still leave three fifths of the grid, and a grid
 * carrying five columns that each want a fifth of it is one whose author has
 * to choose, which is what `optional` is for.
 */
const SHRINK_CAP = "20%";

/** `sort=-updated` → `{key: "updated", desc: true}`. */
export function parseSort(raw: string): { key: string; desc: boolean } | null {
  if (!raw) return null;
  return raw.startsWith("-") ? { key: raw.slice(1), desc: true } : { key: raw, desc: false };
}

/**
 * WHICH GRID THE KEYBOARD IS DRIVING.
 *
 * `j`, `k` and `enter` are bound on `window`, because there is nothing else to
 * bind them to: a grid whose rows are anchors takes no focus of its own, and
 * asking the reader to click a list before they can walk it is not a keyboard
 * shortcut. But several screens carry two or three grids at once — the spend
 * by seat and the recent turns, a node's leases and its duties, a page's grid
 * and another inside the peek rail over it — and `window` is one target, so
 * every mounted grid received every keystroke. One `j` moved two cursors and
 * set two `scrollIntoView`s fighting over the viewport; one Enter opened a
 * seat peek and then a turn peek over it, so the object the reader got was
 * never the one their cursor was on.
 *
 * The reader's LAST POINTER GESTURE says which grid they are in — it is the
 * only signal there is, since neither grid can be focused. With no gesture yet
 * the first DRIVABLE grid mounted drives, which on every screen that has one
 * is the primary grid: a reader who has clicked nothing keeps exactly the
 * behaviour they had. A grid showing its empty state registers nothing, so an
 * empty list at the top of a screen does not swallow the keystrokes meant for
 * the populated one under it.
 *
 * Module state rather than a context: the two grids that need to agree are
 * frequently in different subtrees (a screen and the peek rail over it), and a
 * provider around both would have to be the shell, which knows nothing about
 * grids.
 *
 * AND NOTHING RENDERS WHEN IT CHANGES HANDS. Which grid drives is asked at the
 * keystroke (a `when` that is a function, `lib/keys.ts`), because nothing a
 * grid DRAWS depends on it — the cursor row stays drawn in a grid the reader
 * has left, as it always did. It was a store every grid subscribed to, so a
 * `when` captured at render could be kept fresh: each grid's mount announced
 * itself and drew every row of every grid on the screen a second time, and so
 * did every pointer press that moved the keyboard — 143 to 197 ms for two
 * hundred-row grids under the development build, to change no pixel.
 */
const drivable: string[] = [];
let touched: string | null = null;

function isDriving(gridId: string): boolean {
  if (touched !== null) return touched === gridId;
  return drivable[0] === gridId;
}

export function DataGrid<T>({
  rows,
  bands,
  columns,
  rowKey,
  onRowActivate,
  rowHref,
  isSelected,
  isFailed,
  defaultSort = "",
  /** Sorting is the ENGINE's on a paged answer: it orders the whole set. */
  serverSorted,
  empty,
  footer,
  onLoadMore,
  loadedNote,
  name,
  colsName,
}: {
  rows?: T[];
  bands?: GridBand<T>[];
  columns: GridColumn<T>[];
  rowKey: (row: T) => string;
  /** A plain click. The row is still an anchor when `rowHref` is given. */
  onRowActivate?: (row: T, e: React.MouseEvent | React.KeyboardEvent) => void;
  rowHref?: (row: T) => string;
  isSelected?: (row: T) => boolean;
  isFailed?: (row: T) => boolean;
  defaultSort?: string;
  serverSorted?: boolean;
  empty?: { title: ReactNode; hint?: ReactNode; icon?: MarkName };
  footer?: ReactNode;
  onLoadMore?: () => void;
  /** "40 of 312 loaded" — what an export would actually contain. */
  loadedNote?: string;
  /**
   * Which grid on this screen, for the URL keys.
   *
   * THE PRIMARY GRID ON A SCREEN TAKES `sort=` AND `cols=` and every other
   * one takes `sort.<name>=`. Several screens carry two or three grids — the
   * spend by seat and the recent turns, a node's leases and its duties — and
   * unnamed they would all read the ONE `sort` key: sorting the lower table
   * would silently re-sort the upper one, and a link to a sorted screen would
   * mean something different depending on which table the reader had touched.
   *
   * A name rather than an index, because an index is a fact about the source
   * order: inserting a grid above would move every link's meaning by one.
   */
  name?: string;
  /**
   * WHICH COLUMN SET this grid's `cols=` names, where that is not `name`.
   *
   * The two keys answer two different questions and only usually have one
   * answer. `sort=` is a fact about the QUESTION — on a server-sorted list the
   * key goes to the engine, which orders the whole set the same way whatever
   * draws it — and `cols=` is a fact about the DRAWING. The work screen is
   * where they come apart: its list and its table are one grid with two column
   * sets, so the order is shared between them and the column arrangement is
   * not. A `cols=` carries an ORDER as well as a selection and that order is
   * the set's own declaration order, so one key read against both sets draws
   * the list's columns in the table's order — a row nobody arranged.
   *
   * Defaults to `name`, which is what every grid with one column set wants.
   */
  colsName?: string;
}) {
  const [sortRaw, setSort] = useParam(name ? `sort.${name}` : "sort", defaultSort);
  const columnSet = colsName ?? name;
  const [colsRaw, setCols] = useParam(columnSet ? `cols.${columnSet}` : "cols", "");
  // ONE VALUE PER `sort=`, so everything keyed on the order — the sorted rows,
  // the cursor's walk — is worked out again when the order changes and at no
  // other render. Parsed afresh it was a new object every render, and the grid
  // sorted its whole answer on every one of them.
  const sort = useMemo(() => parseSort(sortRaw), [sortRaw]);
  // THE CURSOR IS A ROW, NOT A PLACE: the slot ([slotOf]) of the row `j` and
  // `k` landed on, and the place it was at then. It was the place alone, and a
  // feed is newest first — so a poll that brought one new row slid every row
  // under the cursor down by one, the highlight moved to the row above the one
  // the reader had walked to, and Enter opened that one. The place is kept for
  // the one step that finds the row gone.
  const [cursor, setCursor] = useState<{ slot: string; at: number } | null>(null);
  // UNIQUE PER MOUNTED GRID, not per `name`. A page and the peek rail over it
  // both render grids at once, and `name` distinguishes the grids on ONE
  // screen — two screens' "recent" grids would mint the same row ids and an
  // `aria-labelledby` would resolve to whichever came first in the document.
  const gridId = useId();

  const shownColumns = useMemo(() => {
    const asked = colsRaw
      .split(",")
      .map((c) => c.trim())
      .filter(Boolean);
    if (asked.length === 0) return columns.filter((c) => !c.optional);
    // THE ORDER THE READER ASKED FOR, not the declaration order: `cols=` is a
    // column arrangement, and re-sorting it into declaration order would make
    // the key look ignored.
    const byKey = new Map(columns.map((c) => [c.key, c]));
    const picked = asked.map((key) => byKey.get(key)).filter((c): c is GridColumn<T> => !!c);
    return picked.length > 0 ? picked : columns.filter((c) => !c.optional);
  }, [columns, colsRaw]);

  const sortRows = useCallback(
    (input: T[]): T[] => {
      if (!sort || serverSorted) return input;
      const column = columns.find((c) => c.key === sort.key);
      if (!column?.sortValue) return input;
      const get = column.sortValue;
      // EACH ROW'S VALUE READ ONCE, and the rows sorted by it: a comparator
      // that read both values on every comparison read each row's about
      // twice log n times, and a column's `sortValue` is free to do work.
      // A COPY, always: sorting the array a parent memoised would mutate the
      // caller's own state and make the next render's diff a lie — and the
      // sort is stable, so equal values keep the order they arrived in.
      return input
        .map((row) => ({ row, value: get(row) }))
        .sort((a, b) => {
          const av = a.value;
          const bv = b.value;
          // An absent value sorts last in both directions: "nothing recorded"
          // is not the smallest value, it is not a value.
          if (av == null && bv == null) return 0;
          if (av == null) return 1;
          if (bv == null) return -1;
          const cmp = compare(av, bv);
          return sort.desc ? -cmp : cmp;
        })
        .map((keyed) => keyed.row);
    },
    [columns, sort, serverSorted],
  );

  // THE ROWS IN THE ORDER THEY DRAW, SORTED ONCE — every band's own rows, or
  // the ungrouped list — and drawn from here rather than sorted again in the
  // render: the grid sorted its answer twice a render, once for the cursor's
  // walk and once to draw it.
  const ordered = useMemo(() => {
    const order = (band: GridBand<T>): GridBand<T> =>
      band.bands
        ? { ...band, bands: band.bands.map(order) }
        : { ...band, rows: sortRows(band.rows) };
    return bands
      ? { bands: bands.map(order), rows: [] }
      : { bands: null, rows: sortRows(rows ?? []) };
  }, [bands, rows, sortRows]);

  const flat = useMemo(() => {
    // THROUGH THE SUB-BANDS TOO, and in the order they draw: this is the list
    // `j`, `k` and `enter` walk, so a row the grid renders and this misses is a
    // row the cursor steps over — and a row counted here that is not rendered
    // puts the cursor one place out from every row after it. Each row carries
    // the bands it stands in, which with its key is its slot ([slotOf]).
    const out: Stop<T>[] = [];
    const walk = (band: GridBand<T>, path: string) => {
      const here = bandPath(path, band.key);
      if (band.bands) for (const sub of band.bands) walk(sub, here);
      else for (const row of band.rows) out.push({ row, path: here });
    };
    if (ordered.bands) for (const band of ordered.bands) walk(band, "");
    else for (const row of ordered.rows) out.push({ row, path: "" });
    return out;
  }, [ordered]);

  // `j` and `k` walk a cursor row; `enter` activates it. No selection, because
  // there is nothing to do with one.
  //
  // THE SCROLL IS THE KEYSTROKE'S, not the state update's. It ran inside the
  // `setCursor` updater, which React calls while it renders — and may call
  // twice — so scrolling the page was a side effect of rendering. Here it runs
  // once, in the handler, against the row already drawn at that index; and a
  // press at either end still brings the cursor row back into view, which is
  // what a reader who scrolled away and pressed `j` is asking for.
  //
  // WHERE THE CURSOR ROW IS NOW, in the walk this render draws: -1 for no
  // cursor, and for a cursor whose row has left the list.
  const cursorAt = cursor
    ? flat.findIndex((stop) => slotOf(stop.path, rowKey(stop.row)) === cursor.slot)
    : -1;
  const step = (by: number) => {
    // A ROW THAT LEFT steps from the gap it left: `j` lands on the row that
    // took its place, `k` on the one before it.
    const from = cursorAt >= 0 ? cursorAt : cursor ? cursor.at - (by > 0 ? 1 : 0) : -1;
    const next = Math.max(0, Math.min(flat.length - 1, from + by));
    const stop = flat[next];
    if (stop === undefined) return;
    const slot = slotOf(stop.path, rowKey(stop.row));
    setCursor({ slot, at: next });
    document.getElementById(rowDomId(gridId, slot))?.scrollIntoView({ block: "nearest" });
  };
  const canDrive = flat.length > 0;
  useEffect(() => {
    if (!canDrive) return;
    drivable.push(gridId);
    return () => {
      const at = drivable.indexOf(gridId);
      if (at >= 0) drivable.splice(at, 1);
      // A grid the reader touched and then left takes nothing with it: the
      // fallback has to be free to name the next one.
      if (touched === gridId) touched = null;
    };
  }, [gridId, canDrive]);

  const claimKeyboard = useCallback(() => {
    touched = gridId;
  }, [gridId]);

  // WHOSE KEYSTROKE THIS IS is asked when the key is pressed — see the
  // registry above — and the cursor half is this render's, which is the one
  // the reader is looking at.
  const driving = () => isDriving(gridId);
  useKeyChords([
    { key: "j", run: () => step(1), when: driving },
    { key: "k", run: () => step(-1), when: driving },
    {
      key: "enter",
      when: () => driving() && cursorAt >= 0 && Boolean(onRowActivate),
      run: (e) => {
        const stop = flat[cursorAt];
        if (stop && onRowActivate) onRowActivate(stop.row, e as unknown as React.KeyboardEvent);
      },
    },
  ]);

  // THE ROW'S CLICK, through a ref, so a caller handing a fresh closure every
  // render — which every caller does — does not give every row a new prop and
  // undo [GridRow]'s memo. The click reads the handler of the latest commit.
  const activation = useRef(onRowActivate);
  useLayoutEffect(() => {
    activation.current = onRowActivate;
  });
  const activate = useCallback((row: T, e: React.MouseEvent) => activation.current?.(row, e), []);
  const activatable = Boolean(onRowActivate);

  function headerClick(column: GridColumn<T>): void {
    if (!column.sortValue) return;
    if (sort?.key === column.key && !sort.desc) setSort(`-${column.key}`);
    else if (sort?.key === column.key && sort.desc) setSort("");
    else setSort(column.key);
  }

  // THE TRACK LIST IS DECLARED ONCE, ON THE WRAP.
  //
  // The head and every row used to be a grid container of its own, each handed
  // this same string — which looks like one layout and is not: an intrinsic
  // track (`max-content`, and `minmax(0, 1fr)` once it is out of free space)
  // resolves against the content of ITS OWN container, so every row sized its
  // columns against its own cells alone. Measured against a running engine: a
  // work table's fifth column started at 78.4px in the head and 74px in the
  // rows, and the audit's last column sat 245px from the heading that named
  // it. A grid whose columns do not line up is not a grid.
  //
  // So the wrap owns the tracks and everything between it and a cell is a
  // `subgrid` passthrough (see `.grid-head`/`.grid-body`/`.grid-band`/
  // `.grid-row` in frame.css) — which is also what makes a head cell and a
  // cell twenty rows below it size the SAME track.
  // AND A SHRINK COLUMN IS CAPPED, because `max-content` is not "shrink to
  // content" — it is GROW to content, without a ceiling, and grid resolves it
  // before it gives anything to a `minmax(0, 1fr)`. So one long value in a
  // narrow column starves every flexible one to ZERO and still overflows.
  //
  // Measured against a running engine, and it is the flexible columns that
  // vanish: the schedules grid in an 822px content column drew Wakes at 502px
  // — sixty per cent of the grid — with Name and Task at 0px, and STILL ran to
  // 1139px of content inside a box that clips. The work table at 1000px drew
  // Assignee at 220px with TITLE at zero. A tracker whose title column is
  // invisible is not a list.
  //
  // `fit-content(<percentage>)` is `min(max-content, max(min-content, <cap>))`
  // — so a column under the cap is untouched and keeps sizing to its content,
  // and only one that would take more than its share gives way. ONE FRACTION
  // FOR EVERY GRID rather than a pixel floor per column, which is a number
  // that would have to be invented nineteen times and re-invented at every
  // width. It resolves against the grid's own content box, so it scales with
  // the window instead of pinning a laptop to a desktop's proportions.
  const template = shownColumns
    .map((c) => c.width ?? (c.shrink ? `fit-content(${SHRINK_CAP})` : "minmax(0, 1fr)"))
    .join(" ");

  // A GROUPED ANSWER WITH NO ROWS ON THIS PAGE IS NOT AN EMPTY ANSWER.
  //
  // A band carries the engine's count over its WHOLE group and a bounded slice
  // of rows, so a band whose slice is empty still says how many are in it —
  // "Ada Okonkwo · 0 of 3" is an answer, and "Nothing matches" drawn over it is
  // a different and false one. The empty state is for a grid with nothing to
  // draw at all, which with bands means no band either.
  if (flat.length === 0 && !bands?.length && empty) {
    return (
      <div className="grid-wrap">
        {/* `compact` IS OUR `inline`: an empty state inside a panel rather
            than across a screen. The mark is left to theirs when a screen
            names none — their default is the same inbox drawing ours was. */}
        <EmptyState
          size="compact"
          icon={empty.icon && <Mark name={empty.icon} size={32} />}
          title={empty.title}
          description={empty.hint}
        />
      </div>
    );
  }

  function renderRow(row: T, path: string): ReactNode {
    const key = rowKey(row);
    const slot = slotOf(path, key);
    // EVERYTHING A ROW IS HANDED IS A VALUE IT DRAWS — see [GridRow] — so a
    // render of this grid that changed nothing about a row draws nothing of it.
    // AND NOTHING IT IS HANDED IS ITS PLACE: a feed is newest first, so one new
    // row at the top moves every other one down a place, and a row handed its
    // index drew again for that alone. Whether the cursor is on it is asked of
    // its SLOT for the same reason — see `cursor`.
    return (
      <GridRow<T>
        key={key}
        row={row}
        columns={shownColumns}
        id={rowDomId(gridId, slot)}
        cursor={cursor?.slot === slot}
        selected={Boolean(isSelected?.(row))}
        failed={Boolean(isFailed?.(row))}
        href={rowHref?.(row)}
        activate={activatable ? activate : undefined}
      />
    );
  }

  /**
   * One band, and the bands inside it.
   *
   * RECURSIVE BECAUSE THE ANSWER IS. A second axis comes back as sub-groups
   * under each group, and nesting a heading under a heading is what a second
   * axis MEANS where every row is a line — the alternative, one flat band per
   * pair, throws away which of the two axes a heading belongs to.
   *
   * NO `depth` CLASS: a sub-band's head is inset and quieter so that a band at
   * its parent's inset does not read as its sibling, and the sheet says that
   * with `.grid-band .grid-band > .grid-band-head` — the nesting IS the fact,
   * and a class spelling it out is a second copy of what the DOM already says.
   */
  function renderBand(band: GridBand<T>, path: string): ReactNode {
    // THE LOADED COUNT IS THIS BAND'S OWN ROWS, its sub-bands' included — a
    // band that carries sub-bands carries no rows of its own, so counting
    // `rows` alone reported every twice-grouped band as holding nothing.
    const loaded = bandRows(band).length;
    return (
      <div key={band.key} className="grid-band">
        <div className="grid-band-head">
          {band.mark}
          <span className="truncate">{band.label}</span>
          {/* THE ENGINE'S COUNT AND THE LOADED COUNT, apart. A band head
              reading 12 over 12 of 300 rows is the number a person plans
              against. */}
          <span className="grid-band-count t-num">
            {band.total != null && band.total !== loaded ? `${loaded} of ${band.total}` : loaded}
          </span>
        </div>
        {band.bands
          ? band.bands.map((sub) => renderBand(sub, bandPath(path, band.key)))
          : band.rows.map((row) => renderRow(row, bandPath(path, band.key)))}
        {band.footer && <div className="grid-band-foot">{band.footer}</div>}
      </div>
    );
  }

  return (
    // THE POINTER IS WHAT SAYS WHICH GRID THE READER IS IN — see the registry
    // above. `pointerdown` rather than `click`, so a drag on a header or a
    // press that never becomes a click still hands the keyboard over.
    <div
      className="grid-wrap"
      style={{ gridTemplateColumns: template }}
      onPointerDown={claimKeyboard}
    >
      <div className="grid-head" role="row">
        {shownColumns.map((column) => {
          const sorted = sort?.key === column.key;
          const className = cx(
            "grid-th",
            column.align === "right" && "right",
            column.shrink && "shrink",
            sorted && "sorted",
            !column.sortValue && "plain",
          );
          // A COLUMN THAT CANNOT BE SORTED IS NOT A BUTTON.
          //
          // Every head used to be one, `disabled` where there was no order to
          // ask for — which is the right BEHAVIOUR and the wrong element. A
          // disabled button is still a button in the accessibility tree, so
          // the heads whose label is a glyph or nothing at all (a type mark, a
          // row action) were announced as "button" with no name, and a reader
          // on a screen reader met one unnamed control per such column per
          // grid. `role="columnheader"` is what those cells are; the sortable
          // ones stay buttons, because a button is what they are.
          const body = (
            <>
              <span className="truncate">{column.header}</span>
              {sorted &&
                (sort?.desc ? (
                  <KeyboardArrowDownGlyph size="xs" />
                ) : (
                  <KeyboardArrowUpGlyph size="xs" />
                ))}
            </>
          );
          if (!column.sortValue) {
            return (
              <div key={column.key} className={className} role="columnheader">
                {body}
              </div>
            );
          }
          return (
            <button
              key={column.key}
              type="button"
              className={className}
              onClick={() => headerClick(column)}
              aria-sort={sorted ? (sort?.desc ? "descending" : "ascending") : undefined}
              // AND A SORTABLE HEAD IS NAMED EVEN WHERE ITS HEAD IS NOT A WORD.
              // The rule above takes the unsortable glyph heads out of the
              // button role; what it cannot do is stop a SORTABLE column being
              // declared with a glyph or an empty head, which renders a
              // control whose whole accessible name is the sort arrow. Such a
              // column already carries the word separately for the phone's
              // card layout (`label`, held by `app/source.test.ts`), so the
              // name is there to be spent.
              aria-label={
                typeof column.header === "string" && column.header ? undefined : column.label
              }
            >
              {body}
            </button>
          );
        })}
      </div>

      <div className="grid-body">
        {ordered.bands
          ? ordered.bands.map((band) => renderBand(band, ""))
          : ordered.rows.map((row) => renderRow(row, ""))}
      </div>

      {(footer || onLoadMore || loadedNote) && (
        <div className="grid-foot">
          {footer}
          <span className="spacer" />
          {loadedNote && <span className="t-caption">{loadedNote}</span>}
          {onLoadMore && (
            <Button size="small" variant="secondary" onClick={onLoadMore}>
              Load more
            </Button>
          )}
        </div>
      )}
      {/* A COLUMN CHOOSER THAT CLEARS. `cols=` is a URL key like any other
          and the way back from a hand-edited one has to be visible. */}
      {colsRaw && (
        <button className="grid-cols-reset" onClick={() => setCols("")}>
          Show every column
        </button>
      )}
    </div>
  );
}

/** One row of the walk `j` and `k` take, and the bands it stands in ([bandPath]). */
interface Stop<T> {
  row: T;
  path: string;
}

/**
 * The bands a row stands in, outermost first, each key ENCODED and ended by a
 * `/` — which encoding never leaves in a key, so no two paths run together.
 */
function bandPath(path: string, band: string): string {
  return `${path}${encodeURIComponent(band)}/`;
}

/**
 * A row WHERE IT STANDS: the bands it is in, and its own key, encoded.
 *
 * NOT THE KEY ALONE, because a grouped answer may put one row in two bands — a
 * label board groups on a multi-valued axis, so a task with two tags stands
 * under both — and the cursor, Enter and an element id are each about one of
 * those, not the row in the abstract. Keyed on the key alone, both copies lit
 * together, carried one id between them, and a step from the second resumed
 * from the first, so the cursor could never pass it. A band's key is unique
 * among its siblings (it is the band's React key), and a row's within its band.
 *
 * ENCODED because a key is whatever a screen's `rowKey` returns, while an id
 * list (`aria-labelledby`) is split on whitespace: a key with a space in it
 * would name two elements, neither of them this row.
 */
function slotOf(path: string, key: string): string {
  return `${path}${encodeURIComponent(key)}`;
}

/**
 * A row's element id: its grid's, and its SLOT ([slotOf]) rather than its
 * place — what the overlay link is named by and what a cursor step scrolls to.
 * Keyed on where the row stands because a place in the walk is not that — see
 * `renderRow`.
 */
function rowDomId(gridId: string, slot: string): string {
  return `${gridId}-row-${slot}`;
}

interface GridRowProps<T> {
  row: T;
  /** The columns as drawn — `cols=` applied, in the reader's order. */
  columns: GridColumn<T>[];
  /** The row's own element id ([rowDomId]): its overlay link's name, and where a cursor step scrolls. */
  id: string;
  cursor: boolean;
  selected: boolean;
  failed: boolean;
  /** The row's link; `undefined` where the grid takes no `rowHref`. */
  href: string | undefined;
  /** The grid's click, stable across renders; `undefined` where it takes none. */
  activate: ((row: T, e: React.MouseEvent) => void) | undefined;
}

/**
 * One row, drawn again only when something it draws has changed.
 *
 * MEMOISED ON VALUES, and every prop is one the row draws: its object, the
 * columns, its id, and three booleans and a string the grid works out for
 * it — and never its PLACE, which a new row above it moves without changing
 * anything it draws. The click is the one function, and the grid hands every row the same
 * one through a ref, because a caller's `onRowActivate` is a fresh closure on
 * every render of every screen. So a cursor stepping from one row to the next
 * draws those two rows; a parent rendering for a reason of its own — a poll
 * that brought the same rows back, a filter typed above the grid — draws none;
 * and a row whose object, columns or state moved is drawn again. A poll's
 * answer keeps the objects of the rows it did not change (`protocol/share.ts`), so
 * what a poll draws is what it changed. Built
 * inline, every one of those drew every row: a `j` on a hundred-row grid was a
 * hundred rows, and a screen's own render was every row of every grid on it.
 *
 * WHICH IS WHY A COLUMN LIST IS MEMOISED BY ITS CALLER: a new list is a new
 * value here, and rightly — a column closing over something new may draw
 * something new — so a screen that builds its columns inline draws every row
 * whenever it renders, exactly as before.
 */
function GridRowView<T>({
  row,
  columns,
  id,
  cursor,
  selected,
  failed,
  href,
  activate,
}: GridRowProps<T>): ReactNode {
  // THE CELL CARRIES ITS COLUMN'S OWN NAME.
  //
  // Below the drawer breakpoint a row is not a row: the column heads go and
  // each cell is drawn as a labelled line, because a nine-column table in a
  // 390px card clips six of them with nothing to scroll (the wrap is
  // `overflow: clip`, which is what keeps the sticky head working). A label
  // per cell is the only way a value keeps its meaning once the head it sat
  // under is gone.
  //
  // FROM THE HEADER WHERE THE HEADER IS A WORD, and from `label` where it is
  // not. A head may be a glyph or nothing at all — a type mark, a row action,
  // a pair of state tags — because the column is twenty pixels wide and a
  // word does not fit in it. `attr()` can only read text, and a head like
  // that leaves the card with an unnamed line: measured on the tracker at
  // 390px as a bare type mark floating between KEY and TITLE, which reads as
  // a rendering fault rather than as a value. The head cannot carry the word
  // — that is what made it a glyph — so the column says it separately, and
  // the card is the only layout that spends it.
  //
  // THE LABEL IS SET UNCONDITIONALLY AND THE SHEET DROPS THE LINE. A column
  // draws no value on a row that has none — `PriorityMark` renders null for
  // `normal`, which nearly every task is — and in the card that left the
  // label alone on a line: `PRIORITY` with nothing beside it. Nothing here
  // can see that, because a component that renders null is an element like
  // any other until the browser draws it; `.grid-cell:empty` in `frame.css`
  // is the browser answering, and the element has to stay in the DOM anyway
  // or the wide layout's positional tracks move. What this file owes that
  // rule is that a cell with no value has no child nodes — which is what
  // `DataGrid.test.tsx` holds, since a mark wrapped in an always-rendered
  // span would defeat it silently.
  const inner = columns.map((column) => (
    <span
      key={column.key}
      className={cx("grid-cell", column.align === "right" && "right", column.shrink && "shrink")}
      data-label={
        (typeof column.header === "string" && column.header ? column.header : column.label) ||
        undefined
      }
    >
      {column.cell(row)}
    </span>
  ));
  const className = cx("grid-row", selected && "selected", failed && "failed", cursor && "cursor");
  const linked = href !== undefined;
  // THE ROW'S OWN LINK IS AN OVERLAY, NOT THE ROW.
  //
  // The row used to BE an `<a>` when `rowHref` was given, and a cell that
  // links — `SeatCell`, `SeatChip`, `KeyCell` — then put an anchor inside an
  // anchor. That is not merely invalid: the HTML parser CLOSES the outer one
  // at the inner one, so the row link covered only the cells before the
  // first seat chip and the rest of the row silently stopped being
  // clickable. Both halves looked identical and one of them did nothing.
  //
  // Stretched over the row instead, the link keeps everything it had — a
  // real href, so ⌘-click, middle-click and "copy link address" all work —
  // and the cells' own links sit above it (`.grid-row a { position:
  // relative }`), so a click on a seat reaches the seat and a click on the
  // row reaches the row.
  //
  // It takes its accessible name FROM THE ROW, which is what the anchor row
  // announced before: the name computation walks the element `aria-labelledby`
  // points at, so a screen reader still reads the cells rather than "link".
  return (
    <div
      id={id}
      className={className}
      role={!linked && activate ? "button" : undefined}
      tabIndex={!linked && activate ? 0 : undefined}
      onClick={!linked && activate ? (e) => activate(row, e) : undefined}
    >
      {linked && (
        <a
          className="row-link"
          href={href}
          aria-labelledby={id}
          onClick={(e) => activate?.(row, e)}
        />
      )}
      {inner}
    </div>
  );
}

const GridRow = memo(GridRowView) as typeof GridRowView;
