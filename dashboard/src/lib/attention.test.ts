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
import { attentionQueue, SUBJECTS, WATCHED, type AttentionInput } from "./attention.ts";

const now = Date.parse("2026-01-01T12:00:00Z");

function input(over: Partial<AttentionInput> = {}): AttentionInput {
  return {
    agents: [],
    sandboxes: [],
    runs: [],
    budget: {},
    engine: { status: "ok", configured: true },
    connected: true,
    authRejected: false,
    now,
    ...over,
  };
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

  test("a refused token and an unreachable engine are different items", () => {
    // The repair differs: one comes back on its own, the other never does.
    expect(attentionQueue(input({ connected: false }))[0]?.id).toBe("offline");
    expect(attentionQueue(input({ authRejected: true }))[0]?.id).toBe("auth");
  });

  // A REFUSED CREDENTIAL IS REPAIRED BY SIGNING IN AS OFTEN AS BY A TOKEN. The
  // item said to "set a token matching one of the api.auth.tokens entries",
  // which a person whose session had ended could not act on at all.
  test("a refused credential names both repairs and no operator token", () => {
    const auth = attentionQueue(input({ authRejected: true }))[0];
    expect(auth?.detail).toContain("Sign in again");
    expect(auth?.detail).toContain("crewlet iam token");
    expect(`${auth?.title} ${auth?.detail}`).not.toMatch(/operator token/);
  });

  // THE ENGINE'S OWN WORD. This case asserted `awaiting_input`, which
  // `sandbox.PendingRun` cannot write — its statuses are `launching`,
  // `running`, `awaiting_clarification`, `resumed`, `done`, `failed` and
  // `reseed` — so the condition it was guarding never fired on a real run and
  // the test passed against a fixture no engine produces.
  //
  // AND FROM THE DURABLE ROW, not the projection. The live push sweeps a
  // sandbox entry after twelve hours, so a run parked on a question — the
  // longest-lived item this list can hold, since it is waiting for a person —
  // used to leave the queue exactly when it had been ignored long enough to
  // matter, and the dashboard reported a quiet company.
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
    // THE RUN'S OWN ADDRESS. It was `#/activity/runs?run=`, a query key the
    // runs screen stopped reading when a run became an object.
    expect(items[0]?.path).toEqual(["activity", "runs", "t1"]);
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

  // THE PROJECTION IS NOT A SOURCE FOR THIS. A parked entry on the live push
  // and nothing on the durable rows means the sweep already dropped it — and
  // the queue must read the rows, so this produces nothing.
  test("a parked entry on the live push alone raises nothing", () => {
    const items = attentionQueue(
      input({
        sandboxes: [
          {
            turn_id: "t1",
            role: "Dev A",
            agent_handle: "dev-a",
            agent_id: "",
            coding_agent: "claude-code",
            sandbox_id: "s1",
            task: "",
            status: "awaiting_clarification",
            started_at: "2026-01-01T11:00:00Z",
            question: "Which branch should I target?",
          },
        ],
      }),
    );
    expect(items.filter((i) => i.subject === "run")).toEqual([]);
  });

  test("a live round that stopped moving is surfaced, and escalates", () => {
    // A spinning row hides exactly this: the animation is identical whether
    // the round started two seconds or eleven minutes ago.
    const call = (updated: string) => ({
      id: "a",
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
        rounds: 3,
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
              rounds: 0,
            },
          },
        ],
      }),
    );
    expect(opening[0]?.detail).toContain("starting");
    expect(opening[0]?.detail).not.toContain("?");

    const stalled = attentionQueue(
      input({ agents: [call(new Date(now - 900_000).toISOString())] }),
    );
    expect(stalled[0]?.severity).toBe("critical");
  });

  // A SPENT BUDGET OUTRANKS ONE MERELY NEAR ITS CAP, and both are read off
  // the shared counter.
  //
  // They used to key on a `refused_at` the engine never wrote, so the critical
  // branch was unreachable on every company — the exact case an operator needs
  // it for. At or past the cap is what a fleet-wide counter can honestly say:
  // sufficient, and not necessary, which is what the 90% rung below it is for.
  test("a spent budget outranks one merely near its cap", () => {
    const spent = attentionQueue(input({ budget: { org: { used: 100, max: 100 } } }));
    expect(spent[0]?.severity).toBe("critical");

    const near = attentionQueue(input({ budget: { org: { used: 95, max: 100 } } }));
    expect(near[0]?.severity).toBe("caution");

    // AND A HEALTHY METER RAISES NOTHING, which is what stops the first
    // two being a check that anything at all is pushed.
    expect(attentionQueue(input({ budget: { org: { used: 10, max: 100 } } }))).toEqual([]);
  });

  // WHAT THE QUIET BAND DRAWS IS TWO-SIDED, and only one side is a type error.
  //
  // TypeScript refuses a condition that names no subject. Nothing but this
  // refuses a SUBJECT no condition can raise — a phrase on the landing screen
  // claiming something is checked when nothing checks it, which is the exact
  // shape of the sentence it replaced: "No seat is stopped, no run is parked on
  // a question, and no budget is refusing", three of the twelve conditions
  // here, read as all of them.
  test("every subject the quiet band draws is one a condition can raise", () => {
    const items = attentionQueue(
      input({
        engine: { status: "ok", configured: false },
        budget: { org: { used: 100, max: 100 } },
        runs: [parked("awaiting_clarification")],
        agents: [
          {
            id: "a",
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
    expect(new Set(items.map((i) => i.subject))).toEqual(new Set(Object.keys(SUBJECTS)));
  });

  // AND THE CLAUSE CARRIES ALL OF THEM. The band drops one string in, so a join
  // that lost its last item would read as a complete sentence.
  test("the clause the screen drops in carries every subject", () => {
    for (const phrase of Object.values(SUBJECTS)) expect(WATCHED).toContain(phrase);
  });
});

describe("ordering", () => {
  test("severity first, then newest — what it costs to ignore", () => {
    const items = attentionQueue(
      input({
        connected: false,
        budget: { org: { used: 95, max: 100 } },
        agents: [
          {
            id: "a",
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
          { id: "a", role: "A", state: "afk", afk_reason: "stall" },
          { id: "b", role: "B", state: "afk", afk_reason: "stall" },
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
