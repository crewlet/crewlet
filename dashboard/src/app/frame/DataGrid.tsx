/**
 * The grid every list in the product is drawn with: the kit's `DataGrid`,
 * bound to this dashboard's address, its peek rail and its keymap.
 *
 * The visual is uilet's (columns, give-way, bands, the row link, the phone
 * card, flush in a card). What lives here is what only this application can
 * answer, and nothing it draws:
 *
 * # The sort and the column set live in the URL
 *
 * Every screen, section and filter is in the URL, so a sorted ops table can be
 * sent to somebody, survives a reload and comes back sorted from Back. THE
 * PRIMARY GRID ON A SCREEN TAKES `sort=` AND `cols=` and every other takes
 * `sort.<name>=` (`name`), because several screens carry two or three grids and
 * unnamed they would all read one key. `colsName` keys the column set apart
 * from the sort where one grid has two column sets (the work list and table):
 * the order is the question's and the arrangement is the drawing's.
 *
 * THE ADDRESS HOLDS ONLY WHAT THE READER CHOSE. `defaultSort` goes to the kit,
 * which draws it on its head as the sort in force and steps a press from it,
 * and a step back to the default is written as no key at all. Read with the
 * default as its fallback, a press on a column at its own descending default
 * wrote the default back and changed nothing.
 *
 * A screen whose order is resolved with more than the address (a saved view's
 * own order behind it) hands the grid the reader's choice as `sort` and takes
 * the press back through `onSortChange`, and keeps its own key.
 *
 * # The row the peek is open on is marked
 *
 * By comparing ADDRESSES: a row's link is its object's page, and the peek
 * names an object whose page is `peekHref` of it, so the mark cannot name a
 * different row from the one the rail shows. `isSelected` stays for a mark
 * that is not the peek.
 *
 * # Which grid the keyboard drives
 *
 * `j`, `k` and `enter` are bound on `window`, because a grid whose rows are
 * links takes no focus of its own. Several screens carry two or three grids at
 * once (and a page's grid sits under another in the peek rail), so the
 * reader's LAST POINTER GESTURE says which grid they are in; with none yet the
 * first drivable grid mounted drives. That decision is the kit's
 * (`useDataGridDriving`); the keys stay this keymap's rows, gated on it, so
 * they are checked against every other key the dashboard binds.
 */

import { useCallback, useRef, type ReactNode } from "react";
import {
  DataGrid as KitDataGrid,
  countDataGridRows,
  useDataGridDriving,
  type DataGridHandle,
  type DataGridProps,
} from "@crewlethq/ui";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { useParam } from "../router.tsx";
import { peekHref, usePeek } from "./peekAddress.ts";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useKeymap } from "../keymap.ts";

export interface GridProps<T> extends Omit<
  DataGridProps<T>,
  "sort" | "onSortChange" | "cols" | "onColsChange" | "handleRef" | "onPointerDown" | "empty"
> {
  /** The sort before the reader chose one, as `key` or `-key`. */
  defaultSort?: string;
  /**
   * The reader's own sort, for a screen that resolves it itself; left out, the
   * grid reads it from the address (`sort=`, or `sort.<name>=`).
   */
  sort?: string;
  /** Where a press on a head goes, beside `sort`. `''` is the default. */
  onSortChange?: (sort: string) => void;
  /** Which grid on this screen, for the URL keys (`sort.<name>=`). */
  name?: string;
  /** Which column set `cols=` names, where that is not `name`. */
  colsName?: string;
  /** What an empty grid says; its mark is named, as every screen names it. */
  empty?: { title: ReactNode; hint?: ReactNode; icon?: GlyphName; action?: ReactNode };
}

export function DataGrid<T>({
  defaultSort = "",
  name,
  colsName,
  empty,
  isSelected,
  rowHref,
  onRowActivate,
  rows,
  bands,
  sort: ownSort,
  onSortChange: onOwnSortChange,
  ...rest
}: GridProps<T>) {
  // THE READER'S CHOICE ALONE, never the default: see the module doc.
  const [addressSort, setAddressSort] = useParam(name ? `sort.${name}` : "sort", "");
  const sort = ownSort ?? addressSort;
  const setSort = onOwnSortChange ?? setAddressSort;
  const columnSet = colsName ?? name;
  const [cols, setCols] = useParam(columnSet ? `cols.${columnSet}` : "cols", "");
  const handle = useRef<DataGridHandle>(null);

  const peek = usePeek();
  const peeked = peek ? peekHref(peek) : "";
  const marked = useCallback(
    (row: T) => Boolean(isSelected?.(row)) || (peeked !== "" && rowHref?.(row) === peeked),
    [isSelected, peeked, rowHref],
  );

  // WHICH GRID THE KEYS DRIVE is the kit's decision: the one the reader last
  // pointed at, else the first mounted with rows. A grid showing its empty
  // state takes part in nothing.
  const { driving, claim } = useDataGridDriving(countDataGridRows(rows, bands) > 0);

  useKeymap({
    "list.next": { run: () => handle.current?.step(1), when: driving },
    "list.previous": { run: () => handle.current?.step(-1), when: driving },
    // A ROW OPENS FROM THE KEYBOARD WHETHER IT ACTS OR ONLY LINKS: the kit's
    // handle follows the cursor row's link where it has no handler, so a grid
    // of links (a directory a row navigates from) has its Enter too.
    "list.open": {
      when: driving && Boolean(onRowActivate || rowHref),
      run: (e) => {
        handle.current?.activate(e as KeyboardEvent);
      },
    },
  });

  return (
    <KitDataGrid
      {...rest}
      rows={rows}
      bands={bands}
      rowHref={rowHref}
      onRowActivate={onRowActivate}
      isSelected={marked}
      sort={sort}
      defaultSort={defaultSort}
      onSortChange={setSort}
      cols={cols}
      onColsChange={setCols}
      handleRef={handle}
      onPointerDown={claim}
      empty={
        empty && {
          title: empty.title,
          hint: empty.hint,
          action: empty.action,
          icon: empty.icon && <NamedGlyph name={empty.icon} size={32} />,
        }
      }
    />
  );
}
