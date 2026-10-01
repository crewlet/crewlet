/**
 * The work list draws the rows that changed, and no others.
 *
 * The list is polled every twenty seconds and renders for a dozen reasons of
 * its own — a keystroke in the filter box, an inbox push, the catalogue
 * landing. Its column list WAS memoised, on a context built from the screen's
 * `chrome` — an object literal, new on every render — and from a lookup of the
 * page's ids, rebuilt from the rows on every poll. So every render and every
 * poll that moved anything drew every row of the list. These cases count the
 * rows the screen's own cells draw ([countingGrid]).
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

vi.mock("~/app/frame/DataGrid.tsx", async (importOriginal) => {
  const { countingGrid } = await import("~/test/rowsDrawn.tsx");
  return countingGrid(importOriginal);
});

import { drawnRows } from "~/test/rowsDrawn.tsx";
import { Work } from "./Work.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { WorkSummary } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The list's own poll — `ItemsView.tsx`'s. */
const POLL_MS = 20_000;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  location.hash = "#/work?shape=table";
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

function item(i: number, title = `task ${i}`): WorkSummary {
  return {
    id: `id-${i}`,
    key: `ENG-${i}`,
    project: "ENG",
    title,
    type: "task",
    status: "todo",
    assignee: "ada",
    updated: "2026-09-30T12:00:00Z",
  } as WorkSummary;
}

/** What the tracker answers now — handed over as a fresh parse, as the wire does. */
let items: WorkSummary[] = [];

async function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) =>
    JSON.parse(
      JSON.stringify(
        what === "work_items"
          ? { items, groups: [], total: items.length, complete: true }
          : what === "work_projects"
            ? { projects: [], total: 0, complete: true }
            : {},
      ),
    ) as unknown;
  store.applyOrg({ name: "Acme", roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }] });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Work />
      </Router>
    </ClientContext.Provider>,
  );
  await poll(0);
  return store;
}

async function poll(ms = POLL_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

// A POLL RENDERS THE SCREEN, which is what made every row draw: the context the
// columns are built from was a literal made on that render, and its id lookup
// was rebuilt from the rows that poll replaced.
test("a poll that changed one item draws that item and no other", async () => {
  items = Array.from({ length: 30 }, (_, i) => item(i));
  await mount();
  expect(drawnRows()).toHaveLength(30);
  await poll();
  expect(drawnRows()).toEqual([]);
  items = items.map((row, i) => (i === 4 ? { ...row, status: "in_progress" } : row));
  await poll();
  expect(drawnRows()).toEqual(["id-4"]);
});
