/**
 * What the thread pane's header says when the reader has chosen nothing.
 *
 * `Card.Header` draws its count for every value that is not `undefined` — 0
 * included — and the value here is `entries`, what this seat said in the ONE
 * thread the reader opened. `queries.conversations` fills `entries` only when a
 * `conversation` is named and answers `[]` otherwise, so coalesced with `?? 0`
 * the header read "Pick a thread 0": a quantity about a thread nobody had named,
 * which could never have been anything but zero.
 *
 * Asserted in BOTH directions. Deleting the count would silence the 0 just as
 * well and lose the one place the pane says how much it is showing, so the
 * second test holds the chip to the answer when a thread IS open.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
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

/** Opaque by design — the notification layer owns the grammar of a key. */
const KEY = "slack:C0TEAM/1709280000.000100";

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/**
 * The seat screen on its Conversations tab, answered the way the engine answers:
 * `entries` is always an array, and it is EMPTY until the question names a
 * conversation. That is the whole hazard, so the fixture keeps it.
 */
function mount(params: string, entries: unknown[]) {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }] });
  const socket = new LiveSocket(store);
  (
    socket as unknown as { query: (what: string, p?: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, p) =>
    Promise.resolve(
      what === "conversations"
        ? {
            handle: "ceo",
            available: true,
            conversations: [{ key: KEY, turns: 2, last_at: "2026-03-01T09:00:00Z" }],
            entries: p?.conversation ? entries : [],
          }
        : { llm_history: [], next: "" },
    );
  location.hash = `#/company/people/ceo?${params}`;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
}

/** The header row a title sits on — the Threads card beside it has a chip too. */
function header(title: string): HTMLElement {
  const el = screen.getByText(title).closest(".crewlet-card__header");
  expect(el, `no card header carries the title ${title}`).not.toBeNull();
  return el as HTMLElement;
}

test("with no thread open the pane counts nothing and does not instruct from its title", async () => {
  mount("tab=threads", []);
  await screen.findByText("Thread turns");
  expect(
    header("Thread turns").querySelector(".crewlet-count"),
    "nothing was asked for, so a 0 here states a quantity about a thread nobody named",
  ).toBeNull();
  // THE INSTRUCTION IS SAID ONCE, by the empty state, which is where it can also
  // say what a thread holds.
  expect(screen.queryByText("Pick a thread")).toBeNull();
  expect(
    screen.getByText(/Choose a thread to see the turns this seat recorded in it/),
  ).toBeTruthy();
});

test("an open thread's turns are counted", async () => {
  mount(`tab=threads&conversation=${encodeURIComponent(KEY)}`, [
    { turn_id: "t1", at: "2026-03-01T09:00:00Z", intent: "answered the question" },
    { turn_id: "t2", at: "2026-03-01T10:00:00Z", intent: "followed up" },
  ]);
  await screen.findByText("Thread turns");
  await waitFor(() =>
    expect(header("Thread turns").querySelector(".crewlet-count")?.textContent).toBe("2"),
  );
});

// A TRIMMED THREAD SAYS SO. The ledger keeps the newest turns of a thread and
// sweeps by age, so what comes back is the survivors — and the oldest one's
// ordinal is what says turns came before it.
test("a thread whose oldest held turn is not its first says how many came before", async () => {
  mount(`tab=threads&conversation=${encodeURIComponent(KEY)}`, [
    { turn_id: "t5", ordinal: 5, at: "2026-03-01T09:00:00Z", intent: "picked it up again" },
    { turn_id: "t6", ordinal: 6, at: "2026-03-01T10:00:00Z", intent: "followed up" },
  ]);
  expect(
    await screen.findByText(/4 earlier turns in this thread are no longer in the ledger/),
  ).toBeTruthy();
});

test("a thread held from its first turn, or from before ordinals, claims nothing", async () => {
  mount(`tab=threads&conversation=${encodeURIComponent(KEY)}`, [
    { turn_id: "t1", ordinal: 1, at: "2026-03-01T09:00:00Z", intent: "answered" },
  ]);
  await screen.findByText("answered");
  expect(screen.queryByText(/no longer in the ledger/)).toBeNull();
  cleanup();
  // AN ENTRY WRITTEN BEFORE THE STORE STAMPED ORDINALS carries none, and says
  // nothing either way.
  mount(`tab=threads&conversation=${encodeURIComponent(KEY)}`, [
    { turn_id: "t1", at: "2026-03-01T09:00:00Z", intent: "answered" },
  ]);
  await screen.findByText("answered");
  expect(screen.queryByText(/no longer in the ledger/)).toBeNull();
});

// WHAT STOPPED A BLOCKED TURN is its own field, and it was on the wire and on
// no screen: the turn read exactly like one that finished the work.
test("a blocked turn says what it was blocked on", async () => {
  mount(`tab=threads&conversation=${encodeURIComponent(KEY)}`, [
    {
      turn_id: "t1",
      at: "2026-03-01T09:00:00Z",
      decision: "done",
      blocked_on: "asked the CTO which region to deploy to",
    },
  ]);
  expect(await screen.findByText(/asked the CTO which region to deploy to/)).toBeTruthy();
});
