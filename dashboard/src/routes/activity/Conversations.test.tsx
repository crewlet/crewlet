/**
 * What the A2A screen claims about a channel it cannot show.
 *
 * `#/activity/a2a/<id>` is a real route — it is where `Open ↗` from the rail
 * lands and what a ⌘-click on a row opens — so the screen has to answer for an
 * id it was handed and did not find. There are three ways not to find one, and
 * only one of them is a fact about the company: the node holds no channel
 * record at all, the read did not come back, or the record was read and does
 * not hold it. Folded together they all render as "It may have been purged",
 * which tells a reader their channel is gone when the truth is that nobody
 * looked.
 *
 * The second case here is about identity rather than content: this body is
 * handed a clock that ticks once a second, and a subtree React REBUILDS on
 * every tick takes the reader's text selection with it.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Conversations } from "./Conversations.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { A2AChannel } from "~/protocol/index.ts";

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
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const ID = "chan-7";

function channel(over: Partial<A2AChannel> = {}): A2AChannel {
  return {
    id: ID,
    requester: "ceo",
    target: "swe",
    messages: 2,
    opened_at: "2026-09-13T10:00:00Z",
    last_at: "2026-09-13T10:02:00Z",
    closed_at: "2026-09-13T10:02:30Z",
    ...over,
  };
}

/** Mount the screen addressed at `ID`, over one stubbed `a2a_channels` answer. */
function mount(answer: () => Promise<unknown>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "a2a_channels" ? answer() : Promise.resolve({});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Conversations channelId={ID} />
      </Router>
    </ClientContext.Provider>,
  );
}

const PURGED = /It may have been purged/;

// THE CONTROL FIRST, or every absence below passes on a screen that never
// renders the claim at all.
test("a record that was read and holds no such channel says so", async () => {
  mount(() => Promise.resolve({ channels: [channel({ id: "chan-other" })], available: true }));
  expect(await screen.findByText(PURGED)).toBeTruthy();
});

// A FAILED READ IS NOT AN ANSWER.
//
// `useQuery` keeps the last good answer and reports the code, so a first-load
// failure leaves `data` null, `rows` empty and `loading` false — which is
// indistinguishable, to a guard that tests `loading` alone, from a record that
// came back without this channel in it. The refusal banner then rendered above
// a confident claim that the channel had been purged.
test("a read that failed does not claim the channel was purged", async () => {
  mount(() => Promise.reject(new Error("timeout")));
  expect(await screen.findByText(/did not answer within 10 seconds/)).toBeTruthy();
  expect(screen.queryByText(PURGED)).toBeNull();
});

// AND NEITHER IS A NODE THAT HOLDS NO RECORD.
//
// `available: false` is the engine saying it cannot reach the coordination
// store where channels live. The screen already renders that as its own banner
// in place of the list; the addressed-channel empty state sits OUTSIDE that
// branch, so it used to run on underneath it and contradict it.
test("a node with no channel record does not claim the channel was purged", async () => {
  mount(() => Promise.resolve({ channels: [], available: false }));
  expect(await screen.findByText(/No agent-to-agent channel record is reachable/)).toBeTruthy();
  expect(screen.queryByText(PURGED)).toBeNull();
});

// THE BODY IS RECONCILED, NOT REBUILT.
//
// Its section wrapper used to be defined inside the render body, so it was a
// new component type on every render and React tore the subtree down and built
// it again instead of updating it. Both callers pass a `now` from the shared
// ticker, so that happened once a second: the assertion is DOM identity,
// because that is exactly what a text selection, a focused control and a
// scroll position are anchored to.
test("a tick of the clock does not rebuild the channel body", async () => {
  vi.useFakeTimers();
  mount(() => Promise.resolve({ channels: [channel()], available: true }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  // The id as the RECORD section renders it — a `<code>` inside the wrapper,
  // rather than the header's own identifier, which sits outside it.
  const code = () => screen.getAllByText(ID).filter((el) => el.tagName === "CODE");
  expect(code()).toHaveLength(1);
  const before = code()[0]!;

  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_000);
  });
  expect(code()[0]).toBe(before);
});

// ---------------------------------------------------------------------------
// A page of the record, and what is counted over it
// ---------------------------------------------------------------------------

/** Mount the plain list over a socket that records what it was asked. */
function mountList(answer: (params: Record<string, unknown>) => unknown, channelId?: string) {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: Record<string, unknown>[] = [];
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what, params = {}) => {
    if (what !== "a2a_channels") return Promise.resolve({});
    asked.push(params);
    return Promise.resolve(answer(params));
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Conversations channelId={channelId} />
      </Router>
    </ClientContext.Provider>,
  );
  return asked;
}

const TOTALS = { channels: 412, open: 3, messages: 977, pairs: 41 };

// THE STAT CARDS ARE THE ENGINE'S TOTALS. Reduced over the rows they described
// one page of the most recently active channels, and changed as a reader paged.
test("the stat cards read the answer's totals, not the page", async () => {
  mountList(() => ({
    channels: [channel({ closed_at: "" })],
    available: true,
    state: "all",
    truncated: true,
    next: { before_time: "2026-09-13T10:02:00Z", before_id: ID },
    totals: TOTALS,
  }));
  expect(await screen.findByText("977")).toBeTruthy();
  expect(screen.getByText("41")).toBeTruthy();
  expect(screen.getByText("of 412 channels in the record")).toBeTruthy();
});

// THE REST IS THE NEXT PAGE, and it is asked for with both halves of `next`.
test("a cut listing pages with the cursor it was handed", async () => {
  const older = channel({ id: "chan-old", requester: "cfo", target: "pm" });
  const asked = mountList((params) =>
    params.before_id
      ? {
          channels: [older],
          available: true,
          state: "all",
          truncated: false,
          next: {},
          totals: TOTALS,
        }
      : {
          channels: [channel()],
          available: true,
          state: "all",
          truncated: true,
          next: { before_time: "2026-09-13T10:02:00Z", before_id: ID },
          totals: TOTALS,
        },
  );
  const button = await screen.findByRole("button", { name: "Load older channels" });
  await act(async () => button.click());
  const page = asked.find((p) => p.before_id);
  expect(page).toEqual({
    state: "all",
    before_time: "2026-09-13T10:02:00Z",
    before_id: ID,
  });
  expect(
    await screen.findByText(/Updates are paused while older channels are loaded/),
  ).toBeTruthy();
  // THE LAST PAGE OFFERS NO FURTHER ONE.
  expect(screen.queryByRole("button", { name: "Load older channels" })).toBeNull();
});

test("a listing that holds every channel offers no older page", async () => {
  // THE CONTROL.
  mountList(() => ({
    channels: [channel()],
    available: true,
    state: "all",
    truncated: false,
    next: {},
    totals: { channels: 1, open: 0, messages: 2, pairs: 1 },
  }));
  expect(await screen.findByText("of 1 channel in the record")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load older channels" })).toBeNull();
});

// A CHANNEL'S OWN PAGE ASKS FOR IT BY ID. Looked up in the listing's page, a
// channel older than the page read as "no such channel".
test("the addressed channel is read by its id, not found in a page", async () => {
  const asked = mountList(
    (params) =>
      params.id === ID
        ? { channels: [channel()], available: true, state: "all", truncated: false, next: {} }
        : {
            channels: [],
            available: true,
            state: "all",
            truncated: true,
            next: {},
            totals: TOTALS,
          },
    ID,
  );
  expect((await screen.findAllByText(ID)).length).toBeGreaterThan(0);
  expect(screen.queryByText(PURGED)).toBeNull();
  expect(asked.some((p) => p.id === ID && p.state === "all")).toBe(true);
  // AND ITS WORDS ARE ON THE EVENT LOG, which is `#/activity/events`: the bare
  // `#/activity` is the Live now screen, which reads no filter at all.
  const words = screen.getAllByRole("link", { name: /Read this channel's events/ })[0]!;
  expect(words.getAttribute("href")).toContain("#/activity/events?");
  // ON THE WHOLE LOG, since the log's own fallback is a day.
  expect(words.getAttribute("href")).toContain("window=30d");
});
