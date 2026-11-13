// @vitest-environment node

/**
 * The attention queue: what needs a person, in one list.
 *
 * Every condition here was already known to the previous dashboard and each
 * lived in a different screen — a paused executor was a badge on the board
 * (and one whose box had been reclaimed appeared NOWHERE), a stopped seat was
 * a card among the healthy ones, a refusing budget was a bar on one seat's
 * page, and an engine with no active configuration was a line in a popover.
 */

import { describe, expect, test } from "vitest";
import type { AtlasReviewItem } from "~/contract/atlas.ts";
import type { BudgetWindow, OrgBudget } from "~/protocol/index.ts";
import type { McpAlarm } from "~/contract/mcp.ts";
import type { AdvisorNotice } from "~/protocol/advisor.ts";
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
import { healthFrame } from "~/test/health.ts";
import { ZERO_VERSIONS } from "~/test/liveCall.ts";
import { fmtAt } from "./format.ts";

const now = Date.parse("2026-01-01T12:00:00Z");

/** A setup finding that blocks the company and is the owner's to fix. */
const SETUP_BLOCKER = {
  class: "integration.credential_rejected",
  severity: "blocks" as const,
  actor: "owner" as const,
  source: "reconcile" as const,
  area: "integrations",
  subject: { kind: "integration", name: "Datadog" },
  title: "Datadog refused the credential.",
  why: "The agents' Datadog tools are off.",
  path: "integrations.datadog",
  evidence: [],
  fix: { kind: "external_step", sentence: "Replace the credential.", options: [] },
};

/** The notice the engine sends for SETUP_BLOCKER. */
const SETUP_NOTICE = { class: SETUP_BLOCKER.class, subject: SETUP_BLOCKER.subject };

/** An alarm's notice, as the engine sends one that has stood past its threshold. */
function alarmNotice(kind: string, nodes = ["node-a"]): AdvisorNotice {
  return {
    class: "alarm",
    subject: { kind: "alarm", name: kind },
    since: "2026-01-01T10:00:00Z",
    nodes,
  };
}

function input(over: Partial<AttentionInput> = {}): AttentionInput {
  return {
    agents: [],
    runs: [],
    budget: null,
    engine: healthFrame({ configured: true }),
    connected: true,
    authRejected: false,
    now,
    nameOf: (key) => key,
    mcpAlarms: [],
    notices: [],
    findings: [],
    setupUnread: [],
    atlasDrift: [],
    ...over,
  };
}

/** A value ada set in the atlas, which a newer read by an integration differs from. */
const DRIFT_ENTRY = {
  id: "0192f3a0-7c1e-7b3a-9d2e-5f6a7b8c9d0e",
  name: "web-1",
} as AtlasReviewItem["entry"];
const DRIFT: AtlasReviewItem = {
  id: `${DRIFT_ENTRY.id}/disagreement/fact/address`,
  kind: "disagreement",
  entry: DRIFT_ENTRY,
  fact: {
    fact: "address",
    value: { type: "ip", text: "10.0.4.12" },
    source: { kind: "person", name: "ada" },
    confidence: "asserted",
    word: "confirmed",
    observed_at: "2026-01-01T09:00:00Z",
    standing: "contested",
    others: [
      {
        value: { type: "ip", text: "10.0.4.19" },
        source: { kind: "integration", name: "aws" },
        confidence: "verified",
        observed_at: "2026-01-01T10:00:00Z",
      },
    ],
  } as AtlasReviewItem["fact"],
  since: "2026-01-01T10:00:00Z",
};

/** One MCP alarm as `mcp_servers_status` carries it, on one server. */
function mcpAlarm(
  kind: McpAlarm["kind"],
  key: string,
  over: Partial<McpAlarm["entries"][number]> = {},
): McpAlarm {
  const entry = {
    key,
    detail: `the engine's measurement of ${key}`,
    since: "2026-01-01T11:50:00Z",
    seat: "",
    fix: { kind: "restart" as const, server: key, seat: "" },
    ...over,
  };
  return { kind, detail: entry.detail, remedy: "What to do.", entries: [entry] };
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
    const items = attentionQueue(input({ engine: healthFrame({ configured: false }) }));
    expect(items[0]?.id).toBe("unconfigured");
    expect(items[0]?.severity).toBe("critical");
    expect(items[0]?.detail).toBe("Import a company to start.");
  });

  test("a refused token and an unreachable engine are different items", () => {
    // The repair differs: one comes back on its own, the other never does.
    expect(attentionQueue(input({ connected: false }))[0]?.id).toBe("offline");
    expect(attentionQueue(input({ authRejected: true }))[0]?.id).toBe("auth");
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
      harness: "claude-code",
      placement: "direct",
      task_description: "",
      question: "Which branch should I target?",
      question_context: "",
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
    // THE RUN'S OWN ADDRESS. It was a `?run=` query on the board's list, a
    // query key the board stopped reading when a run became an object.
    expect(items[0]?.path).toEqual(["live", "executors", "t1"]);
  });

  // AN ENDED RUN WAITS ON NOBODY. A run whose ending is decided keeps its
  // record, and the status it was ended in, until the ending is finished; it
  // is over, and no answer reaches it, so it is never somebody's to answer.
  //
  // Mutation: queue a parked run off its status alone, and the ended run is
  // listed as waiting on an answer.
  test("a parked run whose ending is decided is not waiting on anybody", () => {
    const ending = { at: "2026-01-01T11:30:00Z", reason: "seat_removed", stopped_by: "" };
    const items = attentionQueue(input({ runs: [parked("awaiting_clarification", { ending })] }));
    expect(items.some((item) => item.subject === "run")).toBe(false);
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
    expect(items[0]?.detail).toContain("Answering restarts the work.");
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
      agent_id: "a",
      role: "Dev A",
      handle: "dev-a",
      live_call: {
        versions: ZERO_VERSIONS,
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
    // AS A PERSON READS A MOMENT, never the wire's timestamp.
    expect(item?.detail).toContain(fmtAt("2026-02-01T00:00:00Z", now));
    expect(item?.detail).not.toContain("T00:00:00Z");
    expect(item?.at).toBe("2026-01-01T11:59:00Z");
    // THE CEILING ITSELF, where it is raised, and no link to what was spent.
    expect(item?.remedy).toEqual({ setting: "company_token_budget" });
    expect(item?.path).toBeUndefined();
  });

  test("a seat refusing its own ceiling is raised against the seat", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "a",
            agent_id: "a",
            role: "Dev A",
            handle: "dev-a",
            budget: { windows: [win({ state: "refusing", refused_at: "2026-01-01T11:59:00Z" })] },
          },
        ],
      }),
    );
    expect(items[0]?.id).toBe("seat-budget-a");
    expect(items[0]?.detail).toBe(
      `10 of 100 tokens spent. Raise the limit, or wait until ${fmtAt("2026-01-02T00:00:00Z", now)}.`,
    );
    expect(items[0]?.remedy).toEqual({ setting: "seat_token_budget", seat: "dev-a" });
    // The gate's stamp is the row's age rather than a clause of its sentence.
    expect(items[0]?.at).toBe("2026-01-01T11:59:00Z");
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
      expect(item?.detail).toContain("Raise the company limit");
      expect(item?.detail).toContain("or wait until");
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
        mcpAlarms: [mcpAlarm("mcp_server_failing", "github")],
        atlasDrift: [DRIFT],
        notices: [
          SETUP_NOTICE,
          alarmNotice("backup_age"),
          { class: "conversation.held", subject: { kind: "seat", seat: "dev", name: "task:t-1" } },
        ],
        findings: [SETUP_BLOCKER],
        engine: healthFrame({ configured: false }),
        budget: meter(win({ state: "refusing", used: 100 })),
        runs: [parked("awaiting_clarification")],
        agents: [
          {
            id: "a",
            agent_id: "a",
            role: "Dev A",
            last_error: {
              kind: "llm_unavailable",
              detail: "provider unreachable",
              phase: "execute",
              turn_id: "t1",
              at: "2026-01-01T11:59:00Z",
              event_id: "e1",
            },
          },
          {
            id: "b",
            agent_id: "b",
            role: "Dev B",
            handle: "dev-b",
            live_call: {
              versions: ZERO_VERSIONS,
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

  // AND A SUBJECT THIS READER IS NOT SHOWN IS NOT CLAIMED: a reader the MCP
  // section does not open for is never asked its alarms.
  test("a subject the reader is not shown is left out of the clause", () => {
    expect(watchedIn("seat")).toContain(SUBJECTS.tools);
    const clause = watchedIn("seat", ["tools"]);
    expect(clause).not.toContain(SUBJECTS.tools);
    expect(clause).toContain(SUBJECTS.seat);
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
        engine: healthFrame({ configured: false }),
        agents: [
          {
            id: "b",
            agent_id: "b",
            role: "Dev B",
            handle: "dev-b",
            live_call: {
              versions: ZERO_VERSIONS,
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
            agent_id: "c",
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
    expect(conditionsToDecide(items).map((i) => i.id)).toEqual(["seat-state-c"]);
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
            agent_id: "a",
            role: "Dev A",
            last_error: {
              kind: "llm_unavailable",
              detail: "provider unreachable",
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
    // THE ENGINE'S RAW MESSAGE rides along for the disclosure, never as the detail.
    expect(items.some((i) => i.technical === "provider unreachable")).toBe(true);
    expect(items.some((i) => i.detail.includes("provider unreachable"))).toBe(false);
  });

  test("every id is unique, so a list can key on it", () => {
    const items = attentionQueue(
      input({
        connected: false,
        engine: healthFrame({ configured: false }),
        agents: [
          { id: "a", agent_id: "a", role: "A", activity: "stopped", stopped_reason: "provider" },
          { id: "b", agent_id: "b", role: "B", activity: "stopped", stopped_reason: "provider" },
        ],
      }),
    );
    expect(new Set(items.map((i) => i.id)).size).toBe(items.length);
  });

  test("every item says what it costs to leave it", () => {
    const items = attentionQueue(
      input({ connected: false, engine: healthFrame({ configured: false }) }),
    );
    for (const item of items) {
      expect(item.title.length, item.id).toBeGreaterThan(8);
      expect(item.detail.length, item.id).toBeGreaterThan(20);
    }
  });
});

// WHO PAUSED A SEAT IS SAID BY NAME. The engine names the pauser as it names
// every author (`iam.ActorFor`) — the seat handle a person is bound to — and
// the row read "paused by jane-founder" on Home and in the Inbox — an address
// where a person is meant.
describe("a stopped seat's row", () => {
  test("names the person who paused it, off the chart", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "devrel",
            agent_id: "devrel",
            role: "Agent DevRel",
            handle: "agent-devrel",
            activity: "stopped",
            stopped_reason: "paused",
            paused: {
              by_kind: "human",
              by: "jane-founder",
              at: "2026-01-01T11:00:00Z",
              stop_running: false,
            },
          },
        ],
        nameOf: (key) => (key === "jane-founder" ? "Jane Founder" : key),
      }),
    );
    expect(items.find((i) => i.id === "seat-state-devrel")?.detail).toBe(
      "The seat cannot take work: paused by Jane Founder.",
    );
  });

  // A PAUSE WAITS ON A PERSON: every condition said "Clears on its own" under
  // a "Go to it" button, a pause an admin must lift included.
  test("says a person lifts a pause or a missing placement, and names its button", () => {
    const seat = (id: string, stopped_reason: "paused" | "unplaced") => ({
      id,
      agent_id: id,
      role: id,
      handle: id,
      activity: "stopped" as const,
      stopped_reason,
    });
    const items = attentionQueue(input({ agents: [seat("p", "paused"), seat("u", "unplaced")] }));
    const paused = items.find((i) => i.id === "seat-state-p");
    const unplaced = items.find((i) => i.id === "seat-state-u");
    expect(paused?.clears).toBe("Clears once someone resumes the seat.");
    expect(paused?.action).toBe("Open the seat to resume it");
    expect(unplaced?.clears).toBe("Clears once an admin places the seat on a node.");
    for (const item of [paused, unplaced]) expect(item?.clears).not.toMatch(/on its own/);
  });

  // EACH ROW IS ITS SEAT'S, BY AGENT ID: two seats sharing a name that both
  // stopped were one row, and the queue said one seat was down when two were.
  test("two stopped seats that share a name are two rows", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "one",
            agent_id: "one",
            role: "Support",
            handle: "support",
            activity: "stopped",
            stopped_reason: "provider",
          },
          {
            id: "two",
            agent_id: "two",
            role: "Support",
            handle: "support-2",
            activity: "stopped",
            stopped_reason: "provider",
          },
        ],
      }),
    );
    expect(items.filter((i) => i.id.startsWith("seat-state-")).map((i) => i.id)).toEqual([
      "seat-state-one",
      "seat-state-two",
    ]);
  });
});

// A MODEL AT ITS USAGE LIMIT IS ONE ROW FOR ITS ENTRY, NOT ONE PER SEAT, and a
// seat's own row keeps its id whichever condition it is in. While the engine
// retried a spent subscription every second, each seat flipped between a
// "stopped" row and a "last turn failed" row under two ids, which a list keyed
// on the id drew as one row leaving and another arriving.
describe("a model at its usage limit", () => {
  const limit = {
    parked: true,
    resets_at: "2026-01-01T12:30:00Z",
    since: "2026-01-01T11:59:00Z",
    harness: "claude-code",
    subscription: true,
  };
  const failed = {
    kind: "rate_limit",
    detail: "llm chain exhausted after 1 provider (default)",
    phase: "execute",
    turn_id: "t1",
    at: "2026-01-01T11:59:30Z",
    event_id: "e1",
  };

  test("is one condition for the entry, and no row of each seat's", () => {
    const items = attentionQueue(
      input({
        agents: [
          {
            id: "pm",
            agent_id: "pm",
            role: "PM",
            activity: "stopped",
            stopped_reason: "provider",
            provider_limit: limit,
            last_error: failed,
          },
          {
            id: "qa",
            agent_id: "qa",
            role: "QA",
            activity: "idle",
            provider_limit: limit,
            last_error: failed,
          },
        ],
      }),
    );
    const decide = conditionsToDecide(items);
    expect(decide.map((i) => i.id)).toEqual(["provider-limit-claude-code-plan"]);
    expect(decide[0]?.title).toBe("Claude Code subscription limit reached");
    expect(decide[0]?.remedy).toEqual({ setting: "model_providers" });
  });

  test("a seat's own row keeps its id between stopped and failed", () => {
    const stopped = attentionQueue(
      input({
        agents: [
          { id: "a", agent_id: "a", role: "A", activity: "stopped", stopped_reason: "unplaced" },
        ],
      }),
    );
    const failedTurn = attentionQueue(
      input({
        agents: [{ id: "a", agent_id: "a", role: "A", activity: "idle", last_error: failed }],
      }),
    );
    expect(stopped.map((i) => i.id)).toEqual(["seat-state-a"]);
    expect(failedTurn.map((i) => i.id)).toEqual(["seat-state-a"]);
  });
});

// AN MCP ALARM IS A ROW AN ADMINISTRATOR DECIDES, one per server it stands on,
// in the engine's own judgement: the span, the server and the agent are the
// engine's, and the row only words them, links the server's page and keeps the
// measurement behind the disclosure. Not running is critical for whoever it
// stops; a wait and a refused name cost their tools and nothing more.
describe("an MCP server's alarms", () => {
  // THE ENGINE'S FIX DECIDES WHERE THE ROW OPENS: an alarm a sign-in clears
  // opens the server's Sign in dialog, every other one the server's page.
  // Mutation: open the sign-in for every alarm, or for none, and this is red.
  test("an alarm a sign-in fixes opens the sign-in", () => {
    const items = attentionQueue(
      input({
        mcpAlarms: [
          mcpAlarm("mcp_oauth_needs_sign_in", "linear", {
            fix: { kind: "sign_in", server: "linear", seat: "" },
          }),
          mcpAlarm("mcp_server_failing", "github"),
        ],
      }),
    );
    const byKey = new Map(conditionsToDecide(items).map((i) => [i.id, i]));
    const signIn = byKey.get("mcp-mcp_oauth_needs_sign_in-linear");
    expect(signIn?.path).toEqual(["settings", "mcp-tools", "servers", "linear"]);
    expect(signIn?.query).toEqual({ flow: "signin" });
    expect(byKey.get("mcp-mcp_server_failing-github")?.query).toBeUndefined();
  });

  test("each server an alarm stands on is a row in Needs you", () => {
    const items = attentionQueue(
      input({
        mcpAlarms: [
          mcpAlarm("mcp_server_failing", "github"),
          mcpAlarm("mcp_server_partial", "jira", { seat: "backend-dev" }),
          mcpAlarm("mcp_server_waiting_long", "atlassian", { seat: "pm" }),
          mcpAlarm("mcp_tools_refused", "linear", { since: null }),
        ],
      }),
    );
    const decided = conditionsToDecide(items);
    expect(decided.map((i) => i.id).sort()).toEqual(
      [
        "mcp-mcp_server_failing-github",
        "mcp-mcp_server_partial-jira",
        "mcp-mcp_server_waiting_long-atlassian",
        "mcp-mcp_tools_refused-linear",
      ].sort(),
    );
    const byKey = new Map(decided.map((i) => [i.id, i]));
    const failing = byKey.get("mcp-mcp_server_failing-github");
    expect(failing?.severity).toBe("critical");
    expect(failing?.title).toBe("github is not running");
    expect(failing?.path).toEqual(["settings", "mcp-tools", "servers", "github"]);
    expect(failing?.technical).toBe("the engine's measurement of github");
    expect(failing?.at).toBe("2026-01-01T11:50:00Z");
    const partial = byKey.get("mcp-mcp_server_partial-jira");
    expect(partial?.severity).toBe("critical");
    expect(partial?.title).toBe("jira is not running for backend-dev");
    expect(partial?.who).toBe("backend-dev");
    const waiting = byKey.get("mcp-mcp_server_waiting_long-atlassian");
    expect(waiting?.severity).toBe("caution");
    expect(waiting?.title).toBe("atlassian is still waiting for pm's credential");
    const refused = byKey.get("mcp-mcp_tools_refused-linear");
    expect(refused?.severity).toBe("caution");
    expect(refused?.at).toBeUndefined();
  });

  test("a partial shared server names the nodes, not an agent", () => {
    const [row] = attentionQueue(input({ mcpAlarms: [mcpAlarm("mcp_server_partial", "docs")] }));
    expect(row?.title).toBe("docs is not running on every node");
    expect(row?.who).toBeUndefined();
  });
});

// THE SETUP'S BLOCKERS ARE A PERSON'S, in the Inbox and on Home: each opens the
// advisor, says what to do, and clears when the setup no longer raises it.
test("a finding that blocks the company is a condition a person decides", () => {
  const queue = attentionQueue(input({ notices: [SETUP_NOTICE], findings: [SETUP_BLOCKER] }));
  const row = queue.find((a) => a.subject === "setup");
  expect(row?.title).toBe("Datadog refused the credential.");
  expect(row?.detail).toBe("Replace the credential.");
  expect(row?.path).toEqual(["settings", "advisor"]);
  expect(conditionsToDecide(queue)).toContainEqual(row);
});

// AN ALARM'S FINDING THE ANSWER COULD NOT RAISE (its node has not evaluated
// yet) is still said, in the alarm's words, never dropped without a word.
test("a notice an alarm raised is drawn as that alarm when its finding is missing", () => {
  const queue = attentionQueue(
    input({
      notices: [
        {
          class: "durability.no_verified_backup",
          subject: { kind: "company" },
          since: "2026-01-01T10:00:00Z",
          nodes: ["node-a"],
          alarm: "backup_age",
        },
      ],
    }),
  );
  const row = queue.find((a) => a.id === "alarm-backup_age");
  expect(row?.title).toBe("Backup age has not cleared");
  expect(row?.technical).toBe("Standing on node-a.");
  expect(row?.at).toBe("2026-01-01T10:00:00Z");
  expect(conditionsToDecide(queue)).toContainEqual(row);
});

// ONLY WHAT THE ENGINE NOTICED REACHES THE QUEUE: a blocking finding with no
// notice is the advisor's to show, and a notice whose finding is gone draws nothing.
test("a finding is a condition only through its notice", () => {
  expect(
    attentionQueue(input({ findings: [SETUP_BLOCKER] })).filter((a) => a.subject === "setup"),
  ).toEqual([]);
  expect(
    attentionQueue(input({ notices: [SETUP_NOTICE] })).filter((a) => a.subject === "setup"),
  ).toEqual([]);
});

// AN ALARM THAT HAS STOOD REACHES THE ADMINISTRATORS: one row a kind, opening
// the section that draws it, with the nodes it stands on and since when; an MCP
// alarm is its server's row and is not drawn twice.
test("an alarm that has stood is a condition a person decides", () => {
  const queue = attentionQueue(
    input({
      notices: [
        alarmNotice("backup_age", ["node-a", "node-b"]),
        alarmNotice("llm_sign_in_refused"),
        alarmNotice("mcp_server_failing"),
      ],
    }),
  );
  const backup = queue.find((a) => a.id === "alarm-backup_age");
  expect(backup?.title).toBe("Backup age has not cleared");
  expect(backup?.path).toEqual(["settings", "backups"]);
  expect(backup?.query).toEqual({ at: "alarms" });
  expect(backup?.technical).toBe("Standing on node-a, node-b.");
  expect(backup?.at).toBe("2026-01-01T10:00:00Z");
  expect(queue.find((a) => a.id === "alarm-llm_sign_in_refused")?.path).toEqual([
    "settings",
    "models",
  ]);
  expect(queue.find((a) => a.id === "alarm-mcp_server_failing")).toBeUndefined();
  expect(conditionsToDecide(queue)).toContainEqual(backup);
});

// A SOURCE THE ADVISOR COULD NOT READ IS ONE ROW, saying what it leaves out, so
// a blocker it would have raised never drops out of the queue unsaid.
test("an advisor that could not read everything says so, once", () => {
  expect(attentionQueue(input()).filter((a) => a.subject === "setup")).toEqual([]);
  const queue = attentionQueue(input({ setupUnread: ["alarms", "work"] }));
  const rows = queue.filter((a) => a.id === "setup-unread");
  expect(rows).toHaveLength(1);
  expect(rows[0]?.title).toBe("The advisor couldn't read everything");
  expect(rows[0]?.detail).toMatch(/whether a backup is missing can't be said yet/);
  expect(rows[0]?.detail).toMatch(/last 7 days of work couldn't be read/);
  const paused = attentionQueue(input({ setupUnread: ["pauses"] })).find(
    (a) => a.id === "setup-unread",
  );
  expect(paused?.detail).toMatch(/an agent the engine paused for a failure may be missing/);
  const runs = attentionQueue(input({ setupUnread: ["runs"] })).find(
    (a) => a.id === "setup-unread",
  );
  expect(runs?.detail).toMatch(/a sandbox that ran out of memory may be missing/);
  expect(rows[0]?.path).toEqual(["settings", "advisor"]);
});

// A CONVERSATION THE ENGINE HOLDS REACHES THE ADMINISTRATORS: one row a hold,
// saying there is nothing to decide and what releases it, opening its item.
test("a held conversation is a row that opens its item", () => {
  const queue = attentionQueue(
    input({
      notices: [
        {
          class: "conversation.held",
          subject: { kind: "seat", seat: "dev", name: "task:t-1" },
          since: "2026-01-01T10:00:00Z",
        },
        { class: "conversation.held", subject: { kind: "seat", seat: "dev", name: "chat:room/1" } },
      ],
    }),
  );
  const item = queue.find((a) => a.id === "held-dev-task:t-1");
  expect(item?.title).toBe("Held by the engine: its context did not fit its models");
  expect(item?.path).toEqual(["worklet", "t-1"]);
  expect(item?.who).toBe("dev");
  expect(item?.detail).toContain("There is nothing for you to decide.");
  expect(queue.find((a) => a.id === "held-dev-chat:room/1")?.path).toEqual([
    "company",
    "org",
    "seats",
    "dev",
  ]);
  expect(conditionsToDecide(queue)).toContainEqual(item);
});

test("a newer report that differs from what the reader set is theirs to decide, and opens the entry", () => {
  const queue = attentionQueue(input({ atlasDrift: [DRIFT] }));
  const item = queue.find((a) => a.id === `atlas-drift-${DRIFT.id}`);
  expect(item?.subject).toBe("atlas");
  expect(item?.title).toBe("web-1: a newer report differs from what you set");
  expect(item?.detail).toBe(
    'You set "10.0.4.12". A newer report says "10.0.4.19". The atlas shows both until somebody decides.',
  );
  expect(item?.path).toEqual(["company", "atlas", DRIFT_ENTRY.id]);
  expect(item?.query).toEqual({ tab: "sources" });
  expect(item?.at).toBe("2026-01-01T10:00:00Z");
  expect(conditionsToDecide(queue)).toContainEqual(item);
  // NOTHING ELSE OF THE REVIEW REACHES AN INBOX: with no drift there is no row.
  expect(attentionQueue(input()).some((a) => a.subject === "atlas")).toBe(false);
});
