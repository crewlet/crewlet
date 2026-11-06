/**
 * The kit's `TreeGrid`, bound to this dashboard's address as `DataGrid` is
 * (`app/frame/DataGrid.tsx`): the sort and the column set live in the URL, so a
 * sorted tree survives a reload, comes back from Back and can be sent to
 * somebody. The primary grid on a screen takes `sort=` and `cols=`, every other
 * `sort.<name>=` and `cols.<name>=`.
 *
 * THE ADDRESS HOLDS ONLY WHAT THE READER CHOSE: with no sort the rows are in
 * the tree's own order, which no head marks.
 */

import { type ReactNode } from "react";
import { TreeGrid as KitTreeGrid, type TreeGridProps } from "@crewlethq/ui";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { useParam } from "../router.tsx";

export interface FrameTreeGridProps extends Omit<
  TreeGridProps,
  "sort" | "onSortChange" | "cols" | "onColsChange" | "empty"
> {
  /** Which grid on this screen, for the URL keys (`sort.<name>=`). */
  name?: string;
  /** What an empty tree says; its mark is named, as every screen names it. */
  empty?: { title: ReactNode; hint?: ReactNode; icon?: GlyphName; action?: ReactNode };
}

export function TreeGrid({ name, empty, ...rest }: FrameTreeGridProps) {
  const [sort, setSort] = useParam(name ? `sort.${name}` : "sort", "");
  const [cols, setCols] = useParam(name ? `cols.${name}` : "cols", "");
  return (
    <KitTreeGrid
      {...rest}
      sort={sort}
      onSortChange={setSort}
      cols={cols}
      onColsChange={setCols}
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
