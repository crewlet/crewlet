/**
 * The reads on screen that failed, and the one control that asks them again.
 *
 * A MODULE OF ITS OWN, imported by `lib/useQuery.ts` and `lib/useRest.ts`
 * alike and by the banner that draws the control (`components/common.tsx`'
 * QueryState), so neither loader depends on the other's module: a suite that
 * mocks `useQuery.ts` would otherwise take the REST loader's registration down
 * with it, and every screen reading through `useRest` would fail to render.
 */

import { useEffect } from "react";

/**
 * Every read on screen that the engine failed to answer, each the refetch it
 * asks again with: what one "Try again" asks again ([retryFailedReads]).
 */
const failedReads = new Set<() => void>();

/**
 * ASK AGAIN EVERY READ THAT FAILED, whatever screen drew it: the one control a
 * failed read's banner offers (`components/common.tsx`' QueryState). A banner
 * that said only that the engine failed to answer, and to check logs a
 * dashboard reader has no access to, left the reader nothing to do but
 * reload the page.
 */
export function retryFailedReads(): void {
  for (const ask of [...failedReads]) ask();
}

/**
 * Hold `ask` in the set [retryFailedReads] asks again while `failed` holds. A
 * `useQuery` read and a `useRest` read (`lib/useRest.ts`) alike: the banner
 * draws the one Try again for either, and a REST read left out of the set
 * made it a button that asked nothing.
 */
export function useAskedAgainOnFailure(failed: boolean, ask: () => void): void {
  useEffect(() => {
    if (!failed) return;
    failedReads.add(ask);
    return () => {
      failedReads.delete(ask);
    };
  }, [failed, ask]);
}
