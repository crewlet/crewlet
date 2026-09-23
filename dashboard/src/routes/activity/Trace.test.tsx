/**
 * Where a trace's "In the log" goes.
 *
 * The event log's search box matches a row's summary, type and source, and a
 * trace id is none of those — so a trace searched for there is a log filtered
 * to nothing, which reads as a trace with no events. The log narrows by trace
 * on the server, and a capped trace's newer rows are there, on a window old
 * enough to hold them.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { TraceScreen } from "./Trace.tsx";
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

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

test("'In the log' opens the log on this trace, over the whole log", async () => {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ trace_id: "abc123", events: [], truncated: false });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TraceScreen traceId="abc123" />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    fireEvent.click(await screen.findByRole("button", { name: /In the log/ }));
  });
  const [path, query = ""] = location.hash.split("?");
  expect(path).toBe("#/activity/events");
  const params = new URLSearchParams(query);
  expect(params.get("trace")).toBe("abc123");
  expect(params.get("window")).toBe("30d");
  expect(params.has("q")).toBe(false);
});
