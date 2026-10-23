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
 * first drivable grid mounted drives. A grid showing its empty state
 * registers nothing. Module state rather than a context: the grids that must
 * agree are often in different subtrees, and a provider around both would be
 * the shell, which knows nothing about grids.
 */

import { useCallback, useEffect, useId, useRef, useSyncExternalStore, type ReactNode } from "react";
import {
  DataGrid as KitDataGrid,
  type DataGridBand,
  type DataGridHandle,
  type DataGridProps,
} from "@crewlethq/ui";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { useParam } from "../router.tsx";
import { peekHref, usePeek } from "./DetailRail.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { useKeymap } from "../keymap.ts";

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

/**
 * Whether ANY grid on screen holds `j` and `k`: for a screen that binds the
 * same two keys to something of its own (a task page stepping through the list
 * it was opened from) and must stand aside while a grid takes them.
 */
export function useAnyGridDriving(): boolean {
  useSyncExternalStore(watchDriving, drivingRevision);
  return drivable.length > 0;
}

function isDriving(gridId: string): boolean {
  if (touched !== null) return touched === gridId;
  return drivable[0] === gridId;
}

/** How many rows a grid draws, its bands' rows included. */
function rowCount<T>(rows: T[] | undefined, bands: DataGridBand<T>[] | undefined): number {
  const inBand = (band: DataGridBand<T>): number =>
    band.bands ? band.bands.reduce((n, sub) => n + inBand(sub), 0) : band.rows.length;
  return bands ? bands.reduce((n, band) => n + inBand(band), 0) : (rows?.length ?? 0);
}

export interface GridProps<T> extends Omit<
  DataGridProps<T>,
  "sort" | "onSortChange" | "cols" | "onColsChange" | "handleRef" | "onPointerDown" | "empty"
> {
  /** The sort before the reader chose one, as `key` or `-key`. */
  defaultSort?: string;
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
  ...rest
}: GridProps<T>) {
  const [sort, setSort] = useParam(name ? `sort.${name}` : "sort", defaultSort);
  const columnSet = colsName ?? name;
  const [cols, setCols] = useParam(columnSet ? `cols.${columnSet}` : "cols", "");
  const handle = useRef<DataGridHandle>(null);
  const gridId = useId();

  const peek = usePeek();
  const peeked = peek ? peekHref(peek) : "";
  const marked = useCallback(
    (row: T) => Boolean(isSelected?.(row)) || (peeked !== "" && rowHref?.(row) === peeked),
    [isSelected, peeked, rowHref],
  );

  // A pointer gesture that changes nothing on screen still has to re-render
  // every grid for them to agree about whose keystroke the next one is.
  useSyncExternalStore(watchDriving, drivingRevision);
  const canDrive = rowCount(rows, bands) > 0;
  useEffect(() => {
    if (!canDrive) return;
    drivable.push(gridId);
    announce();
    return () => {
      const at = drivable.indexOf(gridId);
      if (at >= 0) drivable.splice(at, 1);
      // A grid the reader touched and then left takes nothing with it.
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

  useKeymap({
    "list.next": { run: () => handle.current?.step(1), when: driving },
    "list.previous": { run: () => handle.current?.step(-1), when: driving },
    "list.open": {
      when: driving && Boolean(onRowActivate),
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
      onSortChange={setSort}
      cols={cols}
      onColsChange={setCols}
      handleRef={handle}
      onPointerDown={claimKeyboard}
      empty={
        empty && {
          title: empty.title,
          hint: empty.hint,
          action: empty.action,
          icon: empty.icon && <Mark name={empty.icon} size={32} />,
        }
      }
    />
  );
}
