/**
 * Binding the projection mirror to React.
 *
 * Two rules this file exists to keep:
 *
 *  1. **A component subscribes to the SLICES it reads, and no others.** An
 *     `agents` overlay is pushed twice per tool-loop round, so a screen that
 *     woke on every envelope would re-render several times a second for the
 *     length of a turn. The previous dashboard did exactly that — one screen
 *     declared seven slices and read three, another declared two and read
 *     neither, and three declared a slice (`connected`) that was never emitted
 *     at all, so those subscriptions were simply dead.
 *
 *  2. **Nothing here derives state.** `useSyncExternalStore` compares
 *     snapshots by identity; the store mutates its slices in place, so the
 *     per-slice VERSION counter is the snapshot. A hook that returned a
 *     freshly-computed object would re-render on every tick forever.
 */

import { createContext, useCallback, useContext, useMemo, useSyncExternalStore } from "react";
import { useQuery } from "./useQuery.ts";
import type {
  Slice,
  StoreState,
  Store,
  LiveSocket,
  QueryMap,
  QueryName,
} from "~/protocol/index.ts";

export interface Client {
  store: Store;
  socket: LiveSocket;
}

export const ClientContext = createContext<Client | null>(null);

export function useClient(): Client {
  const client = useContext(ClientContext);
  if (!client) throw new Error("useClient outside a ClientContext provider");
  return client;
}

/**
 * Read the store, re-rendering only when one of `slices` moves.
 *
 * `select` runs on every render, not only on a change: it is a plain read off
 * the mutable state, and memoising it against the version counter would be a
 * second cache to keep correct for no measured gain.
 */
export function useSlice<T>(slices: readonly Slice[], select: (state: StoreState) => T): T {
  const { store } = useClient();
  const key = slices.join("|");
  const subscribe = useCallback(
    (fn: () => void) => store.subscribe(slices, fn),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [store, key],
  );
  const version = useSyncExternalStore(
    subscribe,
    () => slices.reduce((n, s) => n + store.version(s), 0),
    () => 0,
  );
  return useMemo(
    () => select(store.state),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [store, version, select],
  );
}

/** The seat roster, live overlay merged. */
export function useAgents() {
  return useSlice(["agents"], (s) => s.agents);
}

export function useSandboxes() {
  return useSlice(["sandboxes"], (s) => s.sandboxes);
}

export function useEvents() {
  return useSlice(["events"], (s) => s.events);
}

/**
 * The phases that COMPLETED while this tab was watching, payload and all.
 *
 * Its own slice, not a read off `events`: those rows are payload-free, and a
 * phase without its payload has no prompts, no response, no tool calls and no
 * decision. A screen merges these over the history its query answered at mount,
 * which is what lets a live phase become a finished one in place instead of
 * disappearing with the `live_call` the projection clears.
 */
export function usePhaseEvents() {
  return useSlice(["phases"], (s) => s.phases);
}

/**
 * How many completed phases this tab has DROPPED since it opened, for a screen
 * that draws [usePhaseEvents] with no query of its own behind it — for which
 * any drop at all may be one of its phases.
 */
export function usePhasesDropped(): number {
  return useSlice(["phases"], (s) => s.phasesDropped);
}

/**
 * Whether this tab has DROPPED a completed phase that arrived after `answer`
 * first rendered — the one loss a screen merging [usePhaseEvents] over its own
 * query answer cannot see in what it holds.
 *
 * The answer covers what completed before it; the slice supplies what
 * completed after, until [MAX_PHASES] pushes it out. `phasesDropped` passing
 * the arrival count noted with the answer means one of those later phases is
 * gone. It may have been another seat's, since the slice is company-wide, so a
 * screen says it MAY be missing one. The phase itself is in the event store,
 * which the screen's own query reads when it is asked again.
 *
 * The mark is taken when the answer renders, so a phase that streamed in while
 * the query was in flight, and is not in its answer, counts as covered.
 */
export function usePhasesDroppedSince(answer: unknown): boolean {
  const { store } = useClient();
  const dropped = usePhasesDropped();
  // Keyed on the answer's IDENTITY, so a poll or a refetch that brings a new
  // answer moves the mark forward.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const mark = useMemo(() => store.state.phaseArrivals, [store, answer]);
  return dropped > mark;
}

export function useOrg() {
  return useSlice(["org"], (s) => s.org);
}

export function useTools() {
  return useSlice(["tools"], (s) => s.tools);
}

export function useTokens() {
  return useSlice(["tokens"], (s) => s.tokens);
}

/**
 * The company's resolved schedules, as the handshake and every config apply
 * push them.
 *
 * THE SLICE HAD NO ACCESSOR. `Store` declares it, lists it, fills it from the
 * snapshot AND from the push, and emits on it — and nothing could read it, so
 * the one screen that wanted schedules asked the engine instead and paid a
 * round trip for rows this client already held, kept fresh on every apply.
 *
 * These are the RESOLVED rows, which is the distinction that matters against
 * the other way to get a seat's schedules: `lib/seats.ts`'s `schedulesOf`
 * reads the `schedules:` a seat AUTHORED out of the company document, so it
 * is operator-gated and carries name, cron and task. A row here carries the
 * effective timezone, the engine's own `next_run`, the `runners` a fire
 * actually reaches, and `problem` when a cron or a zone cannot be read — and
 * it is pushed to every reader, token or not.
 *
 * WHAT IT DOES NOT CARRY is the run ledger. `recent_runs` arrives only on the
 * `schedules` QUESTION, so `routes/activity/Schedules.tsx` polls that
 * deliberately and must not be moved onto this hook: it would silently lose
 * the fires. This is for a reader that wants the schedules and nothing else.
 */
export function useSchedules() {
  return useSlice(["schedules"], (s) => s.schedules ?? []);
}

export function useOrgBudget() {
  return useSlice(["budget"], (s) => s.budget);
}

/**
 * Connection posture.
 *
 * Three states, not two, because the repair differs and a reader cannot guess
 * which one they are looking at: connected, unreachable (comes back on its
 * own), and refused (never does).
 */
export function useConnection() {
  return useSlice(["health"], (s) => ({
    connected: s.connected,
    authRejected: s.authRejected,
    health: s.health,
  }));
}

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

export type { QueryMap, QueryName };
