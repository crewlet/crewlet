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
 *
 * # One read per tab, not one per surface
 *
 * `useQuery` is one poller PER CALL — it shares nothing between two hooks
 * asking the same question — so "the one read everything shares" was a
 * promise this module made and did not keep: the shell and the inbox each
 * polled `stream` every fifteen seconds while three screens polled it every
 * five, the org builder's notes on their own cadences beside them, and the rail
 * could say the engine was configured while a panel open in front of it said
 * it was not, for up to the difference. So the read is SHARED here: one poll
 * per socket, started by the first surface that reads it and stopped with the
 * last, and every surface is handed the same answer at the same moment.
 */

import { useSyncExternalStore } from "react";
import { useClient } from "./store-hooks.ts";
import {
  queryErrorCode,
  queryFailure,
  unavailableRetryMs,
  type LiveSocket,
  type LogRefusal,
  type QueryErrorCode,
  type QueryMap,
  type QueryRefusal,
  type Store,
} from "~/protocol/index.ts";

/**
 * How often the engine's own health is re-read, in milliseconds.
 *
 * FIVE SECONDS, because that is the cadence the engine PUSHES at: the socket
 * ticks `{status, in_flight, shutting_down}` every five seconds, and this read
 * fetches the rest of the same body. Read at any other interval the two halves
 * of one readout disagree by the difference. One read per tab at this cadence
 * is what the fifteen-second polls it replaced cost two of on the landing
 * screen alone.
 */
export const HEALTH_POLL_MS = 5_000;

/** The shared read's answer, in `useQuery`'s own shape. */
export interface EngineHealth {
  data: QueryMap["stream"] | null;
  /** True only before the FIRST answer: a poll keeps the last one on screen. */
  loading: boolean;
  error: QueryErrorCode | null;
  refusal: QueryRefusal | LogRefusal | null;
}

/**
 * The one poll of `stream` a socket has, for as long as anything reads it.
 *
 * Its failure handling is `useQuery`'s, stated once more because this is the
 * one question asked outside that hook: the last good answer is KEPT through a
 * failed ask, an `unavailable` answer is asked again when the engine's hint
 * says (`unavailableRetryMs`) in place of the next tick — and not on a timer
 * at all when the hint is zero, since asking every five seconds a node that
 * said waiting changes nothing is a loop — and a reconnect asks again, because
 * an answer from before it is an answer about an engine that has since moved.
 */
class SharedHealth {
  private snapshot: EngineHealth = { data: null, loading: true, error: null, refusal: null };
  private readonly readers = new Set<() => void>();
  private timer: ReturnType<typeof setTimeout> | 0 = 0;
  private generation = 0;
  private unwatch: (() => void) | null = null;
  private wasConnected = false;

  constructor(
    private readonly socket: LiveSocket,
    private readonly store: Store,
  ) {}

  readonly subscribe = (fn: () => void): (() => void) => {
    this.readers.add(fn);
    if (this.readers.size === 1) this.start();
    return () => {
      this.readers.delete(fn);
      if (this.readers.size === 0) this.stop();
    };
  };

  readonly get = (): EngineHealth => this.snapshot;

  /**
   * Ask now, for everybody reading — a surface that KNOWS the answer just
   * moved (a save it made is applying). It brings the shared poll forward
   * rather than starting a second one, and does nothing with nobody reading.
   */
  readonly refetch = (): void => {
    if (this.readers.size > 0) this.ask();
  };

  private start(): void {
    this.wasConnected = this.store.state.connected;
    this.unwatch = this.store.subscribe(["health"], () => {
      const connected = this.store.state.connected;
      if (connected && !this.wasConnected) this.ask();
      this.wasConnected = connected;
    });
    this.ask();
  }

  /**
   * Nobody reads it any more. THE LAST ANSWER IS KEPT, so the next surface to
   * read it draws what is known at once and replaces it when the poll that
   * mounting restarts comes back.
   */
  private stop(): void {
    this.generation++;
    clearTimeout(this.timer);
    this.timer = 0;
    this.unwatch?.();
    this.unwatch = null;
  }

  private ask(): void {
    const mine = ++this.generation;
    clearTimeout(this.timer);
    this.timer = 0;
    // When this is asked again: the next tick, unless the engine said
    // otherwise. `null` is never on a timer.
    let next: number | null = HEALTH_POLL_MS;
    this.socket
      .query("stream", {})
      .then(
        (data) => {
          if (this.generation !== mine) return;
          this.set({ data, loading: false, error: null, refusal: null });
        },
        (err: unknown) => {
          if (this.generation !== mine) return;
          const code = queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
          const { refusal } = queryFailure(err);
          if (code === "unavailable") next = unavailableRetryMs(refusal);
          this.set({ data: this.snapshot.data, loading: false, error: code, refusal });
        },
      )
      .finally(() => {
        if (this.generation !== mine || next === null) return;
        this.timer = setTimeout(() => this.ask(), next);
      });
  }

  private set(next: EngineHealth): void {
    this.snapshot = next;
    for (const fn of this.readers) fn();
  }
}

/** One shared read per socket, which is one per tab. */
const shared = new WeakMap<LiveSocket, SharedHealth>();

function sharedFor(socket: LiveSocket, store: Store): SharedHealth {
  let read = shared.get(socket);
  if (!read) {
    read = new SharedHealth(socket, store);
    shared.set(socket, read);
  }
  return read;
}

/**
 * The engine's own health: ONE read, shared by everything that shows it.
 *
 * `stream` rather than `health` because a query name may never collide with a
 * push kind, and the query answers the whole body where the push carries three
 * fields of it — including `event_history_seconds`, the read floor three
 * screens used to restate as literal copy, and `configured`, which the rail
 * and the inbox both draw.
 */
export function useEngineHealth(): EngineHealth & { refetch: () => void } {
  const { socket, store } = useClient();
  const read = sharedFor(socket, store);
  const state = useSyncExternalStore(read.subscribe, read.get, read.get);
  return { ...state, refetch: read.refetch };
}
