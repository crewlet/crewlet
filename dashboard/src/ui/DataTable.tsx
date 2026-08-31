/**
 * One sortable table for the whole product.
 *
 * The dashboard this replaces had ten hand-rolled tables and not one of them
 * could be sorted — seven on the Spend screen alone, three on Fleet, two on
 * Schedules — so "which seat is costing the most" was answered by reading
 * every row. Sorting is not a nicety on an operational surface; it is the
 * question.
 *
 * The sort is STABLE and the comparator is three-way. Both matter here: rows
 * arrive from a live push several times a second, and a comparator returning
 * -1 for equal operands (the `a < b ? 1 : -1` idiom the old views used) makes
 * equal rows swap places on every render under V8's TimSort — a list that
 * shuffles itself while somebody is reading it.
 */

import { useMemo, useState, type ReactNode } from "react";
import { Empty } from "./primitives.tsx";
import { Icon, type IconName } from "./Icon.tsx";

export interface Column<T> {
  key: string;
  header: ReactNode;
  /** Rendered cell. */
  cell: (row: T) => ReactNode;
  /** The value sorted on. Omit to make the column unsortable. */
  sortValue?: (row: T) => string | number;
  align?: "left" | "right";
  /** Shrink to content and never wrap. */
  shrink?: boolean;
  width?: string;
}

export interface DataTableProps<T> {
  rows: T[];
  columns: Column<T>[];
  rowKey: (row: T) => string;
  onRowClick?: (row: T) => void;
  isFailed?: (row: T) => boolean;
  isSelected?: (row: T) => boolean;
  defaultSort?: { key: string; dir: "asc" | "desc" };
  empty?: { title: ReactNode; hint?: ReactNode; icon?: IconName };
  maxHeight?: number | string;
}

function compare(a: string | number, b: string | number): number {
  if (typeof a === "number" && typeof b === "number") {
    // NaN sorts last rather than poisoning the comparator's transitivity.
    if (Number.isNaN(a)) return Number.isNaN(b) ? 0 : 1;
    if (Number.isNaN(b)) return -1;
    return a < b ? -1 : a > b ? 1 : 0;
  }
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: "base" });
}

export function DataTable<T>({
  rows,
  columns,
  rowKey,
  onRowClick,
  isFailed,
  isSelected,
  defaultSort,
  empty,
  maxHeight,
}: DataTableProps<T>) {
  const [sort, setSort] = useState<{ key: string; dir: "asc" | "desc" } | null>(
    defaultSort ?? null,
  );

  const sorted = useMemo(() => {
    if (!sort) return rows;
    const col = columns.find((c) => c.key === sort.key);
    if (!col?.sortValue) return rows;
    const get = col.sortValue;
    // Decorate with the original index so the sort is stable: two rows that
    // compare equal keep the order the server sent them in, rather than
    // trading places whenever a push re-renders the list.
    return rows
      .map((row, i) => ({ row, i }))
      .sort((x, y) => {
        const d = compare(get(x.row), get(y.row));
        if (d !== 0) return sort.dir === "asc" ? d : -d;
        return x.i - y.i;
      })
      .map((d) => d.row);
  }, [rows, columns, sort]);

  function toggle(key: string) {
    setSort((prev) =>
      prev?.key === key ? { key, dir: prev.dir === "asc" ? "desc" : "asc" } : { key, dir: "desc" },
    );
  }

  if (!rows.length && empty) {
    return <Empty inline icon={empty.icon} title={empty.title} hint={empty.hint} />;
  }

  return (
    <div className="table-wrap" style={maxHeight ? { maxHeight, overflowY: "auto" } : undefined}>
      <table className="table">
        <thead>
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  className={[
                    c.sortValue ? "sortable" : "",
                    c.align === "right" ? "num" : "",
                    c.shrink ? "shrink" : "",
                  ]
                    .filter(Boolean)
                    .join(" ")}
                  style={c.width ? { width: c.width } : undefined}
                  onClick={c.sortValue ? () => toggle(c.key) : undefined}
                  aria-sort={active ? (sort.dir === "asc" ? "ascending" : "descending") : undefined}
                  scope="col"
                >
                  {c.header}
                  {c.sortValue && (
                    <span className="sort-mark" aria-hidden="true">
                      {active ? (sort.dir === "asc" ? "↑" : "↓") : ""}
                    </span>
                  )}
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.map((row) => (
            <tr
              key={rowKey(row)}
              className={[
                onRowClick ? "clickable" : "",
                isFailed?.(row) ? "failed" : "",
                isSelected?.(row) ? "selected" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              tabIndex={onRowClick ? 0 : undefined}
              onKeyDown={
                onRowClick
                  ? (e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        onRowClick(row);
                      }
                    }
                  : undefined
              }
            >
              {columns.map((c) => (
                <td
                  key={c.key}
                  className={[c.align === "right" ? "num" : "", c.shrink ? "shrink" : ""]
                    .filter(Boolean)
                    .join(" ")}
                >
                  {c.cell(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** A column header with a unit, so a number never has to carry its own. */
export function ColHead({ children, unit }: { children: ReactNode; unit?: string }) {
  return (
    <>
      {children}
      {unit && <span style={{ color: "var(--text-faint)" }}> {unit}</span>}
    </>
  );
}

export function SortHint() {
  return (
    <span className="t-caption row" style={{ gap: 4 }}>
      <Icon name="filter" size="xs" /> click a column to sort
    </span>
  );
}
