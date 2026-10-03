/**
 * Every way into the event log lands on the event log, narrowed the way the
 * link promised.
 *
 * The event log is `#/live/events`; the bare `#/live` is Live now, which
 * reads none of the log's filters. The ways in once addressed the bare path,
 * and the trace's and the channel's carried their id as the log's TEXT search
 * — which matches no summary the engine writes, so each opened an empty log.
 * The log narrows by a trace (`trace=`) and by an agent-to-agent channel
 * (`channel=`) itself now, as the engine does; these hold the links to it.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ChannelScreen } from "./Conversations.tsx";
import { LiveNow } from "./LiveNow.tsx";
import { TraceScreen } from "./Trace.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
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
      <ViewerProvider>
        <Router>{screenNode}</Router>
      </ViewerProvider>
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

test("Live now's event log link opens the log rather than itself", async () => {
  location.hash = "#/live";
  await mount(<LiveNow />, {});
  const link = screen.getByRole("link", { name: "Event log" });
  expect(landed(link.getAttribute("href") ?? "")[0]).toBe("/live/events");
});

test("a trace's way into the log narrows it to that trace", async () => {
  location.hash = `#/live/traces/${TRACE}`;
  await mount(<TraceScreen traceId={TRACE} />, { trace: { trace_id: TRACE, events: [] } });
  fireEvent.click(screen.getByRole("button", { name: "In the log" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  const [path, query] = landed();
  expect(path).toBe("/live/events");
  expect(query.get("trace")).toBe(TRACE);
  expect(query.get("q"), "an id is no summary's text").toBeNull();
});

test("a channel's way into the log narrows it to that channel", async () => {
  location.hash = `#/live/a2a/${CHANNEL}`;
  await mount(<ChannelScreen id={CHANNEL} />, {
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
    events: { events: [], next: null, exhausted: true },
  });
  const link = screen.getByRole("link", { name: /Read this channel's events/ });
  const [path, query] = landed(link.getAttribute("href") ?? "");
  expect(path).toBe("/live/events");
  expect(query.get("channel")).toBe(CHANNEL);
  expect(query.get("q"), "an id is no summary's text").toBeNull();
});
