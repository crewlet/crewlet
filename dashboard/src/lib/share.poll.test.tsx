/**
 * A poll draws the rows it changed, and only those.
 *
 * Every list in this product is a [DataGrid] fed from `useQuery` or
 * `useRestRead`, and every one of them is polled. A poll used to hand the
 * grid an answer parsed afresh off the wire — new objects for every row,
 * whatever the answer said — so the memoised rows drew again on every poll,
 * all of them: two hundred turns every twenty seconds, three hundred and fifty
 * audit rows every minute. These cases mount the real grid under each of the
 * two hooks, count the rows a cell actually draws, and hold the poll to the
 * rows that moved.
 */

import { useMemo } from "react";
import { act, cleanup, render } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "./store-hooks.ts";
import { useQuery } from "./useQuery.ts";
import { useRestRead } from "./restRead.ts";
import { useShared } from "./share.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { TurnRow } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const POLL_MS = 20_000;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  location.hash = "#/";
  reset();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

/** Rows drawn, by key. */
const drawn = { rows: [] as string[] };

function reset() {
  drawn.rows = [];
}

function Counted({ id, text }: { id: string; text: string }) {
  drawn.rows.push(id);
  return <>{text}</>;
}

/** THE SCREEN'S OWN COLUMNS, held as every screen must hold them: memoised. */
function useColumns(): GridColumn<TurnRow>[] {
  return useMemo(
    () => [
      {
        key: "summary",
        header: "What it did",
        cell: (t) => <Counted id={t.turn_id} text={t.summary ?? ""} />,
      },
    ],
    [],
  );
}

function turns(n: number, changed: Record<number, string> = {}): TurnRow[] {
  return Array.from({ length: n }, (_, i) => ({
    turn_id: `t-${i}`,
    started_at: "2026-09-30T12:00:00Z",
    ended_at: "2026-09-30T12:00:05Z",
    duration_ms: 5000,
    complete: true,
    phases: 2,
    iterations: 1,
    failed: false,
    input_tokens: 1,
    output_tokens: 1,
    total_tokens: 2,
    summary: changed[i] ?? `summary ${i}`,
  }));
}

/** What the engine answers now — handed over as a fresh parse each time, as the wire does. */
let answer: TurnRow[] = [];
const wire = () => JSON.parse(JSON.stringify({ turns: answer })) as { turns: TurnRow[] };

function QueryScreen() {
  const list = useQuery("turns", { limit: 50 }, { pollMs: POLL_MS });
  const columns = useColumns();
  return (
    <DataGrid<TurnRow> rows={list.data?.turns ?? []} rowKey={(t) => t.turn_id} columns={columns} />
  );
}

function RestScreen() {
  const read = useRestRead("/turns", async () => wire(), { cadence: () => POLL_MS });
  const columns = useColumns();
  return (
    <DataGrid<TurnRow> rows={read.data?.turns ?? []} rowKey={(t) => t.turn_id} columns={columns} />
  );
}

/** A screen that BUILDS its rows from the answer rather than handing them on. */
function DerivingScreen() {
  const list = useQuery("turns", { limit: 50 }, { pollMs: POLL_MS });
  const columns = useColumns();
  const rows = useShared(
    useMemo(
      () => (list.data?.turns ?? []).map((t) => ({ ...t, summary: `· ${t.summary}` })),
      [list.data],
    ),
  );
  return <DataGrid<TurnRow> rows={rows} rowKey={(t) => t.turn_id} columns={columns} />;
}

async function mount(screen: React.ReactNode) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = async () => wire();
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{screen}</Router>
    </ClientContext.Provider>,
  );
  await poll(0);
}

async function poll(ms = POLL_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

for (const [name, screen] of [
  ["useQuery", <QueryScreen key="q" />],
  ["useRestRead", <RestScreen key="r" />],
  ["a screen's own rows (useShared)", <DerivingScreen key="d" />],
] as const) {
  test(`${name}: a poll whose answer is unchanged redraws no row`, async () => {
    answer = turns(50);
    await mount(screen);
    expect(drawn.rows).toHaveLength(50);
    reset();
    await poll();
    expect(drawn.rows).toEqual([]);
  });

  test(`${name}: a poll that changed one row redraws only that row`, async () => {
    answer = turns(50);
    await mount(screen);
    reset();
    answer = turns(50, { 17: "changed" });
    await poll();
    expect(drawn.rows).toEqual(["t-17"]);
  });

  // A FEED IS NEWEST FIRST: a new row at the top moves every other row down a
  // place, and only the new one has anything new to draw.
  test(`${name}: a poll that added a row at the top draws only that row`, async () => {
    answer = turns(50);
    await mount(screen);
    reset();
    answer = [{ ...turns(1)[0]!, turn_id: "t-new", summary: "new" }, ...turns(50)];
    await poll();
    expect(drawn.rows).toEqual(["t-new"]);
  });
}
