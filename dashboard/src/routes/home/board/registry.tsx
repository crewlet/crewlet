/**
 * Every widget the dashboard's sections offer for Home, found at build time.
 *
 * FOUND, NOT LISTED. A section offers widgets in a `home.widgets.tsx` beside
 * its screens (`src/routes/<section>/home.widgets.tsx`), and every one is read
 * here; there is no table of them to keep. A section that grows a widget adds
 * the file, and a section that is removed takes its widgets with it.
 *
 * WHO MAY ADD WHAT is the navigation's answer for the page a widget names:
 * the workspace's grants and the page's, as the sidebar locks them.
 */

import { WORKSPACES, type Workspace } from "~/app/nav.ts";
import { holdsAny } from "~/lib/access.ts";
import { WIDGET_PROBLEMS, destinationOf, pageOpens, type WidgetDef } from "./kit.tsx";

const found = import.meta.glob<{ widgets: readonly WidgetDef[] }>("../../*/home.widgets.tsx", {
  eager: true,
});

function collect(): ReadonlyMap<string, WidgetDef> {
  const all = new Map<string, WidgetDef>();
  for (const [file, mod] of Object.entries(found)) {
    for (const def of mod.widgets ?? []) {
      // A key offered twice keeps its first offer; the second is reported.
      if (all.has(def.key)) {
        WIDGET_PROBLEMS.push(`${def.key}: offered twice (${file}).`);
        console.error(`Home widget left out: ${def.key} is offered twice (${file}).`);
        continue;
      }
      all.set(def.key, def);
    }
  }
  return all;
}

export const WIDGETS = collect();

export const widgetDef = (key: string): WidgetDef | undefined => WIDGETS.get(key);

/** Whether a reader holding `grants` may read the page a widget summarises, and what it reads itself. */
export function canUse(def: WidgetDef, grants: readonly string[]): boolean {
  const dest = destinationOf(def.from);
  return dest !== undefined && pageOpens(dest, grants) && holdsAny(grants, def.grants);
}

export interface Area {
  key: Workspace;
  label: string;
  /** The sidebar's own sentence for the workspace. */
  hint: string;
  widgets: WidgetDef[];
}

/** The tray's company sections: one per workspace that offers widgets, in the sidebar's order. */
export const AREAS: readonly Area[] = WORKSPACES.flatMap((row) => {
  const widgets = [...WIDGETS.values()].filter((d) => destinationOf(d.from)?.workspace === row.key);
  return widgets.length > 0 ? [{ key: row.key, label: row.label, hint: row.hint, widgets }] : [];
});
