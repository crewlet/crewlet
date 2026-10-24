/**
 * A dashboard's board and every dataset on it, kept current as each dataset's
 * freshness says (D63).
 *
 * EVERY ROW IS READ THROUGH THE ENGINE, AS THIS READER (ADR-0043): the board
 * carries none, and each dataset is asked of the datasets route, which decides
 * it for whoever is signed in. Nothing here derives one viewer's rows from
 * another's, and no dataset is computed from the rows of the store copy this
 * tab holds: a store dataset is read again when the room says its collection
 * moved, so the read rule that hides a row from this reader is the engine's.
 *
 * WHEN A DATASET IS READ AGAIN:
 *
 *  - `live` (a store collection): when a push names the collection, gathered
 *    over a short window so an agent writing ten rows costs one read.
 *  - `signal` (an engine source whose domain announces change): when the
 *    socket's `agents` or `tokens` slice moves, at most once a signal window,
 *    and on its interval where it declares one.
 *  - `interval`: on the interval it declares, never under a minute.
 *  - `manual`: when the board opens, a filter changes, or the reader refreshes.
 *  - `fixed`: with the version, never again.
 *  - `derived` (a calculation): whenever any dataset it stands on is read.
 *
 * A read the engine's budget turned away (`429 read_budget`) is asked again
 * once the wait it named is over, keeping the rows already drawn.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { RestError } from "~/protocol/index.ts";
import type { DashboardBoardAnswer, DashboardBoardDataset } from "~/protocol/index.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { useAgents, useTokens } from "~/lib/store-hooks.ts";
import { readBoard, readDataset } from "./api.ts";
import type { ArtifactDataStore } from "./datastore.ts";
import { ROWS_UNREAD, intervalMs, withDependents, type DatasetState } from "./dashboard.ts";

/** How long pushes naming a live dataset's collection are gathered before it is read again. */
export const LIVE_GATHER_MS = 500;

/**
 * The shortest time between two reads a socket signal triggers: an `agents`
 * push arrives with every turn a seat takes, and the engine answers a viewer's
 * source from its cache for thirty seconds anyway.
 */
export const SIGNAL_WINDOW_MS = 15_000;

export interface BoardData {
  board: RestResult<DashboardBoardAnswer>;
  /** The board's filters as this reader set them, every one at its default until changed. */
  params: Readonly<Record<string, string>>;
  setParam: (key: string, value: string) => void;
  /** Every dataset this tab has asked for, by id. */
  datasets: ReadonlyMap<string, DatasetState>;
  /** Reads these datasets again, with every dataset standing on them; all of them when none are named. */
  refresh: (ids?: Iterable<string>) => void;
}

export interface BoardDataOptions {
  /** Overrides [LIVE_GATHER_MS], for a suite. */
  gatherMs?: number;
  /** Overrides [SIGNAL_WINDOW_MS], for a suite. */
  signalMs?: number;
}

export function useBoardData(
  id: string,
  version: number,
  store: ArtifactDataStore | null,
  options: BoardDataOptions = {},
): BoardData {
  const gatherMs = options.gatherMs ?? LIVE_GATHER_MS;
  const signalMs = options.signalMs ?? SIGNAL_WINDOW_MS;
  const board = useRest<DashboardBoardAnswer>(
    version > 0 ? `board:${id}:${version}` : null,
    (signal) => readBoard(id, version, signal),
  );
  const answer = board.data;
  const datasets = useMemo<readonly DashboardBoardDataset[]>(
    () => answer?.datasets ?? [],
    [answer],
  );

  const [chosen, setChosen] = useState<Record<string, string>>({});
  const params = useMemo(() => {
    const out: Record<string, string> = {};
    for (const p of answer?.board.params ?? []) out[p.key] = chosen[p.key] ?? p.default;
    return out;
  }, [answer, chosen]);
  const setParam = useCallback(
    (key: string, value: string) => setChosen((c) => ({ ...c, [key]: value })),
    [],
  );

  const [states, setStates] = useState<ReadonlyMap<string, DatasetState>>(new Map());
  const generation = useRef(new Map<string, number>());
  const controllers = useRef(new Map<string, AbortController>());
  const retries = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const live = useRef({ id, version, params });
  live.current = { id, version, params };

  const patch = useCallback((dataset: string, change: (s: DatasetState) => DatasetState) => {
    setStates((prev) => {
      const next = new Map(prev);
      next.set(
        dataset,
        change(prev.get(dataset) ?? { answer: null, loading: false, failure: null, received: 0 }),
      );
      return next;
    });
  }, []);

  const readOne = useCallback(
    (dataset: string) => {
      const mine = (generation.current.get(dataset) ?? 0) + 1;
      generation.current.set(dataset, mine);
      controllers.current.get(dataset)?.abort();
      clearTimeout(retries.current.get(dataset));
      const controller = new AbortController();
      controllers.current.set(dataset, controller);
      patch(dataset, (s) => ({ ...s, loading: true }));
      const { id: artifact, version: n, params: sent } = live.current;
      readDataset(artifact, n, dataset, sent, controller.signal).then(
        (got) => {
          if (generation.current.get(dataset) !== mine) return;
          patch(dataset, () => ({
            answer: got,
            loading: false,
            failure: null,
            received: Date.now(),
          }));
        },
        (err: unknown) => {
          if (generation.current.get(dataset) !== mine || controller.signal.aborted) return;
          if (err instanceof RestError && err.status === 429 && err.code === "read_budget") {
            // THE BUDGET REFILLS: keep what is drawn and ask again once it
            // has, by the wait the engine named.
            const wait = Math.max(err.retryAfterSeconds ?? 1, 1) * 1000;
            retries.current.set(
              dataset,
              setTimeout(() => readOne(dataset), wait),
            );
            patch(dataset, (s) => ({ ...s, loading: false }));
            return;
          }
          patch(dataset, (s) => ({ ...s, loading: false, failure: ROWS_UNREAD }));
        },
      );
    },
    [patch],
  );

  const refresh = useCallback(
    (ids?: Iterable<string>) => {
      const all = datasets.map((d) => d.id);
      for (const d of ids ? withDependents(datasets, ids) : all) readOne(d);
    },
    [datasets, readOne],
  );

  // THE BOARD OPENS, OR A FILTER MOVES: every dataset is read.
  useEffect(() => {
    if (datasets.length > 0) refresh();
  }, [datasets, params, refresh]);

  // LIVE: pushes naming a dataset's collection, gathered.
  const pending = useRef(new Set<string>());
  const gather = useRef<ReturnType<typeof setTimeout> | 0>(0);
  useEffect(() => {
    if (!store) return;
    const liveSets = datasets.filter((d) => d.freshness.mode === "live");
    if (liveSets.length === 0) return;
    const off = store.onTouch((touch) => {
      for (const d of liveSets) {
        if (touch.all || touch.collections.has(d.freshness.collection ?? ""))
          pending.current.add(d.id);
      }
      if (pending.current.size === 0 || gather.current) return;
      gather.current = setTimeout(() => {
        gather.current = 0;
        const ids = [...pending.current];
        pending.current.clear();
        refresh(ids);
      }, gatherMs);
    });
    return () => {
      off();
      clearTimeout(gather.current);
      gather.current = 0;
    };
  }, [store, datasets, refresh, gatherMs]);

  // SIGNALS: the socket's slices moving, at most once a window.
  const agents = useAgents();
  const tokens = useTokens();
  useSignal("agents", agents, datasets, refresh, signalMs);
  useSignal("tokens", tokens, datasets, refresh, signalMs);

  // INTERVALS, each its own.
  useEffect(() => {
    const timers = datasets
      .filter((d) => intervalMs(d.freshness) > 0)
      .map((d) => setInterval(() => refresh([d.id]), intervalMs(d.freshness)));
    return () => timers.forEach(clearInterval);
  }, [datasets, refresh]);

  // AND NOTHING OUTLIVES THE BOARD.
  useEffect(() => {
    const open = controllers.current;
    const waits = retries.current;
    return () => {
      for (const c of open.values()) c.abort();
      for (const t of waits.values()) clearTimeout(t);
    };
  }, []);

  return { board, params, setParam, datasets: states, refresh };
}

/** Reads the datasets a socket signal names again when its slice moves, at most once a window. */
function useSignal(
  signal: "agents" | "tokens",
  slice: unknown,
  datasets: readonly DashboardBoardDataset[],
  refresh: (ids: Iterable<string>) => void,
  windowMs: number,
): void {
  const seen = useRef(slice);
  const last = useRef(0);
  const later = useRef<ReturnType<typeof setTimeout> | 0>(0);
  useEffect(() => {
    if (seen.current === slice) return;
    seen.current = slice;
    const ids = datasets
      .filter((d) => d.freshness.mode === "signal" && d.freshness.signal === signal)
      .map((d) => d.id);
    if (ids.length === 0) return;
    const wait = last.current + windowMs - Date.now();
    if (wait <= 0) {
      last.current = Date.now();
      refresh(ids);
      return;
    }
    if (later.current) return;
    later.current = setTimeout(() => {
      later.current = 0;
      last.current = Date.now();
      refresh(ids);
    }, wait);
  }, [slice, signal, datasets, refresh, windowMs]);
  useEffect(() => () => clearTimeout(later.current), []);
}
