// @vitest-environment node

/**
 * The attention queue: what needs a person, in one list.
 *
 * Every condition here was already known to the previous dashboard and each
 * lived in a different screen — a paused coding run was a badge on the board
 * (and a run whose box had been reclaimed appeared NOWHERE), a stopped seat was
 * a card among the healthy ones, a refusing budget was a bar on one seat's
 * page, and an engine with no active configuration was a line in a popover.
 */

import { describe, expect, test } from "vitest";
import type { BudgetWindow, OrgBudget } from "~/protocol/index.ts";
import {
  attentionQueue,
  conditionsToDecide,
  SUBJECTS,
  WHERE_OF,
  watchedIn,
  type AttentionInput,
  type Subject,
  type Where,
} from "./attention.ts";

const now = Date.parse("2026-01-01T12:00:00Z");

function input(over: Partial<AttentionInput> = {}): AttentionInput {
  return {
    agents: [],
    runs: [],
    budget: null,
    engine: { status: "ok", configured: true },
    connected: true,
    authRejected: false,
    now,
    nameOf: (key) => key,
    ...over,
  };
}

/** One capped window of a live meter, on 1 January's day. */
function win(over: Partial<BudgetWindow> = {}): BudgetWindow {
  return {
    period: "day",
    window: "2026-01-01",
    starts_at: "2026-01-01T00:00:00Z",
    resets_at: "2026-01-02T00:00:00Z",
    used: 10,
    limit: 100,
    state: "ok",
    ...over,
  };
}

/** The company's live meter over the given windows. */
function meter(...windows: BudgetWindow[]): OrgBudget {
  return { meter_id: "m-1", seq: 1, timezone: "UTC", org: { windows } };
}

describe("what it surfaces", () => {
  test("a healthy company has nothing waiting", () => {
    expect(attentionQueue(input())).toEqual([]);
  });

  test("an engine with NO ACTIVE CONFIG is critical, not quiet", () => {
    // It looks exactly like a healthy idle one and drops every inbound
    // webhook. This is the whole reason the engine carries the flag.
    const items = attentionQueue(input({ engine: { status: "ok", configured: false } }));
    expect(items[0]?.id).toBe("unconfigured");
    expect(items[0]?.severity).toBe("critical");
    expect(items[0]?.detail).toContain("webhook");
  });

  test("nobody signed in and an unreachable engine are different items", () => {
    // The repair differs: one comes back on its own, the other never does.
    expect(attentionQueue(input({ connected: false }))[0]?.id).toBe("offline");
    expect(attentionQueue(input({ authRejected: true }))[0]?.id).toBe("auth");
  });

  // THE REPAIR IS A SIGN-IN, WHATEVER THE CREDENTIAL. The item said to "set a
  // token matching one of the api.auth.tokens entries", which a person whose
  // session had ended could not act on at all — and there is no token to set
  // any more: an API token is exchanged for a session on the sign-in screen.
  test("nobody signed in is repaired by signing in, and names no token to set", () => {
    const auth = attentionQueue(input({ authRejected: true }))[0];
    expect(auth?.detail).toContain("Sign in");
    expect(`${auth?.title} ${auth?.detail}`).not.toMatch(/operator token|set a token/i);
  });

  // THE ENGINE'S OWN WORD. This case asserted `awaiting_input`, which
  // `sandbox.PendingRun` cannot write — its statuses are `launching`,
  // `running`, `awaiting_clarification`, `resumed`, `done`, `failed` and
  // `reseed` — so the condition it was guarding never fired on a real run and
  // the test passed against a fixture no engine produces.
  //
  // AND FROM THE DURABLE ROW, not the projection: the row carries the pause
  // window the detail counts down, which the live entry does not. A run parked
  // on a question is the longest-lived item this list can hold, since it is
  // waiting for a person, and it has to stay here for as long as it waits.
  const parked = (status: string, over: Record<string, unknown> = {}) =>
    ({
      turn_id: "t1",
      role: "Dev A",
      agent_handle: "dev-a",
      status,
      coding_agent: "claude-code",
      placement: "direct",
      task_description: "",
      question: "Which branch should I target?",
      audience: "",
      audience_handles: [],
      audience_fallback: false,
      branch: "",
      trace_id: "",
      owner: "",
      box_exists: true,
      paused_at: "2026-01-01T11:00:00Z",
      pause_ttl_seconds: 0,
      started_at: "2026-01-01T10:00:00Z",
      updated_at: "2026-01-01T11:00:00Z",
      answerable_in_chat: false,
      ...over,
    }) as never;

  test("a run paused on a question carries the question", () => {
    const items = attentionQueue(input({ runs: [parked("awaiting_clarification")] }));
    expect(items[0]?.detail).toBe("Which branch should I target?");
    // THE RUN'S OWN ADDRESS. It was `#/live/runs?run=`, a query key the
    // runs screen stopped reading when a run became an object.
    expect(items[0]?.path).toEqual(["live", "runs", "t1"]);
  });

  // WHEN IT PARKED, never when it started: this row is about how long
  // somebody has been waited on, and a run that worked for an hour before
  // asking has been waiting for none of it.
  test("it is ordered by when it parked, not when it started", () => {
    const items = attentionQueue(input({ runs: [parked("awaiting_clarification")] }));
    expect(items[0]?.at).toBe("2026-01-01T11:00:00Z");
  });

  // THE DEADLINE IS THE COST OF IGNORING IT: the box is reclaimed at the
  // pause TTL and the work is gone, so a row that said only "waiting on an
  // answer" gave no reason to answer today rather than tomorrow.
  test("a pause window that runs out says when", () => {
    const items = attentionQueue(
      input({ runs: [parked("awaiting_clarification", { pause_ttl_seconds: 7_200 })] }),
    );
    expect(items[0]?.detail).toContain("reclaimed in 1h 0m");
  });

  // NO CLOCK READS SIXTY MINUTES. The hours and the minutes come off one
  // value, so rounding them apart put a "2h 60m" on the row for the thirty
  // seconds before every hour boundary of a countdown that reruns every
  // second — and a bare "60m" through the last hour of one.
  test("a countdown never carries into a sixtieth minute", () => {
    // 2h 59m 45s left: parked an hour before `now`, on a window of 14,385s.
    const nearly = attentionQueue(
      input({ runs: [parked("awaiting_clarification", { pause_ttl_seconds: 14_385 })] }),
    );
    expect(nearly[0]?.detail).toContain("reclaimed in 3h 0m");
    // And 59m 45s left, where there is no hour to carry into.
    const under = attentionQueue(
      input({ runs: [parked("awaiting_clarification", { pause_ttl_seconds: 7_185 })] }),
    );
    expect(under[0]?.detail).toContain("reclaimed in 1h 0m");
  });

  // A TTL OF ZERO IS NOT A DEADLINE OF NOW. Zero means the box is not
  // reclaimed on a timer at all, and inventing a countdown for it would be
  // the zero-value-as-a-setting mistake on a screen.
  test("a run with no pause window is given no false deadline", () => {
    const items = attentionQueue(input({ runs: [parked("awaiting_clarification")] }));
    expect(items[0]?.detail).not.toContain("reclaimed");
  });

  // A BOX REAPED PAST ITS PAUSE TTL is the same fact one step worse: the work
  // is gone and only the question survives, and `sandbox.Awaiting` counts it.
  test("a run whose box was reaped is waiting on a person too", () => {
    const items = attentionQueue(input({ runs: [parked("reseed")] }));
    expect(items[0]?.detail).toContain("Which branch should I target?");
    expect(items[0]?.detail).toContain("already reclaimed");
  });

  // AND A RUNNING ONE IS NOT WAITING ON ANYBODY. Without this the condition
  // above could be `status !== "running"` and still pass every case here.
  test("a running run is not in the queue", () => {
    expect(attentionQueue(input({ runs: [parked("running")] }))).toEqual([]);
  });

  test("a live round that stopped moving is surfaced, and escalates", () => {
    // A spinning row hides exactly this: the animation is identical whether
    // the round started two seconds or eleven minutes ago.
    const call = (updated: string) => ({
      id: "a",
      agent_id: "id-a",
      role: "Dev A",
      handle: "dev-a",
      live_call: {
        turn_id: "t1",
        phase: "execute",
        iteration: 1,
        model: "",
        trigger: null,
        prompt: "",
        prompt_messages: null,
        response: "",
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        tool_executions: null,
        round_num: 3,
        rounds_used: 3,
        in_progress: true,
        updated_at: updated,
      },
    });
    const fresh = attentionQueue(input({ agents: [call(new Date(now - 1000).toISOString())] }));
    expect(fresh).toEqual([]);

    const stale = attentionQueue(input({ agents: [call(new Date(now - 200_000).toISOString())] }));
    expect(stale[0]?.severity).toBe("caution");
    // THE ROUND A PERSON READS, which is the engine's zero-based counter plus
    // one. This row printed the raw counter, so it named a round one lower than
    // the seat card it links to — one call, two numbers, one click apart.
    expect(stale[0]?.detail).toContain("round 4");
    // AND THE OPENING FRAME IS NOT A MISSING VALUE. `-1` is the phase's first
    // model round-trip still in flight, which is exactly the case this row is
    // for; it used to render "round ?".
    const opening = attentionQueue(
      input({
        agents: [
          {
            ...call(new Date(now - 900_000).toISOString()),
            live_call: {
              ...call(new Date(now - 900_000).toISOString()).live_call,
              round_num: -1,
              rounds_used: 0,
            },
          },
        ],
      }),
    );
    expect(opening[0]?.detail).toContain("round 1");
    expect(opening[0]?.detail).not.toContain("?");

    const stalled = attentionQueue(
      input({ agents: [call(new Date(now - 900_000).toISOString())] }),
    );
    expect(stalled[0]?.severity).toBe("critical");
  });

  // THE ENGINE'S STATE, NOT A THRESHOLD OF OURS. A refusing window outranks a
  // near one, and a healthy one raises nothing — whatever the arithmetic says,
  // since the engine judged the window beside the counter.
  //
  // The refusal used to ride a `refused_at` the live push dropped, so the
  // critical branch was unreachable on every company — the exact case an
  // operator needs it for.
  test("a refusing window outranks one merely near its ceiling", () => {
    const refusing = attentionQueue(input({ budget: meter(win({ state: "refusing", used: 97 })) }));
    expect(refusing[0]?.severity).toBe("critical");
    expect(refusing[0]?.title).toContain("daily");

    const near = attentionQueue(input({ budget: meter(win({ state: "near", used: 95 })) }));
    expect(near[0]?.severity).toBe("caution");

    // AND A HEALTHY METER RAISES NOTHING — even at 99%, because the engine
    // said ok. That is what stops the first two being a check that anything
    // at all is pushed, and what proves no threshold of ours is consulted.
    expect(attentionQueue(input({ budget: meter(win({ state: "ok", used: 99 })) }))).toEqual([]);
  });

  // THE WINDOW NAMED IS THE ONE A COMPANY WAITS ON. Refused in its day and its
  // month, a company has room again only when the month turns over.
  test("a refusal names the window that turns over last", () => {
    const [item] = attentionQueue(
      input({
        budget: meter(
          win({ state: "refusing", refused_at: "2026-01-01T11:00:00Z" }),
          win({
            period: "month",
            window: "2026-01",
            resets_at: "2026-02-01T00:00:00Z",
            state: "refusing",
            refused_at: "2026-01-01T11:59:00Z",
          }),
        ),
      }),
    );
    expect(item?.title).toContain("monthly");
    expect(item?.detail).toContain("2026-02-01T00:00:00Z");
    expect(item?.at).toBe("2026-01-01T11:59:00Z");
  });

  test("a seat refusing its own ceiling is raised against the seat", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "a",
            agent_id: "id-dev-a",
            role: "Dev A",
            handle: "dev-a",
            budget: { windows: [win({ state: "refusing", refused_at: "2026-01-01T11:59:00Z" })] },
          },
        ],
      }),
    );
    expect(items[0]?.id).toBe("seat-budget-id-dev-a");
    expect(items[0]?.detail).toContain("11:59");
  });

  // THE ADVICE IS ONE AN OPERATOR CAN TAKE. The counters are windowed and
  // there is no reset: room comes from raising the ceiling or from the window
  // turning over, so an item that sent somebody looking for a reset would
  // send them to a command that no longer exists.
  test("a budget item advises raising the ceiling or waiting, never a reset", () => {
    for (const w of [
      win({ state: "refusing", used: 99, refused_at: "2026-01-01T11:59:00Z" }),
      win({ state: "near", used: 95 }),
    ]) {
      const [item] = attentionQueue(input({ budget: meter(w) }));
      expect(item?.detail).toContain("token_budget.day");
      expect(item?.detail).toContain("turn over");
      expect(item?.detail).not.toMatch(/reset/i);
    }
  });

  // WHAT THE QUIET BAND DRAWS IS TWO-SIDED, and only one side is a type error.
  //
  // TypeScript refuses a condition that names no subject. Nothing but this
  // refuses a SUBJECT no condition can raise — a phrase on the Inbox and Home
  // claiming something is checked when nothing checks it, which is the exact
  // shape of the sentence it replaced: "No seat is stopped, no run is parked on
  // a question, and no budget is refusing", three of the twelve conditions
  // here, read as all of them.
  test("every subject the quiet band draws is one a condition can raise", () => {
    const items = attentionQueue(
      input({
        engine: { status: "ok", configured: false },
        budget: meter(win({ state: "refusing", used: 100 })),
        runs: [parked("awaiting_clarification")],
        agents: [
          {
            id: "a",
            agent_id: "id-a",
            role: "Dev A",
            last_error: {
              kind: "llm_unavailable",
              message: "provider unreachable",
              phase: "execute",
              turn_id: "t1",
              at: "2026-01-01T11:59:00Z",
              event_id: "e1",
            },
          },
          {
            id: "b",
            agent_id: "id-dev-b",
            role: "Dev B",
            handle: "dev-b",
            live_call: {
              turn_id: "t2",
              phase: "execute",
              iteration: 1,
              model: "",
              trigger: null,
              prompt: "",
              prompt_messages: null,
              response: "",
              input_tokens: 0,
              output_tokens: 0,
              total_tokens: 0,
              tool_executions: null,
              round_num: 3,
              rounds_used: 3,
              in_progress: true,
              updated_at: new Date(now - 900_000).toISOString(),
            },
          },
        ],
      }),
    );
    expect(new Set(items.map((i) => i.subject))).toEqual(new Set(Object.keys(SUBJECTS)));
  });

  // AND EACH PLACE'S CLAUSE CARRIES ITS OWN SUBJECTS AND NO OTHER. A quiet
  // Inbox drops one string in, so a join that lost its last item would read
  // as a complete sentence — and one that carried Live's subjects would claim
  // the Inbox checked what it does not list.
  test("each place's clause carries exactly the subjects shown there", () => {
    for (const where of ["seat", "engine", "live"] as Where[]) {
      const clause = watchedIn(where);
      for (const [subject, phrase] of Object.entries(SUBJECTS) as [Subject, string][]) {
        if (WHERE_OF[subject] === where) expect(clause, subject).toContain(phrase);
        else expect(clause, subject).not.toContain(phrase);
      }
    }
  });
});

// ONE HOME PER SUBJECT. A condition drawn in two places is two places to keep
// agreeing about it, and one drawn in none is a condition nobody sees: the
// stalled round that used to be a "seat" row went to the Inbox, which is not
// where a round that has stopped moving is decided.
describe("where a condition is shown", () => {
  test("every subject has exactly one home, and every home holds one", () => {
    const homes = new Set<Where>(["seat", "engine", "live"]);
    for (const subject of Object.keys(SUBJECTS) as Subject[]) {
      expect(homes.has(WHERE_OF[subject]), subject).toBe(true);
    }
    expect(new Set(Object.values(WHERE_OF))).toEqual(homes);
    expect(Object.keys(WHERE_OF).sort()).toEqual(Object.keys(SUBJECTS).sort());
  });

  test("a stalled round is Live's, and a person decides only the seat's", () => {
    const items = attentionQueue(
      input({
        engine: { status: "ok", configured: false },
        agents: [
          {
            id: "b",
            agent_id: "id-dev-b",
            role: "Dev B",
            handle: "dev-b",
            live_call: {
              turn_id: "t2",
              phase: "execute",
              iteration: 1,
              model: "",
              trigger: null,
              prompt: "",
              prompt_messages: null,
              response: "",
              input_tokens: 0,
              output_tokens: 0,
              total_tokens: 0,
              tool_executions: null,
              round_num: 3,
              rounds_used: 3,
              in_progress: true,
              updated_at: new Date(now - 900_000).toISOString(),
            },
          },
          {
            id: "c",
            agent_id: "id-dev-c",
            role: "Dev C",
            handle: "dev-c",
            activity: "stopped",
            stopped_reason: "paused",
          },
        ],
      }),
    );
    const stalled = items.find((i) => i.id.startsWith("stale-"));
    expect(stalled?.subject).toBe("round");
    expect(WHERE_OF[stalled!.subject]).toBe("live");
    // THE INBOX'S ROWS: the stopped seat, and neither the engine's own
    // condition nor the round.
    expect(conditionsToDecide(items).map((i) => i.id)).toEqual(["stopped-id-dev-c"]);
  });
});

describe("ordering", () => {
  test("severity first, then newest — what it costs to ignore", () => {
    const items = attentionQueue(
      input({
        connected: false,
        budget: meter(win({ state: "near", used: 95 })),
        agents: [
          {
            id: "a",
            agent_id: "id-a",
            role: "Dev A",
            last_error: {
              kind: "llm_unavailable",
              message: "provider unreachable",
              phase: "execute",
              turn_id: "t1",
              at: "2026-01-01T11:59:00Z",
              event_id: "e1",
            },
          },
        ],
      }),
    );
    const severities = items.map((i) => i.severity);
    expect(severities).toEqual(
      [...severities].sort((a, b) => (a === b ? 0 : a === "critical" ? -1 : 1)),
    );
    expect(items.some((i) => i.detail.includes("provider unreachable"))).toBe(true);
  });

  test("every id is unique, so a list can key on it", () => {
    const items = attentionQueue(
      input({
        connected: false,
        engine: { status: "ok", configured: false },
        agents: [
          // TWO SEATS SHARING A NAME: keyed by the name, their rows were one id.
          {
            id: "a",
            agent_id: "id-a",
            role: "Engineer",
            activity: "stopped",
            stopped_reason: "provider",
          },
          {
            id: "b",
            agent_id: "id-b",
            role: "Engineer",
            activity: "stopped",
            stopped_reason: "provider",
          },
        ],
      }),
    );
    expect(new Set(items.map((i) => i.id)).size).toBe(items.length);
  });

  test("every item says what it costs to leave it", () => {
    const items = attentionQueue(
      input({ connected: false, engine: { status: "ok", configured: false } }),
    );
    for (const item of items) {
      expect(item.title.length, item.id).toBeGreaterThan(8);
      expect(item.detail.length, item.id).toBeGreaterThan(20);
    }
  });
});

// WHO PAUSED A SEAT IS SAID BY NAME. The engine names the pauser as it records
// every author — a person bound to a seat by that seat's handle — and the row
// read "paused by jane-founder" on Home and in the Inbox — an address where a
// person is meant.
describe("a stopped seat's row", () => {
  test("names the person who paused it, off the chart", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "devrel",
            agent_id: "id-agent-devrel",
            role: "Agent DevRel",
            handle: "agent-devrel",
            activity: "stopped",
            stopped_reason: "paused",
            paused: {
              by: "jane-founder",
              by_kind: "human",
              at: "2026-01-01T11:00:00Z",
              stop_running: false,
            },
          },
        ],
        nameOf: (key) => (key === "jane-founder" ? "Jane Founder" : key),
      }),
    );
    expect(items.find((i) => i.id === "stopped-id-agent-devrel")?.detail).toBe(
      "The seat cannot take work: paused by Jane Founder.",
    );
  });
});
