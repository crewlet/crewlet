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
import { groupLabel, STATUS_TONE, type LabelContext } from "~/lib/work.ts";
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
  return null;
}

/** One group's heading text, in the company's own words. */
export function headingOf(axis: string, group: WorkGroup, ctx: LabelContext): string {
  return groupLabel(axis, group, ctx);
}
