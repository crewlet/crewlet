/**
 * The address of Principles with parts open in edit: `#/company/principles?edit=mission`.
 *
 * ADDRESSABLE, so another screen can send a person to write the charter where
 * it is read: the org chart's company node opens the page with its first part
 * in edit, and a reload of a page in edit opens the same parts again (the
 * words typed are not kept, see `state.ts`; the browser asks before it
 * reloads). Several open parts are one comma-separated value
 * (`edit=mission,policies`), in reading order.
 *
 * The parameter is what the page HOLDS, never a command it replays: the page
 * writes it whenever a part opens or closes, by replacing the entry, so Back
 * leaves the page rather than stepping through every Edit pressed on it.
 */

import { href } from "~/app/router.tsx";
import { PRINCIPLES_PATH } from "~/app/routes.ts";
import { CHARTER_PARTS, type CharterPart } from "./charter.ts";

/** The query key the open parts are held under. */
export const EDIT_PARAM = "edit";

/**
 * The parts an `edit` value names, in reading order, once each. A value naming
 * no part (an old builder address such as `edit=charter`, a typo) opens none.
 */
export function partsIn(value: string | null): CharterPart[] {
  if (!value) return [];
  const named = new Set(value.split(",").map((part) => part.trim()));
  return CHARTER_PARTS.filter((part) => named.has(part));
}

/** The `edit` value for `parts`, or "" for none. */
export function editValue(parts: Iterable<CharterPart>): string {
  const open = new Set(parts);
  return CHARTER_PARTS.filter((part) => open.has(part)).join(",");
}

/**
 * The query that opens Principles with `part` in edit (Mission when unnamed,
 * the first statement an agent reads), for a navigation made in code:
 * `nav.to(PRINCIPLES_PATH, principlesEditQuery())`. For a reader who may not
 * write the charter it is simply the page.
 */
export function principlesEditQuery(part: CharterPart = "mission"): Record<string, string> {
  return { [EDIT_PARAM]: part };
}

/** [principlesEditQuery] as a link's address. */
export function principlesEditHref(part: CharterPart = "mission"): string {
  return href(PRINCIPLES_PATH, principlesEditQuery(part));
}
