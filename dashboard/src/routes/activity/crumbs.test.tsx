/**
 * What the page bar, the browser tab and the palette's recents call an object
 * under Activity.
 *
 * Every screen here draws a name for its object — a turn's plan summary, a
 * trace's opening span, an event's own sentence, a coding run's task, who
 * asked whom on an A2A channel — and for as long as none of them published it,
 * the frame around them had nothing and fell back to the one thing the route
 * carries: the id. So a reader's recents was a stack of uuids, the trail ended
 * in one, and four tabs open on four turns were four indistinguishable hex
 * strings. The machinery was already there and right — `crumbsFor` takes the
 * labels a screen publishes, `Shell` titles the tab from the trail it builds
 * and `remember` stores that same string — and these five screens were the
 * ones not holding up their end of it. `Seat.crumb.test.tsx` is the same bug,
 * caught earlier, on the seat screen.
 *
 * THE ID STAYS WHERE THERE IS NO NAME, which is the other half and the easier
 * one to break: a placeholder is a worse label than an identifier, because two
 * objects wearing "Turn" are two identical recents rows pointing at different
 * places, and the reader cannot tell which one they wanted.
 */

import type { ReactNode } from "react";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { TurnScreen } from "./Turn.tsx";
import { TraceScreen } from "./Trace.tsx";
import { EventScreen } from "./Event.tsx";
import { Runs } from "./Runs.tsx";
import { Conversations } from "./Conversations.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { resetForTest } from "~/lib/recents.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { EventRecord, OrgProjection } from "~/protocol/index.ts";

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
  localStorage.clear();
  resetForTest();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
  document.title = "";
});

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

/**
 * Mount one screen inside the real frame, over a stubbed answer per query.
 *
 * THE FRAME IS THE POINT: the label, the trail, the tab title and the recents
 * entry are four things `Shell` derives from one published value, and a test
 * that rendered the screen alone would assert none of them.
 */
function mount(
  hash: string,
  node: ReactNode,
  answers: Record<string, unknown>,
  org?: OrgProjection,
) {
  location.hash = hash;
  const store = new Store();
  if (org) store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what: string) =>
    Promise.resolve(answers[what] ?? {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>{node}</Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });
const here = () => trail().querySelector("[aria-current='page']")?.textContent;

/** The labels the palette would offer, read the way its own snapshot reads them. */
function recents(): string[] {
  const raw = localStorage.getItem("crewlet_recents");
  return raw ? (JSON.parse(raw) as { label: string }[]).map((r) => r.label) : [];
}

function event(over: Partial<EventRecord> & { type: string }): EventRecord {
  return {
    id: "e-1",
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

const TURN = "3c98da61-0b1e-4f2a-9a77-5c1d2e3f4a5b";

/** The reviewer's account of a turn, as `turn_completed` carries it. */
function turnRecord(planSummary: string): EventRecord {
  return event({
    id: "turn-completed",
    type: "turn_completed",
    payload: { turn_id: TURN, plan_summary: planSummary },
  });
}

// ---------------------------------------------------------------------------
// The turn — the object the reader in the report was looking at
// ---------------------------------------------------------------------------

// THE TWO STRINGS HAVE TO DIFFER or the assertion cannot fail: the name is
// prose and the segment is a uuid.
test("a turn is named by what it did, in the trail, the tab and the recents", async () => {
  mount(`#/activity/turns/${TURN}`, <TurnScreen turnId={TURN} />, {
    turn: { turn_id: TURN, truncated: false, events: [turnRecord("Cut the 0.2.0 release.")] },
  });
  await settle();

  expect(here()).toBe("Cut the 0.2.0 release.");
  expect(trail().textContent, "the trail still carries the uuid").not.toContain(TURN);
  expect(document.title).toBe("Cut the 0.2.0 release. · Crewlet");
  expect(recents()).toEqual(["Cut the 0.2.0 release."]);
});

// ONLY THE LEAD SENTENCE, which is what the header takes — the two must not
// disagree, or the palette offers a string the page never showed.
test("a turn's label is the lead of its summary, not the whole account", async () => {
  mount(`#/activity/turns/${TURN}`, <TurnScreen turnId={TURN} />, {
    turn: {
      turn_id: TURN,
      truncated: false,
      events: [
        turnRecord("Replied in the thread. Then activated mattermost_post_message to confirm."),
      ],
    },
  });
  await settle();

  expect(here()).toBe("Replied in the thread.");
  expect(recents()).toEqual(["Replied in the thread."]);
});

// THE GATE. A turn nothing has named keeps its id, because "Turn" in the trail
// names no turn and two of them in the recents are one row twice.
test("a turn nothing has named keeps its id rather than taking a placeholder", async () => {
  mount(`#/activity/turns/${TURN}`, <TurnScreen turnId={TURN} />, {
    turn: { turn_id: TURN, truncated: false, events: [] },
  });
  await settle();

  expect(here()).toBe(TURN);
  expect(recents(), "a placeholder reached the palette").not.toContain("Turn");
});

// ---------------------------------------------------------------------------
// The other four objects under Activity
// ---------------------------------------------------------------------------

const TRACE = "f3c00fdf0a32e87d479fb1c4e5a60b72";

test("a trace is named by the span it begins at", async () => {
  mount(`#/activity/traces/${TRACE}`, <TraceScreen traceId={TRACE} />, {
    trace: {
      trace_id: TRACE,
      truncated: false,
      events: [
        event({
          type: "notification_received",
          summary: "Founder mentioned CEO in #general",
          trace_id: TRACE,
          span_id: "s1",
        }),
      ],
    },
  });
  await settle();

  expect(here()).toBe("Founder mentioned CEO in #general");
  expect(document.title).toBe("Founder mentioned CEO in #general · Crewlet");
  expect(recents()).toEqual(["Founder mentioned CEO in #general"]);
});

test("a trace with no span in the window keeps its id", async () => {
  mount(`#/activity/traces/${TRACE}`, <TraceScreen traceId={TRACE} />, {
    trace: { trace_id: TRACE, truncated: false, events: [] },
  });
  await settle();

  expect(here()).toBe(TRACE);
  expect(recents(), "a placeholder reached the palette").not.toContain("Trace");
});

const EVENT = "7d0e29e1-4c2b-4a8e-9f10-a1b2c3d4e5f6";

test("an event is named by the sentence the engine wrote for it", async () => {
  mount(`#/activity/events/${EVENT}`, <EventScreen eventId={EVENT} />, {
    event: event({ id: EVENT, type: "agent_turn_completed", summary: "CEO completed a turn" }),
  });
  await settle();

  expect(here()).toBe("CEO completed a turn");
  expect(recents()).toEqual(["CEO completed a turn"]);
});

// AN EVENT WITH NO SUMMARY IS KNOWN BY ITS TYPE everywhere else in the
// product, so the label is the type rather than the id — this is the one
// object here whose fallback is still a name.
test("an event with no summary is named by its type", async () => {
  mount(`#/activity/events/${EVENT}`, <EventScreen eventId={EVENT} />, {
    event: event({ id: EVENT, type: "agent_turn_completed", summary: "" }),
  });
  await settle();

  expect(here()).toBe("agent_turn_completed");
});

const RUN = "85e7b505-6dc9-478f-8501-1f2e3d4c5b6a";

test("a coding run is named by the task it was given", async () => {
  mount(`#/activity/runs/${RUN}`, <Runs runId={RUN} />, {
    sandbox_runs: {
      runs: [
        {
          turn_id: RUN,
          agent_handle: "eng",
          role: "Engineer",
          status: "running",
          coding_agent: "claude",
          placement: "direct",
          task_description: "Drop the Pulsar backend",
          question: "",
          audience: "",
          branch: "",
          trace_id: "",
          owner: "",
          box_exists: true,
          paused_at: "",
          pause_ttl_seconds: 0,
          started_at: "2026-09-13T10:00:00Z",
          updated_at: "2026-09-13T10:05:00Z",
          answerable_in_chat: false,
        },
      ],
    },
  });
  await settle();

  expect(here()).toBe("Drop the Pulsar backend");
  expect(recents()).toEqual(["Drop the Pulsar backend"]);
});

const CHANNEL = "77e58cab-c425-4036-9911-aa0b1c2d3e4f";

test("an A2A channel is named by who asked whom", async () => {
  mount(
    `#/activity/a2a/${CHANNEL}`,
    <Conversations channelId={CHANNEL} />,
    {
      a2a_channels: {
        available: true,
        channels: [
          {
            id: CHANNEL,
            requester: "ceo",
            target: "cto",
            messages: 2,
            opened_at: "2026-09-13T10:00:00Z",
            last_at: "2026-09-13T10:01:00Z",
            closed_at: "",
          },
        ],
      },
    },
    {
      name: "Acme",
      roles: [
        { name: "Chief Executive Officer", handle: "ceo" },
        { name: "Chief Technology Officer", handle: "cto" },
      ],
    },
  );
  await settle();

  expect(here()).toBe("Chief Executive Officer → Chief Technology Officer");
  expect(recents()).toEqual(["Chief Executive Officer → Chief Technology Officer"]);
});
