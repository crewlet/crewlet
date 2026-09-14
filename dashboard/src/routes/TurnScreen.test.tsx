/**
 * The Turn screen as a screen, rather than as three pure functions.
 *
 * Its helpers had tests and its rendering did not, which is exactly backwards
 * for a page whose failures are all claims: this screen states how long a turn
 * took, how it ended and whether anything went wrong, and every one of those
 * is a sentence beside a number rather than a number alone. A caption that
 * says the wrong thing about a right number is the defect this page keeps
 * having, and no test over `turnSpan` or `outcomeOf` can see one.
 *
 * Each case below is a claim the page was making, or refusing to make, that
 * the rows it holds do not support.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { TurnScreen } from "./Turn.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventRecord, TurnAnswer } from "~/protocol/index.ts";

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
});

const TURN = "t-77";

function event(over: Partial<EventRecord> & { type: string }): EventRecord {
  return {
    id: over.type + "-" + (over.timestamp ?? ""),
    source: "engine",
    actor: "CEO",
    summary: "",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    timestamp: "2026-09-13T10:00:00Z",
    ...over,
  } as EventRecord;
}

/** A finished execute phase that took `durationMs`, as the engine publishes it. */
function phase(at: string, durationMs: number, over: Record<string, unknown> = {}): EventRecord {
  return event({
    type: "agent_phase_completed",
    timestamp: at,
    payload: {
      turn_id: TURN,
      phase: "execute",
      iteration: 1,
      role: "CEO",
      model: "claude-sonnet-5",
      total_tokens: 1200,
      rounds_used: 2,
      duration_ms: durationMs,
      ...over,
    },
  });
}

/** Mount the screen over one stubbed answer. */
function mount(answer: Partial<TurnAnswer>) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    what === "turn"
      ? Promise.resolve({ turn_id: TURN, events: [], truncated: false, ...answer })
      : Promise.resolve({});
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <TurnScreen turnId={TURN} />
      </Router>
    </ClientContext.Provider>,
  );
}

// THE DURATION THE ENGINE MEASURED REACHES THE CARD.
//
// It used to be reconstructed by pairing the completed record with the
// `agent_phase_started` that shares its key — and on this screen's own reason
// for existing, a turn deep-linked while it runs, neither half of that pairing
// is in the browser. Asserted through the render because the plumbing is four
// hops long (payload → fromPhaseEvent → phaseDuration → PhaseCard) and each
// hop was capable of dropping it silently.
test("a phase card shows the duration off the record itself", async () => {
  mount({ events: [phase("2026-09-13T10:01:30Z", 90_000)] });
  expect(await screen.findByTitle("how long this phase took")).toHaveProperty(
    "textContent",
    "1m 30s",
  );
});

// A CUT VIEW SAYS SO, IN THE HEADER AND ABOVE THE PANELS.
//
// `EventLog.Turn` orders oldest first and stops at the store's per-turn cap,
// so the rows a long turn loses are its ENDING — the two records this header
// reads its outcome, its wall clock and its plan summary off. With no flag, a
// truncated turn was indistinguishable from one that never finished.
test("a turn read to the store's cap is named as cut, not as unfinished", async () => {
  mount({ events: [phase("2026-09-13T10:01:30Z", 90_000)], truncated: true });
  expect(await screen.findByText(/oldest 1 shown/)).toBeTruthy();
  expect(screen.getByText(/what is missing is the/)).toBeTruthy();
  // The two captions that were making claims the rows do not support: "no
  // turn record" is about the TURN, and "spanning the turn's first and last
  // event" describes a window whose far end is wherever the read stopped.
  expect(screen.getByText("cut off before the turn's own record")).toBeTruthy();
  expect(screen.getByText("at least this — the turn's end is not in this view")).toBeTruthy();
  expect(screen.queryByText("no turn record")).toBeNull();
  expect(screen.queryByText("spanning the turn's first and last event")).toBeNull();
});

// AND IT DOES NOT CLAIM THE TURN WAS CLEAN. "Nothing went wrong" is a claim
// over every row of the turn, and a cut view has rows it never saw — any of
// which could be the guard breach that ended it.
test("a cut view withholds the clean badge, and a complete one gives it", async () => {
  const complete = [
    phase("2026-09-13T10:01:30Z", 90_000),
    event({
      type: "agent_turn_completed",
      timestamp: "2026-09-13T10:01:31Z",
      payload: { turn_id: TURN, failed: false, decision: "done" },
    }),
  ];
  // THE CONTROL FIRST, or the absence below passes on a page that never
  // renders the badge at all.
  mount({ events: complete });
  expect(await screen.findByText("nothing went wrong")).toBeTruthy();

  cleanup();
  mount({ events: complete, truncated: true });
  await screen.findByText(/oldest 2 shown/);
  expect(screen.queryByText("nothing went wrong")).toBeNull();
});

// prompt.size REACHES THE PAGE.
//
// Six small integers per phase were banded into `given` and read by nobody:
// the screen took `prefetch_summary` out of that band and dropped the rest,
// so the only route to a phase's prompt size was the raw payload of a row in
// the residual list.
test("each phase's prompt size is rendered rather than banded and dropped", async () => {
  mount({
    events: [
      phase("2026-09-13T10:01:30Z", 90_000),
      event({
        type: "prompt.size",
        timestamp: "2026-09-13T10:00:01Z",
        payload: {
          turn_id: TURN,
          phase: "execute",
          iteration: 1,
          approximate_tokens: 7400,
          system_chars: 24000,
          user_chars: 1200,
        },
      }),
    ],
  });
  expect(await screen.findByText("Prompt sent")).toBeTruthy();
  expect(screen.getByTitle("the engine's own approximation").textContent).toBe("7,400");
  expect(screen.getByTitle("characters in the system prompt")).toBeTruthy();
});

// A TURN THAT ANSWERED WITH NOTHING IS AN EMPTY STATE, not a blank page — and
// the empty state must not fire on a turn whose phases are arriving on the
// stream instead, which is the deep-link-while-running case.
test("a turn with no events at all says so", async () => {
  mount({ events: [] });
  expect(await screen.findByText("No events for this turn")).toBeTruthy();
});
