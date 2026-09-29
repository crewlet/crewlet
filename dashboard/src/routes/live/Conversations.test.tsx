/**
 * What the A2A screen claims about a channel it cannot show.
 *
 * `#/live/a2a/<id>` is a real route — it is where `Open ↗` from the rail
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

import type { ReactElement } from "react";
import { ChannelPeek, ChannelScreen, Conversations } from "./Conversations.tsx";
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

/** What each question was asked with, in order — the page's own reads. */
let asked: { what: string; params: unknown }[] = [];

/**
 * Mount the channel's page addressed at `ID` (or `node`), over one stubbed
 * `a2a_channels` answer and, optionally, the channel's `events`.
 */
function mount(
  answer: () => Promise<unknown>,
  node: ReactElement = <ChannelScreen id={ID} />,
  events: unknown = { events: [], next: null, exhausted: true },
) {
  asked = [];
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string, params: unknown) => Promise<unknown> }).query = (
    what: string,
    params: unknown,
  ) => {
    asked.push({ what, params });
    return what === "a2a_channels"
      ? answer()
      : what === "events"
        ? Promise.resolve(events)
        : Promise.resolve({});
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{node}</Router>
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

// ONE FACT, ONE EMPTY STATE. Under "No such channel" the page drew a second
// card, "On this channel 0", with an empty state of its own — the same
// absence said twice. The not-found state carries the link to the log; the
// events card is drawn for a known channel, or where the log still holds what
// crossed one whose record is gone.
//
// Mutation: draw the events card unconditionally, and "On this channel" and
// its "No event names this channel" come back under the not-found state.
test("an unknown channel is one not-found state with the log link inside it", async () => {
  mount(() => Promise.resolve({ channels: [], available: true }));
  expect(await screen.findByText(PURGED)).toBeTruthy();
  expect(screen.getByRole("heading", { name: "No such channel" })).toBeTruthy();
  expect(screen.queryByText("On this channel")).toBeNull();
  expect(screen.queryByText("No event names this channel")).toBeNull();
  expect(screen.getByRole("link", { name: "Open in the event log" }).getAttribute("href")).toBe(
    `#/live/events?channel=${ID}`,
  );
});

test("a purged channel whose events the log still holds shows them", async () => {
  mount(() => Promise.resolve({ channels: [], available: true }), undefined, {
    events: [
      {
        id: "e-1",
        type: "a2a_message_sent",
        timestamp: "2026-09-13T10:00:00Z",
        source: "engine",
        actor: "ceo",
        summary: "ceo asked: how long?",
        category: "a2a",
        trace_id: "",
        span_id: "",
        parent_span_id: "",
        topic: "",
        failed: false,
        channel_id: ID,
      },
    ],
    next: null,
    exhausted: true,
  });
  expect(await screen.findByText(/how long\?/)).toBeTruthy();
  expect(screen.getByText(/the log still holds what crossed it/)).toBeTruthy();
  expect(screen.queryByText(PURGED)).toBeNull();
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
  expect(await screen.findByText(/No channel record is reachable from this node/)).toBeTruthy();
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

// A CUT LISTING SAYS IT IS CUT.
//
// The engine answers the most recently active channels up to a limit and says
// `truncated` when the record held more. The screen's counts were captioned
// "across every channel in the record" over exactly that page, and a channel
// missing from it was offered "the 200 most recent" — a number that is the
// engine's to change — whether or not anything had been cut.
test("a cut listing says its counts cover the most recent channels", async () => {
  const cut = () =>
    Promise.resolve({
      channels: [channel({ id: "chan-other" })],
      available: true,
      state: "all",
      truncated: true,
    });
  mount(cut);
  expect(await screen.findByText(/may be older than all of them/)).toBeTruthy();
  cleanup();
  mount(cut, <Conversations />);
  expect(await screen.findByText("across the most recent channels only")).toBeTruthy();
  expect(screen.queryByText("across every channel in the record")).toBeNull();
});

test("a whole listing says it is the whole record", async () => {
  const whole = () =>
    Promise.resolve({
      channels: [channel({ id: "chan-other" })],
      available: true,
      state: "all",
      truncated: false,
    });
  mount(whole);
  expect(await screen.findByText(/holds every one in the record/)).toBeTruthy();
  cleanup();
  mount(whole, <Conversations />);
  expect(await screen.findByText("across every channel in the record")).toBeTruthy();
});

// "READ THIS CHANNEL'S EVENTS" IS THE LOG NARROWED TO THE CHANNEL.
//
// The link carried the channel id as the log's TEXT search, which matches no
// summary the engine writes, so it landed on an empty log. The engine filters
// by the channel id every A2A event carries; the link names it as `channel=`,
// on the rail and on the page alike.
//
// Mutation: point `channelEventsHref` back at `q=`, and every link here fails.
test("the channel's events link is the log narrowed to this channel", async () => {
  const whole = () => Promise.resolve({ channels: [channel()], available: true });
  mount(whole, <ChannelPeek id={ID} />);
  const peekLink = await screen.findByRole("link", { name: /Read this channel's events/ });
  expect(peekLink.getAttribute("href")).toBe(`#/live/events?channel=${ID}`);
  cleanup();
  mount(whole);
  await screen.findByRole("link", { name: /Read this channel's events/ });
  const links = screen.getAllByRole("link", {
    name: /Read this channel's events|Open in the event log/,
  });
  expect(links.length).toBe(2);
  for (const link of links) expect(link.getAttribute("href")).toBe(`#/live/events?channel=${ID}`);
});

// THE PAGE READS WHAT CROSSED THE CHANNEL, asked of the engine by the channel
// id, and draws it in the order it happened: a conversation reads top down.
test("the channel's page lists its own events oldest first", async () => {
  const row = (id: string, at: string, summary: string) => ({
    id,
    type: "a2a_message_sent",
    timestamp: at,
    source: "engine",
    actor: "ceo",
    summary,
    category: "a2a",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
    channel_id: ID,
  });
  mount(() => Promise.resolve({ channels: [channel()], available: true }), undefined, {
    events: [
      row("e-2", "2026-09-13T10:02:00Z", "swe answered: two weeks"),
      row("e-1", "2026-09-13T10:00:00Z", "ceo asked: how long?"),
    ],
    next: null,
    exhausted: true,
  });
  const second = await screen.findByText(/two weeks/);
  const first = screen.getByText(/how long\?/);
  expect(first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  const ask = asked.find((a) => a.what === "events");
  expect(ask?.params).toMatchObject({ channel_id: ID });
});
