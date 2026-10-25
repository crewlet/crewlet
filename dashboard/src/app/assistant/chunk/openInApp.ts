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

import { parseHash } from "../../router.tsx";
import { resolve } from "../../routes.ts";

/** Where an address goes, as the router moves to it. */
export interface InAppTarget {
  path: string[];
  query: Record<string, string>;
}

/** The longest address followed; an engine-built route is a few segments and filters. */
const MAX_ADDRESS = 2048;

/** The page `href` names, or null where it is not one of this dashboard's own. */
export function openInApp(href: string): InAppTarget | null {
  if (href.length > MAX_ADDRESS) return null;
  // A HASH ROUTE AND NOTHING ELSE: `#/` then a path. A backslash or a control
  // character is refused before parsing, because those are what a browser
  // rewrites, and `#//` is the shape of a protocol-relative address.
  if (!href.startsWith("#/") || href.startsWith("#//")) return null;
  if (/[\\\u0000-\u001f\u007f]/.test(href)) return null;
  const route = parseHash(href);
  // A PAGE OF THE FRAME: a sign-in screen (an invitation, a reset link) is
  // never somewhere the assistant sends anybody.
  const where = resolve(route.path);
  if (route.path.length === 0 || !where.resolved || where.workspace === "") return null;
  return { path: route.path, query: Object.fromEntries(route.query) };
}
