/**
 * What the Spend screen takes from the assistant: its window and the split
 * its chart is drawn by, written to the address as its own controls write
 * them.
 *
 * PURE VIEW CHANGES, so neither needs approval (D11): each narrows what the
 * reader can already read, applies at once, and its receipt offers Undo. The
 * window is held to the screen's own offer (`spendOffer`), so the assistant
 * cannot ask for a window the page would quietly replace with its fallback.
 */

import { choiceKey, windowKey, type UrlActionKey } from "~/app/assistant/urlAction.ts";
import { GROUPS } from "~/contract/spend.ts";
import type { Offer } from "~/lib/range.ts";

/** The key the window action writes. */
export function spendRangeKeys(offer: Offer, now: () => number): Record<string, UrlActionKey> {
  return { window: windowKey(offer, now) };
}

/** The key the split action writes: a section, `phase` its fallback, as the screen reads it. */
export function spendGroupKeys(): Record<string, UrlActionKey> {
  return { group: choiceKey("Split by", GROUPS, "section", "phase") };
}
