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

export function useOrg() {
  return useSlice(["org"], (s) => s.org);
}

export function useTools() {
  return useSlice(["tools"], (s) => s.tools);
}

export function useTokens() {
  return useSlice(["tokens"], (s) => s.tokens);
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
