/**
 * Which window the spend figures are OF.
 *
 * The screen has two sources for one breakdown: the rollup the projection
 * pushes, which covers the engine's live window, and a `tokens` query asked for
 * whatever the reader scrubbed to. The badge, the picker and the chart all name
 * the chosen window, so the figures under them have to be that window's or
 * nothing — the whole reason the breakdown stopped taking a day count is that a
 * chart over March with tiles from this afternoon is two facts on one screen
 * that cannot be compared.
 *
 * Which leaves three states to keep apart, and a fallback that used to collapse
 * two of them: the answer has not come back YET, the answer is not coming, and
 * here it is.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Spend } from "./Spend.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { Bucket, Rollup, TurnRow } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  // A WINDOW THE PUSHED ROLLUP DOES NOT COVER, which is what makes the query
  // the only honest source: `1d` is the live window and takes neither branch.
  location.hash = "#/cost?window=30d";
});

afterEach(() => {
  cleanup();
  location.hash = "";
  vi.unstubAllGlobals();
});

function bucket(total: number): Bucket {
  return {
    input_tokens: total,
    output_tokens: 0,
    total_tokens: total,
    calls: 3,
  };
}

function rollup(total: number): Rollup {
  return {
    since: "2026-09-12T10:00:00Z",
    until: "2026-09-13T10:00:00Z",
    totals: bucket(total),
    by_phase: [],
    by_model: [],
    by_provider: [],
    by_worker: [],
    by_agent: [],
    by_turn: [],
    aggregated_through: "2026-09-13T10:00:00Z",
  };
}

/** The live figure, which must never appear under a 30-day heading. */
const LIVE = "5,000 exactly";

/** Every question the screen asked, with its parameters. */
let asks: { what: string; params: unknown }[] = [];

/** Mount the screen with a pushed rollup, one stubbed `tokens` answer and,
 *  optionally, one `turns` answer. */
function mount(asked: () => Promise<unknown>, turns?: () => Promise<unknown>) {
  asks = [];
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string, params: unknown) => Promise<unknown> }).query = (
    what: string,
    params: unknown,
  ) => {
    asks.push({ what, params });
    if (what === "tokens") return asked();
    if (what === "turns" && turns) return turns();
    return new Promise(() => {});
  };
  store.applyTokens(rollup(5_000));
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Spend />
      </Router>
    </ClientContext.Provider>,
  );
}

/** Let the stubbed answer settle without leaving act() warnings behind. */
async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

// THE CONTROL: a window that answered shows ITS figures.
test("a window that answered is what the tiles are of", async () => {
  mount(() => Promise.resolve(rollup(41_000)));
  await settle();
  expect(screen.getByText("41,000 exactly")).toBeTruthy();
  expect(screen.queryByText(LIVE)).toBeNull();
});

// AND THE OTHER CONTROL: the loading fallback is deliberate and stays.
//
// Blanking the whole screen for the moment between scrubbing the picker and
// the answer landing is worse than holding the last figures — but it is only
// defensible while an answer is still coming.
test("the live rollup holds the screen while the window's answer is in flight", () => {
  mount(() => new Promise(() => {}));
  expect(screen.getByText(LIVE)).toBeTruthy();
});

// THE DEFECT: a refusal is not an answer, and must not be dressed as one.
//
// `useQuery` keeps `data` null on a first-load failure, so the fallback served
// the 24-hour pushed rollup to four stat tiles, two bar lists and two tables —
// under a badge, a picker and a chart all saying "30 days", with the only
// error on screen belonging to the chart's own series.
test("a window whose answer was refused shows the refusal, not the live rollup", async () => {
  mount(() => Promise.reject(new Error("timeout")));
  await settle();
  expect(screen.getByText(/did not answer within 10 seconds/)).toBeTruthy();
  expect(screen.queryByText(LIVE)).toBeNull();
  // The heading stays honest about what was asked for; it is the FIGURES that
  // are absent, and an em dash is how this screen says "not this window's".
  expect(screen.getByText("30 days")).toBeTruthy();
  expect(screen.getByText("nothing recorded")).toBeTruthy();
});

/**
 * A RETRIED TRIGGER'S TWO BILLS SAY THEY ARE ONE PIECE OF WORK.
 *
 * A turn id names one RUN (`adr/0017`), so a turn that failed without
 * reaching outside the engine and was redelivered spends twice — and each row
 * is a real bill, so summing them would charge one turn with another's
 * tokens. Unmarked, though, an expensive-looking pair reads as the company
 * having paid for the work twice, which is exactly the conclusion this table
 * invites when it is scanned for what cost the most.
 */
test("two runs of one trigger are two rows, each marked a re-run", async () => {
  const turn = (id: string, key: string | undefined, total: number): TurnRow => ({
    turn_id: id,
    work_key: key,
    role: "CEO",
    started_at: "2026-09-13T09:00:00Z",
    ended_at: "2026-09-13T09:01:00Z",
    duration_ms: 60_000,
    complete: true,
    parked: false,
    phases: 2,
    iterations: 1,
    failed: false,
    input_tokens: total,
    output_tokens: 0,
    total_tokens: total,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
  });
  mount(
    () => Promise.resolve(rollup(0)),
    () =>
      Promise.resolve({
        turns: [
          turn("run-1", "wk-1", 10),
          turn("run-2", "wk-1", 327_000),
          // A turn that ran once, and one with no trigger key at all: neither
          // is a re-run, and grouping the keyless ones together would report
          // every unledgered turn as an attempt at every other.
          turn("run-3", "wk-2", 5),
          turn("run-4", undefined, 5),
          turn("run-5", undefined, 5),
        ],
        next: null,
        coverage: { nodes: [], complete: true },
      }),
  );

  await screen.findByText("run-1".slice(0, 8));
  expect(screen.getAllByText("re-run")).toHaveLength(2);
});

/**
 * A NAMED WINDOW IS COMPANY DAYS, asked as `days`.
 *
 * The usage domain holds whole company days, cut on the company's clock, so the
 * screen names the window by its length and lets the engine cut it — never two
 * instants this browser subtracted on its own clock, which named a different
 * week from the one the engine would. And the per-turn table is the fleet's
 * turn list ranked by tokens, since a company day holds no turn.
 */
test("the breakdown, the chart and the turn table all ask for the window's company days", async () => {
  mount(() => Promise.resolve(rollup(41_000)));
  await settle();
  const params = (what: string) =>
    asks.filter((a) => a.what === what).map((a) => a.params as Record<string, unknown>);
  expect(params("tokens")).toEqual([{ days: 30 }]);
  expect(params("token_series")[0]).toMatchObject({ days: 30, bucket: "day", group: "phase" });
  expect(params("turns")).toEqual([{ days: 30, sort: "-tokens", limit: 50 }]);
  for (const ask of asks) {
    expect(ask.params).not.toHaveProperty("since");
    expect(ask.params).not.toHaveProperty("until");
  }
});
