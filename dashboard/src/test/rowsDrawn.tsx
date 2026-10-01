/**
 * Which grid rows a screen DRAWS, counted by the cells that draw them.
 *
 * Imported by tests only, through `vi.mock` of `~/app/frame/DataGrid.tsx`:
 *
 *     vi.mock("~/app/frame/DataGrid.tsx", (original) => countingGrid(original));
 *
 * The grid stays the real one. What changes is that every column a screen
 * hands it has its `cell` wrapped to note the row it was called for — and the
 * wrapped list is kept per ORIGINAL list (a `WeakMap`), so a screen whose
 * column list holds still hands the grid a list that holds still, and one that
 * builds its list inline hands it a new one every render exactly as before.
 * The count is therefore the work a row's render does, measured on the
 * screen's own columns, and nothing the wrapper adds can make a row draw or
 * spare one.
 */

import type { ReactNode } from "react";
import type { GridColumn } from "~/app/frame/DataGrid.tsx";

/** The keys of the rows drawn since the last [drawnRows] reset. */
const drawn = new Set<string>();

/** The rows drawn since the last call, by key, and the count restarted. */
export function drawnRows(): string[] {
  const keys = [...drawn].sort();
  drawn.clear();
  return keys;
}

type Grid = (props: Record<string, unknown>) => ReactNode;

/** The DataGrid module, its grid counting the rows it draws. */
export async function countingGrid(
  importOriginal: <M>() => Promise<M>,
): Promise<typeof import("~/app/frame/DataGrid.tsx")> {
  const real = await importOriginal<typeof import("~/app/frame/DataGrid.tsx")>();
  const wrapped = new WeakMap<object, GridColumn<unknown>[]>();
  const RealGrid = real.DataGrid as unknown as Grid;
  function CountingGrid(props: Record<string, unknown>): ReactNode {
    const columns = props.columns as GridColumn<unknown>[];
    const rowKey = props.rowKey as (row: unknown) => string;
    let counted = wrapped.get(columns);
    if (!counted) {
      counted = columns.map((column) => ({
        ...column,
        cell: (row: unknown) => {
          drawn.add(rowKey(row));
          return column.cell(row);
        },
      }));
      wrapped.set(columns, counted);
    }
    return <RealGrid {...props} columns={counted} />;
  }
  return { ...real, DataGrid: CountingGrid as unknown as typeof real.DataGrid };
}
