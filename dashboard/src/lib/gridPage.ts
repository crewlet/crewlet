/**
 * A long list the client holds whole, drawn a page at a time.
 *
 * A grid lays out and refits every row it is handed, so a screen that merged a
 * few hundred records and handed them all over paid that cost on every resize.
 * Such a screen sorts the WHOLE list here, by the grid's own column sort
 * values, and hands the grid one page (`serverSorted`, so it does not re-sort
 * the slice): a page sorted on its own would order only the rows loaded and
 * read as an order over everything.
 */

import { useState } from "react";
import { parseGridSort, type DataGridColumn } from "@crewlethq/ui";
import { fmtCount } from "./format.ts";
import { collator } from "./formatters.ts";

/** Words with their digits read as numbers ("b9" before "b10"), as the grid sorts. */
const NUMERIC = collator(undefined, { numeric: true });

/** Rows a page adds: two to three screens of a grid at a desktop's height. */
export const GRID_PAGE_ROWS = 50;

/**
 * `rows` in a new array, in the order a grid with these columns would draw for
 * `sort` (`key` or `-key`). An absent value sorts last both ways, and equal
 * values keep their order, as the kit's grid sorts.
 */
export function sortGridRows<T>(
  rows: readonly T[],
  columns: readonly DataGridColumn<T>[],
  sort: string,
): T[] {
  const parsed = parseGridSort(sort);
  const get = parsed && columns.find((c) => c.key === parsed.key)?.sortValue;
  if (!parsed || !get) return [...rows];
  const sign = parsed.desc ? -1 : 1;
  return [...rows].sort((a, b) => {
    const av = get(a);
    const bv = get(b);
    if (av == null || bv == null) return av == null ? (bv == null ? 0 : 1) : -1;
    if (typeof av === "number" && typeof bv === "number") return sign * (av - bv);
    return sign * NUMERIC.compare(String(av), String(bv));
  });
}

/** What a paged grid takes: its rows, the way to more, and what is loaded. */
export interface GridPage<T> {
  rows: T[];
  onLoadMore: (() => void) | undefined;
  loadedNote: string | undefined;
}

/**
 * The first `count` of `rows`, growing a page per Load more. Back to one page
 * whenever `reset` changes (a new window, filter or sort), since more rows of
 * a different question are not what the reader asked for.
 */
export function useGridPage<T>(
  rows: readonly T[],
  reset: string,
  size = GRID_PAGE_ROWS,
): GridPage<T> {
  const [paged, setPaged] = useState({ reset, count: size });
  const count = paged.reset === reset ? paged.count : size;
  const more = rows.length > count;
  return {
    rows: rows.slice(0, count),
    onLoadMore: more ? () => setPaged({ reset, count: count + size }) : undefined,
    loadedNote: more ? `${fmtCount(count)} of ${fmtCount(rows.length)} shown` : undefined,
  };
}
