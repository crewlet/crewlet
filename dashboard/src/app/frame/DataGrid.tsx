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

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useParam } from "../router.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { Button, Empty, cx } from "~/ui/primitives.tsx";

export interface GridColumn<T> {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  /** The value sorted on. Omit to make the column unsortable. */
  sortValue?: (row: T) => string | number | null | undefined;
  align?: "left" | "right";
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

/** `sort=-updated` → `{key: "updated", desc: true}`. */
export function parseSort(raw: string): { key: string; desc: boolean } | null {
  if (!raw) return null;
  return raw.startsWith("-") ? { key: raw.slice(1), desc: true } : { key: raw, desc: false };
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
  empty?: { title: ReactNode; hint?: ReactNode; icon?: IconName };
  footer?: ReactNode;
  onLoadMore?: () => void;
  /** "40 of 312 loaded" — what an export would actually contain. */
  loadedNote?: string;
}) {
  const [sortRaw, setSort] = useParam("sort", defaultSort);
  const [colsRaw, setCols] = useParam("cols", "");
  const sort = parseSort(sortRaw);
  const body = useRef<HTMLDivElement>(null);
  const [cursor, setCursor] = useState(-1);

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
  useEffect(() => {
    function onKey(e: KeyboardEvent): void {
      const el = document.activeElement;
      const typing =
        el instanceof HTMLElement &&
        (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === "j" || e.key === "k") {
        e.preventDefault();
        setCursor((at) => {
          const next = Math.max(0, Math.min(flat.length - 1, at + (e.key === "j" ? 1 : -1)));
          body.current
            ?.querySelector<HTMLElement>(`[data-row-index="${next}"]`)
            ?.scrollIntoView({ block: "nearest" });
          return next;
        });
      }
      if (e.key === "Enter" && cursor >= 0 && cursor < flat.length) {
        const row = flat[cursor];
        if (row && onRowActivate) {
          e.preventDefault();
          onRowActivate(row, e as unknown as React.KeyboardEvent);
        }
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [flat, cursor, onRowActivate]);

  function headerClick(column: GridColumn<T>): void {
    if (!column.sortValue) return;
    if (sort?.key === column.key && !sort.desc) setSort(`-${column.key}`);
    else if (sort?.key === column.key && sort.desc) setSort("");
    else setSort(column.key);
  }

  const template = shownColumns
    .map((c) => c.width ?? (c.shrink ? "max-content" : "minmax(0, 1fr)"))
    .join(" ");

  if (flat.length === 0 && empty) {
    return (
      <div className="grid-wrap">
        <Empty inline icon={empty.icon} title={empty.title} hint={empty.hint} />
      </div>
    );
  }

  let index = -1;
  function renderRow(row: T): ReactNode {
    index += 1;
    const at = index;
    const key = rowKey(row);
    const inner = shownColumns.map((column) => (
      <span
        key={column.key}
        className={cx("grid-cell", column.align === "right" && "right", column.shrink && "shrink")}
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
    const style = { gridTemplateColumns: template } as React.CSSProperties;
    return rowHref ? (
      <a
        key={key}
        className={className}
        style={style}
        href={rowHref(row)}
        data-row-index={at}
        onClick={(e) => onRowActivate?.(row, e)}
      >
        {inner}
      </a>
    ) : (
      <div
        key={key}
        className={className}
        style={style}
        data-row-index={at}
        role={onRowActivate ? "button" : undefined}
        tabIndex={onRowActivate ? 0 : undefined}
        onClick={(e) => onRowActivate?.(row, e)}
      >
        {inner}
      </div>
    );
  }

  return (
    <div className="grid-wrap">
      <div className="grid-head" style={{ gridTemplateColumns: template }} role="row">
        {shownColumns.map((column) => {
          const sorted = sort?.key === column.key;
          return (
            <button
              key={column.key}
              type="button"
              className={cx(
                "grid-th",
                column.align === "right" && "right",
                column.shrink && "shrink",
                sorted && "sorted",
                !column.sortValue && "plain",
              )}
              onClick={() => headerClick(column)}
              aria-sort={sorted ? (sort?.desc ? "descending" : "ascending") : undefined}
              disabled={!column.sortValue}
            >
              <span className="truncate">{column.header}</span>
              {sorted && <Icon name={sort?.desc ? "chevronDown" : "chevronUp"} size="xs" />}
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
            <Button size="sm" onClick={onLoadMore}>
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
