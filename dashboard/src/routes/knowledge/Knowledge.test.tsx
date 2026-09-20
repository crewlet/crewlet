/**
 * Whether a container's page list saw the whole container.
 *
 * The peek derives one fact from that read alone — "Last written", the newest
 * `updated_at` among the rows it got — and the engine orders a page list by
 * container and TITLE, so a read that did not reach the end is an alphabetical
 * slice and its newest row is a FLOOR on the real answer rather than the
 * answer. The note beside the date is what says so.
 *
 * It used to be INFERRED, `pages.length >= limit`, which is the reading this
 * tree rejects everywhere else: a container holding exactly the limit holds
 * every row it has, and the inference put "newest of the pages this read
 * returned" on it anyway — a caution about missing data, stated as a fact,
 * over a complete answer. The engine takes one row past the limit as evidence
 * and answers `truncated`; this is the client reading it.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { ContainerPeek } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { QueryName } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** A FULL PAGE of rows — the case the old inference got wrong. */
const LIMIT = 3;
const rows = Array.from({ length: LIMIT }, (_, i) => ({
  id: `p-${i}`,
  container: "LEAD",
  title: `Page ${i}`,
  status: "published",
  version: 1,
  author: "agent-ceo",
  updated_at: "2026-09-19T12:00:00Z",
}));

/**
 * The container peek over a socket answering `containers` and `pages`.
 *
 * A REAL PROVIDER rather than a module mock of `~/lib/store-hooks.ts`, for
 * the reason `Goals.test.tsx` records: this screen reaches that module
 * through `useQuery`'s relative import, so an aliased mock is a second
 * instance and the component keeps the real `useClient`, which throws.
 */
function mount(truncated: boolean) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) => {
    switch (what as QueryName) {
      case "containers":
        return Promise.resolve({
          containers: [{ key: "LEAD", name: "Leadership", pages: LIMIT }],
        });
      case "pages":
        return Promise.resolve({ pages: rows, limit: LIMIT, offset: 0, truncated });
      default:
        return Promise.resolve({});
    }
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ContainerPeek id="LEAD" />
      </Router>
    </ClientContext.Provider>,
  );
}

const NOTE = /newest of the pages this read returned/i;

test("a full page the engine calls whole carries no caution", async () => {
  mount(false);
  await waitFor(() => expect(screen.getByText("Leadership")).toBeTruthy());
  expect(screen.queryByText(NOTE)).toBeNull();
});

test("a page the engine calls truncated says the date is only a floor", async () => {
  mount(true);
  await waitFor(() => expect(screen.getByText(NOTE)).toBeTruthy());
});
