/**
 * What a group of rows is headed with, wherever a shape draws one.
 *
 * THE BOARD AND THE LIST GROUP ON THE SAME AXIS and used to head their groups
 * differently: a column carried the status dot and the type icon, a list group
 * carried the word alone. Same answer, same axis, two headings — so a reader
 * who switched shape to see more rows lost the mark they had been scanning for.
 *
 * The mark is the AXIS's, never the group's: only `status` has a dot and only
 * `type` has an icon, because those are the two axes whose values the product
 * draws a mark for anywhere else. An axis with no mark draws none rather than
 * a placeholder — a blank slot on every row of an assignee board is a column
 * of nothing, which is what `.side-mark` exists to avoid in the rail and what
 * a heading does not need at all.
 */

import { cx } from "@crewlethq/ui";
import { TypeIcon, type RowChrome } from "~/components/work.tsx";
import { groupLabel, STATUS_TONE, type Tone, type LabelContext } from "~/lib/work.ts";
import type { WorkGroup } from "~/protocol/index.ts";

/**
 * The dot a status is drawn with.
 *
 * THE TONE TABLE IS `lib/work.ts`'S. The board carried a second copy — six
 * statuses mapped to the same four tones — and a second spelling of one rule
 * is two rules as soon as a company's vocabulary moves. NEUTRAL ADDS NO CLASS
 * because `.dot` is already the neutral dot: a `.dot.neutral` rule does not
 * exist, so emitting the word would be a class that styles nothing and reads
 * to the next person like one that does.
 */
export function statusDot(status: string): string {
  const tone = STATUS_TONE[status];
  return cx("dot", tone !== undefined && tone !== "neutral" && tone);
}

/**
 * The two due bands that are a STATE rather than a place on a calendar.
 *
 * A BAND'S OWN COLOUR IS ITS STATE where the band IS one, and that is true of
 * exactly these two: work somebody has already missed, and work they have
 * today. "This week", "Later" and "No due date" are positions on a calendar,
 * which is identity, and identity is neutral here like everywhere else.
 *
 * TWO KEYS RATHER THAN SIX, deliberately. This is a DRAWING decision about two
 * bands, not a second declaration of the axis: the set, its order and its words
 * are `internal/tracker/grouping.go`'s `dueBands` and reach the screen on the
 * answer, so a band added there draws no mark rather than a wrong one, and no
 * table here can drift from the engine's.
 */
const DUE_BAND_TONE: Record<string, Tone> = {
  overdue: "critical",
  today: "info",
};

/** The mark an axis's value wears, or nothing for an axis that has none. */
export function GroupMark({
  axis,
  groupKey,
  chrome,
}: {
  axis: string;
  groupKey: string;
  chrome: RowChrome;
}) {
  if (axis === "status") return <i className={statusDot(groupKey)} aria-hidden="true" />;
  if (axis === "type") return <TypeIcon type={groupKey} types={chrome.types} />;
  if (axis === "due:bucket") {
    const tone = DUE_BAND_TONE[groupKey];
    return tone ? <i className={cx("dot", tone)} aria-hidden="true" /> : null;
  }
  return null;
}

/** One group's heading text, in the company's own words. */
export function headingOf(axis: string, group: WorkGroup, ctx: LabelContext): string {
  return groupLabel(axis, group, ctx);
}
