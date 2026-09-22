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
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { useParam } from "../router.tsx";
import { Button, EmptyState, cx } from "@crewlethq/ui";
import { KeyboardArrowDownGlyph, KeyboardArrowUpGlyph } from "@crewlethq/icons/glyphs";
// A GRID'S EMPTY MARK IS NAME-KEYED, because `empty.icon` is part of the prop
// every screen fills in — so the name→drawing lookup stays in `~/ui/Icon.tsx`.
import { Mark, type MarkName } from "~/ui/glyph.tsx";
import { useKeyChords } from "~/lib/keys.ts";

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

export interface GridBand<T> {
  key: string;
  label: ReactNode;
  /** The engine's count over the whole group, where it gave one. */
  total?: number;
  rows: T[];
  /** A per-band aggregate line. */
  footer?: ReactNode;
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
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
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
 */
const drivable: string[] = [];
let touched: string | null = null;
let revision = 0;
const watchers = new Set<() => void>();

/** Wake every grid: whose keystroke this is has changed. */
function announce(): void {
  revision += 1;
  for (const watcher of watchers) watcher();
}

function watchDriving(watcher: () => void): () => void {
  watchers.add(watcher);
  return () => {
    watchers.delete(watcher);
  };
}

function drivingRevision(): number {
  return revision;
}

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
}) {
  const [sortRaw, setSort] = useParam(name ? `sort.${name}` : "sort", defaultSort);
  const [colsRaw, setCols] = useParam(name ? `cols.${name}` : "cols", "");
  const sort = parseSort(sortRaw);
  const body = useRef<HTMLDivElement>(null);
  const [cursor, setCursor] = useState(-1);
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
      // A COPY, always: sorting the array a parent memoised would mutate the
      // caller's own state and make the next render's diff a lie.
      return [...input].sort((a, b) => {
        const av = get(a);
        const bv = get(b);
        // An absent value sorts last in both directions: "nothing recorded"
        // is not the smallest value, it is not a value.
        if (av == null && bv == null) return 0;
        if (av == null) return 1;
        if (bv == null) return -1;
        const cmp = compare(av, bv);
        return sort.desc ? -cmp : cmp;
      });
    },
    [columns, sort, serverSorted],
  );

  const flat = useMemo(() => {
    if (bands) return bands.flatMap((b) => sortRows(b.rows));
    return sortRows(rows ?? []);
  }, [bands, rows, sortRows]);

  // `j` and `k` walk a cursor row; `enter` activates it. No selection, because
  // there is nothing to do with one.
  const step = useCallback(
    (by: number) =>
      setCursor((at) => {
        const next = Math.max(0, Math.min(flat.length - 1, at + by));
        body.current
          ?.querySelector<HTMLElement>(`[data-row-index="${next}"]`)
          ?.scrollIntoView({ block: "nearest" });
        return next;
      }),
    [flat.length],
  );
  // `when` below is a value captured at render, so a pointer gesture that
  // changes nothing on screen still has to re-render every grid for them to
  // agree about whose keystroke the next one is.
  useSyncExternalStore(watchDriving, drivingRevision);
  const canDrive = flat.length > 0;
  useEffect(() => {
    if (!canDrive) return;
    drivable.push(gridId);
    announce();
    return () => {
      const at = drivable.indexOf(gridId);
      if (at >= 0) drivable.splice(at, 1);
      // A grid the reader touched and then left takes nothing with it: the
      // fallback has to be free to name the next one.
      if (touched === gridId) touched = null;
      announce();
    };
  }, [gridId, canDrive]);
  const driving = isDriving(gridId);

  const claimKeyboard = useCallback(() => {
    if (touched === gridId) return;
    touched = gridId;
    announce();
  }, [gridId]);

  useKeyChords([
    { key: "j", run: () => step(1), when: driving },
    { key: "k", run: () => step(-1), when: driving },
    {
      key: "enter",
      when: driving && cursor >= 0 && cursor < flat.length && Boolean(onRowActivate),
      run: (e) => {
        const row = flat[cursor];
        if (row && onRowActivate) onRowActivate(row, e as unknown as React.KeyboardEvent);
      },
    },
  ]);

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

  if (flat.length === 0 && empty) {
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

  let index = -1;
  function renderRow(row: T): ReactNode {
    index += 1;
    const at = index;
    const key = rowKey(row);
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
    const inner = shownColumns.map((column) => (
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
    const className = cx(
      "grid-row",
      isSelected?.(row) && "selected",
      isFailed?.(row) && "failed",
      cursor === at && "cursor",
    );
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
    const rowId = `${gridId}-row-${at}`;
    return (
      <div
        key={key}
        id={rowHref ? rowId : undefined}
        className={className}
        data-row-index={at}
        role={!rowHref && onRowActivate ? "button" : undefined}
        tabIndex={!rowHref && onRowActivate ? 0 : undefined}
        onClick={!rowHref && onRowActivate ? (e) => onRowActivate(row, e) : undefined}
      >
        {rowHref && (
          <a
            className="row-link"
            href={rowHref(row)}
            aria-labelledby={rowId}
            onClick={(e) => onRowActivate?.(row, e)}
          />
        )}
        {inner}
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

      <div className="grid-body" ref={body}>
        {bands
          ? bands.map((band) => (
              <div key={band.key} className="grid-band">
                <div className="grid-band-head">
                  <span className="truncate">{band.label}</span>
                  {/* THE ENGINE'S COUNT AND THE LOADED COUNT, apart. A band
                      head reading 12 over 12 of 300 rows is the number a
                      person plans against. */}
                  <span className="grid-band-count t-num">
                    {band.total != null && band.total !== band.rows.length
                      ? `${band.rows.length} of ${band.total}`
                      : band.rows.length}
                  </span>
                </div>
                {sortRows(band.rows).map(renderRow)}
                {band.footer && <div className="grid-band-foot">{band.footer}</div>}
              </div>
            ))
          : sortRows(rows ?? []).map(renderRow)}
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
