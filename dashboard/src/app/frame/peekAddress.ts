/**
 * The peek's ADDRESS: which object it is open on, and how a list opens,
 * moves and closes it.
 *
 * # Why the frame owns it
 *
 * Worklet grew its own peek (`item=` on the board, a rail rendered by the
 * board), so a board could peek a task and nothing else in the product could
 * peek anything. Here it is one query key, `peek=`, so any list can open any
 * kind, and a pasted URL carrying one opens the same thing on arrival. The
 * rail itself is the kit's (`PeekRail`, mounted by `PeekHost.tsx`); what this
 * module keeps is what only this application can answer: the address.
 *
 * # The history rule
 *
 * Opening PUSHES, so Back closes the peek, which is what a reader means by
 * Back with a panel open. Moving it (`[` and `]` stepping through the list)
 * REPLACES: four objects walked through one open rail are one place the
 * reader has been, exactly as four ticked chips are one screen.
 *
 * # Which clicks peek
 *
 * A row is a real link to its object's page, so a middle press and a modified
 * press open a tab; a plain press peeks instead. That rule is the kit's
 * (`peekClickHandler`, `peekRowActivation`), written once there for every
 * consumer, and a row here calls it directly.
 */

import { useCallback } from "react";
import { href, useNavigator, useRoute } from "../router.tsx";
import { KINDS, parseRef, pathOf, refToken, type ObjectRef } from "./objects.ts";

/** The object the peek is open on, or null. */
export function usePeek(): ObjectRef | null {
  const route = useRoute();
  return parseRef(route.query.get("peek"));
}

/**
 * Open, move and close the peek.
 *
 * `open` pushes and `move` replaces; see the history rule above. Both are
 * written here rather than at each call site, because a screen that opened a
 * peek with `filter` would make Back leave the list entirely.
 */
export function usePeekControls(): {
  open: (ref: ObjectRef) => void;
  move: (ref: ObjectRef) => void;
  close: () => void;
} {
  const nav = useNavigator();
  return {
    open: useCallback((ref: ObjectRef) => nav.section("peek", refToken(ref)), [nav]),
    move: useCallback((ref: ObjectRef) => nav.filter({ peek: refToken(ref) }), [nav]),
    close: useCallback(() => nav.filter({ peek: null }), [nav]),
  };
}

/**
 * The href a row carries, so a modifier press opens the object's page, with
 * `query` being what that page carries from the list it was opened from.
 *
 * FOR A KIND WHOSE PEEK IS ITS PAGE (`KindSpec.peekIsPage`), the list it is
 * read beside with the peek open: a person has no screen of their own, and a
 * new tab opened on the bare list was a click that lost who it was about.
 */
export function peekHref(ref: ObjectRef, query?: Record<string, string>): string {
  if (KINDS[ref.kind].peekIsPage) return href(pathOf(ref), { ...query, peek: refToken(ref) });
  return href(pathOf(ref), query);
}
