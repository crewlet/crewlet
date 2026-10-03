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
import type { EngineHealth } from "~/contract/health.ts";
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

/**
 * How many org projections have been pushed — the signal a chart read is
 * asked again on (`lib/chartReads.ts`) and the org builder checks its draft
 * again on, which `useOrg`'s identity is not: an org push deep-equal to the
 * last is shared by the store and moves nothing, and it still says a chart
 * write or a settings revision landed. See `StoreState.orgPushes`.
 */
export function useOrgPushes() {
  return useSlice(["orgPushes"], (s) => s.orgPushes);
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
 * the other way to get a seat's schedules: the org builder's `schedulesOf`
 * (`routes/org/builder/editorForm.ts`) reads the `schedules:` a seat AUTHORED
 * in its runtime half on the org chart, which only a `config:read` reader is
 * served, and carries name, cron and task. A row here carries the effective
 * timezone, the engine's own `next_run`, the `runners` a fire actually
 * reaches, and `problem` when a cron or a zone cannot be read — and it is
 * pushed to every reader holding `state:read`.
 *
 * WHAT IT DOES NOT CARRY is the run ledger. `recent_runs` arrives only on the
 * `schedules` QUESTION, so `routes/agents/Schedules.tsx` polls that
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
 * Four states, not two, because the repair differs and a reader cannot guess
 * which one they are looking at: connected, unreachable (comes back on its
 * own), a refused credential (the reader supplies another), and access
 * refused to a credential the engine accepts (only an administrator repairs
 * it).
 */
export function useConnection() {
  return useSlice(["health"], (s) => ({
    connected: s.connected,
    authRejected: s.authRejected,
    accessRefused: s.accessRefused,
    identityUnverifiable: s.identityUnverifiable,
    health: s.health,
  }));
}

/**
 * The engine's own health: the `health` push, `api.Health` WHOLE — or null
 * while it is not known (the socket is down, or no frame has arrived yet).
 *
 * READ FROM THE SLICE, never asked for. The push used to carry three fields
 * while a `stream` query answered the rest, and five places polled that query
 * at 5 s and 15 s of their own — so the sidebar could say a revision had applied
 * while the panel in front of it said it had not. The snapshot and every
 * five-second tick carry the whole body now, and there is no query to ask.
 */
export function useEngineHealth(): EngineHealth | null {
  return useSlice(["health"], (s) => (s.health.status === "unknown" ? null : s.health));
}

export type { QueryMap, QueryName };
