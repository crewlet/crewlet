/**
 * Whether an address is one of this dashboard's own pages, and which.
 *
 * ONE RULE FOR EVERY PLACE A WRITTEN ADDRESS MAY OPEN IN PLACE: a link in a
 * chat message or an answer (`components/InlineToken.tsx`), an Open in app
 * card (`app/assistant/chunk/openInApp.ts`) and a link preview a read built
 * (`routes/chatlet/MessageLinks.tsx`). Under the `checked` link rule only
 * what the dashboard can vouch for is live, and a page of its own frame is
 * that: it opens in this tab, on this origin, through the router, and can go
 * nowhere the reader could not reach from the sidebar.
 *
 * TWO SPELLINGS, ONE PAGE: the route itself (`#/doclet/pages/…`, what a page
 * link inserts) and the whole address a Copy link writes out
 * (`https://<this dashboard>/#/chatlet/…`), which is that route on this
 * page's own origin and path. Any other origin, a query before the fragment,
 * a backslash or a control character (what a browser rewrites), `#//` (the
 * shape of a protocol-relative address) and a path the router resolves to no
 * framed page (a sign-in screen, Not Found) are not one.
 *
 * WRITTEN BY SOMEBODY ELSE, IT OPENS REBUILT FROM ITS PATH ALONE
 * ([AppTarget.canonical]): the page, with no query the writer chose, as an
 * address on a configured host is rebuilt with none (`checked` in
 * `lib/inlineTokens.ts`). A record's page is its path; a query is a filter, a
 * pane or a prefilled field, which is what an injected instruction would
 * reach for.
 */

import { href, parseHash } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";

/** The longest address followed: an engine-built route is a few segments and filters. */
export const MAX_APP_ADDRESS = 2048;

/** Where an address goes, as the router moves to it. */
export interface AppTarget {
  /** The route as written, query included: for an address the engine built. */
  hash: string;
  /** The page rebuilt from its path alone: what a written link opens. */
  canonical: string;
  path: string[];
  query: Record<string, string>;
}

/** The page of this dashboard `address` names, or null where it is not one. */
export function appRoute(address: string): AppTarget | null {
  if (address.length > MAX_APP_ADDRESS) return null;
  if (/[\\\u0000-\u001f\u007f]/.test(address)) return null;
  const hash = address.startsWith("#") ? address : ownFragment(address);
  if (hash === null || !hash.startsWith("#/") || hash.startsWith("#//")) return null;
  const route = parseHash(hash);
  const where = resolve(route.path);
  if (route.path.length === 0 || !where.resolved || where.workspace === "") return null;
  return {
    hash,
    canonical: href(route.path),
    path: route.path,
    query: Object.fromEntries(route.query),
  };
}

/** The fragment of an address on this page's own origin and path, or null. */
function ownFragment(address: string): string | null {
  if (typeof location === "undefined") return null;
  let url: URL;
  try {
    url = new URL(address);
  } catch {
    return null;
  }
  if (url.origin !== location.origin || url.pathname !== location.pathname || url.search) {
    return null;
  }
  return url.hash || null;
}
