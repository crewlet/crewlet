/**
 * Whether an address the engine built may be opened here, and where it goes.
 *
 * AN OPEN IN APP CARD IS THE ONLY WAY THE ASSISTANT NAVIGATES, and its address
 * is the engine's, built from its own route table and checked against what the
 * person may open (`permitted`). The drawer checks it again before it moves the
 * page: it must be one of this dashboard's own routes (`#/` and a path, never a
 * scheme, a host or `javascript:`), and the path must resolve to a framed page
 * this build draws. Anything else is refused and drawn as text, so a card whose
 * address went wrong on the way says so rather than sending the reader
 * somewhere the engine never meant.
 */

import { appRoute } from "~/lib/appRoute.ts";

/** Where an address goes, as the router moves to it. */
export interface InAppTarget {
  path: string[];
  query: Record<string, string>;
}

/**
 * The page `href` names, or null where it is not one of this dashboard's own:
 * a hash route and nothing else, by the dashboard's one rule for its own
 * pages (`lib/appRoute.ts`). An engine-built card never carries a whole
 * address, so one is refused here even on this origin.
 */
export function openInApp(href: string): InAppTarget | null {
  if (!href.startsWith("#")) return null;
  const target = appRoute(href);
  return target && { path: target.path, query: target.query };
}
