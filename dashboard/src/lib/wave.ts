/**
 * A wave of the socket's queries: at most `size` of one kind of read in flight
 * at once, the rest waiting their turn in the order they asked.
 *
 * A SCREEN THAT ASKS ONE READ PER ROW bursts: a transcript asks one link
 * preview per message that carries links, and a room's Shared list one per
 * link. Past the socket's own count (`MAX_IN_FLIGHT_QUERIES`) the engine stops
 * reading the socket until a query finishes, so an unbounded burst holds the
 * room's own re-reads behind every preview it asked for. Bounded to one wave,
 * the reads around it wait behind one read rather than behind several.
 */

import { useCallback, useEffect, useRef, useState } from "react";

/** A bound on reads in flight, shared by every row that takes a slot of it. */
export interface Wave {
  /**
   * Ask for a slot: `grant` is called once one is free, now or later. The
   * answer gives the slot back, or withdraws the ask while it still waits;
   * calling it again does nothing.
   */
  take(grant: () => void): () => void;
}

/** A wave of `size` slots. */
export function createWave(size: number): Wave {
  let busy = 0;
  const waiting: (() => void)[] = [];
  return {
    take(grant) {
      let held = false;
      let done = false;
      const start = () => {
        held = true;
        busy += 1;
        grant();
      };
      if (busy < size) start();
      else waiting.push(start);
      return () => {
        if (done) return;
        done = true;
        if (!held) {
          waiting.splice(waiting.indexOf(start), 1);
          return;
        }
        busy -= 1;
        waiting.shift()?.();
      };
    },
  };
}

/**
 * A slot of `wave` while `wanted`: `granted` once one is free, and kept from
 * then on, so the read it opened stays live. The slot goes back with
 * `release` (once the read has answered) or when the component goes.
 */
export function useWaveSlot(
  wave: Wave,
  wanted: boolean,
): { granted: boolean; release: () => void } {
  const [granted, setGranted] = useState(false);
  const give = useRef<() => void>(() => {});
  useEffect(() => {
    if (!wanted) return;
    const back = wave.take(() => setGranted(true));
    give.current = back;
    return back;
  }, [wave, wanted]);
  const release = useCallback(() => give.current(), []);
  return { granted, release };
}
