/**
 * FairSplit: the base demo company every topic starts from.
 *
 * It reproduces the company the design screenshots were taken of, in shape
 * and in state, with invented values throughout. Miles is a human seat at the
 * root, his Tier A token `review` bound to it and signed in as the browser,
 * and the Product and Engineering team
 * (Crewlet-PM, the crewlet character in rose, leading Crewlet-SWE, the gemlet
 * in cyan) is building the FairSplit MVP on the FAIR board. Miles paused both
 * agents a little over a day ago, so the chart reads two stopped seats; two
 * scope decisions wait in his inbox beside four updates; one schedule is
 * declared; the engine reports one alarm (no backup taken yet); and no
 * integration is connected.
 *
 * EVERY TIME IS RELATIVE TO `now`, so a page opened next month reads "paused
 * 25h ago" as it did the day it was written. No value here is a secret: every
 * credential is a `${VAR}` reference to a value that does not exist.
 */

import { emptyArtifacts } from "../harness/engine/artifacts.ts";
import type { AgentMemory } from "~/contract/memory.ts";
import type {
  EventRecord,
  Overlay,
  RevisionMeta,
  SeatActivityDay,
  SetupListing,
  ToolRow,
  TurnRow,
  WorkChange,
  WorkComment,
  WorkDecision,
  WorkInboxNotice,
  WorkItem,
} from "~/protocol/types.ts";
import type { DemoCompany, DemoItem, DemoPage, DemoSeat } from "../harness/engine/company.ts";
import { scheduleRows } from "../harness/engine/derive.ts";
import { agentIdFor, fixtureId } from "../harness/engine/ids.ts";
import { emptyDirectory, signInWithToken } from "../harness/engine/iam.ts";
import { tokenSession } from "../harness/engine/principal.ts";
import { fireRecords } from "../harness/engine/schedules.ts";
import { addDays, dateIn, type DemoSpendDay } from "../harness/engine/spend.ts";
import { BUILTIN_TYPES, STATUSES, TRACKER_STREAM } from "../harness/engine/work.ts";
import { fairsplitDocument } from "./fairsplit.document.ts";
import noAddress from "./engine/setup.no-address.json";

const M = 60_000;
const H = 60 * M;
const D = 24 * H;

const ORG = "FairSplit";
const ZONE = "America/Los_Angeles";
const NODE = "node-0";
const PM = "pm";
const SWE = "engineer";
const MILES = "miles";
/** The Product and Engineering team's key. */
const TEAM = "product-and-engineering";

/**
 * The Tier A token Miles reaches the company with (`api.auth.tokens`), bound
 * to his seat in the identity directory and granted admin.
 */
export const MILES_TOKEN = "review";
/** What a write made with that token as a bearer (the CLI) records as its operator. */
const CLI = `token:${MILES_TOKEN}`;
/**
 * The browser session an earlier dashboard visit of Miles's wrote through: a
 * person bound to a seat is recorded AS the seat, and the credential beside
 * it is the session (`iam.ActorFor`).
 */
const EARLIER_SESSION = `session:${fixtureId("miles-session-earlier")}`;

/** What a fixture is anchored to. */
export interface FixtureOptions {
  /** The instant the company reads as now; the present by default. */
  now?: Date;
}

/** The FairSplit demo company, as it stands at `now`. */
export function fairsplit(options: FixtureOptions = {}): DemoCompany {
  const now = options.now ?? new Date();
  const ago = (ms: number) => new Date(now.getTime() - ms).toISOString();
  const later = (ms: number) => new Date(now.getTime() + ms).toISOString();
  const today = dateIn(now, ZONE);
  const day = (offset: number) => addDays(today, offset);
  const pmId = agentIdFor(ORG, PM);
  const sweId = agentIdFor(ORG, SWE);

  // ---- configuration -----------------------------------------------------
  const document = fairsplitDocument();
  const revisionSummaries = [
    "Created FairSplit from the onboarding template",
    "Added the Product and Engineering team and Crewlet-PM",
    "Added Crewlet-SWE to Product and Engineering",
    "Gave Crewlet-PM the daily stand-up schedule",
    "Set a daily token ceiling of 4M on each agent",
    "Wrote the team's goals",
    "Linked the FAIR project and space to the team",
    "Drew Crewlet-PM in rose and Crewlet-SWE as the gemlet in cyan",
  ];
  const revisions = revisionSummaries.map((summary, i) => {
    const at = ago(3 * D - i * 3 * H);
    const meta: RevisionMeta = {
      revision_id: fixtureId(`revision-${i + 1}`),
      summary,
      source: i === 0 ? "cli" : "api",
      created_by: MILES,
      created_by_kind: "human",
      operator_id: i === 0 ? CLI : EARLIER_SESSION,
      created_at: at,
      activated_at: at,
    };
    if (i > 0) meta.parent_revision_id = fixtureId(`revision-${i}`);
    return { meta, payload: document };
  });

  // ---- turns, events and spend --------------------------------------------
  const pmTurns = turnsFor(PM, pmId, "Crewlet-PM", now, [
    { at: 26 * H, failed: true, summary: "", tokens: 41_200, ms: 12_400, item: "FAIR-1" },
    {
      at: 26.4 * H,
      failed: false,
      summary:
        "Reviewed FAIR-2 against its acceptance criteria and asked Miles to settle storage first.",
      tokens: 58_900,
      ms: 9_800,
      item: "FAIR-2",
    },
    {
      at: 27 * H,
      failed: false,
      summary: "Pointed Miles at the brief and the two open scope questions.",
      tokens: 22_100,
      ms: 4_100,
      item: "FAIR-1",
    },
    {
      at: 29.5 * H,
      failed: false,
      summary: "Asked Miles how the MVP should store a group's data.",
      tokens: 31_700,
      ms: 4_300,
      item: "FAIR-1",
    },
    {
      at: 30 * H,
      failed: false,
      summary: "Asked Miles whether the MVP supports unequal splits.",
      tokens: 34_000,
      ms: 4_200,
      item: "FAIR-1",
    },
    {
      at: 2 * D + 8 * H,
      failed: false,
      summary: "Wrote the product brief and filed FAIR-2 to FAIR-7 with acceptance criteria.",
      tokens: 96_300,
      ms: 38_000,
      item: "FAIR-1",
    },
  ]);
  const sweTurns = turnsFor(SWE, sweId, "Crewlet-SWE", now, [
    {
      at: 25.5 * H,
      failed: false,
      summary: "Answered the review on FAIR-2 and pushed the negative-amount test.",
      tokens: 88_400,
      ms: 41_000,
      item: "FAIR-2",
    },
    {
      at: 28 * H,
      failed: false,
      summary: "Picked up the brief and the backlog, and planned FAIR-2.",
      tokens: 26_300,
      ms: 6_200,
      item: "FAIR-1",
    },
    {
      at: 2 * D,
      failed: false,
      summary: "Set up the repository, the package layout and the test runner.",
      tokens: 142_800,
      ms: 154_000,
      item: "FAIR-7",
    },
  ]);
  const turns = [...pmTurns.rows, ...sweTurns.rows].sort((a, b) =>
    b.started_at.localeCompare(a.started_at),
  );

  const events: EventRecord[] = [
    ...pmTurns.events,
    ...sweTurns.events,
    seatPaused(PM, pmId, "Crewlet-PM", ago(25 * H)),
    seatPaused(SWE, sweId, "Crewlet-SWE", ago(25 * H - 20_000)),
    ...revisions.map((r, i) => ({
      id: fixtureId(`event-revision-${i}`),
      type: "config_revision_activated",
      timestamp: r.meta.created_at,
      source: "configplane",
      actor: MILES,
      summary: `Revision activated at epoch ${i + 1}: ${r.meta.summary}`,
      category: "lifecycle",
      trace_id: fixtureId(`trace-revision-${i}`),
      span_id: "",
      parent_span_id: "",
      topic: "config",
      payload: { revision_id: r.meta.revision_id, epoch: i + 1, summary: r.meta.summary },
    })),
  ].sort((a, b) => b.timestamp.localeCompare(a.timestamp));

  const daily: DemoSpendDay[] = [
    ...spendRows(PM, day(-3), 25, 21, 1_120_000),
    ...spendRows(PM, day(-2), 12, 10, 610_000),
    ...spendRows(PM, day(-1), 33, 28, 1_540_000),
    ...spendRows(SWE, day(-3), 8, 1, 2_310_000),
    ...spendRows(SWE, day(-2), 9, 1, 2_890_000),
    ...spendRows(SWE, day(-1), 7, 1, 1_980_000),
  ];

  // ---- seats -----------------------------------------------------------------
  const pmLast = pmTurns.rows[0]!;
  const sweLast = sweTurns.rows[0]!;
  const seats: Record<string, DemoSeat> = {
    [PM]: seat({
      handle: PM,
      role: "Crewlet-PM",
      agentId: pmId,
      overlay: pausedOverlay(ago(25 * H), {
        turn_id: pmLast.turn_id,
        ended_at: pmLast.ended_at,
        outcome: "failed",
      }),
      perDay: days(today, [...quiet(24), 25, 12, 33, 0], [...quiet(24), 21, 10, 28, 0], 47_000),
      reviewed: 5,
      firstPass: 4,
      p50: 4_200,
      p90: 12_000,
      lastTurnAt: pmLast.ended_at,
      history: pmTurns.phases,
      onboardedAt: ago(3 * D - 2 * H),
      memory: {
        diary: [
          "Miles answers scope questions within the day; ask one decision at a time, with a recommendation.",
          "Crewlet-SWE writes a design page before code. Review against the page, then the code.",
          "I cannot run code. Check that the tests cover each acceptance criterion instead.",
        ],
        reflection:
          "Two scope questions are open with Miles. Hold FAIR-4 and FAIR-5 until they are answered.",
      },
      conversations: [
        { key: "native:FAIR-1", turns: 9, last: 26 * H },
        { key: "native:FAIR-2", turns: 2, last: 26.4 * H },
      ],
      ago,
    }),
    [SWE]: seat({
      handle: SWE,
      role: "Crewlet-SWE",
      agentId: sweId,
      overlay: pausedOverlay(ago(25 * H - 20_000), {
        turn_id: sweLast.turn_id,
        ended_at: sweLast.ended_at,
        outcome: "completed",
      }),
      perDay: days(today, [...quiet(24), 8, 9, 7, 0], [...quiet(24), 1, 1, 1, 0], 300_000),
      reviewed: 6,
      firstPass: 5,
      p50: 38_000,
      p90: 144_000,
      lastTurnAt: sweLast.ended_at,
      history: sweTurns.phases,
      onboardedAt: ago(3 * D - 9 * H),
      memory: {
        diary: [
          "FairSplit keeps money as integer cents end to end; never a float.",
          "The test runner is pytest, and `make test` runs it with coverage.",
        ],
        reflection: "FAIR-2 is in review. FAIR-3 can start once the storage question is settled.",
      },
      conversations: [{ key: "native:FAIR-2", turns: 6, last: 25.5 * H }],
      ago,
    }),
  };

  // ---- work -------------------------------------------------------------------
  const work = buildWork(now, ago, later, pmTurns.rows, sweTurns.rows);

  // ---- the knowledge base ------------------------------------------------------
  const pages = buildPages(ago);

  const company: DemoCompany = {
    now: now.toISOString(),
    // Replaced below, by the sign-in that writes the directory it reads.
    principal: tokenSession({
      tokenId: MILES_TOKEN,
      seat: MILES,
      grants: ["admin"],
      lineage: "",
      expiresAt: "",
    }),
    directory: emptyDirectory(),
    config: { revisions, epoch: revisions.length },
    seats,
    events,
    turns,
    tools: TOOLS,
    spend: { daily },
    // THE STAND-UP MISSED ONCE: the engine was down at a tick three days back
    // and came back after its catchup window, so the ledger holds the skip.
    scheduleRuns: fireRecords(
      scheduleRows(document, now).find((r) => r.scope_name === PM && r.name === "daily-standup")!,
      "skipped_catchup",
      new Date(now.getTime() - 3 * D),
    ).runs,
    runs: { live: [], records: [] },
    a2a: [
      {
        id: fixtureId("a2a-1"),
        requester: SWE,
        target: PM,
        messages: 2,
        opened_at: ago(28 * H - 5 * M),
        last_at: ago(28 * H - 3 * M),
        closed_at: ago(28 * H - 3 * M),
      },
    ],
    work,
    pages,
    integrations: {
      catalogue: structuredClone(noAddress) as SetupListing,
      states: {},
      scripted: {},
      runs: {},
      outcomes: {},
    },
    recorded: {},
    artifacts: emptyArtifacts(),
    secrets: [
      {
        name: "ANTHROPIC_API_KEY",
        key_id: "key-1",
        updated_at: ago(3 * D),
        updated_by: MILES,
        updated_by_kind: "human",
        operator_id: CLI,
        source: "cli",
      },
    ],
    engine: buildEngine(now, ago),
  };
  // THE BROWSER: Miles's admin token, bound to his seat in the identity
  // directory, exchanged for a session eight minutes ago, so the hour it
  // lasts has fifty-two minutes left. Nobody has enrolled as a person yet.
  signInWithToken(company, {
    tokenId: MILES_TOKEN,
    seat: MILES,
    grants: ["admin"],
    at: ago(8 * M),
  });
  return company;
}

// ---- seats ---------------------------------------------------------------------

function pausedOverlay(at: string, last: NonNullable<Overlay["last_turn"]>): Overlay {
  return {
    activity: "stopped",
    stopped_reason: "paused",
    current_phase: null,
    current_iteration: 0,
    live_call: null,
    live_call_seq: 0,
    last_error: null,
    turn: null,
    last_turn: last,
    paused: {
      by: MILES,
      by_kind: "human",
      operator_id: EARLIER_SESSION,
      at,
      reason: "weekly limit.",
      stop_running: false,
    },
  };
}

/** `n` quiet days. */
const quiet = (n: number): number[] => Array.from({ length: n }, () => 0);

/** A seat's day history ending today, oldest first: the seat's whole record, which `seat_activity` windows. */
function days(
  today: string,
  turns: number[],
  failed: number[],
  tokensPerTurn: number,
): SeatActivityDay[] {
  return turns.map((n, i) => ({
    day: addDays(today, i - (turns.length - 1)),
    turns: n,
    failed: failed[i] ?? 0,
    tokens: n * tokensPerTurn,
  }));
}

function seat(o: {
  handle: string;
  role: string;
  agentId: string;
  overlay: Overlay;
  perDay: SeatActivityDay[];
  reviewed: number;
  firstPass: number;
  p50: number;
  p90: number;
  lastTurnAt: string;
  history: EventRecord[];
  onboardedAt: string;
  memory: { diary: string[]; reflection: string };
  conversations: { key: string; turns: number; last: number }[];
  ago: (ms: number) => string;
}): DemoSeat {
  const sum = (key: "turns" | "failed" | "tokens") => o.perDay.reduce((n, d) => n + d[key], 0);
  const diary = o.memory.diary.map((content, i) => ({
    id: fixtureId(`${o.handle}-diary-${i}`),
    content,
    retention: "long",
    source: "reflection",
    turn_id: o.history[0]?.tags?.turn_id ?? "",
    created_at: o.ago(30 * 3_600_000 + i * 3_600_000),
    ttl_until: "",
    retrievals: 3 - i,
  }));
  const reflection = {
    id: fixtureId(`${o.handle}-reflection`),
    content: o.memory.reflection,
    retention: "short",
    source: "reflection",
    turn_id: o.history[0]?.tags?.turn_id ?? "",
    created_at: o.lastTurnAt,
    ttl_until: "",
    retrievals: 1,
  };
  const memory: AgentMemory = {
    id: o.handle,
    diary: [reflection, ...diary],
    diary_total: diary.length + 1,
    episodes: [],
    episodes_total: 0,
    skills: [],
    skills_total: 0,
    counterparties: [
      {
        subject: { handle: MILES, name: "Miles" },
        resolved: true,
        traits: {
          prefers: "one decision at a time, with a recommendation",
          answers_in: "the dashboard inbox",
        },
        interactions: 6,
        first_seen_at: o.onboardedAt,
        last_updated_at: o.lastTurnAt,
        last_corroborated_at: o.lastTurnAt,
      },
    ],
    counterparties_total: 1,
    latest_reflection: reflection,
    onboarded_at: o.onboardedAt,
    held_by: NODE,
  };
  return {
    overlay: o.overlay,
    activity: {
      handle: o.handle,
      role: o.role,
      agent_id: o.agentId,
      in_chart: true,
      turns: sum("turns"),
      failed: sum("failed"),
      reviewed: o.reviewed,
      first_pass: o.firstPass,
      first_pass_pct: Math.round((o.firstPass / o.reviewed) * 100),
      sent_back: 0,
      p50_ms: o.p50,
      p90_ms: o.p90,
      tokens: sum("tokens"),
      per_day: o.perDay,
      last_turn_at: o.lastTurnAt,
    },
    memory,
    episode: null,
    history: o.history,
    conversations: {
      rows: o.conversations.map((c) => ({ key: c.key, turns: c.turns, last_at: o.ago(c.last) })),
      entries: o.conversations.map((c, i) => ({
        turn_id: o.history[i]?.tags?.turn_id ?? "",
        at: o.ago(c.last),
        trigger: "work item comment",
        intent:
          i === 0
            ? "Move the work on this item forward and say what changed."
            : "Answer the review.",
        reply: o.memory.reflection,
      })),
    },
    overview: {
      agent_id: o.agentId,
      diary_total: diary.length + 1,
      episodes_total: 0,
      skills_total: 0,
      last_reflection_at: o.lastTurnAt,
      latest_reflection: reflection,
      held_by: NODE,
      unavailable: "",
    },
  };
}

// ---- turns and events ----------------------------------------------------------

interface TurnSpec {
  /** How long ago it ended. */
  at: number;
  failed: boolean;
  summary: string;
  tokens: number;
  ms: number;
  item: string;
}

function turnsFor(handle: string, agentId: string, role: string, now: Date, specs: TurnSpec[]) {
  const iso = (ms: number) => new Date(now.getTime() - ms).toISOString();
  const rows: TurnRow[] = [];
  const events: EventRecord[] = [];
  const phases: EventRecord[] = [];
  specs.forEach((spec, i) => {
    const turnId = fixtureId(`${handle}-turn-${i}`);
    const trace = fixtureId(`${handle}-trace-${i}`);
    const ended = iso(spec.at);
    const started = iso(spec.at + spec.ms);
    const input = Math.round(spec.tokens * 0.9);
    const output = spec.tokens - input;
    const itemNumber = Number(spec.item.split("-")[1]);
    const workItem = {
      backend: "native",
      id: fixtureId(`item-${itemNumber}`),
      key: spec.item,
      project: "FAIR",
    };
    const row: TurnRow = {
      turn_id: turnId,
      agent_id: agentId,
      role,
      started_at: started,
      ended_at: ended,
      duration_ms: spec.ms,
      complete: true,
      phases: spec.failed ? 1 : 2,
      iterations: 1,
      failed: spec.failed,
      input_tokens: input,
      output_tokens: output,
      total_tokens: spec.tokens,
      cache_read_tokens: Math.round(input * 0.6),
      models: "claude-sonnet-5",
      trigger: "work item comment",
      work_item: workItem,
    };
    if (spec.summary) row.summary = spec.summary;
    rows.push(row);
    const tags = { agent_id: agentId, turn_id: turnId, node: NODE };
    const base = {
      source: "agent",
      actor: handle,
      trace_id: trace,
      parent_span_id: "",
      topic: `agent.${handle}`,
    };
    events.push({
      ...base,
      id: fixtureId(`${handle}-turn-${i}-started`),
      type: "agent_turn_started",
      timestamp: started,
      summary: `${role} started a turn on ${spec.item}`,
      category: "system",
      span_id: fixtureId(`${handle}-span-${i}-a`).slice(0, 16),
      tags,
      payload: {
        agent_id: agentId,
        agent_handle: handle,
        role,
        turn_id: turnId,
        trigger: "work item comment",
        started_at: started,
        work_item: workItem,
      },
    });
    const phase: EventRecord = {
      ...base,
      id: fixtureId(`${handle}-turn-${i}-phase`),
      type: "agent_phase_completed",
      timestamp: iso(spec.at + 1_000),
      summary: spec.failed ? `${role} execute phase failed` : `${role} finished its execute phase`,
      category: "system",
      span_id: fixtureId(`${handle}-span-${i}-b`).slice(0, 16),
      tags,
      failed: spec.failed,
      payload: {
        agent_id: agentId,
        role,
        turn_id: turnId,
        iteration: 1,
        phase: "execute",
        model: "claude-sonnet-5",
        provider_key: "default",
        trigger: { type: "work_item_comment", item: spec.item },
        system_prompt: `You are ${role}, a seat in FairSplit's Product and Engineering team.`,
        user_prompt: `A colleague commented on ${spec.item}. Read the thread and move the work forward.`,
        response: spec.failed ? "" : spec.summary,
        input_tokens: input,
        output_tokens: output,
        total_tokens: spec.tokens,
        cache_read_tokens: Math.round(input * 0.6),
        cache_write_tokens: 0,
        rounds_used: spec.failed ? 1 : 3,
        exhausted_rounds: false,
        started_at: started,
        duration_ms: spec.ms - 1_000,
        work_item: workItem,
        decision: spec.failed ? "" : "act",
        failed: spec.failed,
        error: spec.failed ? "The provider answered 529: overloaded, three times in a row." : "",
        error_kind: spec.failed ? "provider" : "",
      },
    };
    events.push(phase);
    phases.push(phase);
    events.push({
      ...base,
      id: fixtureId(`${handle}-turn-${i}-completed`),
      type: "agent_turn_completed",
      timestamp: ended,
      summary: spec.failed
        ? `${role}'s turn failed: the provider was overloaded`
        : `${role} finished a turn on ${spec.item}`,
      category: "system",
      span_id: fixtureId(`${handle}-span-${i}-c`).slice(0, 16),
      tags,
      failed: spec.failed,
      payload: {
        agent_id: agentId,
        role,
        turn_id: turnId,
        model: "claude-sonnet-5",
        execute_model: "claude-sonnet-5",
        review_model: "claude-sonnet-5",
        iterations: 1,
        decision: spec.failed ? "" : "act",
        failed: spec.failed,
        error: spec.failed ? "The provider answered 529: overloaded, three times in a row." : "",
        error_kind: spec.failed ? "provider" : "",
        input_tokens: input,
        output_tokens: output,
        total_tokens: spec.tokens,
        work_item: workItem,
      },
    });
  });
  return { rows, events, phases: phases.sort((a, b) => b.timestamp.localeCompare(a.timestamp)) };
}

function seatPaused(handle: string, agentId: string, role: string, at: string): EventRecord {
  return {
    id: fixtureId(`${handle}-paused`),
    type: "seat_paused",
    timestamp: at,
    // AS THE ENGINE ANNOUNCES IT: the envelope's source and actor are the
    // seat, and who paused it is the payload's (`SeatPaused.SummaryFor`).
    source: role,
    actor: role,
    summary: `${role} was paused by ${MILES}`,
    category: "lifecycle",
    trace_id: fixtureId(`${handle}-paused-trace`),
    span_id: "",
    parent_span_id: "",
    topic: `seat.${handle}`,
    tags: { agent_id: agentId, operator_id: EARLIER_SESSION },
    payload: {
      agent_id: agentId,
      agent_handle: handle,
      role,
      paused_by: MILES,
      paused_by_kind: "human",
      operator_id: EARLIER_SESSION,
      reason: "weekly limit.",
      paused_at: at,
    },
  };
}

/** One seat's spend on one company day, split as a turn spends it. */
function spendRows(
  handle: string,
  day: string,
  turns: number,
  failed: number,
  tokens: number,
): DemoSpendDay[] {
  const row = (phase: string, share: number, calls: number): DemoSpendDay => {
    const total = Math.round(tokens * share);
    const input = Math.round(total * 0.88);
    return {
      day,
      handle,
      phase,
      model: "claude-sonnet-5",
      provider: "default",
      input,
      output: total - input,
      cache_read: Math.round(input * 0.55),
      calls,
    };
  };
  return [
    { ...row("execute", 0.78, turns * 3), turns, failed },
    row("review", 0.14, Math.max(1, turns - failed)),
    row("auxiliary", 0.08, turns * 2),
  ];
}

// ---- the tracker -----------------------------------------------------------------

function buildWork(
  now: Date,
  ago: (ms: number) => string,
  later: (ms: number) => string,
  pmTurns: TurnRow[],
  sweTurns: TurnRow[],
): DemoCompany["work"] {
  let seq = 0;
  const next = () => ++seq;
  const change = (
    kind: string,
    at: number,
    actor: string,
    extra: Partial<WorkChange> = {},
  ): WorkChange => ({
    id: fixtureId(`change-${seq + 1}`),
    kind,
    actor,
    actor_kind: actor === MILES ? "human" : "agent",
    ...(actor === MILES ? { operator_id: EARLIER_SESSION } : {}),
    at: ago(at),
    log_seq: next(),
    ...extra,
  });
  const comment = (
    n: number,
    author: string,
    at: number,
    body: string,
    extra: Partial<WorkComment> = {},
  ): WorkComment => ({
    id: fixtureId(`comment-${n}`),
    task: "",
    author,
    author_kind: author === MILES ? "human" : "agent",
    body,
    created_at: ago(at),
    ...extra,
  });

  const splitDecision: WorkDecision = {
    question: "Should the MVP support unequal splits, or equal splits only?",
    options: [
      {
        id: "equal",
        label: "Equal splits only",
        detail: "Smallest MVP; unequal splits become a later item.",
      },
      {
        id: "equal-exact",
        label: "Equal plus exact amounts per person",
        detail: "One more input form and a sum check; still small.",
      },
      {
        id: "all",
        label: "Equal, exact, percent and shares",
        detail: "More rounding rules to document and test; likely another session.",
      },
    ],
    recommended: "equal",
    rationale: "Equal split plus the leftover-cent rule covers the main cases; cut scope first.",
    evidence: [{ kind: "page", ref: fixtureId("page-brief"), label: "Product brief" }],
    role: "approver",
  };
  const storageDecision: WorkDecision = {
    question: "How should the MVP store a group's data locally?",
    options: [
      {
        id: "json",
        label: "One JSON file per group",
        detail: "Readable, easy to back up, no dependency.",
      },
      {
        id: "sqlite",
        label: "A SQLite database",
        detail: "Queries and integrity checks; one more thing to learn.",
      },
      { id: "csv", label: "CSV files", detail: "Opens in a spreadsheet; awkward for nested data." },
    ],
    recommended: "json",
    rationale: "One file per group is enough for the MVP and keeps FAIR-5 to a day.",
    evidence: [{ kind: "task", ref: fixtureId("item-5"), label: "Store a group's data locally" }],
    role: "approver",
  };
  const interfaceDecision: WorkDecision = {
    question: "Should the MVP be a command-line tool or a small web app?",
    options: [
      { id: "cli", label: "A command-line tool", detail: "Fastest to ship and test." },
      { id: "web", label: "A small web app", detail: "Friendlier, and three times the work." },
    ],
    recommended: "cli",
    rationale: "A command-line tool proves the settle-up arithmetic first.",
    role: "approver",
  };

  const items: DemoItem[] = [];
  const task = (
    n: number,
    fields: Partial<WorkItem> & Pick<WorkItem, "title" | "status">,
    createdAgo: number,
  ): WorkItem => ({
    id: fixtureId(`item-${n}`),
    key: `FAIR-${n}`,
    project: "FAIR",
    filed_unit: TEAM,
    routing_unit: TEAM,
    type: "task",
    priority: "normal",
    reporter: PM,
    watchers: [PM],
    created_at: ago(createdAgo),
    updated_at: ago(createdAgo),
    status_entered_at: ago(createdAgo),
    rank: `m${String(n).padStart(4, "0")}`,
    version: 0,
    ...fields,
  });

  // FAIR-1, the brief: Miles filed it, and its thread holds the scope questions.
  const askInterface = comment(1, PM, 2 * D + 10 * H, "Scope question 1 of 3: the interface.", {
    ask: MILES,
    decision: interfaceDecision,
  });
  const answerInterface = comment(2, MILES, 2 * D + 6 * H, "A command-line tool", {
    answers: askInterface.id,
    choice: "cli",
  });
  const askSplits = comment(
    3,
    PM,
    30 * H,
    "Scope question 2 of 3: split types. Policies only require splits exact to the cent.",
    {
      ask: MILES,
      decision: splitDecision,
    },
  );
  const askStorage = comment(
    4,
    PM,
    29.5 * H,
    "Scope question 3 of 3: storage. The MVP runs on one machine and has no server.",
    { ask: MILES, decision: storageDecision },
  );
  const sweReply = comment(
    5,
    SWE,
    28 * H,
    "@pm Received the brief and backlog. I'll start FAIR-2 with its design page, then the code and tests. I'll link the design page here when it is ready.",
    { mentions: [PM] },
  );
  const pmPointer = comment(
    6,
    PM,
    27 * H,
    "@miles The brief is on the Product brief page in the FAIR space. Two scope questions are waiting on you above; FAIR-2, FAIR-3 and FAIR-6 can go ahead without them.",
    { mentions: [MILES] },
  );
  const brief: DemoItem = {
    task: task(
      1,
      {
        title: "Write the FairSplit product brief and the MVP backlog",
        body:
          "Write the product brief for FairSplit and turn it into a backlog Crewlet-SWE can build from.\n\n" +
          "## Acceptance criteria\n\n" +
          "- The brief names the users, the problem and what the MVP leaves out.\n" +
          "- Every MVP work item has acceptance criteria a test can check.\n" +
          "- Each scope question Miles has to answer is asked once, with a recommendation.",
        status: "in_progress",
        status_group: "active",
        priority: "high",
        reporter: MILES,
        assignee: PM,
        watchers: [MILES, PM],
        tags: ["mvp"],
        status_entered_at: ago(2 * D + 8 * H),
        spend: { turns: 7, tokens: 380_000, rounds: 19, workers: 0, sent_back: 0 },
      },
      2 * D + 20 * H,
    ),
    comments: [askInterface, answerInterface, askSplits, askStorage, sweReply, pmPointer],
    history: [],
    links: [{ kind: "page", other: fixtureId("page-brief"), title: "Product brief" }],
    turns: [],
  };
  brief.history.push(
    change("created", 2 * D + 20 * H, MILES),
    change("assignee", 2 * D + 20 * H - M, MILES, { fields: { assignee: { from: "", to: PM } } }),
    change("status", 2 * D + 8 * H, PM, {
      fields: { status: { from: "todo", to: "in_progress" } },
    }),
    change("comment", 2 * D + 10 * H, PM, {
      comment_id: askInterface.id,
      excerpt: interfaceDecision.question,
    }),
    change("comment", 2 * D + 6 * H, MILES, {
      comment_id: answerInterface.id,
      excerpt: "A command-line tool",
    }),
    change("comment", 30 * H, PM, { comment_id: askSplits.id, excerpt: splitDecision.question }),
    change("comment", 29.5 * H, PM, {
      comment_id: askStorage.id,
      excerpt: storageDecision.question,
    }),
    change("comment", 28 * H, SWE, {
      comment_id: sweReply.id,
      excerpt: sweReply.body.slice(0, 160),
    }),
    change("comment", 27 * H, PM, {
      comment_id: pmPointer.id,
      excerpt: pmPointer.body.slice(0, 160),
    }),
  );
  items.push(brief);

  const review = comment(
    7,
    PM,
    26.5 * H,
    "@miles Reviewed cli.py, store.py and tests/test_cli.py against the acceptance criteria. I can't run code, so I checked the tests cover each criterion: adding an expense, listing a group and refusing a negative amount. I'll close it once the storage question is settled.",
    { mentions: [MILES] },
  );
  const record: DemoItem = {
    task: task(
      2,
      {
        title: "Record a group's expenses from the command line",
        body:
          "`fairsplit add <group> <payer> <amount> <description>` records an expense; `fairsplit list <group>` prints them.\n\n" +
          "## Acceptance criteria\n\n- Amounts are stored as integer cents.\n- A negative or zero amount is refused with a message.\n- `list` prints expenses oldest first.",
        status: "in_review",
        status_group: "active",
        priority: "high",
        assignee: SWE,
        watchers: [PM, SWE],
        tags: ["mvp"],
        status_entered_at: ago(26.5 * H),
        spend: { turns: 4, tokens: 512_000, rounds: 31, workers: 2, sent_back: 0 },
      },
      2 * D + 7 * H,
    ),
    comments: [review],
    history: [],
    links: [
      { kind: "page", other: fixtureId("page-design-2"), title: "Design: recording expenses" },
    ],
    turns: [],
  };
  record.history.push(
    change("created", 2 * D + 7 * H, PM),
    change("assignee", 2 * D + 7 * H - M, PM, { fields: { assignee: { from: "", to: SWE } } }),
    change("status", 27.5 * H, SWE, { fields: { status: { from: "todo", to: "in_progress" } } }),
    change("status", 26.5 * H, SWE, {
      fields: { status: { from: "in_progress", to: "in_review" } },
    }),
    change("comment", 26.5 * H, PM, { comment_id: review.id, excerpt: review.body.slice(0, 160) }),
  );
  items.push(record);

  const simple = (
    n: number,
    title: string,
    fields: Partial<WorkItem>,
    createdAgo: number,
    links: DemoItem["links"] = [],
  ) => {
    const made: DemoItem = {
      task: task(
        n,
        {
          title,
          status: "todo",
          status_group: "not_started",
          assignee: SWE,
          watchers: [PM, SWE],
          tags: ["mvp"],
          ...fields,
        },
        createdAgo,
      ),
      comments: [],
      history: [],
      links,
      turns: [],
    };
    made.history.push(change("created", createdAgo, PM));
    if (made.task.assignee) {
      made.history.push(
        change("assignee", createdAgo - M, PM, {
          fields: { assignee: { from: "", to: made.task.assignee } },
        }),
      );
    }
    items.push(made);
    return made;
  };
  simple(
    3,
    "Compute the fewest payments that settle the group",
    {
      priority: "high",
      body: "Given a group's balances, print the fewest payments that bring every balance to zero, exact to the cent.",
    },
    2 * D + 7 * H - 2 * M,
    [{ kind: "waiting_on", other: fixtureId("item-2"), key: "FAIR-2" }],
  );
  simple(
    4,
    "Split amounts exact to the cent with a leftover-cent rule",
    {
      body: "Split an amount between people so the parts add up to the amount exactly, and write down who gets the leftover cent.",
    },
    2 * D + 7 * H - 4 * M,
  );
  simple(
    5,
    "Store a group's data locally",
    {
      assignee: "",
      body: "Keep a group's members and expenses between runs. Waiting on the storage decision on FAIR-1.",
    },
    2 * D + 7 * H - 6 * M,
  );
  simple(
    6,
    "Write the README and a usage example",
    { priority: "low", due_at: later(5 * D), tags: ["docs"] },
    2 * D + 7 * H - 8 * M,
  );
  const setup = simple(
    7,
    "Set up the repository, the package layout and the test runner",
    {
      status: "done",
      status_group: "done",
      priority: "normal",
      done_at: ago(2 * D),
      status_entered_at: ago(2 * D),
      tags: [],
      spend: { turns: 1, tokens: 142_800, rounds: 14, workers: 0, sent_back: 0 },
    },
    2 * D + 7 * H - 10 * M,
  );
  setup.history.push(
    change("status", 2 * D, SWE, { fields: { status: { from: "todo", to: "done" } } }),
  );
  for (const fix of items) {
    const unassigned = fix.task.assignee === "";
    if (unassigned) delete fix.task.assignee;
    for (const c of fix.comments) c.task = fix.task.id;
    fix.task.version = Math.max(0, ...fix.history.map((h) => h.log_seq));
    // A task last changed at its newest record, a comment included, as the
    // engine's applier stamps it.
    const changed = [
      ...fix.history.map((h) => h.at),
      ...fix.comments.map((c) => c.created_at),
    ].filter(Boolean);
    const newest = changed.reduce(
      (a, b) => (Date.parse(b) > Date.parse(a) ? b : a),
      fix.task.created_at ?? "",
    );
    if (newest) fix.task.updated_at = newest;
  }
  brief.turns = turnsOn("FAIR-1", [...pmTurns, ...sweTurns]);
  record.turns = turnsOn("FAIR-2", [...pmTurns, ...sweTurns]);

  // Miles's inbox: the two open scope questions and four updates unread, and
  // two older notices he has read.
  const notice = (
    n: number,
    item: DemoItem,
    at: number,
    reason: string,
    extra: Partial<WorkInboxNotice>,
  ): WorkInboxNotice => ({
    record_id: fixtureId(`notice-${n}`),
    log_seq: seq - 20 + n,
    log_stream: TRACKER_STREAM,
    log_generation: 1,
    at: ago(at),
    reason,
    primary: true,
    addressed: false,
    kind: "comment",
    subject_id: item.task.id,
    subject_key: item.task.key,
    actor: PM,
    actor_kind: "agent",
    read: false,
    ...extra,
  });
  const askView = (c: WorkComment) => ({ comment: c.id, asked_of: MILES, open: true });
  const inbox: WorkInboxNotice[] = [
    notice(1, record, 26.5 * H, "mention", { comment_id: review.id, excerpt: review.body }),
    notice(2, brief, 27 * H, "mention", { comment_id: pmPointer.id, excerpt: pmPointer.body }),
    notice(3, brief, 28 * H, "reporter", {
      actor: SWE,
      comment_id: sweReply.id,
      excerpt: sweReply.body,
    }),
    notice(4, brief, 29.5 * H, "asked", {
      addressed: true,
      comment_id: askStorage.id,
      excerpt: storageDecision.question,
      ask: askView(askStorage),
    }),
    notice(5, brief, 30 * H, "asked", {
      addressed: true,
      comment_id: askSplits.id,
      excerpt: splitDecision.question,
      ask: askView(askSplits),
    }),
    notice(6, brief, 2 * D + 8 * H, "reporter", { kind: "status", excerpt: "To do → In progress" }),
    notice(7, brief, 2 * D + 10 * H, "asked", {
      addressed: true,
      comment_id: askInterface.id,
      excerpt: interfaceDecision.question,
      ask: askView(askInterface),
      read: true,
    }),
    notice(8, brief, 2 * D + 20 * H, "reporter", {
      kind: "created",
      actor: MILES,
      actor_kind: "human",
      read: true,
    }),
  ];

  return {
    logSeq: seq,
    projects: [
      {
        key: "FAIR",
        name: "FairSplit MVP",
        purpose:
          "The FairSplit command-line MVP: record a group's expenses and settle up with the fewest payments.",
        unit: TEAM,
        lead: PM,
        default_assignee: SWE,
        target_date: dateIn(new Date(now.getTime() + 21 * D), ZONE),
        statuses: STATUSES,
        types: BUILTIN_TYPES,
        fields: [],
        tags: [
          { slug: "mvp", label: "MVP", color: "purple" },
          { slug: "docs", label: "Docs", color: "blue" },
          { slug: "design", label: "Design", color: "amber" },
        ],
        version: 2,
        created_at: ago(2 * D + 21 * H),
      },
    ],
    items,
    types: [],
    fields: [],
    views: [],
    inbox: { [MILES]: inbox },
    people: { [MILES]: { priorities: [], pinned_views: [], favorites: [], version: 0 } },
    files: [
      {
        project: "FAIR",
        path: "docs/brief.md",
        content_type: "text/markdown",
        size: 4_812,
        version: 2,
        created_by: PM,
        created_at: ago(2 * D + 9 * H),
        updated_by: PM,
        updated_at: ago(30 * H),
      },
      {
        project: "FAIR",
        path: "fairsplit/cli.py",
        content_type: "text/x-python",
        size: 6_204,
        version: 3,
        created_by: SWE,
        created_at: ago(27 * H),
        updated_by: SWE,
        updated_at: ago(25.5 * H),
      },
    ],
  };
}

/** The turns charged to one task, newest first, as `work_item_turns` lists them. */
function turnsOn(key: string, rows: TurnRow[]) {
  const mine = rows
    .filter((r) => r.work_item?.key === key)
    .sort((a, b) => b.ended_at.localeCompare(a.ended_at));
  return mine.map((r, i) => ({
    turn_id: r.turn_id,
    ordinal: mine.length - i,
    seat: r.role === "Crewlet-PM" ? PM : SWE,
    trigger: r.trigger ?? "",
    segments: 1,
    tokens: r.total_tokens,
    cache_read: r.cache_read_tokens ?? 0,
    rounds: r.failed ? 1 : 3,
    wall_ms: r.duration_ms,
    outcome: r.failed ? "failed" : "act",
    phases: r.failed ? ["execute"] : ["execute", "review"],
    ...(r.failed ? { failed_in: "execute" } : {}),
    ...(r.summary ? { summary: r.summary } : {}),
    tools: r.failed
      ? []
      : [
          { name: "comment_on_work_item", calls: 1 },
          { name: "search_knowledge", calls: 2 },
        ],
    at: r.ended_at,
  }));
}

// ---- the knowledge base ------------------------------------------------------------

function buildPages(ago: (ms: number) => string): DemoCompany["pages"] {
  const page = (
    slug: string,
    title: string,
    author: string,
    at: number,
    body: string,
    extra: Partial<DemoPage["page"]> = {},
  ): DemoPage => {
    const id = fixtureId(`page-${slug}`);
    return {
      page: {
        id,
        container: "FAIR",
        title,
        status: "published",
        author,
        version: 1,
        updated_at: ago(at),
        revision: 1,
        body,
        created_at: ago(at),
        saved_by: author,
        saved_at: ago(at),
        watchers: [author],
        labels: [],
        ...extra,
      },
      comments: [],
      revisions: [{ version: 1, author, created_at: ago(at), body, title }],
      changes: [
        {
          id: fixtureId(`page-${slug}-created`),
          page_id: id,
          title,
          container: "FAIR",
          kind: "created",
          actor: author,
          actor_kind: "agent",
          at: ago(at),
          log_seq: 1,
        },
      ],
      reads: [
        {
          handle: author === PM ? SWE : PM,
          via: "search_knowledge",
          count: 3,
          last_at: ago(at - 3_600_000),
        },
      ],
    };
  };
  const pages = [
    page(
      "brief",
      "Product brief",
      PM,
      2 * D + 9 * H,
      "# FairSplit product brief\n\n" +
        "## Who it is for\n\nA group of friends who share costs on a trip or in a flat and want to settle up without a spreadsheet.\n\n" +
        "## The problem\n\nWorking out who owes whom after a dozen shared expenses is tedious and easy to get wrong by a cent.\n\n" +
        "## The MVP\n\n- Record a group's expenses from the command line.\n- Print the fewest payments that settle everyone up, exact to the cent.\n\n" +
        "## Not in the MVP\n\n- Currencies other than one per group.\n- A web or mobile interface.\n",
      { labels: ["mvp"] },
    ),
    page(
      "design-2",
      "Design: recording expenses",
      SWE,
      27.8 * H,
      "# Design: recording expenses (FAIR-2)\n\n" +
        "Money is held as integer cents everywhere. `add` parses the amount with `decimal.Decimal`, refuses anything that is not positive, and stores cents.\n\n" +
        '## Commands\n\n```\nfairsplit add trip miles 42.10 "Groceries"\nfairsplit list trip\n```\n',
      { labels: ["design"] },
    ),
    page(
      "decisions",
      "Decision log",
      PM,
      2 * D + 6 * H,
      "# Decision log\n\n| Date | Question | Decided |\n| --- | --- | --- |\n| Day 1 | Command-line tool or web app? | A command-line tool (Miles) |\n",
    ),
  ];
  return {
    logSeq: 6,
    containers: [
      {
        key: "FAIR",
        name: "FairSplit",
        purpose: "The FairSplit brief, its designs and the decisions behind them.",
        created_at: ago(2 * D + 21 * H),
      },
    ],
    pages,
    hits: pages.map((p) => ({
      id: p.page.id,
      title: p.page.title,
      url: `#/knowledge/pages/${p.page.id}`,
      container: "FAIR",
      snippet: (p.page.body ?? "")
        .replace(/[#`|]/g, "")
        .split("\n")
        .filter(Boolean)
        .slice(1, 3)
        .join(" "),
      updated_at: p.page.updated_at,
    })),
  };
}

// ---- the engine ----------------------------------------------------------------------

function buildEngine(now: Date, ago: (ms: number) => string): DemoCompany["engine"] {
  const coverage = { nodes: [{ id: NODE, answered: true, error: "" }], complete: true };
  const domains = ["tracker", "pages", "vectors", "usage"];
  return {
    health: {
      status: "ok",
      node: NODE,
      configured: true,
      version: "main-d30b6d967",
      started_at: ago(10 * H + 19 * M),
      queue: "jetstream",
      clients: 1,
      event_history_seconds: 30 * 86_400,
      spend_history_seconds: 181 * 86_400,
      in_flight: 0,
      shutting_down: false,
      posture: "serve",
      applied_epoch: 8,
      seats: [PM, SWE],
      // The identity estate's one duty, armed at its hourly interval.
      identity_duty_seconds: { iam_sweep: 3600 },
      nodes: 1,
      alarms: { count: 1, worst: "backup_age" },
      seeded_from: coverage,
    },
    fleet: {
      nodes: [
        {
          id: NODE,
          roles: ["data", "seats", "api"],
          broker: "member",
          owner: `${NODE}.a41f`,
          protocol: 3,
          seats: 2,
          expires_in: 14,
          config_epoch: 8,
          config_status: "applied",
          config_revision_id: fixtureId("revision-8"),
          config_reported_at: ago(28 * H),
          projections_ready: 4,
          projections_total: 4,
          in_flight: 0,
          draining: false,
          started_at: ago(10 * H + 19 * M),
          posture: "serve",
        },
      ],
      seats: [
        {
          handle: PM,
          node: NODE,
          owner: `${NODE}.a41f`,
          epoch: 3,
          expires_in: 14,
          acquired_at: ago(10 * H + 18 * M),
        },
        {
          handle: SWE,
          node: NODE,
          owner: `${NODE}.a41f`,
          epoch: 3,
          expires_in: 14,
          acquired_at: ago(10 * H + 18 * M),
        },
      ],
      duties: [
        { duty: "retention", node: NODE, expires_in: 12 },
        { duty: "maintenance", node: NODE, expires_in: 12 },
        { duty: "embedding", node: NODE, expires_in: 12 },
        { duty: "object-collector", node: NODE, expires_in: 12 },
      ],
      unplaceable: [],
      unmanned_roles: [],
      this_node: NODE,
      target_epoch: 8,
      objects: {
        state: "reported",
        backend: "nats",
        node: NODE,
        collect: {
          at: ago(40 * M),
          completed: true,
          listed: 2,
          aged: 2,
          deleted: 0,
          referenced: 2,
          abandoned: 0,
        },
        audit: {
          at: ago(9 * H),
          completed: true,
          referenced: 2,
          missing: 0,
          damaged: 0,
          found: { at: ago(9 * H), completed: true, referenced: 2, missing: 0, damaged: 0 },
        },
      },
    },
    broker: {
      node: NODE,
      kind: "member",
      external: false,
      nodes: [{ node: NODE, peer: "Nq3Vx8", kind: "member", roles: ["data", "seats", "api"] }],
      group: {
        cluster: "crewlet",
        leader: NODE,
        peers: [{ name: NODE, peer: "Nq3Vx8", self: true, leader: true, current: true, active: 0 }],
      },
      group_from: NODE,
      findings: [],
    },
    retention: {
      v: 1,
      node_id: NODE,
      at: now.toISOString(),
      domains: domains.map((domain, i) => ({
        domain,
        stream: `CREWLET_${domain.toUpperCase()}_LOG`,
        generation: 1,
        replay: "full",
        first_seq: 1,
        last_seq: [212, 41, 96, 74][i]!,
        bytes: [1_840_000, 412_000, 3_100_000, 96_000][i]!,
        max_bytes: 1_073_741_824,
        reserve_bytes: 33_554_432,
        headroom_fraction: 0.998,
        trim_floor: 0,
        trim_to: 0,
        trim_floor_state: "published" as const,
        terms: [
          {
            name: "applied" as const,
            state: "ok" as const,
            seq: [212, 41, 96, 74][i]!,
            remedy: "",
          },
          {
            name: "backup_floor" as const,
            state: "ok" as const,
            seq: 0,
            detail: "No backup covers this log yet.",
            remedy: "Take a backup from Settings, Backups and retention.",
          },
        ],
        blocked_by: "backup_floor" as const,
        blocked_since: ago(3 * D),
        prose:
          "The trim waits for a backup: nothing older than the newest backup is removed, and no backup has been taken.",
      })),
      nodes: [
        {
          node_id: NODE,
          counted: true,
          live: true,
          at: ago(30_000),
          domains: Object.fromEntries(
            domains.map((d, i) => [
              d,
              {
                generation: 1,
                seq: [212, 41, 96, 74][i]!,
                applied_through: [212, 41, 96, 74][i]!,
                generation_state: "current" as const,
                lag: 0,
              },
            ]),
          ),
        },
      ],
      register_readable: true,
      read_level: "stale",
      snapshots: [
        {
          node_id: NODE,
          domains: { tracker: 200, pages: 40, vectors: 90, usage: 70 },
          at: ago(2 * H),
          bytes: 2_400_000,
        },
      ],
      replica: {
        store_bytes: 5_448_000,
        projected_join_seconds: 12,
        rejoin_window_seconds: 604_800,
      },
      alarms: [
        {
          kind: "backup_age",
          detail: "No backup has been taken on this fleet, so every log keeps its whole history.",
          remedy:
            "Take a backup from Settings, Backups and retention, or run crewlet backup on a schedule.",
          domains: domains.map((domain) => ({
            domain,
            detail: "No backup covers this log.",
            term: "backup_floor" as const,
            since: ago(3 * D),
          })),
        },
      ],
    },
    backups: { policy: "engine", points: [], history: [], more: false, coverage },
    mcp: { servers: [], nodes: [{ id: NODE, reported: true }] },
    // THE NODE'S ADDRESS (Tier A), which a serving node is never without: the
    // one the browser reaches the dashboard on and every vendor delivers to.
    external_url: "https://fairsplit.example.com",
    company_writers: [],
    tokens: [],
    credentials: {
      node: NODE,
      fleet: true,
      fleet_error: "",
      providers: [
        {
          key: "default",
          type: "anthropic",
          model: "claude-sonnet-5",
          state: "ready",
          ready: 1,
          rate_limit_seconds: 60,
          auth_seconds: 300,
          keys: [
            {
              ref: "${ANTHROPIC_API_KEY}",
              source: "reference",
              hint: "ends in 7Qa1",
              state: "ready",
              cooling_until: null,
              same_as: 0,
              uses: 412,
              in_flight: 0,
            },
          ],
        },
      ],
    },
  };
}

/** The engine's own tools, as the registry lists them. */
const read = { read_only: "yes", destructive: "no", idempotent: "yes", open_world: "no" } as const;
const write = { read_only: "no", destructive: "no", idempotent: "no", open_world: "no" } as const;
const TOOLS: ToolRow[] = [
  {
    name: "search_knowledge",
    description: "Search the company's pages and work items.",
    source: "builtin",
    annotations: read,
    delivers: "",
  },
  {
    name: "list_work_items",
    description: "List work items with the tracker's filters.",
    source: "builtin",
    annotations: read,
    delivers: "",
  },
  {
    name: "get_work_item",
    description: "Read one work item whole, with its thread.",
    source: "builtin",
    annotations: read,
    delivers: "",
  },
  {
    name: "create_work_item",
    description: "File a work item in a project.",
    source: "builtin",
    annotations: write,
    delivers: "tracker",
  },
  {
    name: "update_work_item",
    description: "Change a work item's status, owner or fields.",
    source: "builtin",
    annotations: write,
    delivers: "tracker",
  },
  {
    name: "comment_on_work_item",
    description: "Comment on a work item, or ask a colleague a question on it.",
    source: "builtin",
    annotations: write,
    delivers: "tracker",
  },
  {
    name: "read_page",
    description: "Read one knowledge-base page.",
    source: "builtin",
    annotations: read,
    delivers: "",
  },
  {
    name: "write_page",
    description: "Write a new knowledge-base page.",
    source: "builtin",
    annotations: write,
    delivers: "pages",
  },
  {
    name: "lookup_colleague",
    description: "Find a colleague's seat from a name.",
    source: "builtin",
    annotations: read,
    delivers: "",
  },
  {
    name: "ask_colleague",
    description: "Ask a colleague one question and wait for the answer.",
    source: "a2a",
    annotations: write,
    delivers: "a2a",
  },
  {
    name: "delegate",
    description: "Hand tasks to short-lived workers and collect their answers.",
    source: "builtin",
    annotations: write,
    delivers: "",
  },
];
