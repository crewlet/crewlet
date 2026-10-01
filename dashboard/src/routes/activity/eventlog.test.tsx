/**
 * Every way into the event log lands on the event log.
 *
 * The event log is `#/activity/events`; the bare `#/activity` is LIVE NOW,
 * which reads no filter at all. Four links still addressed the bare path from
 * when it was the log — Live now's own "Event log" button, which therefore
 * reloaded the screen it sat on; a trace's "In the log", whose search was
 * dropped; an A2A channel's "Read this channel's events", whose category and
 * search were dropped; and a seat's "Its events" (`Seat.events.test.tsx`) — so
 * each opened the whole company's live view where it promised a filtered log.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Conversations } from "./Conversations.tsx";
import { LiveNow } from "./LiveNow.tsx";
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
  vi.useFakeTimers({ shouldAdvanceTime: true });
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

const TRACE = "0af7651916cd43dd8448eb211c80319c";
const CHANNEL = "chan-7";

/** mount renders a screen over a socket answering these questions. */
async function mount(screenNode: ReactNode, answers: Record<string, unknown>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(answers[what] ?? {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{screenNode}</Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

/** landed splits where the browser is now into its path and its query. */
function landed(at: string = location.hash): [string, URLSearchParams] {
  const [path, query = ""] = at.replace(/^#/, "").split("?");
  return [path ?? "", new URLSearchParams(query)];
}

test("Live now's event log button opens the log rather than itself", async () => {
  location.hash = "#/activity";
  await mount(<LiveNow />, {});
  fireEvent.click(screen.getByRole("button", { name: "Event log" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(landed()[0]).toBe("/activity/events");
});

test("a trace's way into the log carries its id to the log's search", async () => {
  location.hash = `#/activity/traces/${TRACE}`;
  await mount(<TraceScreen traceId={TRACE} />, { trace: { trace_id: TRACE, events: [] } });
  fireEvent.click(screen.getByRole("button", { name: "In the log" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  const [path, query] = landed();
  expect(path).toBe("/activity/events");
  expect(query.get("q")).toBe(TRACE);
});

test("a channel's way into the log carries its category and id", async () => {
  location.hash = `#/activity/a2a/${CHANNEL}`;
  await mount(<Conversations channelId={CHANNEL} />, {
    a2a_channels: {
      available: true,
      channels: [
        {
          id: CHANNEL,
          requester: "ceo",
          target: "swe",
          messages: 2,
          opened_at: "2026-09-13T10:00:00Z",
          last_at: "2026-09-13T10:02:00Z",
          closed_at: "2026-09-13T10:02:30Z",
        },
      ],
    },
  });
  const link = screen.getByRole("link", { name: /Read this channel's events/ });
  const [path, query] = landed(link.getAttribute("href") ?? "");
  expect(path).toBe("/activity/events");
  expect(query.get("category")).toBe("a2a");
  expect(query.get("q")).toBe(CHANNEL);
});
