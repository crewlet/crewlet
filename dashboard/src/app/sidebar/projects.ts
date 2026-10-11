/**
 * Which projects the person sidebar nests under Work: the reader's own.
 *
 * # Yours, not the company's
 *
 * A sidebar is where a person goes, not a second list of the company's things,
 * so the rows under Work are the projects the reader is IN — and the company's
 * whole list is one row further, "All projects". A project is yours when:
 *
 *  - you LEAD it, or somebody in your line does — the founder at the chart's
 *    root is in every project a report of theirs leads, which is the sense in
 *    which a person who leads the chart is in the company's projects;
 *  - your UNIT owns it, or a unit above yours does — the unit chain is the
 *    teams you work in, outermost first;
 *  - or you HOLD OPEN WORK in it, which is how a person borrowed onto another
 *    team's project finds it.
 *
 * AT MOST SIX, ALPHABETICAL BY NAME. A sidebar that grew a row per project a
 * person had ever touched would be the PROJECTS group this replaced, and an
 * order by activity would move the row a reader is reaching for between the
 * decision and the press.
 *
 * A PURE FUNCTION OVER VALUES, so the rule is exercised without a render: the
 * three sources are three reads (the projects, the chart, the reader's own
 * open work by project), and every one of them may still be in flight.
 */

import type { WorkProjectRow } from "~/protocol/index.ts";

/** How many of the reader's projects the sidebar nests under Work. */
export const YOUR_PROJECTS = 6;

export interface YourProjectsInput {
  projects: readonly WorkProjectRow[];
  /**
   * The reader's own handle (`viewer.handle`), or "" for a key no seat names.
   * THE VIEWER'S, never the chart's lookup of it: the viewer answers before
   * the chart arrives, and a project the reader leads is theirs either way.
   */
  handle: string;
  /** The names of the units the reader works in, outermost first (`Seat.unitChain`). */
  units: readonly string[];
  /** The handles in the reader's line (`viewer.line`): the people below them. */
  line: readonly string[];
  /** The project keys the reader holds open work in. */
  holding: ReadonlySet<string>;
}

/** The reader's projects, alphabetical by name, at most [YOUR_PROJECTS]. */
export function yourProjects({
  projects,
  handle,
  units: chain,
  line,
  holding,
}: YourProjectsInput): WorkProjectRow[] {
  // A UNIT IS ADDRESSED BY NAME (`Unit.name`), and the project's reference
  // carries the name the chart resolved, compared in either case because a
  // filter on a unit matches every spelling it answers to.
  const units = new Set(chain.map((name) => name.toLowerCase()));
  const leads = new Set([handle, ...line].filter(Boolean));
  const mine = projects.filter((p) => {
    if (p.archived) return false;
    if (holding.has(p.key)) return true;
    if (p.lead.handle && leads.has(p.lead.handle)) return true;
    const unit = p.unit.name?.toLowerCase();
    return unit !== undefined && units.has(unit);
  });
  return mine
    .sort((a, b) => (a.name || a.key).localeCompare(b.name || b.key))
    .slice(0, YOUR_PROJECTS);
}
