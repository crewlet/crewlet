/**
 * The one peek in the product, mounted by the shell.
 *
 * # Why the shell and not the screen
 *
 * The rail used to be rendered by whichever screen wanted one, which meant
 * exactly one screen had one: the tracker. Every other list could address a
 * peek — `objects.ts` knows nineteen kinds — and none could open one, so the
 * grammar the addressing was built for existed only on paper.
 *
 * Mounted here it is the opposite: no screen renders a peek and every screen
 * has one. A list opens it by naming what it points at, and a pasted URL with
 * `peek=` in it opens the same thing on arrival.
 *
 * # Stepping belongs to the list, not to the rail
 *
 * `[` and `]` walk the rows the reader is actually looking at — sorted,
 * filtered and paged as they left them — and only the list knows that order.
 * So a list PUBLISHES its order with [usePeekNeighbours] and the rail steps
 * through it; a list that publishes nothing gets a rail with no stepper rather
 * than one that steps through something else.
 *
 * The published order is a plain array of refs rather than the rows
 * themselves: the rail needs identity and nothing else, and handing it rows
 * would make every list's row type part of the frame's contract.
 */

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

import { DetailRail, usePeek, usePeekControls } from "./DetailRail.tsx";
import { PEEKS } from "./peeks.tsx";
import { refToken, type ObjectRef } from "./objects.ts";

interface Neighbours {
  refs: ObjectRef[];
  publish: (refs: ObjectRef[]) => void;
}

const NeighbourContext = createContext<Neighbours | null>(null);

export function PeekNeighbours({ children }: { children: ReactNode }) {
  const [refs, publish] = useState<ObjectRef[]>([]);
  const value = useMemo(() => ({ refs, publish }), [refs]);
  return <NeighbourContext.Provider value={value}>{children}</NeighbourContext.Provider>;
}

/**
 * Publish the order `[` and `]` should walk.
 *
 * KEYED ON THE TOKENS rather than on the array, because a list rebuilds its
 * rows on every push: an effect depending on the array itself would republish
 * several times a second and re-render the rail with it.
 */
export function usePeekNeighbours(refs: ObjectRef[]): void {
  const host = useContext(NeighbourContext);
  const key = refs.map(refToken).join("|");
  const publish = host?.publish;
  useEffect(() => {
    if (!publish) return;
    publish(refs);
    // The list that published last owns the stepper; unmounting clears it so
    // a rail opened from the next screen does not step through the last one's
    // rows.
    return () => publish([]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, publish]);
}

export function PeekHost() {
  const object = usePeek();
  const { open, close } = usePeekControls();
  const host = useContext(NeighbourContext);
  const refs = host?.refs ?? [];

  // A KIND WITH NO BODY CLOSES THE RAIL rather than opening it empty. A
  // `peek=` naming a kind this build cannot render is a hand-edited or
  // out-of-date URL, and an empty panel over the list says less than no panel.
  const body = object ? PEEKS[object.kind] : undefined;
  useEffect(() => {
    if (object && !body) close();
  }, [object, body, close]);
  if (!object || !body) return null;

  const at = refs.findIndex((r) => refToken(r) === refToken(object));
  const step =
    at >= 0 && refs.length > 1
      ? (delta: -1 | 1) => {
          const next = refs[Math.max(0, Math.min(refs.length - 1, at + delta))];
          if (next && refToken(next) !== refToken(object)) open(next);
        }
      : undefined;

  return (
    <DetailRail ref={object} onStep={step}>
      {body({ id: object.id })}
    </DetailRail>
  );
}
