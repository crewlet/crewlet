/**
 * A seat's turn cards draw the turns that changed, and no others.
 *
 * The Turns tab lists the seat's turns as cards rather than grid rows, and
 * every card drew again on every render of the screen: the cards were not
 * memoised, and the turns were grouped afresh from the phases each time any
 * phase arrived — this seat's or any other's, since the store's phase slice is
 * the whole company's — so a busy company redrew every card of every seat page
 * open, each with its phase list, for a phase that belonged to somebody else.
 * A card's draw is counted by the one call it makes to `triggerHeadline`, which
 * nothing else on the screen calls.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

const headlines = vi.hoisted(() => [] as string[]);

vi.mock("~/lib/phases.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/phases.ts")>();
  return {
    ...real,
    triggerHeadline: (trigger: Parameters<typeof real.triggerHeadline>[0]) => {
      headlines.push(trigger?.summary ?? "");
      return real.triggerHeadline(trigger);
    },
  };
});

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventEnvelope } from "~/protocol/index.ts";
import { flushInCase } from "~/test/inCase.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const TURNS = 12;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  headlines.length = 0;
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

/** One completed phase, as the engine publishes it. */
function phase(
  agent: string,
  turn: number,
  name: string,
  iteration: number,
  minute: number,
): EventEnvelope {
  const at = `2026-09-13T10:${String(minute).padStart(2, "0")}:00Z`;
  return {
    id: `ev-${agent}-${turn}-${name}-${iteration}`,
    type: "agent_phase_completed",
    timestamp: at,
    source: "engine",
    actor: "CEO",
    summary: "",
    category: "lifecycle",
    payload: {
      turn_id: `${agent}-turn-${turn}`,
      // TURNS 0 AND 1 ARE TWO RUNS OF ONE TRIGGER, so the screen has attempts to
      // hand two of its cards — and a value to keep for them.
      work_key: `${agent}-work-${Math.max(turn, 1)}`,
      agent_id: agent,
      phase: name,
      iteration,
      role: "CEO",
      model: "claude-sonnet-5",
      total_tokens: 100,
      trigger: { id: `trig-${turn}`, type: "message", summary: `turn ${turn}` },
    },
  } as unknown as EventEnvelope;
}

function mount() {
  const store = new Store();
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }] });
  store.applySeats([{ id: "ceo", agent_id: "a-1", role: "CEO", handle: "ceo" }]);
  const socket = new LiveSocket(store);
  const history = Array.from({ length: TURNS }, (_, i) => phase("a-1", i, "execute", 1, i));
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "agent" ? { llm_history: history, next: "" } : {});
  location.hash = "#/company/people/ceo?tab=turns";
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, answered: flushInCase() };
}

test("another seat's phase draws none of this seat's turn cards", async () => {
  const { store, answered } = mount();
  await answered();
  expect(document.querySelectorAll(".turn-card")).toHaveLength(TURNS);
  headlines.length = 0;

  await answered(() => store.applyEvent(phase("a-2", 0, "execute", 1, 59)));
  expect(headlines, "a phase of another seat's drew this seat's turn cards").toEqual([]);
});

// THE NEWEST TURN, which is where a phase lands: its review follows its
// execute. A phase landing on an older turn makes it the newest and moves it to
// the top, and the card it displaced is drawn too — it is no longer the one
// that opens by default, which is a prop it is handed.
test("a phase landing on a turn draws that turn's card and no other", async () => {
  const { store, answered } = mount();
  await answered();
  const newest = TURNS - 1;
  expect(screen.getAllByText(new RegExp(`turn ${newest}`)).length).toBeGreaterThan(0);
  headlines.length = 0;

  await answered(() => store.applyEvent(phase("a-1", newest, "review", 1, TURNS)));
  expect(headlines).toEqual([`turn ${newest}`]);
});
