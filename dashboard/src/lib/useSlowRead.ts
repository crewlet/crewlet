/**
 * Whether a read has been loading for longer than a reader waits in silence.
 *
 * A SKELETON SAYS "LOADING", NOT "SLOW": the same shimmer at 100 ms and at 20 s
 * leaves a reader no way to tell a slow answer from a stuck one. Past this
 * mark a screen says what it is reading and how to make it cheaper.
 */

import { useEffect, useState } from "react";

/** Past this, a read that has not answered is slow enough to say so. */
export const SLOW_READ_MS = 2_500;

export function useSlowRead(loading: boolean, after: number = SLOW_READ_MS): boolean {
  const [slow, setSlow] = useState(false);
  useEffect(() => {
    if (!loading) {
      setSlow(false);
      return;
    }
    const timer = setTimeout(() => setSlow(true), after);
    return () => clearTimeout(timer);
  }, [loading, after]);
  return loading && slow;
}
