/**
 * The one peek in the product, mounted by the shell in the kit's peek slot.
 *
 * # Why the shell and not the screen
 *
 * The rail used to be rendered by whichever screen wanted one, which meant
 * exactly one screen had one: the tracker. Every other list could address a
 * peek (`objects.ts` knows nineteen kinds) and none could open one. Mounted
 * here it is the opposite: no screen renders a peek and every screen has one.
 * A list opens it by naming what it points at, and a pasted URL with `peek=`
 * in it opens the same thing on arrival.
 *
 * # The rail is the kit's, the wiring is ours
 *
 * The rail, its grip, its drawer and its veil are uilet's `PeekRail`, and the
 * generic half of hosting one (the stepper over a published order, a kind
 * with no body closing the rail, the body keyed on its object) is uilet's
 * `PeekHost`. What stays here is what only this application knows: how an
 * object is addressed (`peekAddress.ts`, the history rule), which body reads
 * each kind (`peeks.tsx`), the boundary a body that throws is caught in, the
 * keys that step and close from anywhere on the page (`keymap.ts`), and the
 * width the reader dragged it to, kept per browser under the frame's own
 * storage key.
 *
 * # Stepping belongs to the list, not to the rail
 *
 * `[` and `]` walk the rows the reader is actually looking at (sorted,
 * filtered and paged as they left them), and only the list knows that order.
 * So a list PUBLISHES its order with [usePeekNeighbours] and the rail steps
 * through it; a list that publishes nothing gets a rail with no stepper.
 */

import { useCallback, useContext } from "react";
import {
  PeekHost as KitPeekHost,
  usePeekNeighbours as useKitPeekNeighbours,
  usePeekStep as useKitPeekStep,
  useStoredWidth,
} from "@crewlethq/ui";

import { usePeek, usePeekControls, peekHref } from "./peekAddress.ts";
import { PEEKS } from "./peeks.tsx";
import { PeekBoundary } from "../boundaries.tsx";
import { KINDS, parseRef, pathOf, refToken, type ObjectRef } from "./objects.ts";
import { samePath, useRoute } from "../router.tsx";
import { useKeymap } from "../keymap.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useClickOnNothing } from "~/ui/clickOnNothing.ts";
import { PEEK_MAX, PEEK_MIN } from "../layout.ts";
import { PeekRestingWidth } from "./peekWidth.ts";

export { PeekNeighbours } from "@crewlethq/ui";

/**
 * What a click on nothing leaves alone while a peek is open: the rail itself,
 * and the assistant beside the page, whose Open in app cards open this very
 * peek and whose conversation is about it.
 */
export const PEEK_KEEPS = ".crewlet-peek-rail, .crewlet-app-shell__aside";

/**
 * Publish the order `[` and `]` should walk, as the objects a list draws.
 *
 * The kit's order is a list of tokens, the host's own identity for an object;
 * here that is [refToken], kind and id, so a page and an item sharing an id
 * are still two rows.
 */
export function usePeekNeighbours(refs: ObjectRef[], query?: Record<string, string>): void {
  useKitPeekNeighbours(refs.map(refToken), query);
}

/** Move the peek to the object a token names, replacing the history entry. */
function useMoveTo(): (token: string) => void {
  const { move } = usePeekControls();
  return useCallback(
    (token: string) => {
      const next = parseRef(token);
      if (next) move(next);
    },
    [move],
  );
}

/**
 * The step `[` and `]` take from the object the peek is open on, through the
 * order the list under it published, or undefined where there is nothing to
 * step through.
 *
 * A STEP MOVES THE PEEK, it does not open another: `move` REPLACES the entry,
 * so four objects walked through one open rail are one place the reader has
 * been and Back leaves the list.
 *
 * EXPORTED FOR A LIST WHOSE OWN WIDGET TAKES THE KEYS. A tree hands focus to
 * its items and reads every printable key as type-ahead, so a `[` pressed on
 * an org chart's card never reached the rail's binding; the chart claims the
 * two keys and steps through THIS, so there is one stepper and one order.
 */
export function usePeekStep(): ((delta: -1 | 1) => void) | undefined {
  const object = usePeek();
  return useKitPeekStep(object ? refToken(object) : null, useMoveTo());
}

export interface PeekHostProps {
  /**
   * What the sheet keeps beside a peek COLUMN with the list at its floor
   * (`listReserve` in `app/layout.ts`); the rail is capped at the sheet less
   * this, so a width dragged on a wide window never takes a narrower one's
   * list away. Undefined while the peek is a drawer.
   */
  reserve?: number | undefined;
}

export function PeekHost({ reserve }: PeekHostProps) {
  const object = usePeek();
  const { close } = usePeekControls();
  const moveTo = useMoveTo();
  const route = useRoute();
  const step = usePeekStep();

  // THE READER'S WIDTH, where they dragged one, over the width the screen
  // under the rail rests it at (`peekWidth.ts`): a canvas rests it at 330.
  const resting = useContext(PeekRestingWidth);
  const stored = useStoredWidth({ key: STORAGE_KEYS.peekWidth, min: PEEK_MIN, max: PEEK_MAX });

  // ESCAPE CLOSES FROM THE PAGE TOO, and from inside a field. The rail
  // answers a press made inside it first, and a press it answered is one the
  // keymap skips, so one Escape is never two closes.
  useKeymap({
    "peek.close": { run: close, when: object !== null },
    "peek.previous": { run: () => step?.(-1), when: Boolean(step) },
    "peek.next": { run: () => step?.(1), when: Boolean(step) },
  });

  // AND A CLICK ON NOTHING CLOSES IT, on every width rather than only behind
  // the drawer's veil: the row or card it was opened from stayed highlighted
  // beside an open rail wherever the reader clicked next.
  useClickOnNothing(object ? close : null, PEEK_KEEPS);

  return (
    <KitPeekHost
      current={object ? refToken(object) : null}
      // A KIND WITH NO BODY CLOSES THE RAIL rather than opening it empty: a
      // `peek=` naming a kind this build cannot render is a hand-edited or
      // out-of-date URL, and an empty panel over the list says less than none.
      render={(token) => {
        const ref = parseRef(token);
        const body = ref ? PEEKS[ref.kind] : undefined;
        return ref && body ? body({ id: ref.id }) : undefined;
      }}
      label={(token) => {
        const ref = parseRef(token);
        return ref ? `${KINDS[ref.kind].label} detail` : "Detail";
      }}
      onClose={close}
      onMove={moveTo}
      // NO `Open` OVER THE PAGE IT WOULD OPEN: for a kind whose peek is its
      // page, that page is the list under the rail, and the link would only
      // close it.
      openHref={(token, query) => {
        const ref = parseRef(token);
        if (!ref) return undefined;
        if (KINDS[ref.kind].peekIsPage && samePath(route.path, pathOf(ref))) return undefined;
        return peekHref(ref, query ? { ...query } : undefined);
      }}
      // A PEEK THAT THROWS TAKES ONLY ITS BODY DOWN: the rail's close, its
      // stepper and Open are outside, and the screen behind is not in this
      // tree at all. See `app/boundaries.tsx`.
      boundary={(token, body) => <PeekBoundary resetKey={token}>{body}</PeekBoundary>}
      width={stored.width ?? resting}
      defaultWidth={resting}
      minWidth={PEEK_MIN}
      maxWidth={PEEK_MAX}
      reserve={reserve}
      onWidthChange={stored.setWidth}
      onWidthCommit={stored.commitWidth}
      labels={{ previousHint: "Previous ([)", nextHint: "Next (])", closeHint: "Close (esc)" }}
    />
  );
}
