/**
 * The engine's own health, as the one read every surface that shows it shares.
 *
 * A MODULE OF ITS OWN, not a hook beside the store's: `store-hooks.ts` is what
 * `useQuery` reads its client and connection from, so a query hook living in
 * `store-hooks.ts` made the two modules import each other. That cycle was
 * invisible until something imported `store-hooks.ts` first — then a suite
 * that mocks `store-hooks.ts` (six do) resolved `useQuery`'s import of it
 * while the mock's own factory was still loading the real module, and
 * `useQuery` bound the REAL `useClient`, which throws without a provider.
 * Nothing about the screens under test had changed; only which module
 * happened to be imported first.
 */

import { useQuery } from "./useQuery.ts";

/**
 * How often the engine's own health is re-read, in milliseconds.
 *
 * FIVE SECONDS, because that is the cadence the engine PUSHES at: the socket
 * ticks `{status, in_flight, shutting_down}` every five seconds, and the query
 * below fetches the rest of the same body. Read at any other interval the two
 * halves of one readout disagree by the difference — which is what the shell
 * and the screens that poll this separately do today at 15 seconds, so the
 * rail can say the engine is configured while a panel open in front of it says
 * it is not, for as long as fifteen seconds after a revision applied.
 */
export const HEALTH_POLL_MS = 5_000;

/**
 * The engine's own health: ONE read, shared by everything that shows it.
 *
 * `stream` rather than `health` because a query name may never collide with a
 * push kind, and the query answers the whole body where the push carries three
 * fields of it — including `event_history_seconds`, the read floor three
 * screens used to restate as literal copy.
 */
export function useEngineHealth() {
  return useQuery("stream", undefined, { pollMs: HEALTH_POLL_MS });
}
