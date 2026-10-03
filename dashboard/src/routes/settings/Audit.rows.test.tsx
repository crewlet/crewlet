/**
 * The audit draws the entries that changed, and no others.
 *
 * Six sources, each polled every minute, folded into one shape this screen
 * builds — so every answer from any of them made every entry a new object, and
 * every poll drew all of them: 350 rows in 103–375 ms under the development
 * build for a poll that changed nothing, and the same for one that changed a
 * single tracker commit. These cases count the rows the screen's own cells
 * draw ([countingGrid]).
 */

import { act, cleanup, render } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

vi.mock("~/app/frame/DataGrid.tsx", async (importOriginal) => {
  const { countingGrid } = await import("~/test/rowsDrawn.tsx");
  return countingGrid(importOriginal);
});

import { drawnRows } from "~/test/rowsDrawn.tsx";
import { Audit } from "./Audit.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The audit's own poll — `Audit.tsx`'s. */
const POLL_MS = 60_000;
const NOW = Date.parse("2026-09-30T12:00:00Z");

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date"] });
  vi.setSystemTime(NOW);
  location.hash = "#/settings/audit";
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ secrets: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    ),
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

/** One tracker commit, a minute apart, newest first. */
function commit(i: number, excerpt = `edit ${i}`) {
  const at = new Date(NOW - (i + 1) * 60_000).toISOString();
  return {
    id: `h-${i}`,
    log_seq: 1_000 - i,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at,
    effective_at: at,
    kind: "updated",
    actor: "U0FOUNDER",
    actor_kind: "operator",
    subject_kind: "task",
    subject_id: `t-${i}`,
    subject_key: `ENG-${i}`,
    excerpt,
    notified: false,
  };
}

/** What each source answers now, handed over as a fresh parse each time. */
let records: ReturnType<typeof commit>[] = [];
const pages = Array.from({ length: 20 }, (_, i) => ({
  id: `p-${i}`,
  page_id: "pg-1",
  kind: "saved",
  actor: "ada",
  actor_kind: "human",
  at: new Date(NOW - (i + 1) * 90_000).toISOString(),
  log_seq: 100 - i,
  title: "Deploy runbook",
  container: "ENG",
  excerpt: `page edit ${i}`,
}));

async function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) =>
    JSON.parse(
      JSON.stringify(
        what === "work_activity"
          ? { records, complete: true }
          : what === "page_activity"
            ? { changes: pages, complete: true }
            : what === "config_audit"
              ? []
              : {},
      ),
    ) as unknown;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Audit />
      </Router>
    </ClientContext.Provider>,
  );
  await poll(0);
  await poll(0);
}

async function poll(ms = POLL_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

test("a poll whose answers are unchanged draws no entry", async () => {
  records = Array.from({ length: 40 }, (_, i) => commit(i));
  await mount();
  expect(drawnRows()).toHaveLength(60);
  await poll();
  expect(drawnRows()).toEqual([]);
});

test("a poll that changed one tracker commit draws that entry and no other", async () => {
  records = Array.from({ length: 40 }, (_, i) => commit(i));
  await mount();
  drawnRows();
  records = records.map((r, i) => (i === 7 ? commit(7, "changed") : r));
  await poll();
  expect(drawnRows()).toEqual(["work:h-7"]);
});
